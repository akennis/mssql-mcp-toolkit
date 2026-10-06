package main

import (
	"strings"
	"testing"
)

const longTextTools = `
- name: paragraphs
  description: Paragraphs.
  query: SELECT LongText
  outputFormat: csv
`

// A value over the limit is shown cut, with how much is missing and the row
// to ask for; a short one is untouched, and the count is in characters.
func TestLongValuesAreCutInAReply(t *testing.T) {
	cs, _ := handleClient(t, longTextTools, func(c *config) { c.displayCellChars = 200 })
	res := callQueryTool(t, cs, "paragraphs", nil)
	text := contentText(res)
	if !strings.Contains(text, strings.Repeat("é", 200)+"…[+50 chars, row 0]") {
		t.Errorf("row 1 not cut to 200 characters:\n%.600s", text)
	}
	if !strings.Contains(text, "…[+4800 chars, row 1]") || !strings.Contains(text, "short one") {
		t.Errorf("rows 1 and 2 wrong:\n%.900s", text)
	}
	if !strings.Contains(text, "Note: 2 value(s) in body are cut to 200 characters. To read one whole, call sales_show_field with Handle="+handleOf(t, res)) {
		t.Errorf("no note on how to read a cut value:\n%s", text)
	}
	if out := structured(t, res); strings.Contains(out.CSV, strings.Repeat("ab", 200)) {
		t.Errorf("structured output carries the whole value")
	}
}

// show_field returns the whole value, in pieces when it is long, and the
// piece it returns is the one asked for.
func TestShowFieldReturnsTheWholeValue(t *testing.T) {
	cs, _ := handleClient(t, longTextTools, func(c *config) { c.displayCellChars = 200 })
	h := handleOf(t, callQueryTool(t, cs, "paragraphs", nil))

	one := contentText(callQueryTool(t, cs, "sales_show_field", map[string]any{"Handle": h, "Row": 0, "Column": "body"}))
	if !strings.Contains(one, "characters 1-250 of 250") || !strings.Contains(one, strings.Repeat("é", 250)) || strings.Contains(one, "more characters") {
		t.Errorf("row 0 = %.300s", one)
	}

	first := contentText(callQueryTool(t, cs, "sales_show_field", map[string]any{"Handle": h, "Row": 1, "Column": "body"}))
	if !strings.Contains(first, "characters 1-4000 of 5000") || !strings.Contains(first, "1000 more characters: call sales_show_field again with Offset=4000") {
		t.Errorf("row 2 first piece = %.200s … %s", first, first[max(0, len(first)-200):])
	}
	rest := contentText(callQueryTool(t, cs, "sales_show_field", map[string]any{"Handle": h, "Row": 1, "Column": "body", "Offset": 4000}))
	if !strings.Contains(rest, "characters 4001-5000 of 5000") || strings.Contains(rest, "more characters") {
		t.Errorf("row 2 last piece = %.200s", rest)
	}

	for name, args := range map[string]map[string]any{
		"row out of range": {"Handle": h, "Row": 9, "Column": "body"},
		"unknown column":   {"Handle": h, "Row": 0, "Column": "nope"},
		"offset past end":  {"Handle": h, "Row": 2, "Column": "body", "Offset": 50},
		"unknown handle":   {"Handle": "zz9", "Row": 0, "Column": "body"},
	} {
		if res := callQueryTool(t, cs, "sales_show_field", args); !res.IsError {
			t.Errorf("%s: want an error, got %s", name, contentText(res))
		}
	}
}

// A re-ordered show still numbers rows by their place in the stored result,
// which is the number show_field takes and the Row column shows.
func TestShowMarkersUseStoredRowNumbers(t *testing.T) {
	cs, _ := handleClient(t, longTextTools, func(c *config) { c.displayCellChars = 200 })
	h := handleOf(t, callQueryTool(t, cs, "paragraphs", nil))
	text := contentText(callQueryTool(t, cs, "sales_show", map[string]any{"Handle": h, "Columns": "id,body", "OrderBy": "id DESC"}))
	if !strings.Contains(text, "…[+4800 chars, row 1]") || !strings.Contains(text, "…[+50 chars, row 0]") {
		t.Errorf("sorted show renumbered the rows:\n%.700s", text)
	}
	if strings.Index(text, "short one") > strings.Index(text, "row 1]") {
		t.Errorf("rows are not in the order asked for:\n%.700s", text)
	}
}

// 0 turns the cut off.
func TestZeroDisplayLimitShowsWhole(t *testing.T) {
	cs, _ := handleClient(t, longTextTools, func(c *config) { c.displayCellChars = 0 })
	text := contentText(callQueryTool(t, cs, "paragraphs", nil))
	if !strings.Contains(text, strings.Repeat("ab", 2500)) || strings.Contains(text, "chars, row") {
		t.Errorf("value was cut with the limit off")
	}
}

// The operators' replies cut values too: a filtered copy of a result does not
// bring the long text back.
func TestOperatorRepliesCutLongValues(t *testing.T) {
	cs, _ := handleClient(t, longTextTools, func(c *config) { c.displayCellChars = 200 })
	h := handleOf(t, callQueryTool(t, cs, "paragraphs", nil))
	res := callQueryTool(t, cs, "sales_filter", map[string]any{"Handle": h, "Where": "id > 1"})
	text := contentText(res)
	if strings.Contains(text, strings.Repeat("ab", 200)) || !strings.Contains(text, "…[+4800 chars, row 0]") {
		t.Errorf("filter reply not cut, or row not its own:\n%.500s", text)
	}
	f := handleOf(t, res)
	whole := contentText(callQueryTool(t, cs, "sales_show_field", map[string]any{"Handle": f, "Row": 0, "Column": "body"}))
	if !strings.Contains(whole, "characters 1-4000 of 5000") {
		t.Errorf("show_field on the derived handle = %.200s", whole)
	}
}

// The Row column is a sorted show's stored position, so the number beside a
// row is the one a [position] and show_field take.
func TestRowColumnIsTheStoredPosition(t *testing.T) {
	cs, _ := handleClient(t, longTextTools, func(c *config) { c.displayCellChars = 200 })
	h := handleOf(t, callQueryTool(t, cs, "paragraphs", nil))
	raw := rawContentText(callQueryTool(t, cs, "sales_show", map[string]any{"Handle": h, "Columns": "id,body", "OrderBy": "id DESC"}))
	lines := strings.Split(raw, "\n")
	var table []string
	for i, l := range lines {
		if strings.HasPrefix(l, "Row,") {
			table = lines[i:]
			break
		}
	}
	if len(table) < 4 || table[0] != "Row,id,body" {
		t.Fatalf("no Row column first:\n%.400s", raw)
	}
	for k, wantRow := range []string{"2,", "1,", "0,"} {
		if !strings.HasPrefix(table[k+1], wantRow) {
			t.Errorf("sorted row %d starts %q, want %q", k, table[k+1], wantRow)
		}
	}
}
