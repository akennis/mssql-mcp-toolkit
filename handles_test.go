package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// handleTools is a --query-tools file over the stub scenarios, for the
// stored-result tests.
const handleTools = `
- name: roster
  description: People.
  query: SELECT PeopleRows
  outputFormat: csv
  columns: [id, first, last, title]
- name: nobody
  description: Nobody.
  query: SELECT PeopleNone
  outputFormat: csv
- name: many
  description: Ten thousand rows.
  query: SELECT ManyRows OFFSET @PageSize @PageNumber
  outputFormat: csv
  parameters:
  - {name: PageSize, type: int, required: false}
  - {name: PageNumber, type: int, required: false}
- name: echo
  description: Echoes its arguments.
  query: SELECT EchoArgs FROM STRING_SPLIT(@PersonID, ',') OFFSET @PageSize @PageNumber
  outputFormat: csv
  parameters:
  - {name: PersonID, type: string, literal: true, accepts: [id]}
  - {name: PageSize, type: int, required: false}
  - {name: PageNumber, type: int, required: false}
- name: four_ids
  description: Rows for ids 1-4.
  query: SELECT FourIDs WHERE id IN (SELECT value FROM STRING_SPLIT(@id, ','))
  outputFormat: csv
  parameters:
  - {name: id, type: string, literal: true}
- name: years
  description: Rows with a school year.
  query: SELECT YearRows WHERE SchoolYear IN (SELECT value FROM STRING_SPLIT(@SchoolYear, ','))
  outputFormat: md
  parameters:
  - {name: SchoolYear, type: string, literal: true}
- name: schedules
  description: Schedules.
  query: SELECT Schedules WHERE PersonID IN (SELECT value FROM STRING_SPLIT(@PersonID, ','))
  outputFormat: csv
  parameters:
  - {name: PersonID, type: string, required: false, accepts: [id]}
- name: birthdays
  description: Birthdays.
  query: SELECT Birthdays
  outputFormat: csv
- name: scores
  description: Scores.
  query: SELECT Scores
  outputFormat: csv
- name: total
  description: One number.
  query: SELECT OneScalar FROM STRING_SPLIT(@PersonID, ',')
  outputFormat: scalar
  parameters:
  - {name: PersonID, type: string, accepts: [id]}
- name: literal
  description: Takes a literal name.
  query: SELECT PeopleRows WHERE name = @Name
  outputFormat: csv
  parameters:
  - {name: Name, type: string}
`

// handleClient connects an in-memory client to a server with stored results
// on, and returns it with the server's config.
func handleClient(t *testing.T, yamlText string, tweak func(*config)) (*mcp.ClientSession, *config) {
	t.Helper()
	specs, err := loadQueryTools(writeQueryToolsFile(t, yamlText), builtinToolNames("sales"))
	if err != nil {
		t.Fatalf("loadQueryTools: %v", err)
	}
	cfg := &config{
		toolPrefix: "sales", connString: "stub",
		queryTimeout: 5 * time.Second, maxOpenConns: 1, maxRows: 200, maxBytes: 256 << 10, maxCellBytes: 4096,
		queryTools:  specs,
		resultStore: true, storeMaxRows: defaultStoreMaxRows, storeMaxBytes: defaultStoreMaxBytes,
		storeUserBytes: defaultStoreUserBytes, storeTTL: time.Hour,
	}
	if tweak != nil {
		tweak(cfg)
	}
	db := openStub(t)
	server := newServer(cfg, db, newConnPool(cfg, db))
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	cs, err := client.Connect(context.Background(), clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, cfg
}

var handleLine = regexp.MustCompile(`(?m)^handle: ([a-z]{2}[0-9]+)`)

// handleOf is the handle a reply names on its first line.
func handleOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool error: %s", contentText(res))
	}
	m := handleLine.FindStringSubmatch(contentText(res))
	if m == nil {
		t.Fatalf("reply names no handle:\n%s", contentText(res))
	}
	return m[1]
}

// structured decodes a reply's structured output.
func structured(t *testing.T, res *mcp.CallToolResult) queryToolOutput {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out queryToolOutput
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A small result reads as it always did — every row — with the handle on
// top, and the handle keeps the columns the reply left out.
func TestQueryToolSmallResultIsInlineWithHandle(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	res := callQueryTool(t, cs, "roster", nil)
	text := contentText(res)
	h := handleOf(t, res)
	if !strings.HasPrefix(text, "handle: "+h+" · 3 rows") {
		t.Errorf("reply does not lead with the handle line:\n%s", text)
	}
	if !strings.Contains(text, "id,first,last,title\n1,ada,lovelace,analyst\n2,alan,turing,fellow\n3,grace,hopper,admiral\n") {
		t.Errorf("rows missing or not every column:\n%s", text)
	}
	if strings.Contains(text, "Profile") {
		t.Errorf("a 3-row result was profiled:\n%s", text)
	}
	out := structured(t, res)
	if out.Handle != h || out.TotalRows != 3 || out.RowCount != 3 || out.CSV != "Row,id,first,last,title\n0,1,ada,lovelace,analyst\n1,2,alan,turing,fellow\n2,3,grace,hopper,admiral\n" {
		t.Errorf("structured output = %+v", out)
	}

	// show reads it back, with the columns the reply did not show.
	shown := contentText(callQueryTool(t, cs, "sales_show", map[string]any{"Handle": h, "Columns": "id,first,last,title"}))
	if !strings.Contains(shown, "id,first,last,title") || !strings.Contains(shown, "3,grace,hopper,admiral") {
		t.Errorf("show = %s", shown)
	}
}

// A large result is captured whole — not just the page asked for — and the
// reply shows a few rows from where the page starts, a profile, and a
// trailer saying the rows are not the whole result.
func TestQueryToolCapturesWholeResultAndPages(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	res := callQueryTool(t, cs, "many", map[string]any{"PageSize": 20, "PageNumber": 3})
	text := contentText(res)
	h := handleOf(t, res)
	if !strings.HasPrefix(text, "handle: "+h+" · SHOWING ROWS 40-44 OF 10000") {
		t.Errorf("handle line does not say the rows are partial:\n%.400s", text)
	}
	if !strings.Contains(text, "Profile (10000 rows):") || !strings.Contains(text, "- id: 10000 distinct") {
		t.Errorf("no profile:\n%.600s", text)
	}
	// However large the page asked for, the reply shows a sample of it.
	if !strings.Contains(text, "\n41,xxxxxxxx\n") || !strings.Contains(text, "\n45,xxxxxxxx\n") || strings.Contains(text, "\n46,") {
		t.Errorf("rows 41-45 not shown:\n%.800s", text)
	}
	if !strings.Contains(text, "45,xxxxxxxx\n… rows 40-44 of 10000 shown; 9995 more not shown. Do not count, list or conclude from these rows alone. The full result is "+h) {
		t.Errorf("no trailer right after the last row:\n%.1200s", text)
	}
	out := structured(t, res)
	if out.FirstRow == nil || *out.FirstRow != 40 || out.RowCount != 5 || out.TotalRows != 10000 || !out.Partial {
		t.Errorf("structured = first %v, count %d, total %d, partial %v", out.FirstRow, out.RowCount, out.TotalRows, out.Partial)
	}

	// show pages at the size asked for.
	page := contentText(callQueryTool(t, cs, "sales_show", map[string]any{"Handle": h, "Columns": "id", "PageSize": 20, "PageNumber": 3}))
	if !strings.Contains(page, "SHOWING ROWS 40-59 OF 10000") || !strings.Contains(page, "\n41\n") || !strings.Contains(page, "\n60\n") {
		t.Errorf("show page 3:\n%.600s", page)
	}
	// Sorted, descending.
	top := contentText(callQueryTool(t, cs, "sales_show", map[string]any{"Handle": h, "Columns": "id", "PageSize": 2, "OrderBy": "id DESC"}))
	if !strings.Contains(top, "id\n10000\n9999\n") {
		t.Errorf("show sorted:\n%.600s", top)
	}

	// No page asked for: a sample of a large result, not all of it.
	sample := structured(t, callQueryTool(t, cs, "many", nil))
	if sample.RowCount != sampleRows {
		t.Errorf("unpaged large result showed %d rows, want a %d-row sample", sample.RowCount, sampleRows)
	}
}

// The capture asks the statement for everything up to the store's limit,
// whatever page the caller wanted, and a result cut at the limit is marked.
func TestQueryToolCaptureBindsWholeResultAndMarksTruncation(t *testing.T) {
	cs, _ := handleClient(t, handleTools, func(c *config) { c.storeMaxRows = 100 })
	h := handleOf(t, callQueryTool(t, cs, "echo", map[string]any{"PersonID": "7", "PageSize": 5, "PageNumber": 9}))
	echo := contentText(callQueryTool(t, cs, "sales_show", map[string]any{"Handle": h, "Columns": "name,value"}))
	if !strings.Contains(echo, "PageNumber,1\n") || !strings.Contains(echo, "PageSize,101\n") {
		t.Errorf("capture did not ask for the whole result:\n%s", echo)
	}
	many := callQueryTool(t, cs, "many", map[string]any{"PageSize": 10})
	if text := contentText(many); !strings.Contains(text, "· SHOWING 5 OF 100 ROWS (truncated:") {
		t.Errorf("a cut capture is not marked:\n%.400s", text)
	}
	if !structured(t, many).Truncated {
		t.Error("structured output not marked truncated")
	}
}

// @handle.Column expands to the handle's distinct values, and the stored
// result records the reference, not the ids.
func TestHandleArgumentExpands(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	h := handleOf(t, callQueryTool(t, cs, "roster", map[string]any{"Columns": "id"}))
	res := callQueryTool(t, cs, "echo", map[string]any{"PersonID": "@" + h + ".id"})
	text := contentText(res)
	if !strings.Contains(text, `PersonID,"1,2,3"`) {
		t.Errorf("%s.id did not expand:\n%s", h, text)
	}
	if !strings.Contains(text, "from: "+h+" = roster(") {
		t.Errorf("derived reply does not show where it came from:\n%s", text)
	}
	// Scalar tools take handles too.
	if got := contentText(callQueryTool(t, cs, "total", map[string]any{"PersonID": "@" + h + ".id"})); got != "42" {
		t.Errorf("scalar with a handle = %q", got)
	}
	// A column named after the parameter can be left off.
	h2 := handleOf(t, callQueryTool(t, cs, "years", map[string]any{"SchoolYear": "2025-2026"}))
	if text := contentText(callQueryTool(t, cs, "echo", map[string]any{"PersonID": "@" + h2})); !strings.Contains(text, `PersonID,"1,2,3"`) {
		t.Errorf("@%s (PersonID implied) did not expand:\n%s", h2, text)
	}
}

func TestHandleArgumentErrors(t *testing.T) {
	cs, _ := handleClient(t, handleTools, func(c *config) { c.storeMaxRows = 100 })
	h := handleOf(t, callQueryTool(t, cs, "roster", map[string]any{"Columns": "id"}))
	cut := handleOf(t, callQueryTool(t, cs, "many", nil))
	for _, c := range []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"unknown handle", "echo", map[string]any{"PersonID": "@zz9.id"}, "unknown handle"},
		{"unknown column", "echo", map[string]any{"PersonID": "@" + h + ".nope"}, "is filled from the"},
		{"not a batch parameter", "literal", map[string]any{"Name": "@" + h + ".first"}, "takes a literal value"},
		{"truncated input", "echo", map[string]any{"PersonID": "@" + cut + ".id"}, "is truncated"},
	} {
		res := callQueryTool(t, cs, c.tool, c.args)
		if !res.IsError || !strings.Contains(contentText(res), c.want) {
			t.Errorf("%s: %v %s, want an error mentioning %q", c.name, res.IsError, contentText(res), c.want)
		}
	}
	// AllowPartial accepts the truncated handle knowingly.
	if res := callQueryTool(t, cs, "echo", map[string]any{"PersonID": "@" + cut + ".id", "AllowPartial": true}); res.IsError {
		t.Errorf("AllowPartial: %s", contentText(res))
	}
}

// A long @handle list is sent in chunks, and every id is sent once.
func TestHandleArgumentChunksLongLists(t *testing.T) {
	// The echoed id lists are long cells; let them through whole.
	cs, _ := handleClient(t, handleTools, func(c *config) { c.maxCellBytes = 0 })
	h := handleOf(t, callQueryTool(t, cs, "many", nil))
	res := callQueryTool(t, cs, "echo", map[string]any{"PersonID": "@" + h + ".id", "PageSize": 50})
	if res.IsError {
		t.Fatal(contentText(res))
	}
	echoed := handleOf(t, res)
	all := contentText(callQueryTool(t, cs, "sales_show", map[string]any{"Handle": echoed, "Columns": "name,value", "PageSize": 100}))
	chunks := regexp.MustCompile(`PersonID,"([0-9,]+)"`).FindAllStringSubmatch(all, -1)
	if len(chunks) != 10 {
		t.Fatalf("got %d chunks, want 10:\n%.500s", len(chunks), all)
	}
	seen := map[string]bool{}
	for _, c := range chunks {
		ids := strings.Split(c[1], ",")
		if len(ids) > batchChunk {
			t.Errorf("a chunk carried %d ids, over %d", len(ids), batchChunk)
		}
		for _, id := range ids {
			if seen[id] {
				t.Errorf("id %s sent twice", id)
			}
			seen[id] = true
		}
	}
	if len(seen) != 10000 {
		t.Errorf("sent %d distinct ids, want 10000", len(seen))
	}
}

// The checks a result gets before the model builds on it.
func TestPlausibilityNotes(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	if text := contentText(callQueryTool(t, cs, "nobody", nil)); !strings.Contains(text, "Note: no rows matched") {
		t.Errorf("empty result has no note:\n%s", text)
	}
	// Ids 1-4 go in; the stub answers for all four, so no note...
	h := handleOf(t, callQueryTool(t, cs, "four_ids", map[string]any{"id": "1,2,3,4"}))
	if text := contentText(callQueryTool(t, cs, "four_ids", map[string]any{"id": "@" + h})); strings.Contains(text, "have no rows") {
		t.Errorf("unexpected coverage note:\n%s", text)
	}
	// ...but ids 1-10000 going in and rows for 1-4 coming back is noted.
	big := handleOf(t, callQueryTool(t, cs, "many", nil))
	if text := contentText(callQueryTool(t, cs, "four_ids", map[string]any{"id": "@" + big})); !strings.Contains(text, "9996 of the 10000 id values from "+big+".id have no rows here") {
		t.Errorf("no coverage note:\n%s", text)
	}
	// A row outside the year asked for is called out.
	if text := contentText(callQueryTool(t, cs, "years", map[string]any{"SchoolYear": "2025-2026"})); !strings.Contains(text, "some rows have a SchoolYear other than the one asked for: 2024-2025 (1)") {
		t.Errorf("no scope note:\n%s", text)
	}
}

// Scalars and single picked records are answers, not lists: no handle.
func TestScalarAndPickedResultsAreNotStored(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	res := callQueryTool(t, cs, "total", map[string]any{"PersonID": "1"})
	if strings.Contains(contentText(res), "handle:") {
		t.Errorf("scalar reply has a handle: %s", contentText(res))
	}
}

// SaveAs labels the handle; the label travels into lineage.
func TestSaveAsLabelsTheHandle(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	res := callQueryTool(t, cs, "roster", map[string]any{"SaveAs": "staff list"})
	if text := contentText(res); !regexp.MustCompile(`^handle: [a-z]{2}[0-9]+ "staff list" · 3 rows`).MatchString(text) {
		t.Errorf("label not shown:\n%s", text)
	}
	if structured(t, res).Label != "staff list" {
		t.Error("label not in structured output")
	}
}

// With handles off the replies are exactly what they were.
func TestHandlesOffLeavesRepliesUnchanged(t *testing.T) {
	cs, _ := handleClient(t, handleTools, func(c *config) { c.resultStore = false })
	res := callQueryTool(t, cs, "roster", nil)
	if text := contentText(res); text != "id,first,last,title\n1,ada,lovelace,analyst\n2,alan,turing,fellow\n3,grace,hopper,admiral\n" {
		t.Errorf("reply = %q", text)
	}
	tools, _ := cs.ListTools(context.Background(), nil)
	for _, tool := range tools.Tools {
		if tool.Name == "sales_show" || tool.Name == "sales_filter" {
			t.Errorf("%s is served with handles off", tool.Name)
		}
	}
	if res := callQueryTool(t, cs, "roster", map[string]any{"SaveAs": "x"}); !res.IsError {
		t.Error("SaveAs accepted with handles off")
	}
}

// The built-in query tool stores its result too, and still caps what it
// shows.
func TestBuiltinQueryToolStoresResult(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "sales_query", Arguments: map[string]any{"query": "SELECT ManyRows", "max_rows": 5}})
	if err != nil {
		t.Fatal(err)
	}
	text := contentText(res)
	h := handleOf(t, res)
	if !strings.Contains(text, "handle: "+h+" · SHOWING 5 OF 10000 ROWS") || !strings.Contains(text, "Profile (10000 rows)") ||
		!strings.Contains(text, "only the first 5 of 10000 rows are shown: do not count") {
		t.Errorf("reply:\n%.600s", text)
	}
	var out queryResult
	b, _ := json.Marshal(res.StructuredContent)
	json.Unmarshal(b, &out)
	if out.RowCount != 5 || out.TotalRows != 10000 || out.Handle != h || !out.Truncated {
		t.Errorf("structured = count %d total %d handle %q truncated %v", out.RowCount, out.TotalRows, out.Handle, out.Truncated)
	}
	if shown := contentText(callQueryTool(t, cs, "sales_show", map[string]any{"Handle": h, "Columns": "id", "PageSize": 2, "PageNumber": 5000})); !strings.Contains(shown, "9999\n10000\n") {
		t.Errorf("last page of the stored query result:\n%s", shown)
	}
}

// A model that drops the @ still reaches the handle, as long as the value
// carries this run's handle letters; any other bare value stays literal.
func TestHandleArgumentWithoutAt(t *testing.T) {
	cs, cfg := handleClient(t, handleTools, nil)
	h := handleOf(t, callQueryTool(t, cs, "roster", map[string]any{"Columns": "id"}))
	for _, arg := range []string{h + ".id", strings.ToUpper(h) + ".id"} {
		if text := contentText(callQueryTool(t, cs, "echo", map[string]any{"PersonID": arg})); !strings.Contains(text, `PersonID,"1,2,3"`) {
			t.Errorf("PersonID=%s did not expand:\n%s", arg, text)
		}
	}
	// A bare handle of this run that the caller does not hold is an error,
	// not a literal that quietly matches nothing.
	res := callQueryTool(t, cs, "echo", map[string]any{"PersonID": cfg.results.tag + "99"})
	if !res.IsError || !strings.Contains(contentText(res), "unknown handle") {
		t.Errorf("unknown bare handle: %s", contentText(res))
	}
	// Other letters: a literal id, bound as given.
	other := "zz"
	if cfg.results.tag == other {
		other = "yy"
	}
	if text := contentText(callQueryTool(t, cs, "echo", map[string]any{"PersonID": other + "12"})); !strings.Contains(text, "PersonID,"+other+"12") {
		t.Errorf("a bare non-handle value was not bound literally:\n%s", text)
	}
	// A literal parameter takes a bare handle-shaped value as a value.
	if res := callQueryTool(t, cs, "literal", map[string]any{"Name": h}); res.IsError {
		t.Errorf("literal parameter rejected %q: %s", h, contentText(res))
	}
}

// The handle line shows how to pass the result on, with its own id columns.
func TestHandleLineShowsUsage(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	res := callQueryTool(t, cs, "years", map[string]any{"SchoolYear": "2025-2026"})
	h := handleOf(t, res)
	if !strings.Contains(contentText(res), "handle: "+h+" · 3 rows · pass on as "+h+".PersonID\n") {
		t.Errorf("no usage example:\n%s", contentText(res))
	}
	// A result with no id columns gets none.
	res = callQueryTool(t, cs, "sales_group_aggregate", map[string]any{"Handle": h, "By": "SchoolYear", "Aggregates": "count() AS n"})
	if strings.Contains(contentText(res), "pass on as") {
		t.Errorf("usage example without an id column:\n%s", contentText(res))
	}
}

// The first reply of a large result shows a sample, however large a page was
// asked for, and says so after its last row; a small result is unchanged.
func TestLargeResultShowsASampleWithTrailer(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	res := callQueryTool(t, cs, "many", map[string]any{"PageSize": 100})
	text := contentText(res)
	h := handleOf(t, res)
	if !strings.HasPrefix(text, "handle: "+h+" · SHOWING 5 OF 10000 ROWS") {
		t.Errorf("head:\n%.300s", text)
	}
	if !strings.Contains(text, "5,xxxxxxxx\n… 9995 more rows not shown. The 5 above are a sample: do not count, list or conclude from them. The full result is "+h+": counts are in its profile, pass on as "+h+".id, or page it with sales_show") {
		t.Errorf("trailer:\n%s", text)
	}
	small := contentText(callQueryTool(t, cs, "roster", nil))
	if strings.Contains(small, "SHOWING") || strings.Contains(small, "not shown") {
		t.Errorf("a complete result is marked partial:\n%s", small)
	}
}

// A markdown table that is only part of the result does not end in a row
// count that reads like the size of the result.
func TestPartialMarkdownHasNoRowCount(t *testing.T) {
	cs, _ := handleClient(t, handleTools+`
- name: many_md
  description: Ten thousand rows as a table.
  query: SELECT ManyRows
  outputFormat: md
`, nil)
	text := contentText(callQueryTool(t, cs, "many_md", nil))
	if strings.Contains(text, "row(s).") || !strings.Contains(text, "| 5 | xxxxxxxx |\n… 9995 more rows not shown.") {
		t.Errorf("partial markdown:\n%s", text)
	}
}

// Ids copied out of the rows a partial reply showed are refused, with the
// handle to pass instead; a deliberate subset goes through with AllowPartial.
// The check matches the column named like the parameter (id here, CustomerID
// in a tool whose parameter is a real id column).
func TestCopiedSampleIdsAreRefused(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	h := handleOf(t, callQueryTool(t, cs, "many", nil)) // shows ids 1-5 of 10000
	res := callQueryTool(t, cs, "four_ids", map[string]any{"id": "5, 4,3,2,1"})
	if !res.IsError || !strings.Contains(contentText(res), `parameter "id" lists exactly the 5 id values of `+h+" that were shown, but "+h+" holds 10000 of them") ||
		!strings.Contains(contentText(res), "Pass id="+h+".id") {
		t.Errorf("copied sample ids: %v %s", res.IsError, contentText(res))
	}
	if res := callQueryTool(t, cs, "four_ids", map[string]any{"id": "1,2,3,4,5", "AllowPartial": true}); res.IsError {
		t.Errorf("AllowPartial: %s", contentText(res))
	}
	// Part of the page, more than the page, or two ids, are ordinary - not a
	// copy of a sample, and each call's own (complete, 4-row) result exactly
	// matches four_ids's fixed fixture data, so AllowPartial sidesteps that
	// unrelated self-match rather than testing it here (see
	// TestCopiedCompleteResultIdsAreRefused for that case).
	for _, ids := range []string{"1,2,3", "1,2,3,4,5,6", "1,2"} {
		if res := callQueryTool(t, cs, "four_ids", map[string]any{"id": ids, "AllowPartial": true}); res.IsError {
			t.Errorf("id=%s refused: %s", ids, contentText(res))
		}
	}
	// Paging through with show: the page just read counts as shown too.
	callQueryTool(t, cs, "sales_show", map[string]any{"Handle": h, "Columns": "id", "PageSize": 4, "PageNumber": 3}) // ids 9-12
	if res := callQueryTool(t, cs, "four_ids", map[string]any{"id": "9,10,11,12", "AllowPartial": true}); res.IsError {
		t.Errorf("id=9,10,11,12 refused even with AllowPartial: %s", contentText(res))
	}
}

// A literal id list that is exactly a stored result's whole column is
// refused even when that result came back complete, not just when it was a
// sample of something bigger: seeing every row is not a license to type the
// ids back in rather than pass the handle on.
func TestCopiedCompleteResultIdsAreRefused(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	h := handleOf(t, callQueryTool(t, cs, "roster", nil)) // 3 rows, ids 1,2,3, shown in full
	res := callQueryTool(t, cs, "four_ids", map[string]any{"id": "1,2,3"})
	if !res.IsError ||
		!strings.Contains(contentText(res), `parameter "id" lists exactly the 3 id values that `+h+" holds") ||
		!strings.Contains(contentText(res), "Pass id="+h+".id") ||
		strings.Contains(contentText(res), "were only a sample") {
		t.Errorf("copied complete-result ids: %v %s", res.IsError, contentText(res))
	}
	if res := callQueryTool(t, cs, "four_ids", map[string]any{"id": "1,2,3", "AllowPartial": true}); res.IsError {
		t.Errorf("AllowPartial: %s", contentText(res))
	}
	// A list that isn't the exact set is ordinary.
	if res := callQueryTool(t, cs, "four_ids", map[string]any{"id": "1,2,4"}); res.IsError {
		t.Errorf("id=1,2,4 refused: %s", contentText(res))
	}
}

// A model that writes an argument as markdown prose (**bold**, `code`, quotes)
// still gets its handle read, and it counts as the same call, so the second
// one reuses the first's result.
func TestHandleArgumentToleratesMarkupAndDedups(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	h := handleOf(t, callQueryTool(t, cs, "roster", map[string]any{"Columns": "id"}))
	for _, v := range []string{"@" + h + ".id**", "`@" + h + ".id`", "\"@" + h + ".id\""} {
		res := callQueryTool(t, cs, "echo", map[string]any{"PersonID": v})
		if !strings.Contains(contentText(res), `PersonID,"1,2,3"`) {
			t.Errorf("%q did not expand:\n%s", v, contentText(res))
		}
	}
	first := handleOf(t, callQueryTool(t, cs, "echo", map[string]any{"PersonID": "@" + h + ".id"}))
	second := handleOf(t, callQueryTool(t, cs, "echo", map[string]any{"PersonID": "@" + h + ".id**"}))
	if first != second {
		t.Errorf("a clean and a marked-up reference stored two results: %s and %s", first, second)
	}
}

// A refusal names the reference probably meant but never repeats the value it
// received.
func TestTypedIDsRefusalDoesNotEchoValue(t *testing.T) {
	err := errTypedIDs(&config{}, nil, queryToolParam{Name: "PersonID"}, "md18 and more")
	for _, want := range []string{"PersonID=md18.PersonID", "no ** or quotes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q: %v", want, err)
		}
	}
	for _, bad := range []string{"received", "and more", "@"} {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("refusal contains %q: %v", bad, err)
		}
	}
}

const aliasTools = `
- name: roster
  description: People.
  query: SELECT PeopleRows
  outputFormat: csv
  columns: [id, first, last, title]
- name: byfirst
  description: Looks people up by first name.
  query: SELECT EchoArgs FROM STRING_SPLIT(@Who, ',') OFFSET @PageSize @PageNumber
  outputFormat: csv
  parameters:
  - {name: Who, type: string, accepts: [first]}
  - {name: PageSize, type: int, required: false}
  - {name: PageNumber, type: int, required: false}
`

// A column that is neither the parameter's name nor one it accepts is refused
// before the query runs, and the refusal only says what to do instead.
func TestHandleArgumentWrongIDColumn(t *testing.T) {
	cs, _ := handleClient(t, aliasTools, nil)
	h := handleOf(t, callQueryTool(t, cs, "roster", nil))
	for _, col := range []string{"id", "last", "nope"} {
		res := callQueryTool(t, cs, "byfirst", map[string]any{"Who": "@" + h + "." + col})
		text := contentText(res)
		if !res.IsError || !strings.Contains(text, "is filled from the first column") || !strings.Contains(text, "Who=<newhandle>.first") {
			t.Errorf("column %s: %v %s", col, res.IsError, text)
		}
		for _, echoed := range []string{h, "." + col, "@" + h} {
			if col != "first" && strings.Contains(text, echoed) {
				t.Errorf("column %s: refusal repeats %q:\n%s", col, echoed, text)
			}
		}
	}
	for _, arg := range []string{"@" + h + ".first", "@" + h + ".FIRST", "@" + h} {
		if res := callQueryTool(t, cs, "byfirst", map[string]any{"Who": arg}); res.IsError {
			t.Errorf("%s: %s", arg, contentText(res))
		}
	}
}

func TestAcceptsValidation(t *testing.T) {
	for name, y := range map[string]string{
		"empty entry": `[{name: x, description: d, query: "SELECT @p AS a FROM STRING_SPLIT(@p, ',')", outputFormat: csv, parameters: [{name: p, accepts: [""]}]}]`,
		"not a batch": `[{name: x, description: d, query: "SELECT @p AS a", outputFormat: csv, parameters: [{name: p, batch: false, accepts: [q]}]}]`,
	} {
		if _, err := loadQueryTools(writeQueryToolsFile(t, y), builtinToolNames("sales")); err == nil {
			t.Errorf("%s: loaded without error", name)
		}
	}
}

// A "pass on as" example offers only columns some tool reads.
func TestHandleUsageSkipsCodeColumns(t *testing.T) {
	setPassableColumns([]*queryToolSpec{{Parameters: []queryToolParam{{Name: "SchCourseID", batch: true}, {Name: "StudentDisciplineID", batch: true, Accepts: []string{"IncidentID"}}}}})
	t.Cleanup(func() { setPassableColumns(nil) })
	got := handleUsage(&storedResult{ID: "ab1", Columns: []string{"CourseID", "StaffID", "SchCourseID", "IncidentID"}})
	if got != "pass on as ab1.SchCourseID or ab1.IncidentID" {
		t.Errorf("handleUsage = %q", got)
	}
}

// A result of people lists PersonID first and leaves out scope ids.
func TestHandleUsagePersonFirst(t *testing.T) {
	setPassableColumns([]*queryToolSpec{{Parameters: []queryToolParam{{Name: "PersonID", batch: true}, {Name: "BuildingSchoolLevelID", batch: true}, {Name: "SchCourseID", batch: true}}}})
	t.Cleanup(func() { setPassableColumns(nil) })
	got := handleUsage(&storedResult{ID: "ab1", Columns: []string{"BuildingSchoolLevelID", "PersonID", "SchoolYearStart"}})
	if got != "pass on as ab1.PersonID" {
		t.Errorf("handleUsage = %q", got)
	}
	got = handleUsage(&storedResult{ID: "ab2", Columns: []string{"SchCourseID", "BuildingSchoolLevelID"}})
	if got != "pass on as ab2.SchCourseID or ab2.BuildingSchoolLevelID" {
		t.Errorf("no-person handleUsage = %q", got)
	}
}

const sourceTools = `
- name: find_person
  description: Finds a person.
  query: SELECT p.ID AS PersonID, p.Name FROM P
  outputFormat: csv
- name: list_class
  description: Lists a class.
  query: SELECT x.Person_ID AS PersonID FROM C
  outputFormat: csv
- name: hidden_only
  description: Shows names, stores the id under the handle.
  query: SELECT p.ID AS PersonID, p.Name AS Name FROM P
  outputFormat: csv
  columns: [Name]
- name: unrelated
  description: No ids.
  query: SELECT 1 AS Total
  outputFormat: csv
- name: detail
  description: Detail per person.
  query: SELECT d.Person_ID AS PersonID, d.Note FROM D WHERE d.Person_ID IN (SELECT value FROM STRING_SPLIT(@PersonID, ','))
  outputFormat: csv
  parameters:
  - {name: PersonID, type: string}
`

// Every id refusal lists every other tool that returns the id, the tools that
// start from nothing first, and says nothing about what was passed.
func TestIDRefusalsListEverySource(t *testing.T) {
	cs, _ := handleClient(t, sourceTools, nil)
	h := handleOf(t, callQueryTool(t, cs, "find_person", nil))
	for name, arg := range map[string]any{"wrong column": "@" + h + ".Name", "typed ids": "1,2,3"} {
		res := callQueryTool(t, cs, "detail", map[string]any{"PersonID": arg})
		text := contentText(res)
		want := "Tools that return it: find_person, list_class, hidden_only. Run one of them, then pass PersonID=<newhandle>.PersonID."
		if !res.IsError || !strings.Contains(text, want) {
			t.Errorf("%s: %v\n%s", name, res.IsError, text)
		}
		for _, bad := range []string{"unrelated", "detail,", ".Name"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s: refusal contains %q:\n%s", name, bad, text)
			}
		}
	}
}

func TestSourceListIsCapped(t *testing.T) {
	cfg := &config{}
	for i := 0; i < maxSourcesListed+5; i++ {
		cfg.queryTools = append(cfg.queryTools, &queryToolSpec{Name: fmt.Sprintf("t%02d", i), Query: "SELECT x AS PersonID"})
	}
	got := recoveryClause(cfg, nil, queryToolParam{Name: "PersonID"})
	if !strings.Contains(got, "t11, and 5 more.") || strings.Contains(got, "t12") {
		t.Errorf("clause = %s", got)
	}
}

func TestRowSelPositions(t *testing.T) {
	for _, c := range []struct {
		sel  string
		n    int
		want []int
		err  bool
	}{
		{"0", 5, []int{0}, false},
		{"-1", 5, []int{4}, false},
		{"4", 5, []int{4}, false},
		{"5", 5, nil, true},
		{"-6", 5, nil, true},
		{"1:3", 5, []int{1, 2}, false},
		{":2", 5, []int{0, 1}, false},
		{"3:", 5, []int{3, 4}, false},
		{"-2:", 5, []int{3, 4}, false},
		{":-1", 5, []int{0, 1, 2, 3}, false},
		{":", 3, []int{0, 1, 2}, false},
		{"::2", 5, []int{0, 2, 4}, false},
		{"1::2", 5, []int{1, 3}, false},
		{"::-1", 3, []int{2, 1, 0}, false},
		{"-1:0:-1", 4, []int{3, 2, 1}, false},
		{"3:1:-1", 5, []int{3, 2}, false},
		{"0:100", 3, []int{0, 1, 2}, false},
		{"-100:2", 3, []int{0, 1}, false},
		{"100:", 3, nil, true},
		{"2:1", 5, nil, true},
		{"::0", 5, nil, true},
		{" 1 : 3 ", 5, []int{1, 2}, false},
	} {
		sel, ok := parseRowSel(c.sel)
		if !ok {
			t.Errorf("%q did not parse", c.sel)
			continue
		}
		got, err := sel.positions(c.n)
		if (err != nil) != c.err || fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("[%s] of %d rows = %v, %v; want %v (error %v)", c.sel, c.n, got, err, c.want, c.err)
		}
	}
	for _, bad := range []string{"", "a", "1:2:3:4", "1.5", "Course Name", "-"} {
		if _, ok := parseRowSel(bad); ok {
			t.Errorf("%q parsed as a position", bad)
		}
	}
}

func TestParseHandleRefPositions(t *testing.T) {
	for _, c := range []struct {
		in, id, col, sel string
		ok               bool
	}{
		{"qx4", "qx4", "", "", true},
		{"@qx4.PersonID", "qx4", "PersonID", "", true},
		{"qx4.PersonID[0]", "qx4", "PersonID", "0", true},
		{"@qx4.PersonID[-1]", "qx4", "PersonID", "-1", true},
		{"**qx4.PersonID[0:5]**", "qx4", "PersonID", "0:5", true},
		{"qx4[0].PersonID", "qx4", "PersonID", "0", true},
		{"qx4[1:3].PersonID", "qx4", "PersonID", "1:3", true},
		{"qx4[2]", "qx4", "", "2", true},
		{"qx4.PersonID[ 1 : 3 ]", "qx4", "PersonID", "1:3", true},
		{"qx4.[Course Name]", "qx4", "Course Name", "", true},
		{"qx4.[Course Name][0:2]", "qx4", "Course Name", "0:2", true},
		{"qx4[0].PersonID[1]", "", "", "", false},
		{"qx4.PersonID[x]", "qx4", "PersonID[x", "", true},
		{"qx4[x].PersonID", "", "", "", false},
		{"qx4.", "qx4", "", "", true},
		{"AML2-AH", "", "", "", false},
	} {
		ref, _, ok := parseHandleRef(c.in)
		sel := ""
		if ref.sel != nil {
			sel = ref.sel.text
		}
		if ok != c.ok || (ok && (ref.id != c.id || ref.column != c.col || sel != c.sel)) {
			t.Errorf("%q = %q %q [%s] %v; want %q %q [%s] %v", c.in, ref.id, ref.column, sel, ok, c.id, c.col, c.sel, c.ok)
		}
	}
}

// A handle takes Python-style row positions after its column or before it.
func TestHandleArgumentRowPositions(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	h := handleOf(t, callQueryTool(t, cs, "roster", map[string]any{"Columns": "id"}))
	for arg, want := range map[string]string{
		h + ".id[0]":          `PersonID,1`,
		"@" + h + ".id[-1]":   `PersonID,3`,
		h + ".id[0:2]":        `PersonID,"1,2"`,
		h + ".id[1:]":         `PersonID,"2,3"`,
		h + ".id[::2]":        `PersonID,"1,3"`,
		h + ".id[::-1]":       `PersonID,"3,2,1"`,
		h + ".id[:100]":       `PersonID,"1,2,3"`,
		h + "[1].id":          `PersonID,2`,
		h + "[1:3]":           `PersonID,"2,3"`,
		"**" + h + ".id[0]**": `PersonID,1`,
	} {
		res := callQueryTool(t, cs, "echo", map[string]any{"PersonID": arg})
		if res.IsError || !strings.Contains(contentText(res), want+"\n") {
			t.Errorf("%s: want %s:\n%s", arg, want, contentText(res))
		}
	}
	for arg, want := range map[string]string{
		h + ".id[7]":   "it has 3 rows; use a position from 0 to 2",
		h + ".id[9:]":  "selects no rows of the 3 it has",
		h + ".id[::0]": "step cannot be 0",
	} {
		res := callQueryTool(t, cs, "echo", map[string]any{"PersonID": arg})
		if !res.IsError || !strings.Contains(contentText(res), want) {
			t.Errorf("%s: want error %q:\n%s", arg, want, contentText(res))
		}
	}
	// The same selection twice is the same call; a different one is not.
	a := handleOf(t, callQueryTool(t, cs, "echo", map[string]any{"PersonID": h + ".id[0]"}))
	b := handleOf(t, callQueryTool(t, cs, "echo", map[string]any{"PersonID": h + ".id[1]"}))
	if a == b {
		t.Errorf("different positions stored one result: %s", a)
	}
}

// Every table leads with the 0-based Row column, whatever tool wrote it.
func TestEveryTableLeadsWithRow(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	raw := rawContentText(callQueryTool(t, cs, "roster", nil))
	if !strings.Contains(raw, "\nRow,id,first,last,title\n0,1,ada,lovelace,analyst\n1,2,alan,turing,fellow\n2,3,grace,hopper,admiral\n") {
		t.Errorf("query tool table:\n%s", raw)
	}
	h := handleOf(t, callQueryTool(t, cs, "roster", nil))
	raw = rawContentText(callQueryTool(t, cs, "sales_filter", map[string]any{"Handle": h, "Where": "id > 1"}))
	if !strings.Contains(raw, "Row,id,first,last,title\n0,2,alan") {
		t.Errorf("a derived result numbers its own rows from 0:\n%s", raw)
	}
	big := handleOf(t, callQueryTool(t, cs, "many", nil))
	raw = rawContentText(callQueryTool(t, cs, "sales_show", map[string]any{"Handle": big, "Columns": "id", "PageSize": 3, "PageNumber": 2}))
	if !strings.Contains(raw, "SHOWING ROWS 3-5 OF 10000") || !strings.Contains(raw, "\n3,") || !strings.Contains(raw, "\n5,") {
		t.Errorf("page 2 should number its rows 3-5:\n%.400s", raw)
	}
}

// A result that already has a Row column keeps it and gets a differently named
// position column.
func TestRowColumnNameAvoidsCollision(t *testing.T) {
	if got := rowColumnName([]string{"id", "row"}); got != "RowNumber" {
		t.Errorf("rowColumnName = %q", got)
	}
	if got := rowColumnName([]string{"id"}); got != "Row" {
		t.Errorf("rowColumnName = %q", got)
	}
}
