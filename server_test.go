package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// End-to-end test over the MCP protocol: a client calls the query tool and gets
// back a structured result set. The database is a stub driver so the test
// needs no SQL Server.

func TestRunQueryToolOverMCP(t *testing.T) {
	ctx := context.Background()
	cs := connectTestClient(t, &config{toolPrefix: "sales", maxRows: 1000, queryTimeout: 5 * time.Second, maxOpenConns: 1})

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) != 3 {
		t.Fatalf("unexpected tool list length: %+v", tools.Tools)
	}
	var hasQuery bool
	for _, tool := range tools.Tools {
		if tool.Name == "sales_query" {
			hasQuery = true
		}
	}
	if !hasQuery {
		t.Fatalf("missing sales_query tool in list: %+v", tools.Tools)
	}

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "sales_query",
		Arguments: map[string]any{"query": "SELECT id, name, price FROM dbo.Widget"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned an error: %s", contentText(res))
	}

	got := decodeResult(t, res)
	if want := []string{"id", "name", "price"}; !equalStrings(got.Columns, want) {
		t.Errorf("columns = %v, want %v", got.Columns, want)
	}
	if got.RowCount != 2 || len(got.Rows) != 2 {
		t.Fatalf("row_count = %d, rows = %v", got.RowCount, got.Rows)
	}
	if got.Rows[0]["name"] != "widget" {
		t.Errorf("rows[0][name] = %v, want widget", got.Rows[0]["name"])
	}
	if got.Rows[0]["price"] != "19.99" { // DECIMAL stays a string
		t.Errorf("rows[0][price] = %v, want 19.99", got.Rows[0]["price"])
	}
	if got.Truncated {
		t.Errorf("result unexpectedly truncated")
	}
	// The text content carries the same rows for clients that only read text,
	// as a markdown table rather than the structured payload encoded a second
	// time: a client honouring both channels would otherwise be charged for
	// every row twice.
	text := contentText(res)
	if !strings.Contains(text, "| id | name | price |") || !strings.Contains(text, "| 1 | widget | 19.99 |") {
		t.Errorf("text content is not a markdown table of the rows:\n%s", text)
	}
	if json.Valid([]byte(text)) {
		t.Errorf("text content is still JSON, duplicating the structured output:\n%s", text)
	}
}

// With the server's caps turned off, max_rows may be left out of the call
// entirely: small models routinely omit it.
func TestRunQueryToolWithoutMaxRowsIsUnlimited(t *testing.T) {
	cs := connectTestClient(t, &config{toolPrefix: "sales", queryTimeout: 5 * time.Second, maxOpenConns: 1})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "sales_query",
		Arguments: map[string]any{"query": "SELECT id, name, price FROM dbo.Widget"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned an error: %s", contentText(res))
	}
	got := decodeResult(t, res)
	if got.RowCount != 2 || got.Truncated {
		t.Errorf("got row_count=%d truncated=%v, want all 2 rows untruncated", got.RowCount, got.Truncated)
	}
}

// With no server-wide cap, a per-call max_rows still applies.
func TestRunQueryToolMaxRowsWithoutServerCap(t *testing.T) {
	cs := connectTestClient(t, &config{toolPrefix: "sales", queryTimeout: 5 * time.Second, maxOpenConns: 1})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "sales_query",
		Arguments: map[string]any{"query": "SELECT id, name, price FROM dbo.Widget", "max_rows": 1},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	got := decodeResult(t, res)
	if got.RowCount != 1 || !got.Truncated {
		t.Errorf("got row_count=%d truncated=%v, want 1 row and truncated=true", got.RowCount, got.Truncated)
	}
}

// Every spelling of "no cap" a model might reach for has to survive schema
// validation and mean the same thing.
func TestRunQueryToolAcceptsEmptyMaxRowsForms(t *testing.T) {
	for _, maxRows := range []any{nil, 0, "0", ""} {
		cs := connectTestClient(t, &config{toolPrefix: "sales", queryTimeout: 5 * time.Second, maxOpenConns: 1})
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "sales_query",
			Arguments: map[string]any{"query": "SELECT id, name, price FROM dbo.Widget", "max_rows": maxRows},
		})
		if err != nil {
			t.Errorf("max_rows=%#v: CallTool: %v", maxRows, err)
			continue
		}
		if res.IsError {
			t.Errorf("max_rows=%#v: tool returned an error: %s", maxRows, contentText(res))
			continue
		}
		if got := decodeResult(t, res); got.RowCount != 2 || got.Truncated {
			t.Errorf("max_rows=%#v: got row_count=%d truncated=%v, want all 2 rows", maxRows, got.RowCount, got.Truncated)
		}
	}
}

// The output budget is on by default. An uncapped result set is read by a
// model with a finite context window, so "no limit" has to be something an
// operator asks for rather than what they get by forgetting to.
func TestLoadConfigDefaultsToABoundedResult(t *testing.T) {
	cfg, err := loadConfig([]string{"--conn-string", "server=localhost", "--tool-prefix", "sales"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.maxRows <= 0 || cfg.maxBytes <= 0 || cfg.maxCellBytes <= 0 {
		t.Errorf("default budget = %d rows/%d bytes/%d cell bytes, want all three capped",
			cfg.maxRows, cfg.maxBytes, cfg.maxCellBytes)
	}

	// 0 stays available as an explicit opt-out.
	cfg, err = loadConfig([]string{"--conn-string", "server=localhost", "--tool-prefix", "sales",
		"--max-rows=0", "--max-bytes=0", "--max-cell-bytes=0"})
	if err != nil {
		t.Fatalf("loadConfig with the caps off: %v", err)
	}
	if cfg.maxRows != 0 || cfg.maxBytes != 0 || cfg.maxCellBytes != 0 {
		t.Errorf("got %d rows/%d bytes/%d cell bytes, want the caps off", cfg.maxRows, cfg.maxBytes, cfg.maxCellBytes)
	}
}

// Two servers differing only in their prefix must expose disjoint tool-name
// sets. This is the whole point of the prefix: a client that aggregates both
// and silently lets one shadow the other would route queries to the wrong
// database with no diagnostic.
func TestToolNamesAreDisjointAcrossPrefixes(t *testing.T) {
	ctx := context.Background()

	names := func(prefix string) map[string]bool {
		cs := connectTestClient(t, &config{queryTimeout: 5 * time.Second, maxOpenConns: 1, toolPrefix: prefix})
		tools, err := cs.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		out := map[string]bool{}
		for _, tool := range tools.Tools {
			out[tool.Name] = true
		}
		return out
	}

	sales, hr := names("sales"), names("hr")
	if !sales["sales_query"] || !hr["hr_query"] {
		t.Fatalf("tool names = %v and %v, want them composed from the prefix", sales, hr)
	}
	for name := range sales {
		if hr[name] {
			t.Errorf("both servers expose %q; the prefix must make the sets disjoint", name)
		}
		// A bare name is the collision the prefix exists to prevent, so no
		// configuration path may reintroduce one.
		if name == "run_query" {
			t.Errorf("a tool is still named %q", name)
		}
	}
}

// A renamed tool is still callable end to end.
func TestPrefixedToolIsCallable(t *testing.T) {
	cs := connectTestClient(t, &config{queryTimeout: 5 * time.Second, maxOpenConns: 1, toolPrefix: "adventureworks"})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "adventureworks_query",
		Arguments: map[string]any{"query": "SELECT id, name, price FROM dbo.Widget"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned an error: %s", contentText(res))
	}
	if got := decodeResult(t, res); got.RowCount != 2 {
		t.Errorf("row_count = %d, want 2", got.RowCount)
	}
}

// The tool description is configurable, and defaults to one naming the
// database from the connection string.
func TestToolDescription(t *testing.T) {
	ctx := context.Background()

	cfg := &config{queryTimeout: 5 * time.Second, maxOpenConns: 1, toolPrefix: "sales",
		connString: "sqlserver://host?database=AdventureWorks"}
	tools, err := connectTestClient(t, cfg).ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var queryTool *mcp.Tool
	for _, tool := range tools.Tools {
		if tool.Name == "sales_query" {
			queryTool = tool
		}
	}
	if queryTool == nil {
		t.Fatalf("missing sales_query tool")
	}
	if got := queryTool.Description; !strings.Contains(got, "AdventureWorks") {
		t.Errorf("default description = %q, want it to name the database", got)
	}

	custom := "Ask the warehouse a question in T-SQL."
	cfg = &config{queryTimeout: 5 * time.Second, maxOpenConns: 1, toolPrefix: "sales", queryDescription: custom, readOnly: true}
	tools, err = connectTestClient(t, cfg).ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var queryTool2 *mcp.Tool
	for _, tool := range tools.Tools {
		if tool.Name == "sales_query" {
			queryTool2 = tool
		}
	}
	if queryTool2 == nil {
		t.Fatalf("missing sales_query tool")
	}
	got := queryTool2.Description
	if !strings.HasPrefix(got, custom) {
		t.Errorf("description = %q, want it to start with the configured text", got)
	}
	if !strings.Contains(got, "read-only mode") {
		t.Errorf("description = %q, want the read-only note appended", got)
	}
}

func TestLoadConfigToolDescriptionDefaultsAndOverrides(t *testing.T) {
	// Unset means "let newServer compose one from the connection string";
	// loadConfig deliberately leaves the field blank rather than resolving it
	// in a second place.
	cfg, err := loadConfig([]string{"--conn-string", "server=localhost", "--tool-prefix", "sales"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !isBlank(cfg.queryDescription) || !isBlank(cfg.getMetadataDescription) || !isBlank(cfg.listMetadataDescription) {
		t.Errorf("got %+v, want every tool description left blank", cfg)
	}

	cfg, err = loadConfig([]string{"--conn-string", "server=localhost", "--tool-prefix", "sales",
		"--query-fn-desc", "custom text",
		"--get-metadata-fn-desc", "custom get text",
		"--list-metadata-fn-desc", "custom list text"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.queryDescription != "custom text" {
		t.Errorf("queryDescription = %q, want the flag value", cfg.queryDescription)
	}
	if cfg.getMetadataDescription != "custom get text" {
		t.Errorf("getMetadataDescription = %q, want the flag value", cfg.getMetadataDescription)
	}
	if cfg.listMetadataDescription != "custom list text" {
		t.Errorf("listMetadataDescription = %q, want the flag value", cfg.listMetadataDescription)
	}
}

// The metadata tools are described on the same terms as the query tool: the
// default names the database, and either can be replaced from the command
// line.
func TestMetadataToolDescriptions(t *testing.T) {
	ctx := context.Background()

	cfg := &config{queryTimeout: 5 * time.Second, maxOpenConns: 1, toolPrefix: "sales",
		connString: "sqlserver://host?database=AdventureWorks"}
	for name, got := range toolDescriptions(t, ctx, cfg) {
		if name == "sales_query" {
			continue
		}
		if !strings.Contains(got, "AdventureWorks") {
			t.Errorf("default %s description = %q, want it to name the database", name, got)
		}
	}

	cfg = &config{queryTimeout: 5 * time.Second, maxOpenConns: 1, toolPrefix: "sales",
		connString:              "sqlserver://host?database=AdventureWorks",
		getMetadataDescription:  "Read the sales data dictionary.",
		listMetadataDescription: "List what the sales data dictionary covers.",
	}
	descriptions := toolDescriptions(t, ctx, cfg)
	if got := descriptions["sales_get_metadata"]; got != cfg.getMetadataDescription {
		t.Errorf("sales_get_metadata description = %q, want the configured text", got)
	}
	if got := descriptions["sales_list_metadata"]; got != cfg.listMetadataDescription {
		t.Errorf("sales_list_metadata description = %q, want the configured text", got)
	}
}

// toolDescriptions is the description advertised for every tool a config
// exposes, keyed by tool name.
func toolDescriptions(t *testing.T, ctx context.Context, cfg *config) map[string]string {
	t.Helper()
	tools, err := connectTestClient(t, cfg).ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := make(map[string]string, len(tools.Tools))
	for _, tool := range tools.Tools {
		got[tool.Name] = tool.Description
	}
	for _, name := range []string{"sales_query", "sales_get_metadata", "sales_list_metadata"} {
		if _, ok := got[name]; !ok {
			t.Fatalf("missing %s tool", name)
		}
	}
	return got
}

func TestRunQueryToolTruncates(t *testing.T) {
	cs := connectTestClient(t, &config{toolPrefix: "sales", maxRows: 1, queryTimeout: 5 * time.Second, maxOpenConns: 1})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "sales_query",
		Arguments: map[string]any{"query": "SELECT id, name, price FROM dbo.Widget"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	got := decodeResult(t, res)
	if got.RowCount != 1 || !got.Truncated || len(got.Notes) == 0 {
		t.Errorf("expected a truncated single-row result, got %+v", got)
	}
}

func TestRunQueryToolReportsSQLErrors(t *testing.T) {
	cs := connectTestClient(t, &config{toolPrefix: "sales", maxRows: 10, queryTimeout: 5 * time.Second, maxOpenConns: 1})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "sales_query",
		Arguments: map[string]any{"query": "SELECT * FROM dbo.Nope"},
	})
	if err != nil {
		t.Fatalf("CallTool returned a protocol error, want a tool error: %v", err)
	}
	if !res.IsError || !strings.Contains(contentText(res), "invalid object name") {
		t.Errorf("expected the SQL error to reach the caller, got IsError=%v %q", res.IsError, contentText(res))
	}
}

func TestRunQueryToolReadOnlyMode(t *testing.T) {
	cs := connectTestClient(t, &config{toolPrefix: "sales", maxRows: 10, queryTimeout: 5 * time.Second, maxOpenConns: 1, readOnly: true})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "sales_query",
		Arguments: map[string]any{"query": "DELETE FROM dbo.Widget"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError || !strings.Contains(contentText(res), "read-only mode") {
		t.Errorf("expected a read-only rejection, got IsError=%v %q", res.IsError, contentText(res))
	}
}

func connectTestClient(t *testing.T, cfg *config) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	db, err := sql.Open("stubmssql", "stub")
	if err != nil {
		t.Fatalf("opening stub database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	server := newServer(cfg, db, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// decodeResult re-decodes the tool's structured output into its Go type.
func decodeResult(t *testing.T, res *mcp.CallToolResult) queryResult {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshaling structured content: %v", err)
	}
	var out queryResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding structured content: %v", err)
	}
	return out
}

// rawContentText is the reply text exactly as the server wrote it, including
// every table's leading Row column.
func rawContentText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// contentText is the reply text with each table's leading Row column taken
// out, so tests of what a result holds do not each repeat the row numbers.
// The tests of the Row column itself read rawContentText.
func contentText(res *mcp.CallToolResult) string { return dropRowColumn(rawContentText(res)) }

// dropRowColumn removes the Row column of a csv table (a header line starting
// "Row," and the data lines after it) and of a markdown table.
func dropRowColumn(text string) string {
	lines := strings.Split(text, "\n")
	inCSV, inMD := false, false
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "Row,"):
			inCSV = true
			lines[i] = l[len("Row,"):]
		case strings.HasPrefix(l, "| Row | "):
			inMD = true
			lines[i] = "| " + l[len("| Row | "):]
		case inCSV && l != "" && !strings.HasPrefix(l, "…") && !strings.HasPrefix(l, "Note:"):
			if _, rest, ok := strings.Cut(l, ","); ok {
				lines[i] = rest
			}
		case inMD && strings.HasPrefix(l, "| "):
			if _, rest, ok := strings.Cut(l[2:], " | "); ok {
				lines[i] = "| " + rest
			} else if strings.HasPrefix(l, "| --- | ") {
				lines[i] = "| " + l[len("| --- | "):]
			}
		default:
			inCSV, inMD = false, false
		}
	}
	return strings.Join(lines, "\n")
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// max_rows is guarded in two places: the JSON Schema the SDK validates
// against, and rowLimit.UnmarshalJSON. Both must reject a negative value
// rather than let it through as "no cap".
func TestRunQueryToolRejectsNegativeMaxRows(t *testing.T) {
	cs := connectTestClient(t, &config{toolPrefix: "sales", queryTimeout: 5 * time.Second, maxOpenConns: 1})

	for _, maxRows := range []any{-1, -500, "-1"} {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "sales_query",
			Arguments: map[string]any{"query": "SELECT id, name, price FROM dbo.Widget", "max_rows": maxRows},
		})
		// Either layer may catch it: a protocol error from schema validation,
		// or a tool error from the decoder. Silently succeeding is the failure.
		if err != nil {
			continue
		}
		if !res.IsError {
			t.Errorf("max_rows=%#v was accepted, want a rejection (got %s)", maxRows, contentText(res))
		}
	}
}

// A statement whose rows sit behind an empty leading result set reaches the
// caller intact over the wire, not just inside runQuery.
func TestRunQueryToolReturnsRowsBehindAnEmptyResultSet(t *testing.T) {
	cs := connectTestClient(t, &config{toolPrefix: "sales", queryTimeout: 5 * time.Second, maxOpenConns: 1})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "sales_query",
		Arguments: map[string]any{"query": "EXEC dbo.OneEmptyThenRows"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned an error: %s", contentText(res))
	}
	if got := decodeResult(t, res); got.RowCount != 2 {
		t.Errorf("row_count = %d, want 2", got.RowCount)
	}
}
