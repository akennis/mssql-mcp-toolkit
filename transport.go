package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The execution modes. They are alternatives, not layers: a process serves one
// client over stdin/stdout, or it listens on a loopback port, never both. The
// mode decides where every setting that names a listener applies, which is why
// --http-addr is rejected outright under stdio rather than quietly ignored —
// an address that does nothing is an operator who thinks the server is
// reachable and cannot see that it is not.
type transportMode string

const (
	// transportStdio is the default: the MCP client launches the binary and
	// speaks JSON-RPC over its standard streams. One client, one process,
	// lifetime tied to the client's.
	transportStdio transportMode = "stdio"
	// transportHTTP serves the streamable HTTP transport on a loopback
	// address, for clients that attach to an already-running server rather
	// than launching one, and for several clients on the machine sharing a
	// single connection pool.
	transportHTTP transportMode = "http"
)

// defaultHTTPAddr is where --transport=http listens when no address is given.
// The host half is not a default anyone may override with a routable address:
// see localhostAddr.
const defaultHTTPAddr = "127.0.0.1:8080"

// httpShutdownGrace is how long a signalled HTTP server waits for the requests
// already in flight. A query that outlives it is abandoned rather than holding
// the process open: the per-query timeout is the limit that matters, and it is
// the operator's to set.
const httpShutdownGrace = 5 * time.Second

// parseTransport turns the --transport value into a mode.
func parseTransport(value string) (transportMode, error) {
	switch mode := transportMode(strings.ToLower(strings.TrimSpace(value))); mode {
	case transportStdio, transportHTTP:
		return mode, nil
	default:
		return "", fmt.Errorf("--transport must be %s or %s, got %q", transportStdio, transportHTTP, value)
	}
}

// localhostAddr checks that addr is a loopback listening address and returns
// it normalized to host:port.
//
// This mode is localhost-only, and the check is what makes that true rather
// than aspirational. The server has no authentication of any kind: anything
// that can reach the port can read everything the SQL login can read. Binding
// it to a routable interface would publish the database to the network, so an
// address that would do that is a startup error, not a warning.
func localhostAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return "", fmt.Errorf("--http-addr must be in host:port form, got %q: %w", addr, err)
	}
	// A named port ("http") would resolve, but it reads as a typo more often
	// than it reads as intent.
	number, err := strconv.Atoi(port)
	if err != nil || number < 0 || number > 65535 {
		return "", fmt.Errorf("--http-addr port must be a number between 0 and 65535, got %q", port)
	}
	switch {
	case host == "":
		// A bare ":8080" listens on every interface, which is the one thing
		// this mode must not do. Read it as the shorthand it is meant to be.
		host = "127.0.0.1"
	case strings.EqualFold(host, "localhost"):
		// Left as written: which loopback address it resolves to is the
		// machine's business.
	default:
		ip := net.ParseIP(host)
		if ip == nil {
			return "", fmt.Errorf("--http-addr host must be localhost or a loopback IP address, got %q", host)
		}
		if !ip.IsLoopback() {
			return "", fmt.Errorf(
				"--http-addr host must be a loopback address: %q would expose this database to the network, "+
					"and this server has no authentication", host)
		}
	}
	return net.JoinHostPort(host, port), nil
}

// serve runs the server under the configured execution mode, and returns when
// the client is done with it or ctx ends.
func serve(ctx context.Context, cfg *config, servers []namedServer) error {
	switch cfg.transport {
	case transportHTTP:
		return serveHTTP(ctx, cfg, servers)
	default:
		// stdio has one stream, so it has one server: buildServers has already
		// collapsed any groups into it.
		return serveStdio(ctx, servers[0].server)
	}
}

func serveStdio(ctx context.Context, server *mcp.Server) error {
	// A closed stdin (io.EOF) or a signal means the client is done with us:
	// that is a normal shutdown, not a failure.
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// serveHTTP listens for every server in servers until ctx ends or one of the
// listeners stops on its own. A plain run passes a single server; a grouped
// one passes the base server plus one per group, each on its own port.
//
// Every listener is opened before anything is served, so a port already in use
// is an error from startup — where the operator is watching — rather than a
// message on a stream nobody is reading. If any listener fails to open, the
// ones already open are closed and none of them serve.
func serveHTTP(ctx context.Context, cfg *config, servers []namedServer) error {
	type instance struct {
		name string
		http *http.Server
		lis  net.Listener
	}

	var running []instance
	for _, s := range servers {
		lis, err := net.Listen("tcp", s.addr)
		if err != nil {
			for _, in := range running {
				in.lis.Close()
			}
			return fmt.Errorf("listening on %s for %s: %w", s.addr, s.name, err)
		}
		running = append(running, instance{
			name: s.name,
			http: &http.Server{
				Handler: streamableHandler(cfg, s.server, s.name),
				// A client that opens a connection and sends nothing must not
				// hold a slot forever. Only the headers are bounded here: the
				// body of a streamable POST stays open for as long as the query
				// it carries.
				ReadHeaderTimeout: 10 * time.Second,
			},
			lis: lis,
		})
	}

	// The resolved address, not the requested one, so a --http-addr of
	// 127.0.0.1:0 tells the operator which port they actually got.
	for _, in := range running {
		log.Printf("listening for MCP over HTTP on http://%s (%s)", in.lis.Addr(), in.name)
	}

	served := make(chan error, len(running))
	for _, in := range running {
		in := in
		go func() { served <- in.http.Serve(in.lis) }()
	}

	shutdownAll := func() {
		// ctx may already be cancelled, so Shutdown gets a fresh deadline of
		// its own; using ctx here would skip the grace period entirely.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownGrace)
		defer cancel()
		for _, in := range running {
			if err := in.http.Shutdown(shutdownCtx); err != nil {
				log.Printf("warning: shutting down %s: %v", in.name, err)
			}
		}
	}

	select {
	case err := <-served:
		// One listener stopped. Bring the rest down too, so the process does
		// not linger half-serving.
		shutdownAll()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownAll()
		return nil
	}
}

// streamableHandler builds the streamable-HTTP handler for one server, with
// the per-request log wrapper when --http-log is on.
//
// Every session gets the same server, and so the same connection pool:
// --max-open-conns is the ceiling for the machine, not per client.
func streamableHandler(cfg *config, server *mcp.Server, name string) http.Handler {
	opts := &mcp.StreamableHTTPOptions{Stateless: cfg.httpStateless}
	if cfg.httpLog {
		// log.Writer(), so the SDK's own diagnostics land on the same stream,
		// with the same prefix, as everything else this process reports.
		opts.Logger = slog.New(slog.NewTextHandler(log.Writer(), &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	var handler http.Handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, opts)
	// The token check sits inside the request log, so a refused request is
	// still logged with the 401 it got.
	if cfg.httpAuthToken != "" {
		handler = requireBearer(handler, cfg.httpAuthToken)
	}
	if cfg.httpLog {
		handler = logRequests(handler, name, cfg.userHeader)
	}
	return handler
}

// sessionIDHeader is what the streamable transport calls a session in flight.
// The SDK keeps its own copy of this name unexported, so the request log has
// to spell it out.
const sessionIDHeader = "Mcp-Session-Id"

// logRequests reports one line per HTTP exchange: the method, the session it
// claimed, and the status it got.
//
// This exists for one question, which is the question every streamable-HTTP
// integration eventually asks: the client says it cannot connect, and the
// server says nothing at all. A 404 on a POST is a client replaying a session
// this process has never heard of; a 400 on a GET is one opening a
// notification stream without a session; a 403 is the rebinding guard reading
// the Host header. Those are four different fixes and they are indistinguish-
// able from the client's side, where all of them read as "failed to connect".
//
// name is the server the request reached, so a grouped run's log tells the
// group listeners apart; it is "" for a plain run and then left off the line.
// userHeader, when set, is the --user-header whose value is logged as the
// caller.
func logRequests(next http.Handler, name, userHeader string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		session := req.Header.Get(sessionIDHeader)
		if session == "" {
			session = "-"
		}
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		method := peekRPCMethod(req)
		next.ServeHTTP(recorder, req)
		// Two durations, because they answer different questions. The first is
		// how long the server took to start answering; the second is how long
		// the exchange held the connection. A streamable response is written
		// early and the stream stays open, so a slow server and a client that
		// simply keeps reading look identical in the total alone.
		answered := "-"
		if !recorder.firstWrite.IsZero() {
			answered = recorder.firstWrite.Sub(start).Round(time.Millisecond).String()
		}
		server := ""
		if name != "" {
			server = " server=" + name
		}
		user := ""
		if userHeader != "" {
			who := strings.TrimSpace(req.Header.Get(userHeader))
			if who == "" {
				who = "-"
			}
			user = " user=" + who
		}
		log.Printf("http %s %s host=%s session=%s rpc=%s%s%s -> %d (answered in %s, held %s)",
			req.Method, req.URL.Path, req.Host, session, method, server, user, recorder.status,
			answered, time.Since(start).Round(time.Millisecond))
	})
}

// methodCallTool is the JSON-RPC method a tool invocation arrives as. The SDK
// keeps its own copy of this name unexported.
const methodCallTool = "tools/call"

// logToolCallHeaders is receiving middleware that writes out every HTTP header
// a tools/call carried, one line per header, before the call runs. It is
// installed only under --http-log-headers.
//
// --http-log answers "did the HTTP exchange succeed"; it cannot answer "what
// did the client actually send", which is the question when a call is routed or
// authorized by a header a proxy rewrites on the way through. Only the HTTP
// transport attaches headers: under stdio req.GetExtra() is nil and the call
// logs a single line saying so.
//
// name is the server the call reached, so a grouped run's log tells the group
// listeners apart; it is "" for a plain run and then left off the line.
func logToolCallHeaders(name string) mcp.Middleware {
	server := ""
	if name != "" {
		server = " server=" + name
	}
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == methodCallTool {
				tool := "-"
				if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && params.Name != "" {
					tool = params.Name
				}
				logHeaders(server, tool, req.GetExtra())
			}
			return next(ctx, method, req)
		}
	}
}

// todayInInstructions ends the initialize instructions with today's date.
// Nothing else tells the model the date, and one left to guess works it out
// from whatever the data suggests - the school year it just queried - which
// can be months out: "aged 15 or over" then quietly takes in or leaves out a
// month's birthdays. The date is filled in at each initialize rather than
// with the rest of the text at startup, since the server runs for days.
func todayInInstructions(now func() time.Time) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if err != nil {
				return res, err
			}
			switch r := res.(type) {
			case *mcp.InitializeResult:
				r.Instructions = withToday(r.Instructions, now())
			case *mcp.DiscoverResult:
				r.Instructions = withToday(r.Instructions, now())
			}
			return res, err
		}
	}
}

func withToday(instructions string, now time.Time) string {
	line := fmt.Sprintf("Today is %s, %s. Use this date, never one inferred from the data; where a date is accepted, an offset such as -15y is counted from it by the server.",
		now.Weekday(), now.Format("2006-01-02"))
	if strings.TrimSpace(instructions) == "" {
		return line
	}
	return strings.TrimRight(instructions, "\n") + "\n\n" + line + "\n"
}

// logHeaders emits the header lines for one tool call. A request with no
// transport headers — every stdio call, and an HTTP one the SDK stripped —
// still gets a line, so "nothing was logged" is never ambiguous.
func logHeaders(server, tool string, extra *mcp.RequestExtra) {
	if extra == nil || len(extra.Header) == 0 {
		log.Printf("tool-call headers%s tool=%s: none (not received over HTTP)", server, tool)
		return
	}
	names := make([]string, 0, len(extra.Header))
	for name := range extra.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := strings.Join(extra.Header[name], ", ")
		if redactedHeaders[http.CanonicalHeaderKey(name)] {
			value = "[redacted]"
		}
		log.Printf("tool-call header%s tool=%s %s: %s", server, tool, name, value)
	}
}

// statusRecorder remembers the status code on its way past.
//
// It has to stay transparent to the streaming machinery underneath it: the SDK
// flushes SSE through an http.ResponseController, which walks Unwrap to find
// the real writer. Without that method every streamed response would sit in a
// buffer until the handler returned.
type statusRecorder struct {
	http.ResponseWriter
	status int
	// firstWrite is when the handler started answering, as opposed to when it
	// finished. Zero until it does.
	firstWrite time.Time
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.mark()
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.mark()
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) mark() {
	if r.firstWrite.IsZero() {
		r.firstWrite = time.Now()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// rpcPeekLimit is how much of a request body the log will read looking for the
// JSON-RPC method. The method name sits in the envelope, ahead of the params,
// so this only has to cover the envelope — not the SQL a tools/call carries.
const rpcPeekLimit = 8 << 10

// peekRPCMethod reports the JSON-RPC method a request carries, and leaves the
// body untouched for the handler behind it.
//
// "http POST / -> 200" says a request succeeded; it does not say whether the
// client was listing tools or running a query, which is the difference between
// a discovery problem and an execution one. The body is the only place that
// distinction is written down.
func peekRPCMethod(req *http.Request) string {
	if req.Body == nil {
		return "-"
	}
	peeked, err := io.ReadAll(io.LimitReader(req.Body, rpcPeekLimit))
	if err != nil {
		// Put back what was read; the handler will meet the same error.
		req.Body = rewound{Reader: bytes.NewReader(peeked), Closer: req.Body}
		return "-"
	}
	req.Body = rewound{Reader: io.MultiReader(bytes.NewReader(peeked), req.Body), Closer: req.Body}

	// Walked token by token rather than unmarshalled, so that a body cut off
	// at the peek limit still yields the method that preceded the cut.
	dec := json.NewDecoder(bytes.NewReader(peeked))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		// A batch, or not JSON at all.
		return "-"
	}
	for {
		key, err := dec.Token()
		if err != nil || key == json.Delim('}') {
			return "-"
		}
		name, ok := key.(string)
		if !ok {
			return "-"
		}
		if name == "method" {
			if value, err := dec.Token(); err == nil {
				if method, ok := value.(string); ok {
					return method
				}
			}
			return "-"
		}
		if err := skipValue(dec); err != nil {
			return "-"
		}
	}
}

// skipValue consumes one JSON value, however nested, so the walk can reach the
// next key.
func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok || (delim != '{' && delim != '[') {
		return nil
	}
	for depth := 1; depth > 0; {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
	}
	return nil
}

// rewound is a request body put back together after the log has read its
// opening bytes: the unread remainder follows the peeked prefix, and closing
// still closes the original.
type rewound struct {
	io.Reader
	io.Closer
}
