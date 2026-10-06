package main

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// The operator engine: an in-memory SQLite database the operator tools run
// their set, filter, grouping and join logic in, so the logic is SQL's —
// NULLs, grouping, joins and distinctness behave as a SQL reader expects —
// rather than a hand-rolled engine's.
//
// A stored result becomes a table the first time an operator reads it, and
// the table is dropped when the store evicts the result. Every value an
// operator is given goes in as a bound parameter, and every column name is
// quoted, so nothing a caller writes becomes SQL text. Text columns are
// declared COLLATE NOCASE, which is how SQL Server's default collation
// compares, so = and GROUP BY agree with the database the rows came from.
//
// The database lives in this process's memory only: it is never a file, and
// it never touches SQL Server.

type calcEngine struct {
	once sync.Once
	db   *sql.DB
	err  error

	// mu serializes all use of the one connection. It is never held while
	// calling into the store, which may call back into drop.
	mu     sync.Mutex
	tables map[*storedResult]string
	seq    int
}

func newCalcEngine() *calcEngine {
	return &calcEngine{tables: map[*storedResult]string{}}
}

// openMemoryDB opens a private in-memory SQLite database on a single
// connection that is never recycled, since recycling it would lose the data.
func openMemoryDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(0)
	db.SetConnMaxLifetime(0)
	return db, nil
}

func (e *calcEngine) open() error {
	e.once.Do(func() { e.db, e.err = openMemoryDB() })
	return e.err
}

// drop removes the table built for r, if there is one. The store calls it on
// eviction.
func (e *calcEngine) drop(r *storedResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	name, ok := e.tables[r]
	if !ok {
		return
	}
	delete(e.tables, r)
	if e.db != nil {
		_, _ = e.db.Exec("DROP TABLE IF EXISTS " + quoteIdent(name))
	}
}

// table returns the table holding r, building it on first use. The caller
// holds e.mu.
func (e *calcEngine) tableLocked(ctx context.Context, r *storedResult) (string, error) {
	if name, ok := e.tables[r]; ok {
		return name, nil
	}
	e.seq++
	name := "h_" + strconv.Itoa(e.seq)
	if err := materialize(ctx, e.db, name, r); err != nil {
		return "", err
	}
	e.tables[r] = name
	return name, nil
}

// materialize creates table name in db and loads r's rows into it.
func materialize(ctx context.Context, db *sql.DB, name string, r *storedResult) error {
	defs := make([]string, len(r.Columns))
	for i, c := range r.Columns {
		defs[i] = quoteIdent(c) + sqliteDecl(r.typeOf(c))
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE "+quoteIdent(name)+" ("+strings.Join(defs, ", ")+")"); err != nil {
		return fmt.Errorf("preparing %s: %w", r.ID, err)
	}
	if len(r.Rows) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(r.Columns)), ", ")
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO "+quoteIdent(name)+" VALUES ("+marks+")")
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	vals := make([]any, len(r.Columns))
	for _, row := range r.Rows {
		for i, c := range r.Columns {
			vals[i] = sqliteValue(row[c], r.typeOf(c))
		}
		if _, err := stmt.ExecContext(ctx, vals...); err != nil {
			tx.Rollback()
			return fmt.Errorf("loading %s: %w", r.ID, err)
		}
	}
	return tx.Commit()
}

// sqliteDecl is the column declaration for a SQL Server type.
func sqliteDecl(t string) string {
	switch {
	case t == "INT" || t == "BIGINT" || t == "SMALLINT" || t == "TINYINT" || t == "BIT":
		return " INTEGER"
	case isNumericType(t):
		return " REAL"
	case isDateType(t):
		return " TEXT"
	case t == "":
		return ""
	default:
		return " TEXT COLLATE NOCASE"
	}
}

// sqliteValue converts a stored value for its column's SQLite affinity. A
// value that does not fit (a decimal that is not a number) goes in as text,
// which is what SQLite would do with it anyway.
func sqliteValue(v any, t string) any {
	if v == nil {
		return nil
	}
	switch {
	case t == "INT" || t == "BIGINT" || t == "SMALLINT" || t == "TINYINT" || t == "BIT":
		switch x := v.(type) {
		case bool:
			if x {
				return int64(1)
			}
			return int64(0)
		case string:
			if n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64); err == nil {
				return n
			}
		}
		return v
	case isNumericType(t):
		if s, ok := v.(string); ok {
			if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
				return f
			}
		}
		return v
	case isDateType(t):
		return normalizeDate(scalarString(v), t)
	}
	switch x := v.(type) {
	case bool:
		if x {
			return int64(1)
		}
		return int64(0)
	case string, int64, float64:
		return x
	default:
		return scalarString(v)
	}
}

// normalizeDate writes a date or timestamp in the form SQLite's date
// functions and a plain text comparison both understand: yyyy-mm-dd for a
// DATE, yyyy-mm-dd hh:mm:ss[.fff] otherwise.
func normalizeDate(s, t string) string {
	s = strings.TrimSpace(s)
	tm, ok := parseTimestamp(s)
	if !ok {
		return s
	}
	if t == "DATE" {
		return tm.Format("2006-01-02")
	}
	return tm.Format("2006-01-02 15:04:05.999999999")
}

// parseTimestamp reads a date or timestamp in any of the forms a stored
// value or a filter literal comes in.
func parseTimestamp(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999", "2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if tm, err := time.Parse(layout, s); err == nil {
			return tm, true
		}
	}
	return time.Time{}, false
}

// quoteIdent quotes a name as a SQLite identifier.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// calcRows is a SQLite result read back: its columns, the SQLite declared
// type of each where there is one, and its rows.
type calcRows struct {
	cols  []string
	decls []string
	rows  []map[string]any
	cut   bool
}

// queryRows runs one statement and reads at most limit rows, reading one more
// to tell a result that is exactly the limit from one that is cut. The caller
// holds the engine's lock, or owns db outright.
func queryRows(ctx context.Context, db *sql.DB, limit int, query string, args ...any) (*calcRows, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	types, _ := rows.ColumnTypes()
	out := &calcRows{cols: uniqueColumnNames(cols), decls: make([]string, len(cols))}
	for i := range cols {
		if i < len(types) {
			out.decls[i] = strings.ToUpper(types[i].DatabaseTypeName())
		}
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if limit > 0 && len(out.rows) >= limit {
			out.cut = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range out.cols {
			switch x := vals[i].(type) {
			case []byte:
				row[c] = string(x)
			default:
				row[c] = x
			}
		}
		out.rows = append(out.rows, row)
	}
	return out, rows.Err()
}

// resultTypes works out the column types of an operator's output: a column
// named like an input column keeps that column's type; any other is typed by
// its values.
func resultTypes(out *calcRows, inputs ...*storedResult) []string {
	types := make([]string, len(out.cols))
	for i, c := range out.cols {
		for _, in := range inputs {
			if col, ok := in.column(c); ok {
				if t := in.typeOf(col); t != "" {
					types[i] = t
					break
				}
			}
		}
		if types[i] != "" {
			continue
		}
		types[i] = inferType(out.rows, c)
	}
	return types
}

// inferType types a derived column by the values in it.
func inferType(rows []map[string]any, col string) string {
	kind := ""
	for _, row := range rows {
		switch row[col].(type) {
		case nil:
			continue
		case int64:
			if kind == "" {
				kind = "BIGINT"
			}
		case float64:
			if kind == "" || kind == "BIGINT" {
				kind = "FLOAT"
			}
		default:
			return "NVARCHAR"
		}
	}
	if kind == "" {
		return "NVARCHAR"
	}
	return kind
}
