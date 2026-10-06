package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The tool answers on two channels: the structured queryResult, which is the
// machine-readable one, and text content, for the clients that ignore
// structured output. They used to carry the same JSON, so a client supporting
// both fed the model every row twice — in the more expensive of the two
// encodings, since MarshalIndent spends a line and a key on every value.
//
// The text channel is a markdown table instead. It is what models read most
// reliably, and it costs a fraction of the tokens: the column names are
// written once in the header rather than once per row.

// resultText renders the text half of the tool result.
//
// A result with no columns — an Exec, or a batch that selected nothing — has
// no table to draw, so it keeps the JSON form, where rows_affected and the
// notes are the whole answer.
func resultText(res *queryResult) (string, error) {
	if len(res.Columns) == 0 {
		text, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return "", err
		}
		return string(text), nil
	}

	var b strings.Builder
	writeTableRow(&b, res.Columns)
	separators := make([]string, len(res.Columns))
	for i := range separators {
		separators[i] = "---"
	}
	writeTableRow(&b, separators)

	cells := make([]string, len(res.Columns))
	for _, row := range res.Rows {
		for i, name := range res.Columns {
			cells[i] = formatCell(row[name])
		}
		writeTableRow(&b, cells)
	}

	// The row count and the notes travel with the table, not only with the
	// structured output: a client reading this channel is reading it *instead*
	// of the other one, and "truncated" is the last thing to lose that way.
	fmt.Fprintf(&b, "\n%d row(s).\n", res.RowCount)
	for _, note := range res.Notes {
		fmt.Fprintf(&b, "Note: %s\n", note)
	}
	return b.String(), nil
}

func writeTableRow(b *strings.Builder, cells []string) {
	b.WriteString("| ")
	b.WriteString(strings.Join(cells, " | "))
	b.WriteString(" |\n")
}

// formatCell writes one value as table text.
//
// A pipe or a newline inside a value would end the cell or the row early and
// silently shift every column after it, so both are neutralised. NULL is
// spelled out rather than left blank, because an empty cell is also what an
// empty string looks like and the difference matters when reading a result.
// Nothing else is altered: the structured output is the verbatim channel, but
// this one should not quietly rewrite values either.
func formatCell(v any) string {
	if v == nil {
		return "NULL"
	}
	s, ok := v.(string)
	if !ok {
		s = fmt.Sprint(v)
	}
	return cellReplacer.Replace(s)
}

// A pipe is escaped rather than dropped; the line breaks and tabs become
// spaces, which is the closest a single table row gets to preserving them.
var cellReplacer = strings.NewReplacer("|", `\|`, "\r\n", " ", "\n", " ", "\r", " ", "\t", " ")

// rowColumnBase is the name of the leading column every table carries: the
// row's position, counted from 0 as a handle position in [ ] counts.
const rowColumnBase = "Row"

// rowColumnName is rowColumnBase, or a variant when the result already has a
// column of that name.
func rowColumnName(cols []string) string {
	name := rowColumnBase
	for taken := true; taken; {
		taken = false
		for _, c := range cols {
			if strings.EqualFold(c, name) {
				name += "Number"
				taken = true
				break
			}
		}
	}
	return name
}

// withRowNumbers is res with the row's 0-based position as its first column.
// position maps a row's index in res to its position. The rows are copied,
// never changed: they are shared with the result store. A result with no
// columns has no table, so it is returned as it is.
func withRowNumbers(res *queryResult, position func(i int) int) *queryResult {
	if len(res.Columns) == 0 {
		return res
	}
	name := rowColumnName(res.Columns)
	clone := *res
	clone.Columns = append([]string{name}, res.Columns...)
	clone.Rows = make([]map[string]any, len(res.Rows))
	for i, row := range res.Rows {
		m := make(map[string]any, len(row)+1)
		for k, v := range row {
			m[k] = v
		}
		m[name] = position(i)
		clone.Rows[i] = m
	}
	return &clone
}

// ownPositions numbers a result's rows from 0 in order.
func ownPositions(i int) int { return i }
