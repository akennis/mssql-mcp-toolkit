package main

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A reply over a stored result shows each value at most --display-cell-chars
// characters long. One incident description is a paragraph, and a table of
// twenty of them fills a small model's window before it has read anything:
// what the reader needs from such a column is to see that it is there and how
// it starts. The stored result keeps the whole value, so nothing is lost —
// the cut value says how much is missing and in which row, and show_field
// returns the rest of that one value.

// shortenCells returns page with every string value in cols longer than limit
// characters cut to limit and followed by a marker, "…[+812 chars, row 3]".
// The row in the marker is the row's place in the stored result, which is what
// show_field takes; rowNo maps a page index to it. Rows are shared with the
// store and never modified: a row that needs a cut is copied first.
//
// It also reports how many values it cut and the columns they were in, in
// column order, for the note that says how to read them whole.
func shortenCells(page []map[string]any, cols []string, limit int, rowNo func(i int) int) ([]map[string]any, int, []string) {
	if limit <= 0 {
		return page, 0, nil
	}
	out := page
	cut := 0
	hit := map[string]bool{}
	for i, row := range page {
		var copied map[string]any
		for _, c := range cols {
			s, ok := row[c].(string)
			// len counts bytes, which is never fewer than the characters, so a
			// value that short is short enough without counting runes.
			if !ok || len(s) <= limit || utf8.RuneCountInString(s) <= limit {
				continue
			}
			if copied == nil {
				if &out[0] == &page[0] {
					out = append([]map[string]any(nil), page...)
				}
				copied = make(map[string]any, len(row))
				for k, v := range row {
					copied[k] = v
				}
				out[i] = copied
			}
			head, n := runePrefix(s, limit)
			copied[c] = fmt.Sprintf("%s…[+%d chars, row %d]", strings.TrimRight(head, " \t\r\n"), n, rowNo(i))
			cut++
			hit[c] = true
		}
	}
	var names []string
	for _, c := range cols {
		if hit[c] {
			names = append(names, c)
		}
	}
	return out, cut, names
}

// runePrefix is the first n characters of s and how many characters follow.
func runePrefix(s string, n int) (string, int) {
	total := utf8.RuneCountInString(s)
	if total <= n {
		return s, 0
	}
	end := 0
	for i := 0; i < n; i++ {
		_, size := utf8.DecodeRuneInString(s[end:])
		end += size
	}
	return s[:end], total - n
}

// rowOrder sorts rows and the stored-result row numbers that go with them
// together.
type rowOrder struct {
	rows []map[string]any
	orig []int
	less func(a, b map[string]any) bool
}

func (o *rowOrder) Len() int           { return len(o.rows) }
func (o *rowOrder) Less(i, j int) bool { return o.less(o.rows[i], o.rows[j]) }
func (o *rowOrder) Swap(i, j int) {
	o.rows[i], o.rows[j] = o.rows[j], o.rows[i]
	o.orig[i], o.orig[j] = o.orig[j], o.orig[i]
}

// storedRowNumber is the 0-based position of the i-th row of r's rows in the
// stored result r was viewed from.
func (r *storedResult) storedRowNumber(i int) int {
	if r.origRow != nil {
		return r.origRow[i]
	}
	return i
}

// showFieldToolName is the show_field tool's name.
func showFieldToolName(cfg *config) string { return prefixedName(cfg, showFieldToolSuffix) }

// fieldChunkChars is the most characters one show_field call returns; a longer
// value is read in pieces with Offset, so a single call cannot spend the
// window either.
const fieldChunkChars = 4000

// registerShowFieldTool adds the show_field tool to a server.
func registerShowFieldTool(mcpServer *mcp.Server, cfg *config, server string) {
	name := showFieldToolName(cfg)
	mcpServer.AddTool(&mcp.Tool{
		Name:  name,
		Title: "Read one full value of a stored result",
		Description: "Read the whole text of one value that a result showed cut off (marked …[+N chars, row R]). " +
			"Give the handle, the row number R from the marker (the same number as the row's Row column) and the column.",
		InputSchema: objectSchema(
			prop{name: "Handle", typ: "string", desc: "The handle the cut value was in (qx4).", required: true},
			prop{name: "Row", typ: "integer", desc: "The row number from the marker, as in the Row column.", required: true},
			prop{name: "Column", typ: "string", desc: "The column the value was in.", required: true},
			prop{name: "Offset", typ: "integer", desc: fmt.Sprintf("Characters to skip; use it to read on when a reply says the value continues (a reply holds %d).", fieldChunkChars)},
		),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: boolPtr(false)},
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return handleShowField(cfg, req, server, name)
	})
}

func handleShowField(cfg *config, req *mcp.CallToolRequest, server, name string) (*mcp.CallToolResult, error) {
	args, err := readArgs(req, "Handle", "Row", "Column", "Offset")
	if err != nil {
		return toolErrorf("%v", err)
	}
	id, err := args.required("Handle")
	if err != nil {
		return toolErrorf("%v", err)
	}
	colName, err := args.required("Column")
	if err != nil {
		return toolErrorf("%v", err)
	}
	row, err := args.integer("Row")
	if err != nil {
		return toolErrorf("%v", err)
	}
	offset, err := args.integer("Offset")
	if err != nil {
		return toolErrorf("%v", err)
	}
	rs, err := callerHandles(cfg, req, true, id)
	if err != nil {
		return toolErrorf("%v", err)
	}
	r := rs[0]
	cols, err := resolveColumns([]string{colName}, r.Columns, "in "+r.ID)
	if err != nil {
		return toolErrorf("%v", err)
	}
	col := cols[0]
	if r.isHidden(col) {
		return toolErrorf("%s is an id kept only to be passed on as %s.%s; it is not shown", col, r.ID, col)
	}
	if row < 0 || row >= len(r.Rows) {
		return toolErrorf("%s has %d rows; Row must be a row number from a marker or the Row column, between 0 and %d", r.ID, len(r.Rows), len(r.Rows)-1)
	}
	if offset < 0 {
		return toolErrorf("Offset must be 0 or more, got %d", offset)
	}
	v := r.Rows[row][col]
	if v == nil {
		return toolErrorf("%s row %d has no value in %s (NULL)", r.ID, row, col)
	}
	text := scalarString(v)
	total := utf8.RuneCountInString(text)
	if offset > total {
		return toolErrorf("%s row %d %s is only %d characters long; Offset %d is past its end", r.ID, row, col, total, offset)
	}
	head, _ := runePrefix(text, offset)
	rest := text[len(head):]
	piece, more := runePrefix(rest, fieldChunkChars)

	var b strings.Builder
	end := offset + utf8.RuneCountInString(piece)
	fmt.Fprintf(&b, "%s row %d %s, characters %d-%d of %d:\n%s\n", r.ID, row, col, offset+1, end, total, piece)
	if more > 0 {
		fmt.Fprintf(&b, "… %d more characters: call %s again with Offset=%d.\n",
			more, callNameOn(cfg, server, name), end)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: b.String()}}}, nil
}
