package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The row cap stops a result set that would otherwise be materialized in full
// and then serialized into the model's context window.
func TestBudgetStopsAtTheRowCap(t *testing.T) {
	res := mustRunBudgeted(t, "SELECT id, body FROM dbo.ManyRows", budget{rows: 200})
	if res.RowCount != 200 || len(res.Rows) != 200 {
		t.Errorf("row_count = %d, rows = %d, want 200 of each", res.RowCount, len(res.Rows))
	}
	if !res.Truncated {
		t.Error("truncated = false, want true")
	}
	if !hasNote(res, "truncated at 200 rows") {
		t.Errorf("notes = %v, want one naming the row cap", res.Notes)
	}
}

// Rows can be few and still enormous, so the row cap alone is not a budget.
// These 500 rows are under the row cap and far over the byte one.
func TestBudgetStopsAtThePayloadBudget(t *testing.T) {
	b := budget{rows: defaultMaxRows, bytes: defaultMaxBytes, cellBytes: defaultMaxCellBytes}
	res := mustRunBudgeted(t, "SELECT id, body FROM dbo.WideRows", b)

	if res.RowCount >= defaultMaxRows {
		t.Errorf("row_count = %d, want the byte budget to bite before the %d-row cap", res.RowCount, defaultMaxRows)
	}
	if res.RowCount == 0 {
		t.Fatal("row_count = 0, want the rows that fit within the budget")
	}
	if !res.Truncated || !hasNote(res, "payload budget") {
		t.Errorf("truncated = %v, notes = %v, want the byte budget named", res.Truncated, res.Notes)
	}
	if size := payloadSize(t, res); size > defaultMaxBytes {
		t.Errorf("payload = %d bytes, want it within the %d-byte budget", size, defaultMaxBytes)
	}
}

// A single oversized value can exhaust a context window on its own, which
// neither of the other two limits sees: one row, well inside both.
func TestBudgetCutsAnOversizedCell(t *testing.T) {
	b := budget{rows: defaultMaxRows, bytes: defaultMaxBytes, cellBytes: defaultMaxCellBytes}
	res := mustRunBudgeted(t, "SELECT id, body FROM dbo.HugeCell", b)

	if res.RowCount != 1 {
		t.Fatalf("row_count = %d, want the one row", res.RowCount)
	}
	body, _ := res.Rows[0]["body"].(string)
	if len(body) <= defaultMaxCellBytes || len(body) > defaultMaxCellBytes+64 {
		t.Errorf("cell is %d bytes, want it cut to about the %d-byte limit", len(body), defaultMaxCellBytes)
	}
	if !strings.Contains(body, "[truncated, 1.0 MiB total]") {
		t.Errorf("cell does not say it was cut, or does not give the original size: %q", body[max(0, len(body)-64):])
	}
	if !res.Truncated || !hasNote(res, "were longer than") {
		t.Errorf("truncated = %v, notes = %v, want the cut reported", res.Truncated, res.Notes)
	}
	if size := payloadSize(t, res); size > defaultMaxBytes {
		t.Errorf("payload = %d bytes, want it within the %d-byte budget", size, defaultMaxBytes)
	}
}

// The first row is admitted whatever it costs: a result the caller can read is
// worth more than a budget kept exactly, and zero rows reads like an empty
// table rather than like a truncation.
func TestBudgetAlwaysReturnsTheFirstRow(t *testing.T) {
	res := mustRunBudgeted(t, "SELECT id, body FROM dbo.WideRows", budget{bytes: 10})
	if res.RowCount != 1 {
		t.Errorf("row_count = %d, want the first row despite the budget", res.RowCount)
	}
	if !res.Truncated {
		t.Error("truncated = false, want true")
	}
}

// Nothing cut, nothing claimed: a caller that sees `truncated` treats the
// answer as partial and asks again, so a false positive costs a round trip.
func TestNothingIsTruncatedWhenEverythingFits(t *testing.T) {
	b := budget{rows: defaultMaxRows, bytes: defaultMaxBytes, cellBytes: defaultMaxCellBytes}
	for _, query := range []string{
		"SELECT id, name, price FROM dbo.Widget",
		"UPDATE dbo.Widget SET price = 1",
	} {
		res := mustRunBudgeted(t, query, b)
		if res.Truncated {
			t.Errorf("%s: truncated = true with nothing cut; notes = %v", query, res.Notes)
		}
	}
}

// 0 keeps its meaning from --max-rows across all three limits, so an operator
// who wants everything says so the same way three times.
func TestAZeroBudgetMeansNoLimit(t *testing.T) {
	res := mustRunBudgeted(t, "SELECT id, body FROM dbo.HugeCell", budget{})
	if res.Truncated {
		t.Errorf("truncated = true with every limit off; notes = %v", res.Notes)
	}
	if body, _ := res.Rows[0]["body"].(string); len(body) != 1<<20 {
		t.Errorf("cell is %d bytes, want the whole 1 MiB", len(body))
	}
}

func TestTruncateCell(t *testing.T) {
	if got, cut := truncateCell("short", 64); got != "short" || cut {
		t.Errorf("truncateCell(short) = %q, %v, want it untouched", got, cut)
	}
	if got, cut := truncateCell("exactly ten", 11); got != "exactly ten" || cut {
		t.Errorf("a value at the limit was cut: %q, %v", got, cut)
	}
	// A cut inside a multi-byte rune would produce a string that is not UTF-8,
	// and it is the JSON encoder that would notice rather than this function.
	got, cut := truncateCell(strings.Repeat("é", 100), 21)
	if !cut {
		t.Fatal("a 200-byte value was not cut at 21 bytes")
	}
	if prefix := strings.TrimSuffix(got, "…[truncated, 200 B total]"); prefix != strings.Repeat("é", 10) {
		t.Errorf("cut landed off a rune boundary: %q", prefix)
	}
	if !json.Valid(mustMarshal(t, got)) {
		t.Errorf("cut value does not survive JSON: %q", got)
	}
}

func TestFormatBytes(t *testing.T) {
	for _, c := range []struct {
		in   int
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{4096, "4.0 KiB"},
		{1 << 20, "1.0 MiB"},
		{3 << 30, "3.0 GiB"},
	} {
		if got := formatBytes(c.in); got != c.want {
			t.Errorf("formatBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// payloadSize is the size of the rows as the client will receive them.
func payloadSize(t *testing.T, res *queryResult) int {
	t.Helper()
	return len(mustMarshal(t, res.Rows))
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	return data
}
