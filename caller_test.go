package main

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRequireBearer(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h := requireBearer(ok, "s3cret")
	for _, c := range []struct {
		name, header string
		want         int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"wrong", "Bearer nope", http.StatusUnauthorized},
		{"no scheme", "s3cret", http.StatusUnauthorized},
		{"prefix of the token", "Bearer s3cre", http.StatusUnauthorized},
		{"right", "Bearer s3cret", http.StatusTeapot},
	} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		if c.header != "" {
			req.Header.Set("Authorization", c.header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.want)
		}
		if c.want == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: 401 without WWW-Authenticate", c.name)
		}
	}
}

func TestLoadConfigAuthAndUserHeaderFlags(t *testing.T) {
	base := []string{"--conn-string", "server=localhost", "--tool-prefix", "sales"}

	cfg, err := loadConfig(append(base, "--transport", "http",
		"--http-auth-token-file", writeTokenFile(t, "  tok-123\n"), "--user-header", "x-openwebui-user-id"))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.httpAuthToken != "tok-123" {
		t.Errorf("token = %q, want the file's content trimmed", cfg.httpAuthToken)
	}
	if cfg.userHeader != "X-Openwebui-User-Id" {
		t.Errorf("user header = %q, want it canonicalized", cfg.userHeader)
	}

	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"token under stdio", []string{"--http-auth-token-file", writeTokenFile(t, "x")}, "applies to --transport=http only"},
		{"user header under stdio", []string{"--user-header", "X-User"}, "applies to --transport=http only"},
		{"empty token file", []string{"--transport", "http", "--http-auth-token-file", writeTokenFile(t, " \n")}, "is empty"},
		{"token with spaces", []string{"--transport", "http", "--http-auth-token-file", writeTokenFile(t, "a b")}, "whitespace"},
		{"missing token file", []string{"--transport", "http", "--http-auth-token-file", filepath.Join(t.TempDir(), "nope")}, "--http-auth-token-file"},
		{"bad header name", []string{"--transport", "http", "--user-header", "X User"}, "not a valid HTTP header name"},
		{"user bytes over total", []string{"--result-store-user-bytes", "100", "--result-store-max-bytes", "10"}, "cannot exceed"},
	} {
		_, err := loadConfig(append(append([]string{}, base...), c.args...))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error = %v, want it to mention %q", c.name, err, c.want)
		}
	}
}

func TestCallerOf(t *testing.T) {
	withHeader := func(name, value string) *mcp.CallToolRequest {
		h := http.Header{}
		if name != "" {
			h.Set(name, value)
		}
		return &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: h}}
	}
	stdio := &config{transport: transportStdio, userHeader: "X-User"}
	if who, ok := callerOf(stdio, withHeader("", "")); !ok || who != localCaller {
		t.Errorf("stdio caller = %q, %v; want the local owner", who, ok)
	}
	shared := &config{transport: transportHTTP}
	if who, ok := callerOf(shared, withHeader("X-User", "alice")); !ok || who != sharedCaller {
		t.Errorf("http without --user-header = %q, %v; want the shared owner", who, ok)
	}
	perUser := &config{transport: transportHTTP, userHeader: "X-User"}
	a, okA := callerOf(perUser, withHeader("X-User", "alice"))
	b, okB := callerOf(perUser, withHeader("X-User", "bob"))
	if !okA || !okB || a == b {
		t.Errorf("alice = %q, bob = %q; want two identified, different owners", a, b)
	}
	for _, req := range []*mcp.CallToolRequest{withHeader("", ""), withHeader("X-User", "   "), {}, nil} {
		if who, ok := callerOf(perUser, req); ok {
			t.Errorf("a call without the header was identified as %q", who)
		}
	}
}

// The header log shows every header but the credentials.
func TestLogHeadersRedactsCredentials(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	logHeaders("", "sales_query", &mcp.RequestExtra{Header: http.Header{
		"Authorization": {"Bearer s3cret"},
		"Cookie":        {"session=abc"},
		"X-Trace-Id":    {"t-1"},
	}})
	out := logs.String()
	if strings.Contains(out, "s3cret") || strings.Contains(out, "session=abc") {
		t.Errorf("credentials reached the log:\n%s", out)
	}
	if !strings.Contains(out, "Authorization: [redacted]") || !strings.Contains(out, "X-Trace-Id: t-1") {
		t.Errorf("log = %s, want Authorization redacted and other headers intact", out)
	}
}

// Every listener of a grouped run — the base port and each group's —
// refuses a request without the token, and serves one with it.
func TestAuthTokenGuardsEveryListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := groupedTestConfig(t, transportHTTP)
	base := reserveLoopbackPort(t)
	cfg.httpAddr = base
	cfg.httpAuthToken = "s3cret"
	servers := buildServers(cfg, openStub(t), nil)
	// Put each group on a free port of its own.
	for i := range servers {
		if servers[i].addr != base {
			servers[i].addr = reserveLoopbackPort(t)
		}
	}
	go serveHTTP(ctx, cfg, servers)
	for _, s := range servers {
		waitForListener(t, s.addr)
	}

	for _, s := range servers {
		// No token: the handshake is refused.
		client := mcp.NewClient(&mcp.Implementation{Name: "no-token", Version: "v0"}, nil)
		if cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + s.addr}, nil); err == nil {
			cs.Close()
			t.Errorf("%s (%s): connected without a token", s.name, s.addr)
		}
		// The token: served.
		client = mcp.NewClient(&mcp.Implementation{Name: "token", Version: "v0"}, nil)
		cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:   "http://" + s.addr,
			HTTPClient: &http.Client{Transport: addHeader{rt: http.DefaultTransport, k: "Authorization", v: "Bearer s3cret"}, Timeout: 5 * time.Second},
		}, nil)
		if err != nil {
			t.Errorf("%s (%s): connect with the token: %v", s.name, s.addr, err)
			continue
		}
		if _, err := cs.ListTools(ctx, nil); err != nil {
			t.Errorf("%s: ListTools with the token: %v", s.name, err)
		}
		cs.Close()
	}
}
