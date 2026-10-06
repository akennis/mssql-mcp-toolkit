// Command mssql-mcp-toolkit is a Model Context Protocol server that exposes a
// single tool for running a T-SQL query against Microsoft SQL Server and
// returning its result set.
//
// The SQL Server connection string is supplied by the MCP client through
// configuration, as are all the other settings: command-line flags are the
// only input this server takes.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	_ "github.com/microsoft/go-mssqldb"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	serverName    = "mssql-mcp-toolkit"
	serverVersion = "0.1.0"
)

// The default output budget. These are caps, not targets: a query that wanted
// fewer rows should say so with TOP or a WHERE clause, and one that hits a cap
// gets told which one it hit.
//
// 200 rows is enough to answer a question or show a model the shape of a
// table, and small enough that a mistaken SELECT * costs a fraction of a
// context window rather than all of it. 256 KiB is the same argument applied
// to width, for the rows that are 200 columns wide. 4 KiB per value is roughly
// a page of text: past that a cell is a document, and a caller that wants the
// document should ask for that one value on its own.
const (
	defaultMaxRows      = 200
	defaultMaxBytes     = 256 * 1024
	defaultMaxCellBytes = 4 * 1024
	// A stored result keeps every value far longer than a reply shows it: the
	// reply cuts a value to defaultDisplayCellChars (see shortenCells) and the
	// field tool hands the rest back on request, so the store has to hold it.
	defaultMaxStoredCellBytes = 64 * 1024
	defaultDisplayCellChars   = 200
)

// httpHeaderName is what an HTTP header name may contain (RFC 9110 token).
var httpHeaderName = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]+$")

// toolNamePattern is what MCP clients can be relied on to accept for a tool
// name: letters, digits, underscore and hyphen.
var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type config struct {
	connString string
	// How this instance is reached. The two modes are exclusive: stdio serves
	// the one client that launched the process, http listens on a loopback
	// address for clients that attach to it. See transportMode.
	transport transportMode
	// httpAddr is the loopback host:port transportHTTP listens on, and is
	// blank under stdio, which has nothing to listen on.
	httpAddr string
	// httpStateless drops the per-session state the streamable transport
	// keeps, for clients that do not carry an Mcp-Session-Id back. See
	// serveHTTP.
	httpStateless bool
	// httpLog turns on one log line per HTTP request. Off by default: it is a
	// diagnostic, and a busy server writes a line per call.
	httpLog bool
	// httpLogHeaders logs every HTTP header received with each tools/call, one
	// line per header. Off by default: it is a debug aid, it is verbose, and it
	// puts whatever the client sent — auth tokens included — into the log. Only
	// the HTTP transport carries headers; a stdio call has none. See
	// logToolCallHeaders.
	httpLogHeaders bool
	// httpAuthToken is the shared secret every HTTP request must carry as
	// "Authorization: Bearer <token>", read from --http-auth-token-file. Blank
	// means the listener takes any request that reaches it. See caller.go.
	httpAuthToken string
	// userHeader is the --user-header name: the HTTP header the client stamps
	// with the signed-in user's id. Stored results are kept per user by it.
	// Blank means every HTTP caller shares one set of handles.
	userHeader string
	// resultStore turns on stored results: every list result gets a handle
	// that later calls can read (@handle.Column), page (show) and combine (the
	// operator tools). The limits below bound it. See store.go.
	resultStore    bool
	storeMaxRows   int
	storeMaxBytes  int
	storeUserBytes int
	storeTTL       time.Duration
	calcSQL        bool
	// results and calc are the process-wide result store and operator engine,
	// created once by ensureRuntime and shared by every server this process
	// runs, so a handle made on one group's port is readable on another's.
	results *resultStore
	calc    *calcEngine
	// The output budget. Each uses 0 for "no cap", and all three default to a
	// real limit: an uncapped result is read by a model with a finite context
	// window, and one SELECT is enough to fill it. See budget.
	maxRows      int
	maxBytes     int
	maxCellBytes int
	// maxStoredCellBytes is maxCellBytes for a value captured into the result
	// store; displayCellChars is how much of a value any stored result's reply
	// shows (0 shows it whole). See shortenCells.
	maxStoredCellBytes int
	displayCellChars   int
	queryTimeout       time.Duration
	readOnly           bool
	maxOpenConns       int
	// toolPrefix names every tool this instance exposes (<prefix>_query). It
	// is required at startup; see missingPrefixError.
	toolPrefix string
	// descrDatabase is the database name the descriptions and instructions
	// use, for the common case where the catalog in the connection string is
	// not what anyone asking a question calls it. Blank falls back to the
	// connection string's own name; see displayDatabase.
	descrDatabase string
	// The remaining fields are how this instance introduces itself. Each is
	// blank unless the operator set it, and is resolved to a default derived
	// from the connection string by newServer, which is where identity is
	// composed.
	//
	// One description per tool: the text a model reads when it picks a tool is
	// per-tool, so an operator who wants to say what lives in this database
	// can say it on each of them.
	queryDescription        string
	getMetadataDescription  string
	listMetadataDescription string
	serverLabel             string
	// toolCallName is how the client composes the name a model calls a tool
	// by, when it namespaces tools by server: a template over {server} (the
	// group's label) and {tool} (the published tool name). Open WebUI uses
	// {server}_{tool} with its connection ID as {server}. The loader writes
	// cross-references and initialize text in this form, so the model can
	// copy a name it reads. "{tool}" means the client uses names as served.
	toolCallName  string
	instructions  string
	metadataFiles map[string]string
	metadataDir   string
	// queryToolsPath is the --query-tools file, a YAML list describing extra
	// tools this server should expose, each one a named, parameterized query
	// with its own connection string and output format. Blank means the server
	// exposes only its built-in tools.
	queryToolsPath string
	// queryTools is queryToolsPath parsed and validated. It is nil when the
	// flag was not given.
	queryTools []*queryToolSpec
	// querySharedParams is the --query-tools file's `sharedParameters:` block:
	// the parameters many tools take under one name, described once in the
	// initialize instructions instead of on every tool. See queryToolNotes.
	querySharedParams []queryToolParam
	// queryToolRewrite qualifies tool mentions in text rendered per server
	// after loading (the shared-parameter notes); nil without --query-tools.
	queryToolRewrite mentionRewriter
	// queryToolGroups is the logical-server partition declared by the
	// --query-tools file, ordered by the port each group listens on. It is
	// empty when no tool declares a group, and is only acted on under
	// --transport=http; see buildServers.
	queryToolGroups []*queryToolGroup
	// queryToolsOnly drops the built-in tools — <prefix>_query and the two
	// metadata tools — so the server exposes only the tools defined by
	// --query-tools. It requires --query-tools: with the built-ins gone and no
	// custom tools, the server would expose nothing.
	queryToolsOnly bool
	// metadataDirSchema is the schema that files in metadataDir document when
	// their names do not name one themselves, so a directory of Orders.md /
	// Customers.md can answer for dbo.orders and dbo.customers.
	metadataDirSchema string
}

// queryInput is the tool's argument schema. Fields without `omitempty` are
// required in the generated JSON Schema.
//
// The max_rows description is written by queryInputSchema instead of a struct
// tag, because it names the server's configured cap.
type queryInput struct {
	Query   string   `json:"query" jsonschema:"a single T-SQL statement to execute against SQL Server, for example: SELECT TOP 10 * FROM schema.table"`
	MaxRows rowLimit `json:"max_rows,omitempty"`
}

// queryInputSchema is the schema inferred from queryInput, with max_rows
// widened to accept a JSON number or the same number quoted as a string. The
// SDK validates arguments against this schema before they reach the handler,
// so the string form has to be admitted here as well as in
// rowLimit.UnmarshalJSON.
func queryInputSchema(configuredMaxRows int) (*jsonschema.Schema, error) {
	schema, err := jsonschema.For[queryInput](nil)
	if err != nil {
		return nil, err
	}
	maxRows, ok := schema.Properties["max_rows"]
	if !ok {
		return nil, errors.New("inferred schema has no max_rows property")
	}
	// Type and Types are mutually exclusive; clear the inferred single type.
	maxRows.Type = ""
	// null is admitted alongside the number and string forms because a model
	// that means "no cap" spells it several ways (omitted, null, 0, ""), and
	// the SDK validates against this schema before rowLimit.UnmarshalJSON —
	// which already treats all of them as "not supplied" — gets a look.
	maxRows.Types = []string{"integer", "string", "null"}
	maxRows.Minimum = float64Ptr(0) // constrains the number form only
	maxRows.Pattern = `^[0-9]*$`    // constrains the string form only
	omitted := "Omit it and every row the query produces is returned."
	if configuredMaxRows > 0 {
		omitted = fmt.Sprintf(
			"Omit it to use this server's cap of %d rows; any larger value is silently lowered to that cap.",
			configuredMaxRows)
	}
	maxRows.Description = "OPTIONAL, and safe to leave out entirely: a cap on how many rows come back, " +
		"as a whole number of rows: preferably a JSON number (max_rows: 100), " +
		"though the same value quoted as a string (max_rows: \"100\") is also accepted. " +
		omitted +
		" This truncates the result set after the fact, so prefer TOP or a WHERE clause in the query itself when you want fewer rows."
	return schema, nil
}

// queryResult is the tool's structured output.
//
// Query restates the statement exactly as it was received, so that a caller
// logging or auditing the response has the SQL that produced it without
// having to correlate the result back to its own request.
type queryResult struct {
	Query        string           `json:"query" jsonschema:"the T-SQL statement that was executed, exactly as it was received"`
	Columns      []string         `json:"columns" jsonschema:"column names, in result-set order"`
	Rows         []map[string]any `json:"rows" jsonschema:"result rows, one object per row keyed by column name"`
	RowCount     int              `json:"row_count" jsonschema:"number of rows returned"`
	RowsAffected int64            `json:"rows_affected" jsonschema:"rows affected, for statements that report it; -1 when unknown"`
	Truncated    bool             `json:"truncated" jsonschema:"true when anything was cut: rows dropped at the row limit or the payload budget, or an oversized cell value shortened"`
	Notes        []string         `json:"notes,omitempty" jsonschema:"advisory messages about the execution, such as truncation or extra result sets"`
	// Handle names the stored copy of this result (see store.go): the whole
	// result set up to the store's limits, which later calls page with the
	// show tool, pass to batch parameters as @handle.Column, and combine with
	// the operator tools. Empty when results are not stored.
	Handle string `json:"handle,omitempty" jsonschema:"the stored result's handle, for show, handle.Column arguments and the operator tools"`
	// TotalRows is how many rows the stored result holds, which can be more
	// than row_count when only a page or a sample is shown.
	TotalRows int `json:"total_rows,omitempty" jsonschema:"rows in the stored result; row_count is how many of them are shown"`

	// types is each column's SQL Server type name, in Columns order, as the
	// driver reported it. The profile and the operator engine read it.
	types []string
	// rowsCut is set when the scan stopped at the row or payload limit, as
	// opposed to shortening a cell; rowsCutNote is the note it added.
	rowsCut     bool
	rowsCutNote string
}

// truncate records that data was withheld, and why. Truncated is one flag for
// three limits because the caller's response to all three is the same — what
// came back is not all of it — while the note says which one to act on.
func (r *queryResult) truncate(note string) {
	r.Truncated = true
	r.Notes = append(r.Notes, note)
}

func main() {
	log.SetFlags(0)
	log.SetPrefix(serverName + ": ")

	if err := run(os.Args[1:]); err != nil {
		// --help is a request that was served, not a failure: flag.Parse has
		// already printed the usage text, so exit quietly and successfully.
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Fatal(err)
	}
}

// run holds everything main used to do inline. main's only job is turning the
// error into an exit status, so that the cleanup deferred here actually runs
// instead of being skipped by log.Fatal's os.Exit.
func run(args []string) error {
	cfg, err := loadConfig(args)
	if err != nil {
		return err
	}
	// A user header nobody authenticated is a name any local process can
	// type: handles are then separated between honest callers only.
	if cfg.userHeader != "" && cfg.httpAuthToken == "" {
		log.Printf("warning: --user-header is set without --http-auth-token-file; any process that can reach the port can claim to be any user")
	}

	db, err := openDB(cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	// Signal-aware context so the process shuts down cleanly when the MCP
	// client stops it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A failed ping is reported but not fatal: the database may come up after
	// the client has already launched us, and every tool call reconnects.
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = db.PingContext(pingCtx)
	cancel()
	if err != nil {
		log.Printf("warning: initial connection failed: %v", err)
	}

	// The pool owns the connections the --query-tools tools reach through their
	// own connection strings. The one from --conn-string stays db's, closed
	// above; the pool closes only what it opened itself.
	pool := newConnPool(cfg, db)
	defer pool.closeExtra()

	return serve(ctx, cfg, buildServers(cfg, db, pool))
}

// serverBuild is the recipe for one MCP server: which query-tool specs it
// carries, whether the built-in query and metadata tools come with them, and
// optional overrides for the initialize label and instructions. A plain
// single-server run uses the zero-ish defaults through newServer; grouped runs
// fill it in per group.
type serverBuild struct {
	label        string
	instructions string
	builtins     bool
	specs        []*queryToolSpec
	// server is the {server} this server's tools are called under — its
	// group's label, or "" for the base server — for text that names them.
	server string
	// show and operators carry the stored-result tools: show on every
	// server, the operators on one. Both need --result-store.
	show      bool
	operators bool
}

// newServer builds the one server a plain run exposes: the built-in tools
// (unless --query-tools-only) and every --query-tools tool.
func newServer(cfg *config, db *sql.DB, pool *connPool) *mcp.Server {
	return newServerFor(cfg, db, pool, serverBuild{
		builtins:  !cfg.queryToolsOnly,
		specs:     cfg.queryTools,
		show:      true,
		operators: true,
	})
}

func newServerFor(cfg *config, db *sql.DB, pool *connPool, build serverBuild) *mcp.Server {
	if pool == nil {
		pool = newConnPool(cfg, db)
	}
	cfg.ensureRuntime()
	// loadConfig makes this unreachable. Getting here means a config was built
	// in code without a prefix, and composing "_query" from it would hand
	// every instance the same name again — the one failure this server is not
	// able to detect at runtime. A server with no built-in tools has no name to
	// compose, so a blank prefix is allowed there.
	if isBlank(cfg.toolPrefix) && build.builtins {
		panic("newServer: config has no tool prefix")
	}
	database := displayDatabase(cfg)

	instructions := build.instructions
	if isBlank(instructions) {
		instructions = cfg.instructions
	}
	if isBlank(instructions) {
		instructions = defaultInstructions(cfg, database)
	}
	// Whatever text is in force, the notes on the query tools it carries go
	// after it: they describe declared parameters, not editorial policy.
	if notes := queryToolNotes(cfg, build.specs); notes != "" {
		instructions = strings.TrimRight(instructions, "\n") + "\n\n" + notes
	}
	if notes := handleNotes(cfg, build); notes != "" {
		instructions = strings.TrimRight(instructions, "\n") + "\n\n" + notes
	}

	label := build.label
	if isBlank(label) {
		label = serverLabelFor(cfg)
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    label,
		Version: serverVersion,
	}, &mcp.ServerOptions{Instructions: instructions})

	// build.builtins carries the built-in query and metadata tools. It is off
	// under --query-tools-only and off for every per-group server. loadConfig
	// has already checked at least one tool is left to expose.
	if build.builtins {
		name := toolName(cfg.toolPrefix, queryToolSuffix)

		description := cfg.queryDescription
		if isBlank(description) {
			description = defaultQueryToolDescription(database, cfg.toolPrefix)
		}
		// The read-only note is appended to whatever description is in force: it
		// describes how the server will actually behave, not editorial copy.
		if cfg.readOnly {
			description += " This server is running in read-only mode: only SELECT/WITH queries are accepted."
		}

		// A bad schema is a programming error, not a runtime condition: AddTool
		// itself panics for the same reason.
		inputSchema, err := queryInputSchema(cfg.maxRows)
		if err != nil {
			panic(fmt.Sprintf("building %s input schema: %v", name, err))
		}

		mcp.AddTool(server, &mcp.Tool{
			Name:        name,
			Title:       "Run T-SQL query",
			Description: description,
			InputSchema: inputSchema,
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    cfg.readOnly,
				DestructiveHint: boolPtr(!cfg.readOnly),
				OpenWorldHint:   boolPtr(true),
			},
		}, func(ctx context.Context, req *mcp.CallToolRequest, in queryInput) (*mcp.CallToolResult, *queryResult, error) {
			return handleQueryFor(ctx, cfg, db, req, in)
		})

		registerMetadataTools(server, cfg)
	}

	registerQueryTools(server, cfg, pool, build.specs)
	if len(build.specs) > 0 {
		registerDescribeTool(server, cfg, build.specs)
	}
	if cfg.handlesOn() {
		if build.show {
			registerShowTool(server, cfg, build.server)
			registerShowFieldTool(server, cfg, build.server)
		}
		if build.operators {
			registerOperatorTools(server, cfg, build.server)
		}
	}

	server.AddReceivingMiddleware(todayInInstructions(time.Now))
	server.AddReceivingMiddleware(breakFailureLoops(cfg))

	// --http-log-headers: dump the headers every tools/call arrived with. Added
	// as receiving middleware so it covers the built-in and the --query-tools
	// tools alike, without each handler having to reach for the request.
	if cfg.httpLogHeaders {
		server.AddReceivingMiddleware(logToolCallHeaders(label))
	}

	return server
}

// namedServer is one MCP server this process runs: the name it reports at
// initialize, the loopback address it listens on under --transport=http, and
// the server itself. Under stdio there is exactly one and addr is unused.
type namedServer struct {
	name   string
	addr   string
	server *mcp.Server
}

// buildServers turns the config into the set of MCP servers to run. Without
// tool groups, or under stdio, that is a single server carrying every tool,
// exactly as before. With groups under --transport=http it is one server per
// group — each on its own port, none carrying the built-in tools — plus a base
// server on --http-addr for the built-ins and any ungrouped query tool.
func buildServers(cfg *config, db *sql.DB, pool *connPool) []namedServer {
	if pool == nil {
		pool = newConnPool(cfg, db)
	}
	cfg.ensureRuntime()
	if cfg.transport != transportHTTP || len(cfg.queryToolGroups) == 0 {
		return []namedServer{{
			name:   serverLabelFor(cfg),
			addr:   cfg.httpAddr,
			server: newServer(cfg, db, pool),
		}}
	}

	var servers []namedServer

	// The base server keeps the built-in tools and anything left ungrouped. It
	// is skipped only when there would be nothing on it: --query-tools-only and
	// every tool assigned to a group.
	ungrouped := specsInGroup(cfg.queryTools, "")
	opGroup := operatorsGroup(cfg)
	if !cfg.queryToolsOnly || len(ungrouped) > 0 {
		servers = append(servers, namedServer{
			name: serverLabelFor(cfg),
			addr: cfg.httpAddr,
			server: newServerFor(cfg, db, pool, serverBuild{
				builtins: !cfg.queryToolsOnly,
				specs:    ungrouped,
				show:     true,
				// With no group claiming the operators, they stay here.
				operators: opGroup == nil,
			}),
		})
	}

	database := displayDatabase(cfg)
	for _, g := range cfg.queryToolGroups {
		servers = append(servers, namedServer{
			name: groupLabel(cfg, g),
			addr: withPort(cfg.httpAddr, g.resolvedPort),
			server: newServerFor(cfg, db, pool, serverBuild{
				label:        groupLabel(cfg, g),
				instructions: groupInstructions(cfg, g, database),
				builtins:     false,
				specs:        specsInGroup(cfg.queryTools, g.Name),
				server:       groupServerName(g),
				show:         true,
				operators:    g.Operators,
			}),
		})
	}
	return servers
}

// operatorsGroup is the group that carries the operator tools, or nil.
func operatorsGroup(cfg *config) *queryToolGroup {
	for _, g := range cfg.queryToolGroups {
		if g.Operators {
			return g
		}
	}
	return nil
}

// specsInGroup returns the specs whose group is name. The empty string selects
// the ungrouped ones.
func specsInGroup(specs []*queryToolSpec, name string) []*queryToolSpec {
	var out []*queryToolSpec
	for _, s := range specs {
		if s.Group == name {
			out = append(out, s)
		}
	}
	return out
}

// groupLabel is the initialize name for a group's server: its configured
// label, or "<server-label>-<group>".
func groupLabel(cfg *config, g *queryToolGroup) string {
	if s := strings.TrimSpace(g.Label); s != "" {
		return s
	}
	return serverLabelFor(cfg) + "-" + g.Name
}

// withPort swaps the port of a host:port address, keeping the host. A
// malformed address falls back to loopback, which localhostAddr has already
// ruled out by the time this runs.
func withPort(addr string, port int) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// resolveGroupPorts works out the TCP port each group listens on, against the
// base port from --http-addr, and rejects a map that would collide or fall
// outside the valid range. It runs only for --transport=http.
func resolveGroupPorts(cfg *config) error {
	_, basePortStr, err := net.SplitHostPort(cfg.httpAddr)
	if err != nil {
		return fmt.Errorf("--http-addr %q: %w", cfg.httpAddr, err)
	}
	basePort, err := strconv.Atoi(basePortStr)
	if err != nil {
		return fmt.Errorf("--http-addr %q: port is not a number", cfg.httpAddr)
	}

	taken := make(map[int]string)

	// The base server holds --http-addr itself, unless --query-tools-only has
	// left it with no built-ins and no ungrouped tool to serve.
	baseActive := !cfg.queryToolsOnly || len(specsInGroup(cfg.queryTools, "")) > 0
	if baseActive {
		if basePort == 0 {
			return errors.New("--http-addr port 0 (any free port) cannot be used with tool groups: the groups are numbered from a fixed base")
		}
		taken[basePort] = serverLabelFor(cfg)
	}

	claim := func(g *queryToolGroup, port int) error {
		if port < 1 || port > 65535 {
			return fmt.Errorf("group %q resolves to port %d, outside 1-65535", g.Name, port)
		}
		if owner, ok := taken[port]; ok {
			return fmt.Errorf("group %q and %s would both listen on port %d", g.Name, owner, port)
		}
		taken[port] = "group " + g.Name
		g.resolvedPort = port
		return nil
	}

	// Explicit port / order first, so the auto-assigned groups fill the gaps
	// around them rather than landing on top.
	var auto []*queryToolGroup
	for _, g := range cfg.queryToolGroups {
		switch {
		case g.Port != 0:
			if err := claim(g, g.Port); err != nil {
				return err
			}
		case g.Order != 0:
			if err := claim(g, basePort+g.Order); err != nil {
				return err
			}
		default:
			auto = append(auto, g)
		}
	}
	next := basePort + 1
	for _, g := range auto {
		for taken[next] != "" {
			next++
		}
		if err := claim(g, next); err != nil {
			return err
		}
		next++
	}

	sort.Slice(cfg.queryToolGroups, func(i, j int) bool {
		return cfg.queryToolGroups[i].resolvedPort < cfg.queryToolGroups[j].resolvedPort
	})
	return nil
}

func handleQuery(ctx context.Context, cfg *config, db *sql.DB, in queryInput) (*mcp.CallToolResult, *queryResult, error) {
	if isBlank(in.Query) {
		return nil, nil, errors.New("query is empty")
	}
	if cfg.readOnly {
		if err := checkReadOnly(in.Query); err != nil {
			return nil, nil, withQuery(err, in.Query)
		}
	}

	// Both the configured cap and the per-call one use 0 for "no limit", so the
	// smaller of two limits is only meaningful when both are set. Only the row
	// cap is per-call: the byte budgets protect the model's context window,
	// which is not the caller's to spend.
	b := budgetFor(cfg)
	if requested := int(in.MaxRows); requested > 0 && (b.rows <= 0 || requested < b.rows) {
		b.rows = requested
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.queryTimeout)
	defer cancel()

	res, err := runQuery(ctx, db, in.Query, b)
	if err != nil {
		// Distinguish the deadline from a client cancellation: reporting a
		// cancelled call as a timeout invites the model to rewrite a query
		// that was never slow.
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			err = fmt.Errorf("query timed out after %s: %w", cfg.queryTimeout, err)
		case errors.Is(ctx.Err(), context.Canceled):
			err = fmt.Errorf("query cancelled by the client: %w", err)
		default:
			err = fmt.Errorf("query failed: %w", err)
		}
		return nil, nil, withQuery(err, in.Query)
	}

	// Also render the payload as text, for clients that ignore structured tool
	// output. See resultText: it is a markdown table rather than the same JSON
	// again, so a client honouring both channels does not pay for every row
	// twice.
	text, err := resultText(withRowNumbers(res, ownPositions))
	if err != nil {
		return nil, nil, withQuery(fmt.Errorf("encoding result: %w", err), in.Query)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}, res, nil
}

func openDB(cfg *config) (*sql.DB, error) {
	db, err := sql.Open("sqlserver", cfg.connString)
	if err != nil {
		return nil, fmt.Errorf("opening connection: %w", err)
	}
	db.SetMaxOpenConns(cfg.maxOpenConns)
	db.SetMaxIdleConns(cfg.maxOpenConns)
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(30 * time.Minute)
	return db, nil
}

func loadConfig(args []string) (*config, error) {
	cfg := &config{}

	// Flags are the whole configuration surface. There are deliberately no
	// environment twins: with one process per database, a setting that can
	// arrive from two places is a setting you have to check in two places when
	// the wrong database answers a question.
	fs := flag.NewFlagSet(serverName, flag.ContinueOnError)
	fs.StringVar(&cfg.connString, "conn-string", "",
		"REQUIRED: SQL Server connection string, in ADO or sqlserver:// URL form")
	fs.StringVar(&cfg.toolPrefix, "tool-prefix", "",
		"REQUIRED (except with --query-tools-only): prefix for every built-in tool this server exposes, so <prefix>_query cannot collide with another database's")
	transport := fs.String("transport", string(transportStdio),
		"execution mode: stdio, for a client that launches this binary and talks over its standard streams, or http, to listen on localhost instead")
	httpAddr := fs.String("http-addr", defaultHTTPAddr,
		"loopback address to listen on with --transport=http; the host must be localhost or a loopback IP, and the flag is rejected under --transport=stdio")
	fs.BoolVar(&cfg.httpStateless, "http-stateless", false,
		"with --transport=http, serve without per-session state, for clients that do not send Mcp-Session-Id back; GET and DELETE then return 405")
	fs.BoolVar(&cfg.httpLog, "http-log", false,
		"with --transport=http, log one line per HTTP request: method, host, session id and status")
	fs.BoolVar(&cfg.httpLogHeaders, "http-log-headers", false,
		"with --transport=http, log every HTTP header received with each tool call; verbose; Authorization and Cookie values are redacted")
	authTokenFile := fs.String("http-auth-token-file", "",
		"with --transport=http, a file holding a shared secret; every request must then carry \"Authorization: Bearer <secret>\" or is refused with 401")
	fs.StringVar(&cfg.userHeader, "user-header", "",
		"with --transport=http, the header that carries the calling user's id (for Open WebUI, X-OpenWebUI-User-Id); stored results are kept per user, and a call without it gets no handle")
	fs.BoolVar(&cfg.resultStore, "result-store", true,
		"store every list result under a short handle that later calls can page, pass to batch parameters as handle.Column, and combine with the operator tools; false turns handles off")
	fs.IntVar(&cfg.storeMaxRows, "result-store-max-rows", defaultStoreMaxRows,
		"most rows one stored result keeps; a result cut here is marked truncated")
	fs.IntVar(&cfg.storeMaxBytes, "result-store-max-bytes", defaultStoreMaxBytes,
		"memory the result store may use for every user together, in bytes; the least recently used results are dropped past it")
	fs.IntVar(&cfg.storeUserBytes, "result-store-user-bytes", defaultStoreUserBytes,
		"memory one user's stored results may use, in bytes, so one user cannot push out everyone else's")
	fs.DurationVar(&cfg.storeTTL, "result-store-ttl", defaultStoreTTL,
		"how long a stored result is kept after it was last used")
	fs.BoolVar(&cfg.calcSQL, "calc-sql", false,
		"also expose calc_sql, which runs a read-only SQLite SELECT over stored results; off by default")
	fs.IntVar(&cfg.maxRows, "max-rows", defaultMaxRows,
		"maximum rows returned per query; 0 means no limit")
	fs.IntVar(&cfg.maxBytes, "max-bytes", defaultMaxBytes,
		"maximum total size of the returned rows, in bytes of JSON; 0 means no limit")
	fs.IntVar(&cfg.maxCellBytes, "max-cell-bytes", defaultMaxCellBytes,
		"maximum size of a single returned value, in bytes; longer values are cut and marked; 0 means no limit")
	fs.IntVar(&cfg.maxStoredCellBytes, "max-stored-cell-bytes", defaultMaxStoredCellBytes,
		"maximum size of a single value kept in a stored result, in bytes; what show_field can return; 0 means no limit")
	fs.IntVar(&cfg.displayCellChars, "display-cell-chars", defaultDisplayCellChars,
		"longest value, in characters, a reply over a stored result shows; the rest is read with show_field; 0 shows values whole")
	fs.DurationVar(&cfg.queryTimeout, "query-timeout", 30*time.Second,
		"per-query timeout")
	fs.BoolVar(&cfg.readOnly, "read-only", false,
		"reject anything that is not a SELECT/WITH query")
	fs.IntVar(&cfg.maxOpenConns, "max-open-conns", 4,
		"maximum open database connections")
	fs.StringVar(&cfg.descrDatabase, "descr-db", "",
		"the database name used in tool descriptions and instructions, for when users know this database by a different name than the connection string does; the default is the database from --conn-string")
	fs.StringVar(&cfg.queryDescription, "query-fn-desc", "",
		"description advertised for the query tool; the default names the database from --descr-db or --conn-string")
	fs.StringVar(&cfg.getMetadataDescription, "get-metadata-fn-desc", "",
		"description advertised for the get_metadata tool; the default names the database from --descr-db or --conn-string")
	fs.StringVar(&cfg.listMetadataDescription, "list-metadata-fn-desc", "",
		"description advertised for the list_metadata tool; the default names the database from --descr-db or --conn-string")
	fs.StringVar(&cfg.serverLabel, "server-label", "",
		"name this server reports at initialize; the default is "+serverName+"-<prefix>")
	fs.StringVar(&cfg.toolCallName, "tool-call-name", defaultToolCallName,
		"how the client names tools to the model when it namespaces them by server: a template over {server} (the group's label) and {tool}; Open WebUI uses {server}_{tool} with its connection ID as {server}")
	fs.StringVar(&cfg.instructions, "instructions", "",
		"instructions delivered to the model at initialize; the default describes this database and how to query it")
	metadataFiles := make(stringMapFlag)
	fs.Var(&metadataFiles, "metadata-file",
		"specifically configured schema.table:path/to/file.md pair (can be repeated)")
	fs.StringVar(&cfg.metadataDir, "metadata-dir", "",
		"directory containing markdown or html metadata files named schema.table.md or schema.view.md")
	fs.StringVar(&cfg.metadataDirSchema, "metadata-dir-schema", "",
		"schema to assume for files in --metadata-dir that are named table.md rather than schema.table.md")
	fs.StringVar(&cfg.queryToolsPath, "query-tools", "",
		"path to a YAML file describing extra tools to expose, each a named parameterized query with its own connection string and output format (csv, md, or scalar)")
	fs.BoolVar(&cfg.queryToolsOnly, "query-tools-only", false,
		"expose only the tools defined by --query-tools, dropping the built-in <prefix>_query and metadata tools; requires --query-tools")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if isBlank(cfg.connString) {
		return nil, errors.New("no connection string: pass --conn-string")
	}
	if cfg.maxRows < 0 {
		return nil, fmt.Errorf("--max-rows must be 0 (no limit) or positive, got %d", cfg.maxRows)
	}
	if cfg.maxBytes < 0 {
		return nil, fmt.Errorf("--max-bytes must be 0 (no limit) or positive, got %d", cfg.maxBytes)
	}
	if cfg.maxCellBytes < 0 {
		return nil, fmt.Errorf("--max-cell-bytes must be 0 (no limit) or positive, got %d", cfg.maxCellBytes)
	}
	if cfg.maxStoredCellBytes < 0 {
		return nil, fmt.Errorf("--max-stored-cell-bytes must be 0 (no limit) or positive, got %d", cfg.maxStoredCellBytes)
	}
	if cfg.displayCellChars < 0 {
		return nil, fmt.Errorf("--display-cell-chars must be 0 (show whole) or positive, got %d", cfg.displayCellChars)
	}
	if cfg.queryTimeout <= 0 {
		return nil, fmt.Errorf("--query-timeout must be positive, got %s", cfg.queryTimeout)
	}
	if cfg.maxOpenConns <= 0 {
		return nil, fmt.Errorf("--max-open-conns must be positive, got %d", cfg.maxOpenConns)
	}
	mode, err := parseTransport(*transport)
	if err != nil {
		return nil, err
	}
	cfg.transport = mode
	// The --http-* flags belong to the http mode and to nothing else.
	// Accepting one under stdio and ignoring it would leave an operator
	// believing the port in their config is listening when no port is open at
	// all, so say so instead — but only when they actually passed it, since a
	// flag always carries its default.
	if cfg.transport == transportStdio {
		for _, name := range []string{"http-addr", "http-stateless", "http-log", "http-log-headers", "http-auth-token-file", "user-header"} {
			if isFlagSet(fs, name) {
				return nil, fmt.Errorf(
					"--%s applies to --transport=http only: a stdio server talks over its standard streams and listens on nothing", name)
			}
		}
	} else if cfg.httpAddr, err = localhostAddr(*httpAddr); err != nil {
		return nil, err
	}
	if strings.TrimSpace(*authTokenFile) != "" {
		if cfg.httpAuthToken, err = readAuthToken(*authTokenFile); err != nil {
			return nil, err
		}
	}
	cfg.userHeader = strings.TrimSpace(cfg.userHeader)
	if cfg.userHeader != "" {
		if !httpHeaderName.MatchString(cfg.userHeader) {
			return nil, fmt.Errorf("--user-header %q is not a valid HTTP header name", cfg.userHeader)
		}
		cfg.userHeader = http.CanonicalHeaderKey(cfg.userHeader)
	}
	if cfg.storeMaxRows <= 0 {
		return nil, fmt.Errorf("--result-store-max-rows must be positive, got %d", cfg.storeMaxRows)
	}
	if cfg.storeMaxBytes <= 0 || cfg.storeUserBytes <= 0 {
		return nil, errors.New("--result-store-max-bytes and --result-store-user-bytes must be positive")
	}
	if cfg.storeUserBytes > cfg.storeMaxBytes {
		return nil, fmt.Errorf("--result-store-user-bytes (%d) cannot exceed --result-store-max-bytes (%d)", cfg.storeUserBytes, cfg.storeMaxBytes)
	}
	if cfg.storeTTL <= 0 {
		return nil, fmt.Errorf("--result-store-ttl must be positive, got %s", cfg.storeTTL)
	}
	// The prefix is checked after the connection string, so that the error can
	// suggest one parsed from it. --query-tools-only exposes no <prefix>_*
	// tools, so the prefix is optional there; a prefix that is given is still
	// validated, since it names the server in the client's list.
	cfg.toolPrefix = normalizePrefix(cfg.toolPrefix)
	if cfg.toolPrefix == "" {
		if !cfg.queryToolsOnly {
			return nil, missingPrefixError(cfg.connString)
		}
	} else if err := checkToolPrefix(cfg.toolPrefix); err != nil {
		return nil, err
	}

	if cfg.metadataDir != "" {
		fi, err := os.Stat(cfg.metadataDir)
		if err != nil {
			return nil, fmt.Errorf("--metadata-dir path error: %w", err)
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("--metadata-dir is not a directory: %s", cfg.metadataDir)
		}
	}
	cfg.descrDatabase = strings.TrimSpace(cfg.descrDatabase)
	cfg.metadataDirSchema = strings.TrimSpace(cfg.metadataDirSchema)
	if cfg.metadataDirSchema != "" {
		// The schema only ever qualifies names read out of the directory, so
		// without one it would silently do nothing.
		if cfg.metadataDir == "" {
			return nil, errors.New("--metadata-dir-schema needs a --metadata-dir to apply to")
		}
		if strings.Contains(cfg.metadataDirSchema, ".") {
			return nil, fmt.Errorf("--metadata-dir-schema must be a bare schema name, got %q", cfg.metadataDirSchema)
		}
	}
	for k, path := range metadataFiles {
		fi, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("metadata file for %s error: %w", k, err)
		}
		if fi.IsDir() {
			return nil, fmt.Errorf("metadata file for %s is a directory: %s", k, path)
		}
	}
	cfg.metadataFiles = metadataFiles

	if err := checkToolCallName(cfg.toolCallName); err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.queryToolsPath) != "" {
		// Under --query-tools-only the built-in tools are not registered, so
		// there are no names for a custom tool to collide with.
		// The describe tool is registered beside the query tools either way,
		// so its name is reserved either way.
		reserved := builtinToolNames(cfg.toolPrefix)
		if cfg.queryToolsOnly {
			reserved = handleToolNames(cfg)
			reserved[describeToolName(cfg)] = true
		}
		file, err := parseQueryToolsFileAs(cfg.queryToolsPath, reserved, cfg.toolCallName)
		if err != nil {
			return nil, err
		}
		cfg.queryTools = file.Specs
		cfg.queryToolGroups = file.Groups
		cfg.querySharedParams = file.SharedParams
		cfg.queryToolRewrite = file.rewrite
	}
	if cfg.queryToolsOnly && len(cfg.queryTools) == 0 {
		return nil, errors.New("--query-tools-only needs --query-tools: with the built-in tools dropped and no custom tools defined, the server would expose nothing")
	}

	// Tool groups become separate listeners, which only exist under
	// --transport=http. Under stdio they are inert (one stream, one server), so
	// the port map is only worked out — and only checked — for http.
	if cfg.transport == transportHTTP && len(cfg.queryToolGroups) > 0 {
		if err := resolveGroupPorts(cfg); err != nil {
			return nil, err
		}
	}

	return cfg, nil
}

// builtinToolNames is every tool name this server exposes on its own, so a
// --query-tools file cannot quietly define one that shadows it.
func builtinToolNames(prefix string) map[string]bool {
	names := make(map[string]bool, len(toolSuffixes))
	for _, suffix := range toolSuffixes {
		names[toolName(prefix, suffix)] = true
	}
	return names
}

// isFlagSet reports whether name was given on the command line, as opposed to
// sitting at its default. A flag that only applies in one mode has to tell the
// two apart before it can complain about the other.
func isFlagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func boolPtr(b bool) *bool { return &b }

func float64Ptr(f float64) *float64 { return &f }

func anyPtr(a any) *any { return &a }
