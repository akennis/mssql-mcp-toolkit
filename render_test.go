package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResultTextIsAMarkdownTable(t *testing.T) {
	res := mustRunQuery(t, "SELECT id, name, price FROM dbo.Widget", 0)
	text, err := resultText(res)
	if err != nil {
		t.Fatalf("resultText: %v", err)
	}

	want := "| id | name | price |\n| --- | --- | --- |\n| 1 | widget | 19.99 |\n| 2 | gadget | 4.50 |\n"
	if !strings.HasPrefix(text, want) {
		t.Errorf("text =\n%s\nwant it to start with\n%s", text, want)
	}
	if !strings.Contains(text, "2 row(s).") {
		t.Errorf("text does not report the row count:\n%s", text)
	}
}

// The text channel used to be the same JSON as the structured output, so a
// client honouring both fed the model every row twice, in the more expensive
// of the two encodings.
func TestResultTextIsCheaperThanTheJSONItReplaced(t *testing.T) {
	res := mustRunBudgeted(t, "SELECT id, body FROM dbo.WideRows", budget{rows: 50})
	text, err := resultText(res)
	if err != nil {
		t.Fatalf("resultText: %v", err)
	}
	indented, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	// Not a token count, but the same thing measured coarsely: the column
	// names are written once here and once per row there.
	if len(text) >= len(indented) {
		t.Errorf("markdown table is %d bytes, the JSON it replaced %d; want materially smaller", len(text), len(indented))
	}
}

// A statement with no result set has no table to draw, and rows_affected and
// the notes are the whole answer, so it keeps the JSON form.
func TestResultTextFallsBackToJSONWithoutColumns(t *testing.T) {
	res := mustRunQuery(t, "UPDATE dbo.Widget SET price = 1", 0)
	text, err := resultText(res)
	if err != nil {
		t.Fatalf("resultText: %v", err)
	}
	if !json.Valid([]byte(text)) {
		t.Errorf("text is not JSON:\n%s", text)
	}
	if !strings.Contains(text, `"rows_affected": 3`) {
		t.Errorf("text does not carry rows_affected:\n%s", text)
	}
}

// A client reading the text channel is reading it instead of the structured
// one, so a truncation reported only there is a truncation it never learns of.
func TestResultTextCarriesTheNotes(t *testing.T) {
	res := mustRunQuery(t, "SELECT id, body FROM dbo.ManyRows", 3)
	text, err := resultText(res)
	if err != nil {
		t.Fatalf("resultText: %v", err)
	}
	if !strings.Contains(text, "Note: result truncated at 3 rows") {
		t.Errorf("text does not carry the truncation note:\n%s", text)
	}
}

func TestFormatCell(t *testing.T) {
	for _, c := range []struct {
		name string
		in   any
		want string
	}{
		{"null is spelled out", nil, "NULL"},
		{"empty string stays empty", "", ""},
		{"number", int64(42), "42"},
		// A raw pipe would end the cell early and shift every column after it.
		{"pipe is escaped", "a|b", `a\|b`},
		{"newline becomes a space", "line one\nline two", "line one line two"},
		{"crlf becomes one space", "a\r\nb", "a b"},
		{"tab becomes a space", "a\tb", "a b"},
	} {
		if got := formatCell(c.in); got != c.want {
			t.Errorf("%s: formatCell(%#v) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
