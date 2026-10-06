package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const sharedParamsYAML = `
sharedParameters:
  - name: PersonID
    type: string
    description: Database Person ID; one id or several comma-separated.
  - name: PageSize
    type: int
    required: false
    description: Rows per page.
  - name: StartDate
    type: date
    required: false
    description: Only rows on or after this date.
tools:
  - name: th_profile
    description: A person's profile.
    details: Every column of dbo.Person; use th_ping to check the server is up.
    query: SELECT id FROM dbo.Person WHERE id = @PersonID AND @PageSize IS NOT NULL AND @StartDate IS NOT NULL
    parameters:
      - name: PersonID
      - name: PageSize
      - name: StartDate
        description: Overrides the default window.
    columns: [id, name]
    outputFormat: csv
  - name: th_ping
    description: Is the server up.
    query: SELECT 1 AS ok
    outputFormat: scalar
`

func loadShared(t *testing.T) *queryToolFile {
	t.Helper()
	f, err := parseQueryToolsFile(writeQueryToolsFile(t, sharedParamsYAML), builtinToolNames("things"))
	if err != nil {
		t.Fatalf("parseQueryToolsFile: %v", err)
	}
	return f
}

func TestSharedParametersFillTypeAndRequiredButNotDescription(t *testing.T) {
	f := loadShared(t)
	if len(f.SharedParams) != 3 {
		t.Fatalf("SharedParams = %d entries, want 3", len(f.SharedParams))
	}
	s := specNamed(t, f, "th_profile")
	props := s.inputSchema().Properties

	if p := props["PageSize"]; p.Type != "integer" || p.Description != "" {
		t.Errorf("PageSize schema = type %q description %q; want integer with no description", p.Type, p.Description)
	}
	if got := s.inputSchema().Required; strings.Join(got, ",") != "PersonID" {
		t.Errorf("required = %v; want PersonID (shared default) only", got)
	}
	// A description the tool writes itself is kept, and the shared one is
	// not appended to it.
	if p := props["StartDate"]; p.Description != "Overrides the default window." || p.Pattern != dateArgPattern {
		t.Errorf("StartDate schema = %q / pattern %q; want the tool's own text and the date pattern", p.Description, p.Pattern)
	}
	if _, ok := props["Columns"]; ok {
		t.Errorf("Columns must not be advertised")
	}
	if !strings.Contains(s.Details, "th_ping") {
		t.Errorf("details = %q; want them loaded", s.Details)
	}
}

func TestSharedParametersRejectBadEntries(t *testing.T) {
	cases := map[string]string{
		"missing description": "sharedParameters:\n  - name: PageSize\n    type: int\ntools:\n  - name: t\n    description: d\n    query: SELECT 1\n    outputFormat: scalar\n",
		"duplicate":           "sharedParameters:\n  - name: A\n    description: a\n  - name: a\n    description: b\ntools:\n  - name: t\n    description: d\n    query: SELECT 1\n    outputFormat: scalar\n",
		"bad type":            "sharedParameters:\n  - name: A\n    type: blob\n    description: a\ntools:\n  - name: t\n    description: d\n    query: SELECT 1\n    outputFormat: scalar\n",
	}
	for name, body := range cases {
		if _, err := parseQueryToolsFile(writeQueryToolsFile(t, body), nil); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestSharedParametersMustBeInRootFile(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root.yaml")
	inc := filepath.Join(dir, "inc.yaml")
	os.WriteFile(inc, []byte("sharedParameters:\n  - name: A\n    description: a\ntools:\n  - name: t\n    description: d\n    query: SELECT 1\n    outputFormat: scalar\n"), 0o600)
	os.WriteFile(root, []byte("include:\n  - inc.yaml\n"), 0o600)
	if _, err := parseQueryToolsFile(root, nil); err == nil || !strings.Contains(err.Error(), "belongs in the root file") {
		t.Errorf("err = %v; want the include rejected", err)
	}
}

func TestQueryToolNotesListOnlyWhatTheServerUses(t *testing.T) {
	f := loadShared(t)
	cfg := &config{toolPrefix: "things", toolCallName: defaultToolCallName, queryTools: f.Specs, querySharedParams: f.SharedParams}

	notes := queryToolNotes(cfg, f.Specs)
	for _, want := range []string{"things_describe_tool", "PersonID: Database Person ID", "PageSize: Rows per page", "yyyy-mm-dd"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q:\n%s", want, notes)
		}
	}
	// StartDate carries its own description on the only tool that takes it,
	// so the shared text is not the one in force and is left out.
	if strings.Contains(notes, "StartDate:") {
		t.Errorf("notes list StartDate, whose only use overrides the shared text:\n%s", notes)
	}
	if strings.Contains(notes, "RequireSingle") {
		t.Errorf("notes mention RequireSingle, which no tool here offers:\n%s", notes)
	}
	if queryToolNotes(cfg, nil) != "" {
		t.Error("notes for a server with no query tools should be empty")
	}
}

func TestInitializeCarriesQueryToolNotesAndDescribeTool(t *testing.T) {
	f := loadShared(t)
	cfg := &config{
		connString:        "stub",
		toolPrefix:        "things",
		toolCallName:      defaultToolCallName,
		queryTimeout:      5 * time.Second,
		maxOpenConns:      1,
		maxRows:           100,
		queryTools:        f.Specs,
		querySharedParams: f.SharedParams,
	}
	cs := connectTestClient(t, cfg)
	init := cs.InitializeResult()
	if !strings.Contains(init.Instructions, "About the query tools:") || !strings.Contains(init.Instructions, "PersonID:") {
		t.Errorf("instructions lack the query-tool notes:\n%s", init.Instructions)
	}

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "things_describe_tool",
		Arguments: map[string]any{"tool": "TH_PROFILE"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	text := contentText(res)
	for _, want := range []string{
		"# th_profile",
		"A person's profile.",
		"Every column of dbo.Person; use th_ping",
		"- PersonID (string, required): Database Person ID",
		"- PageSize (int, optional): Rows per page.",
		"- StartDate (date, optional): Overrides the default window. " + dateArgHint,
		"Columns: id, name",
		"Output: csv",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("describe output lacks %q:\n%s", want, text)
		}
	}

	res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "things_describe_tool",
		Arguments: map[string]any{"tool": "nope"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError || !strings.Contains(contentText(res), "th_profile, th_ping") {
		t.Errorf("unknown tool: IsError=%v text=%q; want an error naming the tools", res.IsError, contentText(res))
	}
}

func TestDescribeToolFindsGroupedToolsByCallName(t *testing.T) {
	f, err := parseQueryToolsFileAs(writeQueryToolsFile(t, prefixedToolsYAML), builtinToolNames("things"), "{server}_{tool}")
	if err != nil {
		t.Fatalf("parseQueryToolsFileAs: %v", err)
	}
	cfg := &config{toolPrefix: "things", toolCallName: "{server}_{tool}", queryTools: f.Specs, queryToolGroups: f.Groups}

	for _, name := range []string{"th_bill_invoice_total", "things-billing_th_bill_invoice_total", "mcp_things-billing_th_bill_invoice_total"} {
		s := findQueryTool(cfg, name)
		if s == nil || s.Name != "th_bill_invoice_total" {
			t.Errorf("findQueryTool(%q) = %v; want th_bill_invoice_total", name, s)
		}
	}
	text := describeQueryTool(cfg, findQueryTool(cfg, "th_bill_invoice_total"))
	if !strings.HasPrefix(text, "# things-billing_th_bill_invoice_total (on the things-billing server)") {
		t.Errorf("heading = %q; want the call name and server", strings.SplitN(text, "\n", 2)[0])
	}
	// A description's cross-reference to a sibling is written as the model
	// calls it, and the group's notes name the group's own describe tool.
	if !strings.Contains(text, "things-people_th_ppl_find_person (on the things-people server)") {
		t.Errorf("description not rewritten:\n%s", text)
	}
	billing := specsInGroup(f.Specs, "billing")
	if notes := queryToolNotes(cfg, billing); !strings.Contains(notes, "things-billing_things_describe_tool") {
		t.Errorf("group notes = %q; want the namespaced describe tool", notes)
	}
}

// An id-list parameter's schema says it takes a list and a @handle unless its
// own text already says it takes a handle; literal parameters are
// left alone.
func TestBatchParamSchemaCarriesListHint(t *testing.T) {
	cases := []struct {
		p    queryToolParam
		want string
	}{
		{queryToolParam{Name: "PersonID", batch: true}, batchParamHint},
		{queryToolParam{Name: "PersonID", batch: true, Description: "Specific students."}, "Specific students. " + batchParamHint},
		{queryToolParam{Name: "FacultyID", batch: true, Description: "Faculty id, as @handle.FacultyID."}, "Faculty id, as @handle.FacultyID."},
		{queryToolParam{Name: "SchoolYear", batch: true, Literal: true}, ""},
		{queryToolParam{Name: "NameContains", Description: "Part of the name."}, "Part of the name."},
		{queryToolParam{Name: "PageSize"}, ""},
	}
	for _, c := range cases {
		if got := advertisedParamDescription(c.p); got != c.want {
			t.Errorf("%s (batch %v, %q): got %q, want %q", c.p.Name, c.p.batch, c.p.Description, got, c.want)
		}
	}
}
