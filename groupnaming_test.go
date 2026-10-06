package main

import (
	"strings"
	"testing"
	"time"
)

func TestPublishedName(t *testing.T) {
	cases := []struct{ prefix, name, want string }{
		{"", "find_person", "find_person"},
		{"dir", "find_person", "dir_find_person"},
		{"st_dir", "st_student_profile", "st_dir_student_profile"},
		{"st_dir", "find_person", "st_dir_find_person"},
		{"st_dir", "dir_thing", "st_dir_dir_thing"},    // overlap is leading only
		{"st_dir", "st_dir_already", "st_dir_already"}, // whole prefix shared
		{"st_dir", "student_st_profile", "st_dir_student_st_profile"},
		{"st", "st", "st_st"}, // "st" alone is not "st_" so nothing is shared
	}
	for _, c := range cases {
		if got := publishedName(c.prefix, c.name); got != c.want {
			t.Errorf("publishedName(%q, %q) = %q, want %q", c.prefix, c.name, got, c.want)
		}
	}
}

const prefixedToolsYAML = `
groups:
  people:
    label: things-people
    prefix: th_ppl
    port: 9101
    description: Finding people. Pair with th_invoice_total for money.
  billing:
    label: things-billing
    prefix: th_bill
    port: 9102
    description: Money.
  audit:
    port: 9103
    description: Unlabelled group.
tools:
  - name: th_find_person
    group: people
    description: One person by id. Use th_person_count for the total, th_invoice_total for money, and th_invoice_total again here. Related to th_find_person_history.
    query: SELECT id, name FROM dbo.Person WHERE id = @id
    parameters:
      - name: id
        type: int
        description: person id, from th_invoice_total or healthcheck
    outputFormat: csv
  - name: th_find_person_history
    group: people
    description: History of th_find_person.
    query: SELECT 1 AS ok
    outputFormat: scalar
  - name: th_person_count
    group: people
    description: How many people. Already tagged - th_invoice_total (on the things-billing server) - stays as is.
    query: SELECT COUNT(*) AS n FROM dbo.Person
    outputFormat: scalar
  - name: th_invoice_total
    group: billing
    description: One invoice total for the person th_find_person returns.
    query: SELECT total FROM dbo.Invoice WHERE id = @id
    parameters:
      - name: id
        type: int
        description: invoice id
    outputFormat: scalar
  - name: th_invoice_list
    group: billing
    description: Invoices.
    query: SELECT 1 AS ok
    outputFormat: scalar
  - name: th_audit_log
    group: audit
    description: Audit entries; see th_find_person.
    query: SELECT 1 AS ok
    outputFormat: scalar
  - name: th_audit_trail
    group: audit
    description: Trail.
    query: SELECT 1 AS ok
    outputFormat: scalar
  - name: th_reporter
    group: billing
    description: One tag per prefixed group - th_invoice_total / th_invoice_list - but each unprefixed tool - th_audit_log, th_audit_trail - gets its own.
    query: SELECT 1 AS ok
    outputFormat: scalar
  - name: healthcheck
    description: Ungrouped ping; see th_find_person.
    query: SELECT 1 AS ok
    outputFormat: scalar
`

func loadPrefixed(t *testing.T) *queryToolFile {
	t.Helper()
	f, err := parseQueryToolsFile(writeQueryToolsFile(t, prefixedToolsYAML), builtinToolNames("things"))
	if err != nil {
		t.Fatalf("parseQueryToolsFile: %v", err)
	}
	return f
}

func specNamed(t *testing.T, f *queryToolFile, name string) *queryToolSpec {
	t.Helper()
	for _, s := range f.Specs {
		if s.Name == name {
			return s
		}
	}
	var names []string
	for _, s := range f.Specs {
		names = append(names, s.Name)
	}
	t.Fatalf("no tool published as %q in %v", name, names)
	return nil
}

func TestGroupPrefixPublishesNames(t *testing.T) {
	f := loadPrefixed(t)
	want := []string{"th_ppl_find_person", "th_ppl_find_person_history", "th_ppl_person_count", "th_bill_invoice_total", "th_bill_invoice_list", "th_audit_log", "th_audit_trail", "th_bill_reporter", "healthcheck"}
	for i, s := range f.Specs {
		if s.Name != want[i] {
			t.Errorf("spec[%d].Name = %q, want %q", i, s.Name, want[i])
		}
	}
	if g := f.Groups[0]; g.Prefix != "th_ppl" {
		t.Errorf("people prefix = %q, want th_ppl", g.Prefix)
	}
}

func TestToolMentionsAreRewrittenAndQualified(t *testing.T) {
	f := loadPrefixed(t)

	find := specNamed(t, f, "th_ppl_find_person")
	wantDesc := "One person by id. Use th_ppl_person_count for the total, th_bill_invoice_total (on the things-billing server) for money, and th_bill_invoice_total again here. Related to th_ppl_find_person_history."
	if find.Description != wantDesc {
		t.Errorf("find_person description =\n  %q\nwant\n  %q", find.Description, wantDesc)
	}
	// Parameter descriptions get the same treatment; the ungrouped tool is
	// left alone because the base server's label is not known at load time.
	if got, want := find.Parameters[0].Description, "person id, from th_bill_invoice_total (on the things-billing server) or healthcheck"; got != want {
		t.Errorf("parameter description = %q, want %q", got, want)
	}

	// A mention the operator already qualified is not tagged twice.
	count := specNamed(t, f, "th_ppl_person_count")
	if want := "How many people. Already tagged - th_bill_invoice_total (on the things-billing server) - stays as is."; count.Description != want {
		t.Errorf("person_count description = %q, want %q", count.Description, want)
	}

	// The other direction: billing mentions a people tool.
	inv := specNamed(t, f, "th_bill_invoice_total")
	if want := "One invoice total for the person th_ppl_find_person (on the things-people server) returns."; inv.Description != want {
		t.Errorf("invoice_total description = %q, want %q", inv.Description, want)
	}

	// A group with no prefix keeps its names but still tags foreign mentions;
	// a group with no label is named by its group name.
	audit := specNamed(t, f, "th_audit_log")
	if want := "Audit entries; see th_ppl_find_person (on the things-people server)."; audit.Description != want {
		t.Errorf("audit_log description = %q, want %q", audit.Description, want)
	}
	hc := specNamed(t, f, "healthcheck")
	if want := "Ungrouped ping; see th_ppl_find_person (on the things-people server)."; hc.Description != want {
		t.Errorf("healthcheck description = %q, want %q", hc.Description, want)
	}

	// Sibling mentions from a prefixed group share one tag; tools from an
	// unprefixed group are tagged one by one. (th_reporter is itself in
	// billing, so the billing mentions are local and untagged here — the
	// people tool exercises the prefixed case.)
	rep := specNamed(t, f, "th_bill_reporter")
	if want := "One tag per prefixed group - th_bill_invoice_total / th_bill_invoice_list - but each unprefixed tool - th_audit_log (on the audit server), th_audit_trail (on the audit server) - gets its own."; rep.Description != want {
		t.Errorf("reporter description = %q, want %q", rep.Description, want)
	}

	// Group descriptions feed the initialize instructions and are rewritten too.
	if want := "Finding people. Pair with th_bill_invoice_total (on the things-billing server) for money."; f.Groups[0].Description != want {
		t.Errorf("people group description = %q, want %q", f.Groups[0].Description, want)
	}
	// The SQL is never touched.
	if !strings.Contains(inv.Query, "dbo.Invoice") || strings.Contains(inv.Query, "th_bill") {
		t.Errorf("query was rewritten: %q", inv.Query)
	}
}

func TestUnlabelledGroupTagUsesGroupName(t *testing.T) {
	f, err := parseQueryToolsFile(writeQueryToolsFile(t, `
tools:
  - name: a_tool
    group: alpha
    description: See b_tool.
    query: SELECT 1 AS ok
    outputFormat: scalar
  - name: b_tool
    group: beta
    description: Other.
    query: SELECT 1 AS ok
    outputFormat: scalar
`), nil)
	if err != nil {
		t.Fatalf("parseQueryToolsFile: %v", err)
	}
	if want := "See b_tool (on the beta server)."; f.Specs[0].Description != want {
		t.Errorf("description = %q, want %q", f.Specs[0].Description, want)
	}
}

func TestGroupPrefixRejects(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"bad prefix characters": {`
groups:
  people:
    prefix: "no spaces"
tools:
  - name: find_person
    group: people
    description: x
    query: SELECT 1 AS ok
    outputFormat: scalar
`, "prefix"},
		"published name collides": {`
groups:
  people:
    prefix: pp
tools:
  - name: find_person
    group: people
    description: x
    query: SELECT 1 AS ok
    outputFormat: scalar
  - name: pp_find_person
    description: x
    query: SELECT 1 AS ok
    outputFormat: scalar
`, `both publish as "pp_find_person"`},
		"published name is reserved": {`
groups:
  people:
    prefix: things
tools:
  - name: query
    group: people
    description: x
    query: SELECT 1 AS ok
    outputFormat: scalar
`, "built-in"},
		"published name too long": {`
groups:
  people:
    prefix: ` + strings.Repeat("p", 40) + `
tools:
  - name: ` + strings.Repeat("n", 40) + `
    group: people
    description: x
    query: SELECT 1 AS ok
    outputFormat: scalar
`, "1-64 characters"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseQueryToolsFile(writeQueryToolsFile(t, c.yaml), builtinToolNames("things"))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one containing %q", err, c.want)
			}
		})
	}
}

func TestGroupedServersExposePublishedNames(t *testing.T) {
	f := loadPrefixed(t)
	cfg := &config{
		transport: transportHTTP, httpAddr: "127.0.0.1:8080", toolPrefix: "things",
		queryTimeout: time.Second, maxOpenConns: 1, maxRows: 10,
		queryTools: f.Specs, queryToolGroups: f.Groups,
	}
	if err := resolveGroupPorts(cfg); err != nil {
		t.Fatalf("resolveGroupPorts: %v", err)
	}
	servers := buildServers(cfg, openStub(t), nil)
	byName := map[string]namedServer{}
	for _, s := range servers {
		byName[s.name] = s
	}
	people := byName["things-people"]
	if people.server == nil {
		t.Fatalf("no people server in %v", byName)
	}
	got := toolNames(t, people.server)
	if !has(got, "th_ppl_find_person") || !has(got, "th_ppl_person_count") || has(got, "th_find_person") {
		t.Errorf("people tools = %v, want the published th_ppl_* names and not the declared ones", got)
	}
	billing := byName["things-billing"]
	if got := toolNames(t, billing.server); !has(got, "th_bill_invoice_total") {
		t.Errorf("billing tools = %v, want th_bill_invoice_total", got)
	}

	// The initialize text names the prefix so the model can tell its own
	// tools from a sibling's by name alone.
	text := groupInstructions(cfg, f.Groups[0], "Things")
	if !strings.Contains(text, "called th_ppl_… (for example th_ppl_find_person)") {
		t.Errorf("people instructions do not mention the prefix:\n%s", text)
	}
	if text := groupInstructions(cfg, f.Groups[2], "Things"); strings.Contains(text, "is called ") {
		t.Errorf("unprefixed group instructions mention a prefix:\n%s", text)
	}
}

func TestToolCallName(t *testing.T) {
	cases := []struct{ template, server, tool, want string }{
		{"{tool}", "st_dir", "st_search_people", "st_search_people"},
		{"{server}_{tool}", "st_dir", "st_search_people", "st_dir_st_search_people"},
		{"{server}_{tool}", "", "healthcheck", "healthcheck"}, // ungrouped: no server to name
		{"mcp__{server}__{tool}", "st_dir", "st_search_people", "mcp__st_dir__st_search_people"},
		{"", "st_dir", "st_search_people", "st_search_people"},
	}
	for _, c := range cases {
		if got := toolCallName(c.template, c.server, c.tool); got != c.want {
			t.Errorf("toolCallName(%q, %q, %q) = %q, want %q", c.template, c.server, c.tool, got, c.want)
		}
	}
	for _, bad := range []string{"", "{server}", "tool"} {
		if err := checkToolCallName(bad); err == nil {
			t.Errorf("checkToolCallName(%q) accepted, want an error", bad)
		}
	}
	if _, err := loadConfig([]string{"--conn-string", "server=localhost", "--tool-prefix", "sales", "--tool-call-name", "{server}"}); err == nil || !strings.Contains(err.Error(), "--tool-call-name") {
		t.Errorf("loadConfig accepted a --tool-call-name without {tool}: %v", err)
	}
}

func TestMentionsFollowClientCallName(t *testing.T) {
	f, err := parseQueryToolsFileAs(writeQueryToolsFile(t, prefixedToolsYAML), builtinToolNames("things"), "{server}_{tool}")
	if err != nil {
		t.Fatalf("parseQueryToolsFileAs: %v", err)
	}
	// Served names are unchanged — the client adds its namespace, not us.
	find := specNamed(t, f, "th_ppl_find_person")
	// Every mention, own group included, is the string the model must emit;
	// an ungrouped tool has no server to name and is left bare.
	want := "One person by id. Use things-people_th_ppl_person_count for the total, things-billing_th_bill_invoice_total (on the things-billing server) for money, and things-billing_th_bill_invoice_total again here. Related to things-people_th_ppl_find_person_history."
	if find.Description != want {
		t.Errorf("description =\n  %q\nwant\n  %q", find.Description, want)
	}
	if got, want := find.Parameters[0].Description, "person id, from things-billing_th_bill_invoice_total (on the things-billing server) or healthcheck"; got != want {
		t.Errorf("parameter description = %q, want %q", got, want)
	}
	// A group with no label is namespaced by its name, as its tag says.
	audit := specNamed(t, f, "th_audit_log")
	if want := "Audit entries; see things-people_th_ppl_find_person (on the things-people server)."; audit.Description != want {
		t.Errorf("audit_log description = %q, want %q", audit.Description, want)
	}
	// The namespace carries the server, so even the unprefixed audit group
	// is tagged once per description, not once per tool.
	rep := specNamed(t, f, "th_bill_reporter")
	if want := "audit_th_audit_log (on the audit server), audit_th_audit_trail - gets"; !strings.Contains(rep.Description, want) {
		t.Errorf("reporter description = %q, want it to contain %q", rep.Description, want)
	}

	// The initialize text shows the full call form with a real example.
	cfg := &config{transport: transportHTTP, httpAddr: "127.0.0.1:8080", toolPrefix: "things", toolCallName: "{server}_{tool}",
		queryTools: f.Specs, queryToolGroups: f.Groups}
	text := groupInstructions(cfg, f.Groups[0], "Things")
	if !strings.Contains(text, "called things-people_th_ppl_… (for example things-people_th_ppl_find_person)") {
		t.Errorf("instructions do not show the call form:\n%s", text)
	}
	// An unprefixed group still has the client's namespace to point at.
	if text := groupInstructions(cfg, f.Groups[2], "Things"); !strings.Contains(text, "called audit_… (for example audit_th_audit_log)") {
		t.Errorf("unprefixed group instructions do not show the namespace:\n%s", text)
	}
}
