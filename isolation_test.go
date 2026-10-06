package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connectAs opens an HTTP client session to addr that stamps every request
// with the bearer token and, when user is set, the user header.
func connectAs(t *testing.T, ctx context.Context, addr, user string) *mcp.ClientSession {
	t.Helper()
	var rt http.RoundTripper = addHeader{rt: http.DefaultTransport, k: "Authorization", v: "Bearer s3cret"}
	if user != "" {
		rt = addHeader{rt: rt, k: "X-User-Id", v: user}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "client-" + user, Version: "v0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   "http://" + addr,
		HTTPClient: &http.Client{Transport: rt, Timeout: 10 * time.Second},
	}, nil)
	if err != nil {
		t.Fatalf("connect as %q: %v", user, err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// One user's handles are unknown to another through every way a handle can
// be read — @handle arguments, show and the operators — and a call without
// the user header neither gets a handle nor reads one.
func TestHandlesArePerUserOverHTTP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	specs, err := loadQueryTools(writeQueryToolsFile(t, handleTools), builtinToolNames("sales"))
	if err != nil {
		t.Fatal(err)
	}
	addr := reserveLoopbackPort(t)
	cfg := &config{
		transport: transportHTTP, httpAddr: addr, httpAuthToken: "s3cret", userHeader: "X-User-Id",
		toolPrefix: "sales", connString: "stub", queryTimeout: 5 * time.Second, maxOpenConns: 1,
		maxRows: 200, queryTools: specs,
		resultStore: true, storeMaxRows: 1000, storeMaxBytes: 1 << 24, storeUserBytes: 1 << 22, storeTTL: time.Hour,
	}
	go serveHTTP(ctx, cfg, oneServer(cfg, newServer(cfg, openStub(t), nil)))
	waitForListener(t, addr)

	alice := connectAs(t, ctx, addr, "alice")
	bob := connectAs(t, ctx, addr, "bob")
	anon := connectAs(t, ctx, addr, "")

	// Handle text is only unique per user, so Alice's must be one Bob has
	// never been issued for the test to mean anything: her second, while he
	// has none.
	handleOf(t, callQueryTool(t, alice, "roster", nil))
	aliceHandle := handleOf(t, callQueryTool(t, alice, "roster", nil))

	for _, c := range []struct {
		name string
		tool string
		args map[string]any
	}{
		{"@handle argument", "echo", map[string]any{"PersonID": "@" + aliceHandle + ".id"}},
		{"show", "sales_show", map[string]any{"Handle": aliceHandle, "Columns": "id"}},
		{"filter", "sales_filter", map[string]any{"Handle": aliceHandle, "Where": "id = 1"}},
		{"set operator", "sales_set_union", map[string]any{"Handle": aliceHandle + "," + aliceHandle, "Key": "id"}},
		{"join", "sales_join", map[string]any{"Left": aliceHandle, "Right": aliceHandle, "On": "id"}},
		{"calc", "sales_calc", map[string]any{"Expression": "@" + aliceHandle + ".row_count"}},
	} {
		res := callQueryTool(t, bob, c.tool, c.args)
		if !res.IsError || !strings.Contains(contentText(res), "unknown handle") {
			t.Errorf("bob via %s: %v %s, want unknown handle", c.name, res.IsError, contentText(res))
		}
		// Alice herself can.
		if res := callQueryTool(t, alice, c.tool, c.args); res.IsError {
			t.Errorf("alice via %s: %s", c.name, contentText(res))
		}
	}

	// No header: rows, but no handle, and no reading one.
	res := callQueryTool(t, anon, "roster", nil)
	if text := contentText(res); handleLine.MatchString(text) || !strings.Contains(text, "did not carry the X-User-Id header") || !strings.Contains(text, "1,ada") {
		t.Errorf("anonymous reply:\n%s", text)
	}
	if res := callQueryTool(t, anon, "sales_show", map[string]any{"Handle": aliceHandle, "Columns": "id"}); !res.IsError {
		t.Error("an anonymous call read a handle")
	}
	if res := callQueryTool(t, anon, "echo", map[string]any{"PersonID": "@" + aliceHandle + ".id"}); !res.IsError {
		t.Error("an anonymous call expanded a handle")
	}
}
