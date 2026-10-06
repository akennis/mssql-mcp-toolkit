package main

import (
	"strings"
	"testing"
	"time"
)

func TestOpenDBAppliesThePoolLimit(t *testing.T) {
	db, err := openDB(&config{connString: "sqlserver://localhost?database=master", maxOpenConns: 3})
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
	if got := db.Stats().MaxOpenConnections; got != 3 {
		t.Errorf("MaxOpenConnections = %d, want 3", got)
	}
}

func TestOpenDBRejectsAMalformedConnectionString(t *testing.T) {
	db, err := openDB(&config{connString: "sqlserver://loc alhost:not-a-port", maxOpenConns: 1})
	if err == nil {
		db.Close()
		t.Fatal("openDB accepted a malformed connection string, want an error")
	}
}

// run must return its errors rather than exiting, so that the cleanup it
// defers — closing the connection pool, releasing the signal handler — is
// actually reached. main is a thin wrapper that turns the error into a status.
func TestRunReturnsErrorsInsteadOfExiting(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"config error", nil, "no connection string"},
		{"missing tool prefix", []string{"--conn-string", "server=localhost"}, "--tool-prefix"},
		{"bad flag value", []string{"--conn-string", "server=localhost", "--tool-prefix", "sales", "--max-rows", "-1"}, "--max-rows"},
		{"openDB error", []string{"--conn-string", "sqlserver://loc alhost:not-a-port", "--tool-prefix", "sales"}, "opening connection"},
	}
	for _, c := range cases {
		err := run(c.args)
		if err == nil {
			t.Errorf("%s: run(%v) = nil, want an error", c.name, c.args)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: run(%v) error = %v, want it to mention %q", c.name, c.args, err, c.want)
		}
	}
}

// newServer must not panic on any configuration loadConfig will accept: the
// input schema is built there, and a failure would take the process down after
// the client had already launched it.
func TestNewServerBuildsForEveryValidConfig(t *testing.T) {
	for _, cfg := range []*config{
		{queryTimeout: time.Second, maxOpenConns: 1, toolPrefix: "sales"},
		{queryTimeout: time.Second, maxOpenConns: 1, toolPrefix: "sales", maxRows: 500},
		{queryTimeout: time.Second, maxOpenConns: 1, toolPrefix: "sales", readOnly: true,
			queryDescription: "y", serverLabel: "z", instructions: "i"},
		{queryTimeout: time.Second, maxOpenConns: 1, toolPrefix: "sales",
			connString: "server=host;database=AdventureWorks"},
	} {
		if s := newServer(cfg, openStub(t), nil); s == nil {
			t.Errorf("newServer(%+v) = nil", cfg)
		}
	}
}

// The max_rows description names the configured cap, so the model is told the
// real ceiling rather than a generic one.
func TestQueryInputSchemaDescribesTheConfiguredCap(t *testing.T) {
	schema, err := queryInputSchema(500)
	if err != nil {
		t.Fatalf("queryInputSchema: %v", err)
	}
	desc := schema.Properties["max_rows"].Description
	if !strings.Contains(desc, "cap of 500 rows") {
		t.Errorf("description = %q, want it to name the 500-row cap", desc)
	}

	schema, err = queryInputSchema(0)
	if err != nil {
		t.Fatalf("queryInputSchema: %v", err)
	}
	desc = schema.Properties["max_rows"].Description
	if !strings.Contains(desc, "every row") {
		t.Errorf("description = %q, want it to say every row comes back", desc)
	}
}
