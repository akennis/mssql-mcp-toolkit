package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// oneServer wraps a single server the way buildServers would for a run with no
// tool groups, so the transport tests can keep constructing servers directly.
func oneServer(cfg *config, s *mcp.Server) []namedServer {
	return []namedServer{{name: serverLabelFor(cfg), addr: cfg.httpAddr, server: s}}
}

// The default execution mode is the one every existing client config relies
// on: adding the http mode must not move a single stdio server onto a port.
func TestLoadConfigDefaultsToStdio(t *testing.T) {
	cfg, err := loadConfig([]string{"--conn-string", "server=localhost", "--tool-prefix", "sales"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.transport != transportStdio {
		t.Errorf("transport = %q, want %q", cfg.transport, transportStdio)
	}
	if cfg.httpAddr != "" {
		t.Errorf("httpAddr = %q, want it blank: stdio listens on nothing", cfg.httpAddr)
	}
}

func TestLoadConfigReadsTheHTTPMode(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{nil, defaultHTTPAddr},
		{[]string{"--http-addr", "127.0.0.1:9999"}, "127.0.0.1:9999"},
		{[]string{"--http-addr", "localhost:9999"}, "localhost:9999"},
		{[]string{"--http-addr", "[::1]:9999"}, "[::1]:9999"},
		// A bare port reads as shorthand for localhost, not as an invitation
		// to bind every interface on the machine.
		{[]string{"--http-addr", ":9999"}, "127.0.0.1:9999"},
		// Any loopback address, not just the canonical one.
		{[]string{"--http-addr", "127.0.0.2:9999"}, "127.0.0.2:9999"},
	}
	for _, c := range cases {
		args := append([]string{"--conn-string", "server=localhost", "--tool-prefix", "sales", "--transport", "http"}, c.args...)
		cfg, err := loadConfig(args)
		if err != nil {
			t.Errorf("loadConfig(%v): %v", c.args, err)
			continue
		}
		if cfg.transport != transportHTTP {
			t.Errorf("loadConfig(%v) transport = %q, want %q", c.args, cfg.transport, transportHTTP)
		}
		if cfg.httpAddr != c.want {
			t.Errorf("loadConfig(%v) httpAddr = %q, want %q", c.args, cfg.httpAddr, c.want)
		}
	}
}

// The listening address is exclusive to the http mode. Under stdio it is
// refused rather than ignored: an ignored address is an operator who believes
// a port is open when nothing is listening.
func TestHTTPAddrIsExclusiveToTheHTTPMode(t *testing.T) {
	base := []string{"--conn-string", "server=localhost", "--tool-prefix", "sales"}

	_, err := loadConfig(append(base, "--http-addr", "127.0.0.1:9999"))
	if err == nil || !strings.Contains(err.Error(), "--http-addr") {
		t.Errorf("error = %v, want --http-addr rejected under the default stdio mode", err)
	}

	_, err = loadConfig(append(base, "--transport", "stdio", "--http-addr", "127.0.0.1:9999"))
	if err == nil || !strings.Contains(err.Error(), "--transport=http") {
		t.Errorf("error = %v, want an error naming the mode the flag belongs to", err)
	}

	// Passing the flag's own default value is still passing it: the operator
	// wrote an address down, and it still will not listen.
	if _, err := loadConfig(append(base, "--http-addr", defaultHTTPAddr)); err == nil {
		t.Error("loadConfig accepted --http-addr under stdio because the value happened to be the default")
	}

	// Every other flag that only means something to a listener is refused the
	// same way, for the same reason.
	for _, flag := range []string{"--http-stateless", "--http-log", "--http-log-headers"} {
		if _, err := loadConfig(append(base, flag)); err == nil ||
			!strings.Contains(err.Error(), flag) || !strings.Contains(err.Error(), "--transport=http") {
			t.Errorf("loadConfig(%s) error = %v, want it refused under stdio", flag, err)
		}
		// ...and accepted under http.
		if _, err := loadConfig(append(base, "--transport", "http", flag)); err != nil {
			t.Errorf("loadConfig(--transport=http %s): %v", flag, err)
		}
	}
}

// Stateless mode exists for clients that do not carry Mcp-Session-Id back.
// What makes it work for them is that there is no session to carry: the server
// must not issue one, and must answer a request that has none.
func TestHTTPStatelessModeIssuesNoSession(t *testing.T) {
	post := func(t *testing.T, stateless bool) *http.Response {
		t.Helper()
		cfg := &config{
			transport: transportHTTP, httpAddr: reserveLoopbackPort(t), httpStateless: stateless,
			toolPrefix: "sales", queryTimeout: time.Second, maxOpenConns: 1,
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		go serveHTTP(ctx, cfg, oneServer(cfg, newServer(cfg, openStub(t), nil)))
		waitForListener(t, cfg.httpAddr)

		body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
			`{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}`)
		req, err := http.NewRequest(http.MethodPost, "http://"+cfg.httpAddr, body)
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("initialize: %v", err)
		}
		t.Cleanup(func() { res.Body.Close() })
		if res.StatusCode != http.StatusOK {
			t.Fatalf("initialize status = %d, want 200", res.StatusCode)
		}
		return res
	}

	if got := post(t, false).Header.Get(sessionIDHeader); got == "" {
		t.Error("the default stateful mode issued no session id")
	}
	if got := post(t, true).Header.Get(sessionIDHeader); got != "" {
		t.Errorf("stateless mode issued session id %q, want none: a client that ignores it must not be held to it", got)
	}
}

func TestLoadConfigRejectsAnUnknownTransport(t *testing.T) {
	for _, mode := range []string{"sse", "tcp", "HTTPS", ""} {
		args := []string{"--conn-string", "server=localhost", "--tool-prefix", "sales", "--transport", mode}
		_, err := loadConfig(args)
		if err == nil || !strings.Contains(err.Error(), "--transport") {
			t.Errorf("loadConfig(--transport=%q) error = %v, want a --transport complaint", mode, err)
		}
	}
}

// A mode name is a name, not a spelling exercise.
func TestTransportNameIsCaseAndSpaceInsensitive(t *testing.T) {
	for _, mode := range []string{"HTTP", " http ", "Http"} {
		cfg, err := loadConfig([]string{
			"--conn-string", "server=localhost", "--tool-prefix", "sales", "--transport", mode,
		})
		if err != nil {
			t.Errorf("loadConfig(--transport=%q): %v", mode, err)
			continue
		}
		if cfg.transport != transportHTTP {
			t.Errorf("loadConfig(--transport=%q) transport = %q, want %q", mode, cfg.transport, transportHTTP)
		}
	}
}

// This mode is localhost-only, and that is enforced rather than documented.
// The server has no authentication: a routable bind address publishes every
// table the SQL login can read to whoever can reach the port.
func TestHTTPModeRefusesToBindOffLocalhost(t *testing.T) {
	for _, addr := range []string{
		"0.0.0.0:8080",
		"[::]:8080",
		"192.168.1.10:8080",
		"10.0.0.5:8080",
		"db.internal.example.com:8080",
		"8080",            // not host:port at all
		"127.0.0.1:http",  // named port
		"127.0.0.1:99999", // out of range
		"127.0.0.1:-1",    // out of range
	} {
		_, err := loadConfig([]string{
			"--conn-string", "server=localhost", "--tool-prefix", "sales",
			"--transport", "http", "--http-addr", addr,
		})
		if err == nil || !strings.Contains(err.Error(), "--http-addr") {
			t.Errorf("loadConfig(--http-addr=%q) error = %v, want it refused", addr, err)
		}
	}
}

// run reports a bad execution mode the same way it reports every other config
// error: as a returned error, so main can exit non-zero having run its
// cleanup.
func TestRunRejectsABadTransportBeforeConnecting(t *testing.T) {
	err := run([]string{"--conn-string", "server=localhost", "--tool-prefix", "sales", "--transport", "carrier-pigeon"})
	if err == nil || !strings.Contains(err.Error(), "--transport") {
		t.Errorf("run error = %v, want a --transport complaint", err)
	}
}

// End to end over a real loopback socket: a client that attaches to the
// running server by URL gets the same tools, and the same rows, as one that
// launched it over stdio.
func TestServeHTTPServesMCPOverLoopback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := sql.Open("stubmssql", "stub")
	if err != nil {
		t.Fatalf("opening stub database: %v", err)
	}
	defer db.Close()

	// serveHTTP owns its listener, so the test has to name a port rather than
	// read one back: take one the kernel just handed out and let go of it, so
	// this cannot collide with whatever else is listening on the machine.
	// That leaves a window between releasing it and the server binding it,
	// which is why the test probes rather than assuming.
	addr := reserveLoopbackPort(t)
	cfg := &config{
		transport: transportHTTP,
		httpAddr:  addr,
		// With the request log on, so the wrapper it puts around every
		// response stays transparent to the streaming underneath: a
		// statusRecorder that swallowed the flush would hang this round trip
		// rather than fail it.
		httpLog:      true,
		toolPrefix:   "sales",
		maxRows:      1000,
		queryTimeout: 5 * time.Second,
		maxOpenConns: 1,
	}

	served := make(chan error, 1)
	go func() { served <- serveHTTP(ctx, cfg, oneServer(cfg, newServer(cfg, db, nil))) }()

	waitForListener(t, addr)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + addr}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools.Tools) != 3 {
		t.Fatalf("unexpected tool list: %+v", tools.Tools)
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
	if got := decodeResult(t, res); got.RowCount != 2 {
		t.Errorf("row_count = %d, want 2", got.RowCount)
	}

	// Cancelling the context is how a signal reaches the server: it must stop
	// and report a clean shutdown rather than an error.
	cs.Close()
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serveHTTP = %v, want nil on a cancelled context", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("serveHTTP did not return after its context was cancelled")
	}
}

// addHeader is an http.RoundTripper that stamps one header onto every request,
// so a test can prove a header the client sent reaches the server's log.
type addHeader struct {
	rt   http.RoundTripper
	k, v string
}

func (a addHeader) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set(a.k, a.v)
	return a.rt.RoundTrip(req)
}

// --http-log-headers writes the headers each tools/call arrived with to the
// log, so an operator debugging header-based routing or auth can see what the
// server received rather than what the client believes it sent.
func TestHTTPLogHeadersRecordsToolCallHeaders(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	addr := reserveLoopbackPort(t)
	cfg := &config{
		transport:      transportHTTP,
		httpAddr:       addr,
		httpLogHeaders: true,
		toolPrefix:     "sales",
		maxRows:        1000,
		queryTimeout:   5 * time.Second,
		maxOpenConns:   1,
	}
	go serveHTTP(ctx, cfg, oneServer(cfg, newServer(cfg, openStub(t), nil)))
	waitForListener(t, addr)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   "http://" + addr,
		HTTPClient: &http.Client{Transport: addHeader{rt: http.DefaultTransport, k: "X-Trace-Id", v: "abc-123"}},
	}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

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

	out := logs.String()
	if !strings.Contains(out, "tool-call header") || !strings.Contains(out, "tool=sales_query") {
		t.Fatalf("log does not name the tool call:\n%s", out)
	}
	if !strings.Contains(out, "X-Trace-Id: abc-123") {
		t.Errorf("log is missing the header the client sent:\n%s", out)
	}
}

// A port already in use has to fail at startup, where whoever ran the command
// is watching, rather than after the process has settled in.
func TestServeHTTPReportsAnAddressAlreadyInUse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	defer listener.Close()

	cfg := &config{
		transport: transportHTTP, httpAddr: listener.Addr().String(),
		toolPrefix: "sales", queryTimeout: time.Second, maxOpenConns: 1,
	}
	err = serveHTTP(context.Background(), cfg, oneServer(cfg, newServer(cfg, openStub(t), nil)))
	if err == nil || !strings.Contains(err.Error(), "listening on "+cfg.httpAddr) {
		t.Errorf("serveHTTP = %v, want it to name the address it could not bind", err)
	}
}

// serve is the dispatch: the mode in the config decides which server runs.
// Under stdio a closed stdin is a normal shutdown, not a failure.
func TestServeStdioTreatsAClosedContextAsANormalShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := &config{toolPrefix: "sales", queryTimeout: time.Second, maxOpenConns: 1}
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, oneServer(cfg, newServer(cfg, openStub(t), nil))) }()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("serve = %v, want a clean return on a cancelled context", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("serve did not return after its context was cancelled")
	}
}

// reserveLoopbackPort picks a port that was free a moment ago, and hands it
// back as a host:port string.
func reserveLoopbackPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return addr
}

// waitForListener blocks until something accepts connections at addr.
func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s", addr)
}

// The request log reads the JSON-RPC method out of the body, which means it
// reads the body. Whatever it takes it has to put back: a handler that
// received a truncated request because the log got there first would be a
// diagnostic that breaks the thing it is diagnosing.
func TestPeekRPCMethodLeavesTheBodyIntact(t *testing.T) {
	// Larger than rpcPeekLimit, so the peek cannot have consumed all of it,
	// and with the method sitting ahead of the bulk the way MCP writes it.
	query := strings.Repeat("x", 3*rpcPeekLimit)
	body := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"sales_query","arguments":{"query":"` + query + `"}}}`

	req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:1/", strings.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if got := peekRPCMethod(req); got != "tools/call" {
		t.Errorf("peekRPCMethod = %q, want tools/call even though the body outruns the peek limit", got)
	}
	read, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("reading the rewound body: %v", err)
	}
	if string(read) != body {
		t.Errorf("body after peeking is %d bytes, want the original %d", len(read), len(body))
	}
}

func TestPeekRPCMethodOnBodiesItCannotRead(t *testing.T) {
	cases := map[string]string{
		"a notification":      `{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		"method last":         `{"jsonrpc":"2.0","params":{"a":[1,2,{"b":"c"}]},"id":1,"method":"tools/list"}`,
		"a batch":             `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`,
		"not json":            `<html>`,
		"no method":           `{"jsonrpc":"2.0","id":1}`,
		"a non-string method": `{"method":42}`,
	}
	want := map[string]string{
		"a notification": "notifications/initialized",
		"method last":    "tools/list",
	}
	for name, body := range cases {
		req, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:1/", strings.NewReader(body))
		if err != nil {
			t.Fatalf("%s: building request: %v", name, err)
		}
		expected, ok := want[name]
		if !ok {
			expected = "-" // unreadable is reported, never guessed at
		}
		if got := peekRPCMethod(req); got != expected {
			t.Errorf("%s: peekRPCMethod = %q, want %q", name, got, expected)
		}
	}
}

// A request with no body at all must not panic the log.
func TestPeekRPCMethodWithoutABody(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:1/", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if got := peekRPCMethod(req); got != "-" {
		t.Errorf("peekRPCMethod = %q, want -", got)
	}
}
