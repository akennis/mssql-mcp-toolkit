package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func openStub(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("stubmssql", "stub")
	if err != nil {
		t.Fatalf("opening stub database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// mustRunQuery runs a query under a row cap alone, which is what the tests
// that predate the byte budgets are about. mustRunBudgeted is for the ones
// that exercise the other two limits.
func mustRunQuery(t *testing.T, query string, limit int) *queryResult {
	t.Helper()
	return mustRunBudgeted(t, query, budget{rows: limit})
}

func mustRunBudgeted(t *testing.T, query string, b budget) *queryResult {
	t.Helper()
	res, err := runQuery(context.Background(), openStub(t), query, b)
	if err != nil {
		t.Fatalf("runQuery(%q): %v", query, err)
	}
	return res
}

// hasNote reports whether any note contains substr.
func hasNote(res *queryResult, substr string) bool {
	for _, n := range res.Notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}

// A stored procedure whose first result set is empty must not be reported as
// having produced nothing: the rows are in the set behind it.
func TestRunQuerySkipsLeadingEmptyResultSets(t *testing.T) {
	for _, c := range []struct {
		query   string
		skipped string
	}{
		{"EXEC dbo.OneEmptyThenRows", "skipped 1 leading empty result set"},
		{"EXEC dbo.TwoEmptyThenRows", "skipped 2 leading empty result set"},
	} {
		res := mustRunQuery(t, c.query, 0)
		if want := []string{"id", "name", "price"}; !equalStrings(res.Columns, want) {
			t.Errorf("%s: columns = %v, want %v", c.query, res.Columns, want)
		}
		if res.RowCount != 2 {
			t.Errorf("%s: row_count = %d, want 2", c.query, res.RowCount)
		}
		if !hasNote(res, c.skipped) {
			t.Errorf("%s: notes = %v, want one mentioning %q", c.query, res.Notes, c.skipped)
		}
		// The old behavior emitted this alongside real rows, which is a
		// contradiction the caller cannot act on.
		if hasNote(res, "no result set") {
			t.Errorf("%s: notes = %v, want no \"no result set\" note when rows were returned", c.query, res.Notes)
		}
	}
}

func TestRunQueryReportsFurtherResultSets(t *testing.T) {
	res := mustRunQuery(t, "EXEC dbo.RowsThenMore", 0)
	if res.RowCount != 2 {
		t.Errorf("row_count = %d, want 2", res.RowCount)
	}
	if !hasNote(res, "1 further result set") {
		t.Errorf("notes = %v, want one reporting the trailing result set", res.Notes)
	}
}

// A batch that genuinely produces nothing still says so.
func TestRunQueryNoResultSet(t *testing.T) {
	res := mustRunQuery(t, "EXEC dbo.NoResultSet", 0)
	if len(res.Columns) != 0 || res.RowCount != 0 {
		t.Errorf("got columns=%v row_count=%d, want an empty result", res.Columns, res.RowCount)
	}
	if !hasNote(res, "no result set") {
		t.Errorf("notes = %v, want one reporting that nothing came back", res.Notes)
	}
	if hasNote(res, "skipped") {
		t.Errorf("notes = %v, want no skip note when there was nothing to skip", res.Notes)
	}
}

// The point of uniqueColumnNames: every column's value reaches the caller,
// even when the server hands back names that collide.
func TestRunQueryKeepsEveryDuplicateColumnValue(t *testing.T) {
	res := mustRunQuery(t, "SELECT DuplicateColumns", 0)
	if len(res.Columns) != 4 {
		t.Fatalf("columns = %v, want 4", res.Columns)
	}
	row := res.Rows[0]
	if len(row) != 4 {
		t.Fatalf("row has %d keys (%v), want 4 - a value was overwritten", len(row), row)
	}
	got := make(map[int64]bool)
	for _, v := range row {
		n, ok := v.(int64)
		if !ok {
			t.Fatalf("row = %v, want int64 values", row)
		}
		got[n] = true
	}
	for _, want := range []int64{1, 2, 3, 4} {
		if !got[want] {
			t.Errorf("value %d is missing from %v", want, row)
		}
	}
}

func TestRunQueryTruncatesAtLimit(t *testing.T) {
	res := mustRunQuery(t, "SELECT id, name, price FROM dbo.Widget", 1)
	if res.RowCount != 1 || !res.Truncated {
		t.Errorf("got row_count=%d truncated=%v, want 1 row and truncated=true", res.RowCount, res.Truncated)
	}
	if !hasNote(res, "truncated at 1 rows") {
		t.Errorf("notes = %v, want one reporting the truncation", res.Notes)
	}
}

func TestRunQueryExecPathReportsRowsAffected(t *testing.T) {
	res := mustRunQuery(t, "UPDATE dbo.Widget SET price = 1", 0)
	if res.RowsAffected != 3 {
		t.Errorf("rows_affected = %d, want 3", res.RowsAffected)
	}
	if !hasNote(res, "executed as a command") {
		t.Errorf("notes = %v, want one explaining the exec path", res.Notes)
	}
}

// A write with an OUTPUT clause has rows to return, so it stays on the query
// path and reports rows_affected as unknown.
func TestRunQueryOutputClauseStaysOnQueryPath(t *testing.T) {
	res := mustRunQuery(t, "DELETE FROM dbo.Widget OUTPUT deleted.id, deleted.name, deleted.price", 0)
	if res.RowCount != 2 {
		t.Errorf("row_count = %d, want the OUTPUT rows", res.RowCount)
	}
	if res.RowsAffected != -1 {
		t.Errorf("rows_affected = %d, want -1 (unknown) on the query path", res.RowsAffected)
	}
}

// USE and SET take effect on one pooled connection only. They go through the
// query path, so a batch that sets an option and then selects still returns
// its rows, and the caller is told the setting will not carry over.
func TestRunQuerySessionScopedStatements(t *testing.T) {
	if res := mustRunQuery(t, "USE OtherDb", 0); !hasNote(res, "does not carry over") {
		t.Errorf("USE: notes = %v, want one about session state", res.Notes)
	}

	res := mustRunQuery(t, "SET NOCOUNT ON; SELECT id, name, price FROM dbo.Widget", 0)
	if res.RowCount != 2 {
		t.Errorf("SET+SELECT: row_count = %d, want 2 - the batch's rows were discarded", res.RowCount)
	}
	if !hasNote(res, "does not carry over") {
		t.Errorf("SET+SELECT: notes = %v, want one about session state", res.Notes)
	}

	if res := mustRunQuery(t, "SELECT id, name, price FROM dbo.Widget", 0); hasNote(res, "does not carry over") {
		t.Errorf("plain SELECT: notes = %v, want no session-state note", res.Notes)
	}
}

func TestRunQueryPropagatesSQLErrors(t *testing.T) {
	_, err := runQuery(context.Background(), openStub(t), "SELECT * FROM dbo.Nope", budget{})
	if err == nil || !strings.Contains(err.Error(), "invalid object name") {
		t.Errorf("runQuery error = %v, want the driver's error", err)
	}
}

// The result restates the statement verbatim - including comments and
// whitespace - so a caller logging the response has the SQL that produced it.
func TestRunQueryEchoesTheExecutedStatement(t *testing.T) {
	for _, query := range []string{
		"  SELECT id, name, price\n  FROM dbo.Widget -- every widget\n",
		"UPDATE dbo.Widget SET price = 1", // the exec path
	} {
		if res := mustRunQuery(t, query, 0); res.Query != query {
			t.Errorf("query = %q, want it restated verbatim as %q", res.Query, query)
		}
	}
}
