package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/golang-sql/civil"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func writeQueryToolsFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "query-tools.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

func TestLoadQueryToolsValid(t *testing.T) {
	path := writeQueryToolsFile(t, `
- name: widgets_by_id
  description: One widget by id.
  query: SELECT id, name, price FROM dbo.Widget WHERE id = @id
  parameters:
    - name: id
      type: int
      description: widget id
  outputFormat: csv
- name: widget_count
  description: How many widgets.
  query: SELECT COUNT(*) AS n FROM dbo.Widget
  outputFormat: scalar
`)

	specs, err := loadQueryTools(path, builtinToolNames("sales"))
	if err != nil {
		t.Fatalf("loadQueryTools: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("got %d specs, want 2", len(specs))
	}
	if specs[0].format != formatCSV {
		t.Errorf("spec 0 format = %q, want csv", specs[0].format)
	}
	if specs[1].format != formatScalar {
		t.Errorf("spec 1 format = %q, want scalar", specs[1].format)
	}
	if len(specs[0].Parameters) != 1 || specs[0].Parameters[0].Name != "id" {
		t.Errorf("spec 0 parameters = %+v", specs[0].Parameters)
	}
}

// A multi-line block scalar is the reason the file is YAML: a tested statement
// pastes in as-is, newlines and all, instead of being escaped onto one line.
func TestLoadQueryToolsBlockScalarQuery(t *testing.T) {
	specs, err := loadQueryTools(writeQueryToolsFile(t, `
- name: widgets_by_id
  description: One widget by id.
  query: |
    SELECT id, name, price
    FROM dbo.Widget
    WHERE id = @id
  parameters:
    - name: id
      type: int
  outputFormat: csv
`), builtinToolNames("sales"))
	if err != nil {
		t.Fatalf("loadQueryTools: %v", err)
	}
	if !strings.Contains(specs[0].Query, "\n") || !strings.Contains(specs[0].Query, "WHERE id = @id") {
		t.Errorf("block scalar query not preserved: %q", specs[0].Query)
	}
}

func TestLoadQueryToolsRejects(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty list", `[]`, "defines no tools"},
		{"missing name", `[{description: d, query: "SELECT 1 AS a", outputFormat: csv}]`, "name is required"},
		{"bad name", `[{name: "a b", description: d, query: "SELECT 1 AS a", outputFormat: csv}]`, "letters, digits"},
		{"reserved name", `[{name: sales_query, description: d, query: "SELECT 1 AS a", outputFormat: csv}]`, "built-in"},
		{"duplicate name", `[{name: dup, description: d, query: "SELECT 1 AS a", outputFormat: csv}, {name: dup, description: d, query: "SELECT 1 AS a", outputFormat: csv}]`, "more than once"},
		{"missing description", `[{name: x, query: "SELECT 1 AS a", outputFormat: csv}]`, "description is required"},
		{"missing query", `[{name: x, description: d, outputFormat: csv}]`, "query is required"},
		{"bad format", `[{name: x, description: d, query: "SELECT 1 AS a", outputFormat: xml}]`, "unknown outputFormat"},
		{"missing format", `[{name: x, description: d, query: "SELECT 1 AS a"}]`, "outputFormat is required"},
		{"result column without scalar", `[{name: x, description: d, query: "SELECT 1 AS a", outputFormat: csv, resultColumn: a}]`, "only applies when outputFormat"},
		{"unused parameter", `[{name: x, description: d, query: "SELECT 1 AS a", outputFormat: csv, parameters: [{name: p}]}]`, "never uses @p"},
		{"bad parameter type", `[{name: x, description: d, query: "SELECT @p AS a", outputFormat: csv, parameters: [{name: p, type: datetime}]}]`, "unknown type"},
		{"duplicate parameter", `[{name: x, description: d, query: "SELECT @p AS a", outputFormat: csv, parameters: [{name: p}, {name: p}]}]`, "declared more than once"},
		{"unknown field", `[{name: x, description: d, query: "SELECT 1 AS a", outputFormat: csv, bogus: true}]`, "bogus"},
		{"bare mapping is a single tool", `{name: x}`, "description is required"},
		{"scalar root", `just a string`, "defines no tools"},
		{"unknown wrapper field", `{tools: [], bogus: true}`, "reading"},
		{"empty column", `[{name: x, description: d, query: "SELECT 1 AS a", outputFormat: csv, columns: [a, ""]}]`, "columns[1] is empty"},
		{"duplicate column", `[{name: x, description: d, query: "SELECT 1 AS a", outputFormat: csv, columns: [a, A]}]`, "listed more than once"},
		{"columns with scalar", `[{name: x, description: d, query: "SELECT 1 AS a", outputFormat: scalar, columns: [a]}]`, "columns applies to outputFormat"},
		{"pickRecord with scalar", `[{name: x, description: d, query: "SELECT 1 AS a", outputFormat: scalar, pickRecord: true}]`, "pickRecord applies to outputFormat"},
		{"reserved Columns parameter", `[{name: x, description: d, query: "SELECT @Columns AS a", outputFormat: csv, parameters: [{name: Columns}]}]`, "is reserved"},
		{"requireAnyOf undeclared", `[{name: x, description: d, query: "SELECT @p AS a", outputFormat: csv, parameters: [{name: p, required: false}], requireAnyOf: [q]}]`, "not a declared parameter"},
		{"requireAnyOf required param", `[{name: x, description: d, query: "SELECT @p AS a", outputFormat: csv, parameters: [{name: p}], requireAnyOf: [p]}]`, "already required"},
		{"requireAnyOf duplicate", `[{name: x, description: d, query: "SELECT @p AS a", outputFormat: csv, parameters: [{name: p, required: false}], requireAnyOf: [p, P]}]`, "more than once"},
		{"requireAnyOf empty", `[{name: x, description: d, query: "SELECT @p AS a", outputFormat: csv, parameters: [{name: p, required: false}], requireAnyOf: [""]}]`, "requireAnyOf[0] is empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadQueryTools(writeQueryToolsFile(t, c.body), builtinToolNames("sales"))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

// The example file shipped in the repo has to stay loadable, or it teaches the
// wrong shape.
func TestExampleQueryToolsFileIsValid(t *testing.T) {
	specs, err := loadQueryTools("query-tools.example.yaml", builtinToolNames("sales"))
	if err != nil {
		t.Fatalf("query-tools.example.yaml failed to load: %v", err)
	}
	byFormat := map[outputFormat]int{}
	var haveColumns, havePickRecord, haveConnString, haveRequireAnyOf bool
	for _, s := range specs {
		byFormat[s.format]++
		haveColumns = haveColumns || len(s.Columns) > 0
		havePickRecord = havePickRecord || s.PickRecord != pickRecordOff
		haveConnString = haveConnString || s.ConnectionString != ""
		haveRequireAnyOf = haveRequireAnyOf || len(s.RequireAnyOf) > 0
	}
	// The example is meant to demonstrate every feature.
	for _, f := range []outputFormat{formatCSV, formatMD, formatScalar} {
		if byFormat[f] == 0 {
			t.Errorf("example file has no %q tool", f)
		}
	}
	if !haveColumns || !havePickRecord || !haveConnString || !haveRequireAnyOf {
		t.Errorf("example file should demonstrate columns (%v), pickRecord (%v), connectionString (%v) and requireAnyOf (%v)",
			haveColumns, havePickRecord, haveConnString, haveRequireAnyOf)
	}
}

func TestLoadQueryToolsMissingFile(t *testing.T) {
	_, err := loadQueryTools(filepath.Join(t.TempDir(), "nope.yaml"), nil)
	if err == nil || !strings.Contains(err.Error(), "--query-tools") {
		t.Fatalf("error = %v, want a --query-tools file error", err)
	}
}

func TestLoadConfigReadsQueryToolsFlag(t *testing.T) {
	path := writeQueryToolsFile(t, `
- name: widget_count
  description: How many widgets.
  query: SELECT COUNT(*) AS n FROM dbo.Widget
  outputFormat: scalar
`)

	cfg, err := loadConfig([]string{
		"--conn-string", "sqlserver://localhost?database=AdventureWorks",
		"--tool-prefix", "sales",
		"--query-tools", path,
	})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if len(cfg.queryTools) != 1 || cfg.queryTools[0].Name != "widget_count" {
		t.Fatalf("cfg.queryTools = %+v", cfg.queryTools)
	}
}

func TestLoadConfigQueryToolsOnlyRequiresQueryTools(t *testing.T) {
	_, err := loadConfig([]string{
		"--conn-string", "sqlserver://localhost?database=AdventureWorks",
		"--tool-prefix", "sales",
		"--query-tools-only",
	})
	if err == nil || !strings.Contains(err.Error(), "--query-tools-only needs --query-tools") {
		t.Fatalf("error = %v, want --query-tools-only without --query-tools to be rejected", err)
	}
}

func TestLoadConfigQueryToolsOnlyAllowsMissingPrefix(t *testing.T) {
	path := writeQueryToolsFile(t, `
- name: widgets
  description: All widgets.
  query: SELECT id, name, price FROM dbo.Widget
  outputFormat: csv
`)

	cfg, err := loadConfig([]string{
		"--conn-string", "sqlserver://localhost?database=AdventureWorks",
		"--query-tools", path,
		"--query-tools-only",
	})
	if err != nil {
		t.Fatalf("loadConfig with --query-tools-only and no --tool-prefix: %v", err)
	}
	if cfg.toolPrefix != "" {
		t.Errorf("toolPrefix = %q, want empty", cfg.toolPrefix)
	}

	// Without --query-tools-only the missing prefix is still fatal.
	if _, err := loadConfig([]string{
		"--conn-string", "sqlserver://localhost?database=AdventureWorks",
		"--query-tools", path,
	}); err == nil {
		t.Fatal("loadConfig without --tool-prefix or --query-tools-only should fail")
	}
}

func TestQueryToolsOnlyDropsBuiltinTools(t *testing.T) {
	path := writeQueryToolsFile(t, `
- name: widgets
  description: All widgets.
  query: SELECT id, name, price FROM dbo.Widget
  outputFormat: csv
`)

	specs, err := loadQueryTools(path, nil)
	if err != nil {
		t.Fatalf("loadQueryTools: %v", err)
	}
	cfg := &config{
		connString:     "stub",
		queryTimeout:   5 * time.Second,
		maxOpenConns:   1,
		maxRows:        100,
		queryTools:     specs,
		queryToolsOnly: true,
	}

	cs := connectTestClient(t, cfg)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	// The custom tool and the describe tool that rides along with any query
	// tool: no <prefix>_query, no metadata tools. With no prefix the describe
	// tool is registered under its bare suffix.
	if want := []string{"describe_tool", "widgets"}; !reflect.DeepEqual(names, want) {
		t.Errorf("tool list = %v, want exactly %v under --query-tools-only", names, want)
	}
}

func TestLoadConfigRejectsQueryToolShadowingBuiltin(t *testing.T) {
	path := writeQueryToolsFile(t, `
- name: sales_query
  description: d
  query: SELECT 1 AS a
  outputFormat: csv
`)

	_, err := loadConfig([]string{
		"--conn-string", "sqlserver://localhost?database=AdventureWorks",
		"--tool-prefix", "sales",
		"--query-tools", path,
	})
	if err == nil || !strings.Contains(err.Error(), "built-in") {
		t.Fatalf("error = %v, want a shadowing complaint", err)
	}
}

func TestLoadQueryToolsColumnsAndPickRecord(t *testing.T) {
	specs, err := loadQueryTools(writeQueryToolsFile(t, `
- name: one_person
  description: One person.
  query: SELECT id, first, last, title FROM dbo.PeopleRows WHERE last = @last
  parameters:
    - name: last
      type: string
  outputFormat: csv
  columns: [id, last]
  pickRecord: true
`), builtinToolNames("sales"))
	if err != nil {
		t.Fatalf("loadQueryTools: %v", err)
	}
	s := specs[0]
	if !s.PickRecord.always() || len(s.Columns) != 2 || s.Columns[0] != "id" || s.Columns[1] != "last" {
		t.Fatalf("spec = %+v", s)
	}
}

func TestResolveColumns(t *testing.T) {
	have := []string{"OrderID", "OrderDate", "TotalDue"}

	got, err := resolveColumns([]string{"totaldue", "orderid"}, have, "in the result set")
	if err != nil {
		t.Fatalf("resolveColumns: %v", err)
	}
	if len(got) != 2 || got[0] != "TotalDue" || got[1] != "OrderID" {
		t.Errorf("resolveColumns = %v, want [TotalDue OrderID] (result-set spelling, requested order)", got)
	}

	if all, _ := resolveColumns(nil, have, "in the result set"); len(all) != 3 {
		t.Errorf("resolveColumns(nil, ...) = %v, want all columns", all)
	}

	if _, err := resolveColumns([]string{"Nope"}, have, "in the result set"); err == nil || !strings.Contains(err.Error(), "not in the result set") {
		t.Errorf("resolveColumns with an unknown column: err = %v", err)
	}
}

func TestReferencedParams(t *testing.T) {
	got := referencedParams("SELECT @a, @B FROM t WHERE c = @a AND d = @@ROWCOUNT -- @nope\n AND e = '@alsonope'")
	for _, want := range []string{"a", "b"} {
		if !got[want] {
			t.Errorf("referencedParams missing %q (got %v)", want, got)
		}
	}
	for _, notWant := range []string{"nope", "alsonope", "rowcount"} {
		if got[notWant] {
			t.Errorf("referencedParams should not contain %q (got %v)", notWant, got)
		}
	}
}

func TestCoerceParam(t *testing.T) {
	cases := []struct {
		typ  string
		in   any
		want any
		ok   bool
	}{
		{"int", float64(5), int64(5), true},
		{"int", "7", int64(7), true},
		{"int", float64(5.5), nil, false},
		{"int", "x", nil, false},
		{"number", float64(1.5), float64(1.5), true},
		{"number", "2.5", float64(2.5), true},
		{"bool", true, true, true},
		{"bool", "true", true, true},
		{"bool", "nope", nil, false},
		{"string", "hi", "hi", true},
		{"string", float64(3), "3", true},
		{"date", "2026-09-03", civil.Date{Year: 2026, Month: 9, Day: 3}, true},
		{"date", "2026-13-03", nil, false},
		{"date", "next week", nil, false},
		{"date", float64(20260903), nil, false},
	}
	for _, c := range cases {
		got, err := coerceParam(queryToolParam{Name: "p", Type: c.typ}, c.in)
		if c.ok {
			if err != nil || got != c.want {
				t.Errorf("coerceParam(%s, %v) = %v, %v; want %v", c.typ, c.in, got, err, c.want)
			}
		} else if err == nil {
			t.Errorf("coerceParam(%s, %v) = %v, nil; want an error", c.typ, c.in, got)
		}
	}
}

func TestParseDateArg(t *testing.T) {
	// A Tuesday, late in the day, in a non-UTC zone: the result must be the
	// calendar day at midnight regardless of the hour or zone of "now".
	now := time.Date(2026, 3, 31, 22, 15, 0, 0, time.FixedZone("EDT", -4*3600))
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	cases := []struct {
		in   string
		want time.Time
	}{
		{"2025-09-03", day(2025, 9, 3)},
		{" 2025-09-03 ", day(2025, 9, 3)},
		{"today", day(2026, 3, 31)},
		{"Today", day(2026, 3, 31)},
		{"yesterday", day(2026, 3, 30)},
		{"tomorrow", day(2026, 4, 1)},
		{"0d", day(2026, 3, 31)},
		{"-7d", day(2026, 3, 24)},
		{"-7D", day(2026, 3, 24)},
		{"+3d", day(2026, 4, 3)},
		{"3d", day(2026, 4, 3)},
		{"-2w", day(2026, 3, 17)},
		{"-1m", day(2026, 2, 28)}, // clamped to month end, not normalized to Mar 3
		{"+1m", day(2026, 4, 30)},
		{"-3m", day(2025, 12, 31)}, // crosses a year boundary
		{"-1y", day(2025, 3, 31)},
		{"-45d", day(2026, 2, 14)}, // crosses a month boundary
	}
	for _, c := range cases {
		got, err := parseDateArg(c.in, now)
		if err != nil {
			t.Errorf("parseDateArg(%q): %v", c.in, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("parseDateArg(%q) = %s; want %s", c.in, got.Format("2006-01-02"), c.want.Format("2006-01-02"))
		}
	}
	// A leap day minus a year lands on Feb 28.
	if got, _ := parseDateArg("-1y", time.Date(2028, 2, 29, 12, 0, 0, 0, time.UTC)); !got.Equal(day(2027, 2, 28)) {
		t.Errorf("parseDateArg(-1y from 2028-02-29) = %s; want 2027-02-28", got.Format("2006-01-02"))
	}
	for _, bad := range []string{"", "d", "-d", "7", "-7 d", "7 days ago", "-7h", "2026/09/03", "09-03-2026", "P7D", "--7d", "9999999999999999999d"} {
		if got, err := parseDateArg(bad, now); err == nil {
			t.Errorf("parseDateArg(%q) = %s; want an error", bad, got.Format("2006-01-02"))
		}
	}
}

func TestInputSchemaDateParamCarriesPattern(t *testing.T) {
	optional := false
	spec := &queryToolSpec{
		Name:  "t",
		Query: "SELECT @StartDate",
		Parameters: []queryToolParam{
			{Name: "StartDate", Type: "date", Description: "Earliest day to include.", Required: &optional},
		},
	}
	ps := spec.inputSchema().Properties["StartDate"]
	if ps.Type != "string" {
		t.Errorf("type = %q; want string", ps.Type)
	}
	if ps.Pattern != dateArgPattern {
		t.Errorf("pattern = %q; want dateArgPattern", ps.Pattern)
	}
	// The accepted forms are stated once in the initialize instructions, not
	// appended to every date parameter.
	if want := "Earliest day to include."; ps.Description != want {
		t.Errorf("description = %q; want %q", ps.Description, want)
	}
	// Every form parseDateArg accepts must also pass the advertised pattern,
	// or a validating client would reject arguments the server would take.
	re := regexp.MustCompile(dateArgPattern)
	for _, ok := range []string{"2026-09-03", "today", "Yesterday", "tomorrow", "-7d", "+3D", "0d", "-2w", "-3M", "-1y"} {
		if !re.MatchString(ok) {
			t.Errorf("pattern rejects %q, which parseDateArg accepts", ok)
		}
	}
	for _, bad := range []string{"7 days ago", "-7h", "2026/09/03", "P7D"} {
		if re.MatchString(bad) {
			t.Errorf("pattern accepts %q, which parseDateArg rejects", bad)
		}
	}
}

func TestConnPoolReusesPrimaryAndCachesExtra(t *testing.T) {
	primary := openStub(t)
	cfg := &config{connString: "primary-conn", maxOpenConns: 1}
	pool := newConnPool(cfg, primary)

	opened := 0
	restore := dialConn
	dialConn = func(_ *config, connString string) (*sql.DB, error) {
		opened++
		db, err := sql.Open("stubmssql", connString)
		if err != nil {
			return nil, err
		}
		return db, nil
	}
	t.Cleanup(func() { dialConn = restore })

	if db, err := pool.get(""); err != nil || db != primary {
		t.Errorf("get(\"\") = %v, %v; want the primary handle", db, err)
	}
	if db, err := pool.get("primary-conn"); err != nil || db != primary {
		t.Errorf("get(primary-conn) = %v, %v; want the primary handle", db, err)
	}
	a1, err := pool.get("other-conn")
	if err != nil {
		t.Fatalf("get(other-conn): %v", err)
	}
	a2, _ := pool.get("other-conn")
	if a1 != a2 {
		t.Errorf("get(other-conn) returned two different handles")
	}
	if opened != 1 {
		t.Errorf("dialConn called %d times, want 1", opened)
	}
	pool.closeExtra()
	if len(pool.extra) != 0 {
		t.Errorf("closeExtra left %d handles", len(pool.extra))
	}
}

// --- end to end, over an in-memory client/server pair ---

type elicitHandler = func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error)

func connectQueryToolClient(t *testing.T, specsYAML string, elicit elicitHandler) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	specs, err := loadQueryTools(writeQueryToolsFile(t, specsYAML), builtinToolNames("sales"))
	if err != nil {
		t.Fatalf("loadQueryTools: %v", err)
	}
	cfg := &config{
		toolPrefix:   "sales",
		connString:   "stub",
		queryTimeout: 5 * time.Second,
		maxOpenConns: 1,
		maxRows:      100,
		queryTools:   specs,
	}

	db := openStub(t)
	server := newServer(cfg, db, newConnPool(cfg, db))
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, &mcp.ClientOptions{
		ElicitationHandler: elicit,
	})
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func callQueryTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s) protocol error: %v", name, err)
	}
	return res
}

func TestQueryToolCSV(t *testing.T) {
	cs := connectQueryToolClient(t, `
- name: widgets
  description: All widgets.
  query: SELECT id, name, price FROM dbo.Widget
  outputFormat: csv
`, nil)

	res := callQueryTool(t, cs, "widgets", nil)
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", contentText(res))
	}
	got := contentText(res)
	if !strings.Contains(got, "id,name,price") || !strings.Contains(got, "1,widget,19.99") {
		t.Errorf("CSV output = %q", got)
	}
}

func TestQueryToolMarkdown(t *testing.T) {
	cs := connectQueryToolClient(t, `
- name: widgets
  description: All widgets.
  query: SELECT id, name, price FROM dbo.Widget
  outputFormat: md
`, nil)

	got := contentText(callQueryTool(t, cs, "widgets", nil))
	if !strings.Contains(got, "| id | name | price |") || !strings.Contains(got, "| 1 | widget | 19.99 |") {
		t.Errorf("markdown output = %q", got)
	}
}

func TestQueryToolColumnsProjectCSV(t *testing.T) {
	cs := connectQueryToolClient(t, `
- name: widgets
  description: Widgets, price then id only.
  query: SELECT id, name, price FROM dbo.Widget
  columns: [price, id]
  outputFormat: csv
`, nil)

	got := contentText(callQueryTool(t, cs, "widgets", map[string]any{"Columns": "price,id"}))
	if !strings.Contains(got, "price,id") || !strings.Contains(got, "19.99,1") {
		t.Errorf("projected CSV = %q, want a price,id table", got)
	}
	if strings.Contains(got, "name") || strings.Contains(got, "widget") {
		t.Errorf("projected CSV still carries the name column: %q", got)
	}
}

func TestQueryToolColumnsProjectMarkdown(t *testing.T) {
	cs := connectQueryToolClient(t, `
- name: widgets
  description: Widgets, name only.
  query: SELECT id, name, price FROM dbo.Widget
  columns: [name]
  outputFormat: md
`, nil)

	got := contentText(callQueryTool(t, cs, "widgets", map[string]any{"Columns": "name"}))
	if !strings.Contains(got, "| name |") || !strings.Contains(got, "| widget |") {
		t.Errorf("projected markdown = %q", got)
	}
	if strings.Contains(got, "price") {
		t.Errorf("projected markdown still carries price: %q", got)
	}
}

func TestQueryToolColumnsUnknownColumn(t *testing.T) {
	cs := connectQueryToolClient(t, `
- name: widgets
  description: Widgets.
  query: SELECT id, name, price FROM dbo.Widget
  columns: [nope]
  outputFormat: csv
`, nil)

	res := callQueryTool(t, cs, "widgets", map[string]any{"Columns": "nope"})
	if !res.IsError || !strings.Contains(contentText(res), "not in the result set") {
		t.Errorf("want an unknown-column error, got IsError=%v %q", res.IsError, contentText(res))
	}
}

func TestQueryToolColumnsArgOmittedReturnsEveryColumn(t *testing.T) {
	// Columns is advertised as required — it's in the schema's `required` list
	// and its description says so — so a capable model passes it and keeps
	// responses narrow. But a client that omits it anyway (a weak local model
	// whose harness doesn't enforce JSON Schema `required`) should still get a
	// usable result rather than a hard error it may not recover from.
	cs := connectQueryToolClient(t, `
- name: widgets
  description: Widgets.
  query: SELECT id, name, price FROM dbo.Widget
  columns: [id, name, price]
  outputFormat: csv
`, nil)

	got := contentText(callQueryTool(t, cs, "widgets", nil))
	if !strings.Contains(got, "id,name,price") || !strings.Contains(got, "1,widget,19.99") {
		t.Errorf("Columns-omitted CSV = %q, want every column", got)
	}

	got = contentText(callQueryTool(t, cs, "widgets", map[string]any{"Columns": "   "}))
	if !strings.Contains(got, "id,name,price") || !strings.Contains(got, "1,widget,19.99") {
		t.Errorf("blank Columns CSV = %q, want every column", got)
	}
}

func TestQueryToolColumnsArgIgnoredWithoutMenu(t *testing.T) {
	cs := connectQueryToolClient(t, `
- name: widgets
  description: Widgets, no configured menu.
  query: SELECT id, name, price FROM dbo.Widget
  outputFormat: csv
`, nil)

	got := contentText(callQueryTool(t, cs, "widgets", map[string]any{"Columns": "price"}))
	if !strings.Contains(got, "id,name,price") {
		t.Errorf("output = %q, want every column since no menu is configured to select from", got)
	}
}

func TestQueryToolPickRecordSingleRow(t *testing.T) {
	// DuplicateColumns yields exactly one row, so no elicitation is needed.
	cs := connectQueryToolClient(t, `
- name: one_row
  description: One row, two fields.
  query: SELECT * FROM dbo.DuplicateColumns
  columns: [a_2, column_4]
  outputFormat: csv
  pickRecord: true
`, nil)

	res := callQueryTool(t, cs, "one_row", map[string]any{"Columns": "a_2,column_4"})
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", contentText(res))
	}
	got := contentText(res)
	if !strings.Contains(got, "a_2,column_4") || !strings.Contains(got, "2,4") {
		t.Errorf("pick-record single row = %q", got)
	}
}

func TestQueryToolPickRecordElicitsAndProjects(t *testing.T) {
	var saw *mcp.ElicitParams
	elicit := func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		saw = req.Params
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"row": "2"}}, nil
	}
	cs := connectQueryToolClient(t, `
- name: person_ref
  description: Pick a person, get id and last name.
  query: SELECT id, first, last, title FROM dbo.PeopleRows
  columns: [id, last]
  outputFormat: csv
  pickRecord: true
`, elicit)

	res := callQueryTool(t, cs, "person_ref", map[string]any{"Columns": "id,last"})
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", contentText(res))
	}
	got := contentText(res)
	if !strings.Contains(got, "id,last") || !strings.Contains(got, "2,turing") {
		t.Errorf("pick-record result = %q, want record 2 projected to id,last", got)
	}
	if strings.Contains(got, "first") || strings.Contains(got, "title") || strings.Contains(got, "fellow") {
		t.Errorf("pick-record result leaked unprojected fields: %q", got)
	}
	// The picker the user saw carried every column of every row; the message
	// itself doesn't repeat them.
	titles := pickerTitles(t, saw)
	for _, want := range []string{"1 — 1 · ada · lovelace · analyst", "3 — 3 · grace · hopper · admiral"} {
		if !slices.Contains(titles, want) {
			t.Errorf("picker options missing %q; got %q", want, titles)
		}
	}
	if strings.Contains(saw.Message, "lovelace") {
		t.Errorf("message should not duplicate the candidates:\n%s", saw.Message)
	}
}

// pickerSchema is the schema an elicitation requested, parsed back out of
// the wire form the client handler receives it in.
func pickerSchema(t *testing.T, params *mcp.ElicitParams) *jsonschema.Schema {
	t.Helper()
	if params == nil {
		t.Fatal("no elicitation was sent")
	}
	raw, err := json.Marshal(params.RequestedSchema)
	if err != nil {
		t.Fatalf("requested schema: %v", err)
	}
	schema := &jsonschema.Schema{}
	if err := json.Unmarshal(raw, schema); err != nil {
		t.Fatalf("requested schema: %v", err)
	}
	return schema
}

// pickerTitles is the option labels of the record picker an elicitation
// requested, in order.
func pickerTitles(t *testing.T, params *mcp.ElicitParams) []string {
	t.Helper()
	row := pickerSchema(t, params).Properties["row"]
	if row == nil {
		t.Fatalf("requested schema has no row property")
	}
	titles := make([]string, len(row.OneOf))
	for i, o := range row.OneOf {
		titles[i] = o.Title
	}
	return titles
}

func TestQueryToolPickRecordDeclined(t *testing.T) {
	elicit := func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "cancel"}, nil
	}
	cs := connectQueryToolClient(t, `
- name: person_ref
  description: Pick a person.
  query: SELECT id, first, last, title FROM dbo.PeopleRows
  columns: [id, last]
  outputFormat: csv
  pickRecord: true
`, elicit)

	res := callQueryTool(t, cs, "person_ref", map[string]any{"Columns": "id,last"})
	if !res.IsError || !strings.Contains(contentText(res), "no record was chosen") {
		t.Errorf("want a 'no record was chosen' error, got IsError=%v %q", res.IsError, contentText(res))
	}
}

func TestQueryToolPickRecordWithoutElicitationCapability(t *testing.T) {
	cs := connectQueryToolClient(t, `
- name: person_ref
  description: Pick a person.
  query: SELECT id, first, last, title FROM dbo.PeopleRows
  columns: [id, last]
  outputFormat: csv
  pickRecord: true
`, nil)

	res := callQueryTool(t, cs, "person_ref", map[string]any{"Columns": "id,last"})
	if !res.IsError || !strings.Contains(contentText(res), "cannot be asked") {
		t.Errorf("want a graceful 'cannot be asked' error, got IsError=%v %q", res.IsError, contentText(res))
	}
}

func TestLoadQueryToolsPickRecordOptional(t *testing.T) {
	specs, err := loadQueryTools(writeQueryToolsFile(t, `
- name: person_ref
  description: Find people; one on request.
  query: SELECT id, first, last, title FROM dbo.PeopleRows
  columns: [id, last]
  outputFormat: csv
  pickRecord: optional
- name: person_list
  description: Plain list.
  query: SELECT id, first, last, title FROM dbo.PeopleRows
  outputFormat: csv
`), builtinToolNames("sales"))
	if err != nil {
		t.Fatalf("loadQueryTools: %v", err)
	}
	opt, plain := specs[0], specs[1]
	if !opt.PickRecord.optional() || opt.PickRecord.always() {
		t.Fatalf("pickRecord: optional parsed as %q", opt.PickRecord)
	}
	prop := opt.inputSchema().Properties[requireSingleParamName]
	if prop == nil || prop.Type != "boolean" {
		t.Fatalf("optional tool must advertise a boolean %s; got %+v", requireSingleParamName, prop)
	}
	for _, r := range opt.inputSchema().Required {
		if r == requireSingleParamName {
			t.Errorf("%s must not be required", requireSingleParamName)
		}
	}
	if _, ok := plain.inputSchema().Properties[requireSingleParamName]; ok {
		t.Errorf("a tool without pickRecord must not advertise %s", requireSingleParamName)
	}
}

func TestLoadQueryToolsPickRecordRejectsBadValues(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`
- name: t
  description: d
  query: SELECT id FROM dbo.PeopleRows
  outputFormat: csv
  pickRecord: sometimes
`, "unknown value"},
		{`
- name: t
  description: d
  query: SELECT id FROM dbo.PeopleRows WHERE id = @RequireSingle
  parameters:
    - name: RequireSingle
      type: bool
  outputFormat: csv
`, "reserved for record selection"},
	} {
		_, err := loadQueryTools(writeQueryToolsFile(t, tc.body), builtinToolNames("sales"))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("want an error containing %q, got %v", tc.want, err)
		}
	}
}

const requireSingleTool = `
- name: person_ref
  description: Find people; one on request.
  query: SELECT TOP (ISNULL(@PageSize, 100)) id, first, last, title FROM dbo.PeopleRows WHERE last LIKE @last
  parameters:
    - name: last
      type: string
      required: false
    - name: PageSize
      type: int
      required: false
  columns: [id, last]
  outputFormat: csv
  pickRecord: optional
`

func TestQueryToolRequireSingleUnsetReturnsTheList(t *testing.T) {
	elicited := false
	elicit := func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		elicited = true
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"row": "1"}}, nil
	}
	cs := connectQueryToolClient(t, requireSingleTool, elicit)

	res := callQueryTool(t, cs, "person_ref", map[string]any{"Columns": "id,last"})
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", contentText(res))
	}
	got := contentText(res)
	for _, want := range []string{"1,lovelace", "2,turing", "3,hopper"} {
		if !strings.Contains(got, want) {
			t.Errorf("list result missing %q: %q", want, got)
		}
	}
	if elicited {
		t.Errorf("no elicitation should happen without RequireSingle")
	}
}

func TestQueryToolRequireSingleElicitsWithTitledOptions(t *testing.T) {
	var saw *mcp.ElicitParams
	elicit := func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		saw = req.Params
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"row": "3"}}, nil
	}
	cs := connectQueryToolClient(t, requireSingleTool, elicit)

	res := callQueryTool(t, cs, "person_ref", map[string]any{"Columns": "id,last", "RequireSingle": true, "PageSize": 10})
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", contentText(res))
	}
	got := contentText(res)
	if !strings.Contains(got, "3,hopper") || strings.Contains(got, "turing") || strings.Contains(got, "admiral") {
		t.Errorf("result = %q, want only record 3 projected to id,last", got)
	}
	if saw == nil {
		t.Fatal("no elicitation was sent")
	}
	if strings.Contains(saw.Message, "full page") {
		t.Errorf("3 rows on a page of 10 must not warn about a full page:\n%s", saw.Message)
	}

	// The picker is a titled enum over the record numbers, labelled by the
	// row's values.
	row := pickerSchema(t, saw).Properties["row"]
	if row == nil || row.Type != "string" || len(row.OneOf) != 3 {
		t.Fatalf("row property = %+v, want a string with 3 oneOf options", row)
	}
	if v, _ := (*row.OneOf[1].Const).(string); v != "2" {
		t.Errorf("option 2 const = %v, want \"2\"", *row.OneOf[1].Const)
	}
	if title := row.OneOf[1].Title; !strings.HasPrefix(title, "2 — ") || !strings.Contains(title, "alan · turing") {
		t.Errorf("option 2 title = %q, want the numbered row values", title)
	}
}

func TestQueryToolRequireSingleWarnsWhenPageIsFull(t *testing.T) {
	var sawMessage string
	elicit := func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		sawMessage = req.Params.Message
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"row": "1"}}, nil
	}
	cs := connectQueryToolClient(t, requireSingleTool, elicit)

	res := callQueryTool(t, cs, "person_ref", map[string]any{"Columns": "id,last", "RequireSingle": "true", "PageSize": 3})
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", contentText(res))
	}
	if !strings.Contains(sawMessage, "one full page of 3") {
		t.Errorf("3 rows on a page of 3 should warn that more may exist:\n%s", sawMessage)
	}
}

func TestQueryToolRequireSingleNoRowsIsEmptyNotError(t *testing.T) {
	cs := connectQueryToolClient(t, `
- name: person_ref
  description: Find people; one on request.
  query: SELECT id, first, last, title FROM dbo.PeopleNone
  columns: [id, last]
  outputFormat: csv
  pickRecord: optional
`, nil)

	res := callQueryTool(t, cs, "person_ref", map[string]any{"Columns": "id,last", "RequireSingle": true})
	if res.IsError {
		t.Fatalf("no match must be an ordinary empty result, got error: %s", contentText(res))
	}
	if got := contentText(res); !strings.Contains(got, "id,last") || strings.Contains(got, "lovelace") {
		t.Errorf("empty result = %q", got)
	}
}

func TestQueryToolRequireSingleWithoutElicitationCapability(t *testing.T) {
	cs := connectQueryToolClient(t, requireSingleTool, nil)

	res := callQueryTool(t, cs, "person_ref", map[string]any{"Columns": "id,last", "RequireSingle": true})
	if !res.IsError || !strings.Contains(contentText(res), "without RequireSingle") {
		t.Errorf("want an error pointing at the RequireSingle switch, got IsError=%v %q", res.IsError, contentText(res))
	}
}

func TestQueryToolRequireSingleRejectedWhereNotOffered(t *testing.T) {
	cs := connectQueryToolClient(t, `
- name: person_list
  description: Plain list.
  query: SELECT id, first, last, title FROM dbo.PeopleRows
  outputFormat: csv
`, nil)

	res := callQueryTool(t, cs, "person_list", map[string]any{"RequireSingle": true})
	if !res.IsError || !strings.Contains(contentText(res), "unrecognized parameter(s) RequireSingle") {
		t.Errorf("want an unrecognized-parameter error, got IsError=%v %q", res.IsError, contentText(res))
	}
}

func TestQueryToolScalarSingleRow(t *testing.T) {
	cs := connectQueryToolClient(t, `
- name: widget_total
  description: Total.
  query: SELECT total FROM dbo.OneScalar
  outputFormat: scalar
`, nil)

	res := callQueryTool(t, cs, "widget_total", nil)
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", contentText(res))
	}
	if got := strings.TrimSpace(contentText(res)); got != "42" {
		t.Errorf("scalar output = %q, want 42", got)
	}
}

func TestQueryToolScalarMultipleColumnsNeedsResultColumn(t *testing.T) {
	cs := connectQueryToolClient(t, `
- name: widget_val
  description: Value.
  query: SELECT id, name, price FROM dbo.Widget
  outputFormat: scalar
`, nil)

	res := callQueryTool(t, cs, "widget_val", nil)
	if !res.IsError || !strings.Contains(contentText(res), "no result column is configured") {
		t.Errorf("want a 'configure resultColumn' error, got IsError=%v %q", res.IsError, contentText(res))
	}
}

func TestQueryToolScalarResultColumn(t *testing.T) {
	cs := connectQueryToolClient(t, `
- name: pick_a2
  description: The a_2 column.
  query: SELECT * FROM dbo.DuplicateColumns
  outputFormat: scalar
  resultColumn: a_2
`, nil)

	res := callQueryTool(t, cs, "pick_a2", nil)
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", contentText(res))
	}
	if got := strings.TrimSpace(contentText(res)); got != "2" {
		t.Errorf("scalar output = %q, want 2", got)
	}
}

func TestQueryToolScalarElicitsRecordChoice(t *testing.T) {
	var saw *mcp.ElicitParams
	elicit := func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		saw = req.Params
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"row": "2"}}, nil
	}
	cs := connectQueryToolClient(t, `
- name: one_name
  description: A single name.
  query: SELECT name FROM dbo.ManyScalars
  outputFormat: scalar
`, elicit)

	res := callQueryTool(t, cs, "one_name", nil)
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", contentText(res))
	}
	if got := strings.TrimSpace(contentText(res)); got != "bob" {
		t.Errorf("scalar output = %q, want bob (record 2)", got)
	}
	if titles := pickerTitles(t, saw); !slices.Equal(titles, []string{"1 — alice", "2 — bob", "3 — carol"}) {
		t.Errorf("picker options = %q, want the numbered values", titles)
	}
}

func TestQueryToolScalarElicitDeclined(t *testing.T) {
	elicit := func(_ context.Context, _ *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "decline"}, nil
	}
	cs := connectQueryToolClient(t, `
- name: one_name
  description: A single name.
  query: SELECT name FROM dbo.ManyScalars
  outputFormat: scalar
`, elicit)

	res := callQueryTool(t, cs, "one_name", nil)
	if !res.IsError || !strings.Contains(contentText(res), "no record was chosen") {
		t.Errorf("want a 'no record was chosen' error, got IsError=%v %q", res.IsError, contentText(res))
	}
}

func TestQueryToolScalarWithoutElicitationCapability(t *testing.T) {
	cs := connectQueryToolClient(t, `
- name: one_name
  description: A single name.
  query: SELECT name FROM dbo.ManyScalars
  outputFormat: scalar
`, nil) // no elicitation handler -> capability not advertised

	res := callQueryTool(t, cs, "one_name", nil)
	if !res.IsError || !strings.Contains(contentText(res), "cannot be asked") {
		t.Errorf("want a graceful 'cannot be asked' error, got IsError=%v %q", res.IsError, contentText(res))
	}
}

func TestQueryToolMissingRequiredParameter(t *testing.T) {
	cs := connectQueryToolClient(t, `
- name: by_id
  description: By id.
  query: SELECT id, name, price FROM dbo.Widget WHERE id = @id
  parameters:
    - name: id
      type: int
  outputFormat: csv
`, nil)

	res := callQueryTool(t, cs, "by_id", nil)
	if !res.IsError || !strings.Contains(contentText(res), `missing required parameter "id"`) {
		t.Errorf("want a missing-parameter error, got IsError=%v %q", res.IsError, contentText(res))
	}
}

func TestQueryToolBlankRequiredParameterIsMissing(t *testing.T) {
	cs := connectQueryToolClient(t, `
- name: by_name
  description: By name.
  query: SELECT id, name, price FROM dbo.Widget WHERE name = @name
  parameters:
    - name: name
  outputFormat: csv
`, nil)

	for _, v := range []string{"", "   "} {
		res := callQueryTool(t, cs, "by_name", map[string]any{"name": v})
		if !res.IsError || !strings.Contains(contentText(res), `missing required parameter "name"`) {
			t.Errorf("name=%q: want a missing-parameter error, got IsError=%v %q", v, res.IsError, contentText(res))
		}
	}
}

func TestQueryToolAcceptsParameters(t *testing.T) {
	cs := connectQueryToolClient(t, `
- name: by_id
  description: By id.
  query: SELECT id, name, price FROM dbo.Widget WHERE id = @id
  parameters:
    - name: id
      type: int
  outputFormat: csv
`, nil)

	res := callQueryTool(t, cs, "by_id", map[string]any{"id": 1})
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", contentText(res))
	}
	if !strings.Contains(contentText(res), "id,name,price") {
		t.Errorf("output = %q", contentText(res))
	}
}

func TestQueryToolUnknownParameterIsRejected(t *testing.T) {
	cs := connectQueryToolClient(t, `
- name: by_id
  description: By id.
  query: SELECT id, name, price FROM dbo.Widget WHERE id = @id
  parameters:
    - name: id
      type: int
  outputFormat: csv
`, nil)

	// A misnamed key (case, underscores, a typo, ...) must not be silently
	// treated as "the real parameter was omitted" — that would leave an
	// optional filter unapplied with no error at all. It has to fail loudly
	// instead, naming both the bad key and what the tool actually accepts.
	res := callQueryTool(t, cs, "by_id", map[string]any{"id": 1, "Id_Contains": "x"})
	if !res.IsError {
		t.Fatalf("want an error for an unrecognized parameter, got success: %s", contentText(res))
	}
	if got := contentText(res); !strings.Contains(got, "Id_Contains") || !strings.Contains(got, "id") {
		t.Errorf("error should name the bad key and the expected parameter, got %q", got)
	}
}

func TestQueryToolRequireAnyOf(t *testing.T) {
	cs := connectQueryToolClient(t, `
- name: find
  description: Find.
  query: >
    SELECT id, name, price FROM dbo.Widget
    WHERE (@first IS NULL OR name LIKE @first + '%') AND (@last IS NULL OR name LIKE '%' + @last)
  parameters:
    - name: first
      required: false
    - name: last
      required: false
  requireAnyOf: [first, last]
  outputFormat: csv
`, nil)

	// Each filter is optional on its own, but a call that sets none of them
	// (or only blank ones) would return the whole table — reject it before
	// the query runs, naming the parameters that would have satisfied it.
	for name, args := range map[string]map[string]any{
		"none":       {},
		"null":       {"first": nil, "last": nil},
		"blank":      {"first": "", "last": "   "},
		"blank+null": {"first": " ", "last": nil},
	} {
		t.Run(name, func(t *testing.T) {
			res := callQueryTool(t, cs, "find", args)
			if !res.IsError {
				t.Fatalf("want an error, got success: %s", contentText(res))
			}
			if got := contentText(res); !strings.Contains(got, "at least one of first, last") {
				t.Errorf("error should name the parameters, got %q", got)
			}
		})
	}

	res := callQueryTool(t, cs, "find", map[string]any{"first": "", "last": "x"})
	if res.IsError {
		t.Fatalf("one non-blank filter should be enough, got error: %s", contentText(res))
	}
}

func TestQueryToolListedByServer(t *testing.T) {
	cs := connectQueryToolClient(t, `
- name: widgets
  description: All widgets.
  query: SELECT id, name, price FROM dbo.Widget
  outputFormat: csv
`, nil)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	if !contains(names, "widgets") || !contains(names, "sales_query") {
		t.Errorf("tool list = %v, want both sales_query and widgets", names)
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func TestQueryToolUnrecognizedParameterSuggestsCase(t *testing.T) {
	cs := connectQueryToolClient(t, `
- name: by_name
  description: By name.
  query: SELECT id FROM dbo.Widget WHERE name = @SearchFirstName
  parameters:
    - name: SearchFirstName
      required: false
  requireAnyOf: [SearchFirstName]
  outputFormat: csv
`, nil)

	res := callQueryTool(t, cs, "by_name", map[string]any{"SearchFirstname": "Earl"})
	got := contentText(res)
	if !res.IsError || !strings.Contains(got, "case-sensitive") || !strings.Contains(got, "SearchFirstname is spelled SearchFirstName") {
		t.Errorf("want a case hint, got IsError=%v %q", res.IsError, got)
	}
}

// Columns is not a parameter: it is never advertised, and one that a client
// sends anyway is ignored, with a note, so the call still succeeds.
func TestQueryToolColumnsArgIsIgnoredWithNote(t *testing.T) {
	specs, err := loadQueryTools(writeQueryToolsFile(t, `
- name: widgets
  description: Widgets.
  query: SELECT id, name, price FROM dbo.Widget
  columns: [id, name, price]
  outputFormat: csv
`), builtinToolNames("sales"))
	if err != nil {
		t.Fatalf("loadQueryTools: %v", err)
	}
	schema := specs[0].inputSchema()
	if _, ok := schema.Properties[columnsParamName]; ok {
		t.Errorf("schema advertises %s", columnsParamName)
	}
	for _, r := range schema.Required {
		if r == columnsParamName {
			t.Errorf("%s must not be required", columnsParamName)
		}
	}

	cs := connectQueryToolClient(t, `
- name: widgets
  description: Widgets.
  query: SELECT id, name, price FROM dbo.Widget
  columns: [id, name, price]
  outputFormat: csv
`, nil)
	res := callQueryTool(t, cs, "widgets", map[string]any{"Columns": "price"})
	got := contentText(res)
	if res.IsError || !strings.Contains(got, "id,name,price") || !strings.Contains(got, "Columns is not a parameter") {
		t.Errorf("want every column and a note, got IsError=%v %q", res.IsError, got)
	}
	// Garbage of the kind a small model produces is ignored the same way.
	res = callQueryTool(t, cs, "widgets", map[string]any{"Columns": `price""",Required:true`})
	if res.IsError || !strings.Contains(contentText(res), "id,name,price") {
		t.Errorf("malformed Columns broke the call: %q", contentText(res))
	}
}
