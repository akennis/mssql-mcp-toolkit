package main

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const groupedToolsYAML = `
groups:
  people:
    label: things-people
    port: 9101
    description: Finding people.
  billing:
    label: things-billing
    order: 5
    description: Money.
tools:
  - name: find_person
    group: people
    description: One person by id.
    query: SELECT id, name FROM dbo.Person WHERE id = @id
    parameters:
      - name: id
        type: int
        description: person id
    outputFormat: csv
  - name: person_count
    group: people
    description: How many people.
    query: SELECT COUNT(*) AS n FROM dbo.Person
    outputFormat: scalar
  - name: invoice_total
    group: billing
    description: One invoice total.
    query: SELECT total FROM dbo.Invoice WHERE id = @id
    parameters:
      - name: id
        type: int
        description: invoice id
    outputFormat: scalar
  - name: healthcheck
    description: Ungrouped ping.
    query: SELECT 1 AS ok
    outputFormat: scalar
`

func TestParseQueryToolsFileGroups(t *testing.T) {
	f, err := parseQueryToolsFile(writeQueryToolsFile(t, groupedToolsYAML), builtinToolNames("things"))
	if err != nil {
		t.Fatalf("parseQueryToolsFile: %v", err)
	}
	if len(f.Specs) != 4 {
		t.Fatalf("got %d specs, want 4", len(f.Specs))
	}
	// Groups are ordered by first appearance among the tools.
	if len(f.Groups) != 2 || f.Groups[0].Name != "people" || f.Groups[1].Name != "billing" {
		t.Fatalf("groups = %+v, want [people billing]", f.Groups)
	}
	if f.Groups[0].Label != "things-people" || f.Groups[0].Port != 9101 {
		t.Errorf("people group = %+v, want label things-people port 9101", f.Groups[0])
	}
	if f.Groups[1].Order != 5 {
		t.Errorf("billing group order = %d, want 5", f.Groups[1].Order)
	}
	if got := specsInGroup(f.Specs, "people"); len(got) != 2 {
		t.Errorf("people has %d tools, want 2", len(got))
	}
	if got := specsInGroup(f.Specs, ""); len(got) != 1 || got[0].Name != "healthcheck" {
		t.Errorf("ungrouped = %+v, want [healthcheck]", got)
	}
}

func TestParseQueryToolsFileSynthesizesUndeclaredGroup(t *testing.T) {
	f, err := parseQueryToolsFile(writeQueryToolsFile(t, `
tools:
  - name: a_tool
    group: solo
    description: A tool.
    query: SELECT 1 AS ok
    outputFormat: scalar
`), nil)
	if err != nil {
		t.Fatalf("parseQueryToolsFile: %v", err)
	}
	if len(f.Groups) != 1 || f.Groups[0].Name != "solo" || f.Groups[0].Label != "" {
		t.Fatalf("groups = %+v, want one synthesised 'solo' with no label", f.Groups)
	}
}

func TestParseQueryToolsFileRejects(t *testing.T) {
	cases := map[string]string{
		"unused groups entry": `
groups:
  ghost:
    port: 9000
tools:
  - name: a_tool
    group: real
    description: A tool.
    query: SELECT 1 AS ok
    outputFormat: scalar
`,
		"port and order together": `
groups:
  both:
    port: 9000
    order: 2
tools:
  - name: a_tool
    group: both
    description: A tool.
    query: SELECT 1 AS ok
    outputFormat: scalar
`,
		"out of range port": `
groups:
  big:
    port: 70000
tools:
  - name: a_tool
    group: big
    description: A tool.
    query: SELECT 1 AS ok
    outputFormat: scalar
`,
		"unknown wrapper key": `
groupz:
  x: {}
tools:
  - name: a_tool
    description: A tool.
    query: SELECT 1 AS ok
    outputFormat: scalar
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseQueryToolsFile(writeQueryToolsFile(t, body), nil); err == nil {
				t.Fatal("parseQueryToolsFile succeeded, want an error")
			}
		})
	}
}

// The shipped example is the bare-list format; it must keep parsing, and it
// must not grow phantom groups.
func TestLegacyListFileHasNoGroups(t *testing.T) {
	f, err := parseQueryToolsFile("query-tools.example.yaml", builtinToolNames("sales"))
	if err != nil {
		t.Fatalf("parseQueryToolsFile: %v", err)
	}
	if len(f.Groups) != 0 {
		t.Errorf("legacy file yielded %d groups, want 0", len(f.Groups))
	}
	for _, s := range f.Specs {
		if s.Group != "" {
			t.Errorf("tool %s has group %q, want none", s.Name, s.Group)
		}
	}
}

func TestResolveGroupPorts(t *testing.T) {
	groups := func(gs ...*queryToolGroup) []*queryToolGroup { return gs }

	t.Run("explicit, offset and auto together", func(t *testing.T) {
		cfg := &config{
			transport: transportHTTP, httpAddr: "127.0.0.1:8080",
			queryToolGroups: groups(
				&queryToolGroup{Name: "auto1"},
				&queryToolGroup{Name: "fixed", Port: 9000},
				&queryToolGroup{Name: "offset", Order: 3},
				&queryToolGroup{Name: "auto2"},
			),
		}
		if err := resolveGroupPorts(cfg); err != nil {
			t.Fatalf("resolveGroupPorts: %v", err)
		}
		got := map[string]int{}
		for _, g := range cfg.queryToolGroups {
			got[g.Name] = g.resolvedPort
		}
		want := map[string]int{"auto1": 8081, "auto2": 8082, "offset": 8083, "fixed": 9000}
		for name, port := range want {
			if got[name] != port {
				t.Errorf("%s -> %d, want %d (all: %v)", name, got[name], port, got)
			}
		}
		// Sorted by resolved port.
		for i := 1; i < len(cfg.queryToolGroups); i++ {
			if cfg.queryToolGroups[i-1].resolvedPort > cfg.queryToolGroups[i].resolvedPort {
				t.Errorf("groups not sorted by port: %v", cfg.queryToolGroups)
			}
		}
	})

	t.Run("group collides with the base port", func(t *testing.T) {
		cfg := &config{
			transport: transportHTTP, httpAddr: "127.0.0.1:8080",
			queryToolGroups: groups(&queryToolGroup{Name: "x", Port: 8080}),
		}
		if err := resolveGroupPorts(cfg); err == nil {
			t.Fatal("resolveGroupPorts succeeded, want a base-port collision error")
		}
	})

	t.Run("two groups collide", func(t *testing.T) {
		cfg := &config{
			transport: transportHTTP, httpAddr: "127.0.0.1:8080",
			queryToolGroups: groups(
				&queryToolGroup{Name: "x", Port: 9000},
				&queryToolGroup{Name: "y", Order: 920},
			),
		}
		if err := resolveGroupPorts(cfg); err == nil {
			t.Fatal("resolveGroupPorts succeeded, want a collision error")
		}
	})

	t.Run("port 0 base rejected with groups", func(t *testing.T) {
		cfg := &config{
			transport: transportHTTP, httpAddr: "127.0.0.1:0",
			queryToolGroups: groups(&queryToolGroup{Name: "x"}),
		}
		if err := resolveGroupPorts(cfg); err == nil {
			t.Fatal("resolveGroupPorts succeeded, want port-0 rejection")
		}
	})
}

func TestBuildServersGroupedHTTP(t *testing.T) {
	cfg := groupedTestConfig(t, transportHTTP)

	servers := buildServers(cfg, openStub(t), nil)
	if len(servers) != 3 {
		t.Fatalf("got %d servers, want 3 (base + people + billing)", len(servers))
	}
	byName := map[string]namedServer{}
	for _, s := range servers {
		byName[s.name] = s
	}

	base, ok := byName[serverLabelFor(cfg)]
	if !ok {
		t.Fatalf("no base server named %q in %v", serverLabelFor(cfg), byName)
	}
	if base.addr != "127.0.0.1:8080" {
		t.Errorf("base addr = %q, want 127.0.0.1:8080", base.addr)
	}
	if got := toolNames(t, base.server); !has(got, "things_query") || !has(got, "healthcheck") || has(got, "find_person") {
		t.Errorf("base tools = %v, want built-ins + healthcheck and not find_person", got)
	}

	people, ok := byName["things-people"]
	if !ok {
		t.Fatalf("no people server in %v", byName)
	}
	if people.addr != "127.0.0.1:9101" {
		t.Errorf("people addr = %q, want 127.0.0.1:9101", people.addr)
	}
	if got := toolNames(t, people.server); !has(got, "find_person") || !has(got, "person_count") || has(got, "things_query") || has(got, "invoice_total") {
		t.Errorf("people tools = %v, want just its two tools and no built-ins", got)
	}

	billing, ok := byName["things-billing"]
	if !ok {
		t.Fatalf("no billing server in %v", byName)
	}
	if billing.addr != "127.0.0.1:8085" { // order: 5 -> base 8080 + 5
		t.Errorf("billing addr = %q, want 127.0.0.1:8085", billing.addr)
	}
}

func TestBuildServersStdioCollapsesGroups(t *testing.T) {
	cfg := groupedTestConfig(t, transportStdio)

	servers := buildServers(cfg, openStub(t), nil)
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want 1 under stdio", len(servers))
	}
	got := toolNames(t, servers[0].server)
	for _, name := range []string{"things_query", "find_person", "person_count", "invoice_total", "healthcheck"} {
		if !has(got, name) {
			t.Errorf("stdio server missing %q; has %v", name, got)
		}
	}
}

// With --query-tools-only and every tool grouped, the base --http-addr port
// has nothing to serve and is skipped: one listener per group, no more.
func TestBuildServersQueryToolsOnlyAllGroupedSkipsBase(t *testing.T) {
	f, err := parseQueryToolsFile(writeQueryToolsFile(t, `
groups:
  a: {port: 9001}
  b: {port: 9002}
tools:
  - name: t_a
    group: a
    description: A.
    query: SELECT 1 AS ok
    outputFormat: scalar
  - name: t_b
    group: b
    description: B.
    query: SELECT 1 AS ok
    outputFormat: scalar
`), nil)
	if err != nil {
		t.Fatalf("parseQueryToolsFile: %v", err)
	}
	cfg := &config{
		transport: transportHTTP, httpAddr: "127.0.0.1:8080", queryToolsOnly: true,
		queryTimeout: time.Second, maxOpenConns: 1, maxRows: 10,
		queryTools: f.Specs, queryToolGroups: f.Groups,
	}
	if err := resolveGroupPorts(cfg); err != nil {
		t.Fatalf("resolveGroupPorts: %v", err)
	}
	servers := buildServers(cfg, openStub(t), nil)
	if len(servers) != 2 {
		t.Fatalf("got %d servers, want 2 (no base)", len(servers))
	}
	for _, s := range servers {
		if s.addr == "127.0.0.1:8080" {
			t.Errorf("a server is still bound to the base port: %+v", s)
		}
	}
}

// --- helpers ---

func groupedTestConfig(t *testing.T, mode transportMode) *config {
	t.Helper()
	f, err := parseQueryToolsFile(writeQueryToolsFile(t, groupedToolsYAML), builtinToolNames("things"))
	if err != nil {
		t.Fatalf("parseQueryToolsFile: %v", err)
	}
	cfg := &config{
		transport: mode, toolPrefix: "things",
		queryTimeout: time.Second, maxOpenConns: 1, maxRows: 10,
		queryTools: f.Specs, queryToolGroups: f.Groups,
	}
	if mode == transportHTTP {
		cfg.httpAddr = "127.0.0.1:8080"
		if err := resolveGroupPorts(cfg); err != nil {
			t.Fatalf("resolveGroupPorts: %v", err)
		}
	}
	return cfg
}

func toolNames(t *testing.T, server *mcp.Server) []string {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { ss.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "probe", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })

	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("listing tools: %v", err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func has(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
