package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The prefix is required, and the failure has to be legible: whoever hits it
// is looking at a server that used to start.
func TestLoadConfigRequiresAToolPrefix(t *testing.T) {
	_, err := loadConfig([]string{"--conn-string", "server=localhost;database=AdventureWorks"})
	if err == nil {
		t.Fatal("loadConfig with no prefix = nil, want an error")
	}
	if !strings.Contains(err.Error(), "--tool-prefix") {
		t.Errorf("error = %v, want it to name --tool-prefix", err)
	}
}

// The error turns a hard failure into a one-line fix by suggesting a prefix
// read out of the connection string — in every form the string is written in.
func TestMissingPrefixErrorSuggestsAPrefix(t *testing.T) {
	cases := []struct {
		name       string
		connString string
		want       string
	}{
		{"URL form", "sqlserver://user:pw@host:1433?database=AdventureWorks&encrypt=true", "--tool-prefix=adventureworks"},
		{"ADO form", "server=host,1433;user id=u;password=p;database=Sales_DW;encrypt=true", "--tool-prefix=sales_dw"},
		{"initial catalog", "Server=host;Initial Catalog=HR Warehouse;Integrated Security=SSPI", "--tool-prefix=hr_warehouse"},
	}
	for _, c := range cases {
		err := missingPrefixError(c.connString)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %v, want it to suggest %q", c.name, err, c.want)
		}
	}

	// A connection string naming no database gets the bare message rather than
	// a suggestion built out of nothing.
	err := missingPrefixError("server=localhost;trusted_connection=yes")
	if err == nil || strings.Contains(err.Error(), "try --tool-prefix=") {
		t.Errorf("error = %v, want no suggestion when no database is named", err)
	}
}

func TestDatabaseName(t *testing.T) {
	cases := []struct {
		connString string
		want       string
	}{
		{"sqlserver://user:pw@host:1433?database=AdventureWorks&encrypt=true", "AdventureWorks"},
		{"sqlserver://host/SQLEXPRESS?Database=Sales", "Sales"},
		{"sqlserver://user:pw@host:1433?encrypt=true", ""},
		{"server=host,1433;user id=u;password=p;database=Sales;encrypt=true", "Sales"},
		{"Server=host;Initial Catalog=Sales;Integrated Security=SSPI", "Sales"},
		{"server=localhost;trusted_connection=yes", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := databaseName(c.connString); got != c.want {
			t.Errorf("databaseName(%q) = %q, want %q", c.connString, got, c.want)
		}
	}
}

// A prefix that is present but empty is absent: passing --tool-prefix= is a
// config that looks filled in and is not.
func TestBlankToolPrefixIsMissing(t *testing.T) {
	for _, blank := range []string{"", "   ", "_", "-"} {
		_, err := loadConfig([]string{"--conn-string", "server=localhost", "--tool-prefix", blank})
		if err == nil {
			t.Errorf("--tool-prefix=%q was accepted, want the missing-prefix error", blank)
		}
	}
}

// An operator who thinks of the prefix as a stem writes the separator
// themselves; both spellings have to name the same tools.
func TestToolPrefixNormalizesTrailingSeparator(t *testing.T) {
	for _, prefix := range []string{"sales_", "sales-", "sales__", " sales "} {
		cfg, err := loadConfig([]string{"--conn-string", "server=localhost", "--tool-prefix", prefix})
		if err != nil {
			t.Fatalf("--tool-prefix=%q: loadConfig: %v", prefix, err)
		}
		if got := toolName(cfg.toolPrefix, queryToolSuffix); got != "sales_query" {
			t.Errorf("--tool-prefix=%q composes %q, want sales_query", prefix, got)
		}
	}
}

// It is the composed name that has to be acceptable to a client, not the
// prefix: a prefix well inside the length limit can still push the longest
// tool name past it.
func TestToolPrefixValidatesComposedNames(t *testing.T) {
	// 55 characters: <prefix>_query fits inside the 64-character limit, the
	// longest composed name does not.
	long := strings.Repeat("a", 55)
	longest := ""
	for _, s := range toolSuffixes {
		if len(s) > len(longest) {
			longest = s
		}
	}
	cases := []struct {
		prefix   string
		wantName string
	}{
		{long, long + "_" + longest},
		{"has space", "has space_" + longest},
		{"bad/name", "bad/name_" + longest},
	}
	for _, c := range cases {
		_, err := loadConfig([]string{"--conn-string", "server=localhost", "--tool-prefix", c.prefix})
		if err == nil {
			t.Errorf("--tool-prefix=%q was accepted, want an error", c.prefix)
			continue
		}
		// The longest offending tool name is what the operator has to shorten
		// or fix, so it is what the message has to show.
		if !strings.Contains(err.Error(), c.wantName) {
			t.Errorf("--tool-prefix=%q: error = %v, want it to name %q", c.prefix, err, c.wantName)
		}
	}
}

// Instructions are delivered once at initialize and shape the whole session,
// so an empty one wastes the strongest per-database lever there is.
func TestInitializeCarriesInstructionsAndAServerLabel(t *testing.T) {
	cs := connectTestClient(t, &config{
		queryTimeout: 5 * time.Second, maxOpenConns: 1, toolPrefix: "sales",
		connString: "server=host;database=AdventureWorks",
	})
	init := cs.InitializeResult()
	if !strings.Contains(init.Instructions, "AdventureWorks") {
		t.Errorf("instructions = %q, want them to name the database", init.Instructions)
	}
	if !strings.Contains(init.Instructions, "sales_query") {
		t.Errorf("instructions = %q, want them to name the query tool", init.Instructions)
	}
	// N registrations of one binary must not show one name N times.
	if init.ServerInfo.Name != "sqlserver-mcp-sales" {
		t.Errorf("server name = %q, want it derived from the prefix", init.ServerInfo.Name)
	}
}

func TestServerLabelOverride(t *testing.T) {
	cs := connectTestClient(t, &config{
		queryTimeout: 5 * time.Second, maxOpenConns: 1, toolPrefix: "sales",
		serverLabel: "Sales warehouse",
	})
	if got := cs.InitializeResult().ServerInfo.Name; got != "Sales warehouse" {
		t.Errorf("server name = %q, want the configured label", got)
	}
}

func TestInstructionsOverride(t *testing.T) {
	cs := connectTestClient(t, &config{
		queryTimeout: 5 * time.Second, maxOpenConns: 1, toolPrefix: "sales",
		instructions: "Ask the warehouse about orders only.",
	})
	got := cs.InitializeResult().Instructions
	if !strings.HasPrefix(got, "Ask the warehouse about orders only.\n\n") {
		t.Errorf("instructions = %q, want the configured text", got)
	}
	// The date is a fact, not policy, so it follows configured text too.
	if want := time.Now().Format("2006-01-02"); !strings.Contains(got, want) {
		t.Errorf("instructions = %q, want today's date", got)
	}
}

// The date goes in at each initialize, from the clock then, not at startup.
func TestTodayInInstructions(t *testing.T) {
	day := time.Date(2026, 9, 24, 13, 0, 0, 0, time.Local)
	mw := todayInInstructions(func() time.Time { return day })
	h := mw(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return &mcp.InitializeResult{Instructions: "Base.\n"}, nil
	})
	res, err := h(context.Background(), "initialize", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "Base.\n\nToday is Thursday, 2026-09-24."
	if got := res.(*mcp.InitializeResult).Instructions; !strings.HasPrefix(got, want) {
		t.Errorf("instructions = %q, want prefix %q", got, want)
	}
	if got := withToday("", day); !strings.HasPrefix(got, "Today is Thursday, 2026-09-24.") {
		t.Errorf("no base text: %q", got)
	}
}

// Read-only is how the server will actually behave, so the model is told in
// the instructions as well as the tool description.
func TestInstructionsNoteReadOnly(t *testing.T) {
	cfg := &config{toolPrefix: "sales", readOnly: true}
	if got := defaultInstructions(cfg, "AdventureWorks"); !strings.Contains(got, "read-only") {
		t.Errorf("instructions = %q, want a read-only note", got)
	}
	cfg.readOnly = false
	if got := defaultInstructions(cfg, "AdventureWorks"); strings.Contains(got, "read-only") {
		t.Errorf("instructions = %q, want no read-only note when writes are allowed", got)
	}
}

// A config built without a prefix is a programming error: composing "_query"
// from it would hand every instance the same name again.
func TestNewServerPanicsWithoutAPrefix(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("newServer with no prefix returned normally, want a panic")
		}
	}()
	newServer(&config{queryTimeout: time.Second, maxOpenConns: 1}, openStub(t), nil)
}

// A tool call still reaches the database when the server was built by
// loadConfig rather than a test literal, which is the path a real client takes.
func TestLoadedConfigNamesTheToolAfterThePrefix(t *testing.T) {
	cfg, err := loadConfig([]string{"--conn-string", "sqlserver://host?database=AdventureWorks", "--tool-prefix", "sales"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	tools, err := connectTestClient(t, cfg).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	// The three database tools, and the stored-result tools beside them;
	// every one of them carries the prefix.
	if want := 3 + len(handleToolNames(cfg)) - 1; len(tools.Tools) != want { // calc_sql is off by default
		t.Fatalf("got %d tools, want %d: %+v", len(tools.Tools), want, tools.Tools)
	}
	var queryTool *mcp.Tool
	for _, tool := range tools.Tools {
		if !strings.HasPrefix(tool.Name, "sales_") {
			t.Errorf("tool %q does not carry the prefix", tool.Name)
		}
		if tool.Name == "sales_query" {
			queryTool = tool
		}
	}
	if queryTool == nil {
		t.Fatalf("missing sales_query tool: %+v", tools.Tools)
	}
	if !strings.Contains(queryTool.Description, "AdventureWorks") {
		t.Errorf("description = %q, want it to name the database", queryTool.Description)
	}
}

// The catalog in the connection string is often not what anyone asking a
// question calls the database, and the name in the descriptions is what the
// model matches a question against — so --descr-db overrides it everywhere the
// database is named, and nothing else changes.
func TestDescrDatabaseOverridesTheConnectionStringName(t *testing.T) {
	cfg, err := loadConfig([]string{
		"--conn-string", "sqlserver://host?database=SALES_DW_PRD01",
		"--tool-prefix", "sales",
		"--descr-db", "  AdventureWorks sales warehouse  ",
	})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	const want = "AdventureWorks sales warehouse"
	if got := displayDatabase(cfg); got != want {
		t.Errorf("displayDatabase = %q, want %q", got, want)
	}

	cs := connectTestClient(t, cfg)
	if init := cs.InitializeResult(); !strings.Contains(init.Instructions, want) {
		t.Errorf("instructions = %q, want them to name %q", init.Instructions, want)
	}
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range tools.Tools {
		// The stored-result tools never reach the database, so they have no
		// database to name.
		if handleToolNames(cfg)[tool.Name] {
			continue
		}
		if !strings.Contains(tool.Description, want) {
			t.Errorf("%s description = %q, want it to name %q", tool.Name, tool.Description, want)
		}
		if strings.Contains(tool.Description, "SALES_DW_PRD01") {
			t.Errorf("%s description = %q, want it not to name the connection string's catalog", tool.Name, tool.Description)
		}
	}
}

// Without --descr-db the connection string's own database name still stands,
// and a blank one counts as absent rather than as a database called "".
func TestDisplayDatabaseFallsBackToTheConnectionString(t *testing.T) {
	for _, descr := range []string{"", "   "} {
		cfg := &config{connString: "server=host;database=AdventureWorks", descrDatabase: descr}
		if got := displayDatabase(cfg); got != "AdventureWorks" {
			t.Errorf("--descr-db=%q: displayDatabase = %q, want AdventureWorks", descr, got)
		}
	}
}

// The prefix is a collision guard, not a fact about the database: describing
// it as a "system database" told the model something untrue about every server
// whose prefix differed from its catalog name.
func TestDescriptionsDoNotCallThePrefixASystemDatabase(t *testing.T) {
	cfg := &config{
		queryTimeout: 5 * time.Second, maxOpenConns: 1, toolPrefix: "wh",
		connString: "server=host;database=AdventureWorks",
	}
	tools, err := connectTestClient(t, cfg).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range tools.Tools {
		if strings.Contains(tool.Description, "system database") {
			t.Errorf("%s description = %q, want no system-database note", tool.Name, tool.Description)
		}
	}
}
