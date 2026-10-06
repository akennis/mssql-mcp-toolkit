package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	mssql "github.com/microsoft/go-mssqldb"
)

// runQuery executes one T-SQL statement and materializes its first result set,
// within the caller's budget of rows, payload bytes and bytes per cell.
//
// args, when given, are bound parameters for the statement — normally
// sql.Named values, as the --query-tools handlers pass. The ad-hoc query tool
// calls this with none.
func runQuery(ctx context.Context, db *sql.DB, query string, b budget, args ...any) (*queryResult, error) {
	if execOnly(query) {
		return runExec(ctx, db, query, args...)
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := &queryResult{
		Query:        query,
		Columns:      []string{},
		Rows:         []map[string]any{},
		RowsAffected: -1,
	}

	// Batches and stored procedures can emit several result sets, and the
	// leading ones can be empty (a procedure that assigns before it selects).
	// Skip forward to the first set that actually has columns, rather than
	// committing to set #1 and reporting "no rows" while the data sits in the
	// set behind it.
	skipped := 0
	for {
		cols, err := rows.Columns()
		if err != nil {
			return nil, err
		}
		if len(cols) > 0 {
			if err := scanResultSet(rows, cols, b, res); err != nil {
				return nil, err
			}
			break
		}
		if !rows.NextResultSet() {
			break
		}
		skipped++
	}

	// Whatever follows the returned set is counted so the caller knows to
	// re-query for it.
	extra := 0
	for rows.NextResultSet() {
		extra++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if skipped > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("skipped %d leading empty result set(s); the rows below come from result set %d", skipped, skipped+1))
	}
	if extra > 0 {
		res.Notes = append(res.Notes, fmt.Sprintf("statement produced %d further result set(s); only one is returned", extra))
	}
	if len(res.Columns) == 0 {
		res.Notes = append(res.Notes, "statement returned no result set; rows_affected is not available for this statement form")
	}
	if sessionScoped(query) {
		res.Notes = append(res.Notes, "this statement changes session state (USE/SET); connections are pooled, so the change does not carry over to the next call - qualify names across databases instead, or put the setting in the same batch as the query that needs it")
	}
	return res, nil
}

// runExec handles statements that report an affected-row count instead of a
// result set.
func runExec(ctx context.Context, db *sql.DB, query string, args ...any) (*queryResult, error) {
	out, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	res := &queryResult{
		Query:        query,
		Columns:      []string{},
		Rows:         []map[string]any{},
		RowsAffected: -1,
	}
	if n, err := out.RowsAffected(); err == nil {
		res.RowsAffected = n
	}
	res.Notes = append(res.Notes, "statement executed as a command; no result set was requested")
	return res, nil
}

// scanResultSet materializes the result set rows is currently positioned on,
// stopping at whichever of the budget's limits is reached first. cols is that
// set's column list, already read by the caller.
func scanResultSet(rows *sql.Rows, cols []string, b budget, res *queryResult) error {
	types, err := rows.ColumnTypes()
	if err != nil {
		return err
	}
	res.Columns = uniqueColumnNames(cols)
	res.types = make([]string, len(cols))
	for i, t := range types {
		res.types[i] = strings.ToUpper(t.DatabaseTypeName())
	}

	values := make([]any, len(cols))
	targets := make([]any, len(cols))
	for i := range values {
		targets[i] = &values[i]
	}

	used, cells := 0, 0
	for rows.Next() {
		if b.rows > 0 && len(res.Rows) >= b.rows {
			res.cutRows(fmt.Sprintf("result truncated at %d rows; refine the query (for example with TOP or a WHERE clause) to see more", b.rows))
			break
		}
		if err := rows.Scan(targets...); err != nil {
			return err
		}
		row := make(map[string]any, len(cols))
		rowCuts := 0
		for i, name := range res.Columns {
			value, cut := convertValue(values[i], types[i].DatabaseTypeName(), b.cellBytes)
			if cut {
				rowCuts++
			}
			row[name] = value
		}
		if b.bytes > 0 {
			// The first row is admitted whatever it costs. A result that is
			// one oversized row is still something the caller can read and act
			// on; zero rows and a note about bytes reads like an empty table.
			size := rowSize(row)
			if len(res.Rows) > 0 && used+size > b.bytes {
				res.cutRows(fmt.Sprintf("result truncated at %s of data after %d row(s); the payload budget was reached before the row limit, so select fewer columns or narrow the rows to see more",
					formatBytes(used), len(res.Rows)))
				break
			}
			used += size
		}
		res.Rows = append(res.Rows, row)
		cells += rowCuts
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if cells > 0 {
		res.truncate(fmt.Sprintf("%d cell value(s) were longer than %s and were cut; each one says so in place. Fetch a single oversized value on its own, or slice it with SUBSTRING, to read it in full",
			cells, formatBytes(b.cellBytes)))
	}
	res.RowCount = len(res.Rows)
	return nil
}

// cutRows records that rows were withheld at the row or payload limit, which
// a stored copy has to tell apart from a shortened cell.
func (r *queryResult) cutRows(note string) {
	r.rowsCut = true
	r.rowsCutNote = note
	r.truncate(note)
}

// convertValue turns a driver value into something that survives a JSON
// round-trip without losing meaning, cut to maxCell bytes if it is longer than
// that. It reports whether the value was cut.
//
// Only the text forms can be oversized: a number or a timestamp has a bounded
// encoding, and the point of the cell budget is the NVARCHAR(MAX) column and
// the base64'd blob, either of which can exhaust a context window on its own.
func convertValue(v any, dbType string, maxCell int) (any, bool) {
	switch val := v.(type) {
	case nil:
		return nil, false
	case time.Time:
		return val.Format(time.RFC3339Nano), false
	case string:
		return truncateCell(val, maxCell)
	case []byte:
		converted := convertBytes(val, strings.ToUpper(dbType))
		if s, ok := converted.(string); ok {
			return truncateCell(s, maxCell)
		}
		return converted, false
	default:
		return v, false
	}
}

func convertBytes(b []byte, dbType string) any {
	switch dbType {
	case "UNIQUEIDENTIFIER":
		var u mssql.UniqueIdentifier
		if err := u.Scan(b); err == nil {
			return u.String()
		}
	case "DECIMAL", "NUMERIC", "MONEY", "SMALLMONEY":
		// Kept as a string so precision is not lost to float64.
		return string(b)
	case "BINARY", "VARBINARY", "IMAGE", "TIMESTAMP", "ROWVERSION", "UDT":
		return base64.StdEncoding.EncodeToString(b)
	}
	if utf8.Valid(b) {
		return string(b)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// uniqueColumnNames names anonymous columns and disambiguates duplicates, so
// that no value is lost when a row becomes a JSON object.
//
// A generated name can collide with a real column further along ("a", "a_2",
// "a" would otherwise produce "a_2" twice), so every candidate is checked
// against the names already taken and suffixed until it is free.
func uniqueColumnNames(cols []string) []string {
	taken := make(map[string]bool, len(cols))
	out := make([]string, len(cols))
	for i, name := range cols {
		if name == "" {
			name = fmt.Sprintf("column_%d", i+1)
		}
		base := name
		for n := 2; taken[name]; n++ {
			name = fmt.Sprintf("%s_%d", base, n)
		}
		taken[name] = true
		out[i] = name
	}
	return out
}
