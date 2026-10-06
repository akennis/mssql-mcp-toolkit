package main

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Who is calling, and whether they may call at all.
//
// The HTTP listener is loopback-only, but loopback is not one person: every
// process on the host can reach it, and a client such as Open WebUI serves
// many users through one connection. Two settings close that gap.
//
// --http-auth-token-file makes the listener refuse any request that does not
// carry the shared secret, so only the client that was given the token can
// call a tool at all. --user-header names the header that client stamps with
// the signed-in user's id, and stored results (see store.go) are kept per
// user by that id. The header is a claim, not a proof: it is trusted because
// the token says who sent it, which is why a header without a token draws a
// startup warning.

// localCaller is the owner of every stored result under stdio, where the one
// client that launched the process is the only caller there is.
const localCaller = "local"

// sharedCaller is the owner under HTTP when no --user-header is configured:
// every caller of the listener shares one set of handles, which is exactly
// the trust the listener already extends to them.
const sharedCaller = "shared"

// readAuthToken reads the shared secret from a file. The token lives in a
// file rather than on the command line, where ps would show it to every user
// of the host.
func readAuthToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("--http-auth-token-file: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("--http-auth-token-file: %s is empty", path)
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return "", fmt.Errorf("--http-auth-token-file: the token in %s contains whitespace; it must be a single line with no spaces", path)
	}
	return token, nil
}

// requireBearer rejects every request that does not carry
// "Authorization: Bearer <token>", before the MCP handler sees it. The
// comparison is constant-time so response timing says nothing about how much
// of a guess was right.
func requireBearer(next http.Handler, token string) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		got := []byte(strings.TrimSpace(req.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mssql-mcp-toolkit"`)
			http.Error(w, "missing or wrong bearer token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, req)
	})
}

// callerOf is the owner a tool call's stored results belong to. ok is false
// when the server needs an identity (--user-header is set) and the call did
// not carry one; such a call still gets its rows, but no handle is created
// or read for it.
func callerOf(cfg *config, req *mcp.CallToolRequest) (string, bool) {
	if cfg.transport != transportHTTP {
		return localCaller, true
	}
	if cfg.userHeader == "" {
		return sharedCaller, true
	}
	if req == nil || req.Extra == nil || req.Extra.Header == nil {
		return "", false
	}
	user := strings.TrimSpace(req.Extra.Header.Get(cfg.userHeader))
	if user == "" {
		return "", false
	}
	return "user:" + user, true
}

// noIdentityNote is what a reply says in place of a handle when the call
// carried no user identity.
func noIdentityNote(cfg *config) string {
	return fmt.Sprintf("no handle: this call did not carry the %s header, so its result was not stored and no handle can be read", cfg.userHeader)
}

// redactedHeaders are the headers --http-log-headers never writes out in
// full: they carry credentials.
var redactedHeaders = map[string]bool{
	"Authorization":       true,
	"Proxy-Authorization": true,
	"Cookie":              true,
}
