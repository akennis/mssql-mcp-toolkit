package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// A stub SQL driver, so the tests exercise the real database/sql plumbing —
// multiple result sets, column types, context cancellation — without needing a
// SQL Server. Which result sets a query produces is chosen by a marker word in
// the query text; anything unrecognized gets the default widget result set.

func init() { sql.Register("stubmssql", stubDriver{}) }

// stubSet is one result set: its columns, their SQL Server type names, and its
// rows.
type stubSet struct {
	cols  []string
	types []string
	data  [][]driver.Value
}

// widgetSet is the default result set, used by every test that only needs
// "some rows to come back".
func widgetSet() stubSet {
	return stubSet{
		cols:  []string{"id", "name", "price"},
		types: []string{"INT", "NVARCHAR", "DECIMAL"},
		data: [][]driver.Value{
			{int64(1), "widget", []byte("19.99")},
			{int64(2), "gadget", []byte("4.50")},
		},
	}
}

// emptySet is a result set with no columns at all, the shape SQL Server
// produces for a statement inside a batch that selects nothing.
func emptySet() stubSet { return stubSet{} }

// bigSet is a result set built to overflow something: n rows of one INT and
// one NVARCHAR column holding cellBytes characters. A large n exercises the
// row cap, a large cellBytes the per-cell limit, and a moderate amount of both
// the payload budget in between them.
func bigSet(n, cellBytes int) stubSet {
	set := stubSet{cols: []string{"id", "body"}, types: []string{"INT", "NVARCHAR"}}
	body := strings.Repeat("x", cellBytes)
	for i := range n {
		set.data = append(set.data, []driver.Value{int64(i + 1), body})
	}
	return set
}

// stubScenarios maps a marker word in the query to the result sets it yields.
// Markers are matched case-sensitively against the raw query text; the longest
// match wins, so one marker may contain another.
var stubScenarios = map[string][]stubSet{
	// A stored procedure that assigns before it selects: the rows live in the
	// second result set, behind an empty one.
	"OneEmptyThenRows": {emptySet(), widgetSet()},
	// Two leading empty sets, then the data.
	"TwoEmptyThenRows": {emptySet(), emptySet(), widgetSet()},
	// Rows first, then more sets the caller has to re-query for.
	"RowsThenMore": {widgetSet(), widgetSet()},
	// A batch that produces nothing at all.
	"NoResultSet": {emptySet()},
	// Ten thousand small rows: the row cap is what has to stop this one.
	"ManyRows": {bigSet(10000, 8)},
	// One row holding a megabyte, which the per-cell limit has to cut.
	"HugeCell": {bigSet(1, 1<<20)},
	// Rows too wide to be counted in rows: 500 of these is well over the
	// default payload budget, but well under the default row cap.
	"WideRows": {bigSet(500, 2000)},
	// Three rows whose body is a paragraph: 250, 5000 and 9 characters, the
	// first two in multi-byte characters.
	"LongText": {{
		cols:  []string{"id", "body"},
		types: []string{"INT", "NVARCHAR"},
		data: [][]driver.Value{
			{int64(1), strings.Repeat("é", 250)},
			{int64(2), strings.Repeat("ab", 2500)},
			{int64(3), "short one"},
		},
	}},
	// One row, one column: the shape a scalar query tool wants.
	"OneScalar": {{
		cols:  []string{"total"},
		types: []string{"INT"},
		data:  [][]driver.Value{{int64(42)}},
	}},
	// Several rows, one column: a scalar query tool has to ask which record.
	"ManyScalars": {{
		cols:  []string{"name"},
		types: []string{"NVARCHAR"},
		data:  [][]driver.Value{{"alice"}, {"bob"}, {"carol"}},
	}},
	// Several rows, several columns: what a pick-record tool reviews in full
	// before returning one row's configured fields.
	"PeopleRows": {{
		cols:  []string{"id", "first", "last", "title"},
		types: []string{"INT", "NVARCHAR", "NVARCHAR", "NVARCHAR"},
		data: [][]driver.Value{
			{int64(1), "ada", "lovelace", "analyst"},
			{int64(2), "alan", "turing", "fellow"},
			{int64(3), "grace", "hopper", "admiral"},
		},
	}},
	// One person: a resolver that found exactly the student asked for.
	"OnePerson": {{
		cols:  []string{"PersonID", "first", "last"},
		types: []string{"INT", "NVARCHAR", "NVARCHAR"},
		data:  [][]driver.Value{{int64(7), "ada", "lovelace"}},
	}},
	// The same shape with nothing in it: a search that matched no one.
	"PeopleNone": {{
		cols:  []string{"id", "first", "last", "title"},
		types: []string{"INT", "NVARCHAR", "NVARCHAR", "NVARCHAR"},
	}},
	// Four ids, for checking which of a handle's ids found rows.
	"FourIDs": {{
		cols:  []string{"id"},
		types: []string{"INT"},
		data:  [][]driver.Value{{int64(1)}, {int64(2)}, {int64(3)}, {int64(4)}},
	}},
	// Rows that carry a school year, one of them outside the year asked for.
	"YearRows": {{
		cols:  []string{"PersonID", "SchoolYear"},
		types: []string{"INT", "NVARCHAR"},
		data: [][]driver.Value{
			{int64(1), "2025-2026"}, {int64(2), "2025-2026"}, {int64(3), "2024-2025"},
		},
	}},
	// Student schedules: 1 and 4 are in a homeroom and nothing else, 2 is in
	// a homeroom and English, 3 is in Algebra only, 5 has a course with no
	// name.
	"Schedules": {{
		cols:  []string{"PersonID", "CourseName", "Credits", "StartDate"},
		types: []string{"INT", "NVARCHAR", "DECIMAL", "DATE"},
		data: [][]driver.Value{
			{int64(1), "Homeroom 9", []byte("0.00"), time.Date(2025, 9, 3, 0, 0, 0, 0, time.UTC)},
			{int64(2), "Homeroom 9", []byte("0.00"), time.Date(2025, 9, 3, 0, 0, 0, 0, time.UTC)},
			{int64(2), "English 9", []byte("1.00"), time.Date(2025, 9, 3, 0, 0, 0, 0, time.UTC)},
			{int64(3), "Algebra", []byte("1.00"), time.Date(2025, 9, 4, 0, 0, 0, 0, time.UTC)},
			{int64(4), "HOMEROOM 10", []byte("0.00"), time.Date(2025, 9, 5, 0, 0, 0, 0, time.UTC)},
			{int64(5), nil, []byte("0.50"), time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)},
		},
	}},
	// Birthdates as a timestamp column, and a date column, relative to today:
	// 1 turns 15 today, 2 turns 15 tomorrow, 3 is 16.
	"Birthdays": {{
		cols:  []string{"PersonID", "Birthdate", "Seen"},
		types: []string{"INT", "DATETIME", "DATE"},
		data: [][]driver.Value{
			{int64(1), addMonthsClamped(stubToday, -15*12), stubToday},
			{int64(2), addMonthsClamped(stubToday, -15*12).AddDate(0, 0, 1), stubToday.AddDate(0, 0, -1)},
			{int64(3), addMonthsClamped(stubToday, -16*12), time.Date(2025, 9, 3, 0, 0, 0, 0, time.UTC)},
		},
	}},
	// Scores for the statistics: 10, 20, 30, 40 as decimals.
	"Scores": {{
		cols:  []string{"PersonID", "Score"},
		types: []string{"INT", "DECIMAL"},
		data: [][]driver.Value{
			{int64(1), []byte("10.0")}, {int64(2), []byte("20.0")}, {int64(3), []byte("30.0")}, {int64(4), []byte("40.0")},
		},
	}},
	// Duplicate and anonymous column names, which have to survive the trip
	// through a JSON object without losing a value.
	"DuplicateColumns": {{
		cols:  []string{"a", "a_2", "a", ""},
		types: []string{"INT", "INT", "INT", "INT"},
		data:  [][]driver.Value{{int64(1), int64(2), int64(3), int64(4)}},
	}},
}

// stubToday is today's date the way parseDateArg counts it.
var stubToday, _ = parseDateArg("today", time.Now())

type stubDriver struct{}

func (stubDriver) Open(string) (driver.Conn, error) { return stubConn{}, nil }

type stubConn struct{}

func (stubConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (stubConn) Close() error                        { return nil }
func (stubConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (stubConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := stubDispatch(ctx, query); err != nil {
		return nil, err
	}
	// EchoArgs answers with the arguments it was bound with, one row per
	// named argument in name order, so a test can see what reached SQL.
	if strings.Contains(query, "EchoArgs") {
		set := stubSet{cols: []string{"name", "value"}, types: []string{"NVARCHAR", "NVARCHAR"}}
		sorted := append([]driver.NamedValue(nil), args...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
		for _, a := range sorted {
			v := "NULL"
			if a.Value != nil {
				v = fmt.Sprint(a.Value)
			}
			set.data = append(set.data, []driver.Value{a.Name, v})
		}
		return &stubRows{sets: []stubSet{set}}, nil
	}
	best := ""
	for marker := range stubScenarios {
		if strings.Contains(query, marker) && len(marker) > len(best) {
			best = marker
		}
	}
	if best != "" {
		return &stubRows{sets: stubScenarios[best]}, nil
	}
	return &stubRows{sets: []stubSet{widgetSet()}}, nil
}

func (stubConn) ExecContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if err := stubDispatch(ctx, query); err != nil {
		return nil, err
	}
	return stubResult(3), nil
}

// stubDispatch handles the behaviors that are the same on both the query and
// the exec path: a failing statement, and one that blocks until the caller's
// context ends.
func stubDispatch(ctx context.Context, query string) error {
	if strings.Contains(query, "Nope") {
		return stubError("mssql: invalid object name 'dbo.Nope'")
	}
	if strings.Contains(query, "WAITFOR") {
		<-ctx.Done()
		return ctx.Err()
	}
	return ctx.Err()
}

type stubError string

func (e stubError) Error() string { return string(e) }

type stubResult int64

func (r stubResult) LastInsertId() (int64, error) { return 0, nil }
func (r stubResult) RowsAffected() (int64, error) { return int64(r), nil }

type stubRows struct {
	sets []stubSet
	set  int // index of the current result set
	pos  int // index of the next row within it
}

func (r *stubRows) current() stubSet {
	if r.set >= len(r.sets) {
		return stubSet{}
	}
	return r.sets[r.set]
}

func (r *stubRows) Columns() []string { return r.current().cols }
func (r *stubRows) Close() error      { return nil }

func (r *stubRows) Next(dest []driver.Value) error {
	set := r.current()
	if r.pos >= len(set.data) {
		return io.EOF
	}
	copy(dest, set.data[r.pos])
	r.pos++
	return nil
}

func (r *stubRows) ColumnTypeDatabaseTypeName(i int) string {
	types := r.current().types
	if i >= len(types) {
		return ""
	}
	return types[i]
}

// driver.RowsNextResultSet, so the tests can cover statements that produce
// more than one result set.

func (r *stubRows) HasNextResultSet() bool { return r.set+1 < len(r.sets) }

func (r *stubRows) NextResultSet() error {
	if !r.HasNextResultSet() {
		return io.EOF
	}
	r.set++
	r.pos = 0
	return nil
}
