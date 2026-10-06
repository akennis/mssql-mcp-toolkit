package main

import (
	"errors"
	"flag"
	"strings"
	"testing"
	"time"
)

// --help is a request that was served. It must not print an error line or exit
// non-zero: the README tells users to run it to read the tool description.
func TestHelpIsNotAnError(t *testing.T) {
	for _, arg := range []string{"-h", "--help"} {
		_, err := loadConfig([]string{arg})
		if !errors.Is(err, flag.ErrHelp) {
			t.Errorf("loadConfig(%s) error = %v, want flag.ErrHelp", arg, err)
		}
		// run must hand the same sentinel back untouched, so main can exit 0.
		if err := run([]string{arg}); !errors.Is(err, flag.ErrHelp) {
			t.Errorf("run(%s) error = %v, want flag.ErrHelp", arg, err)
		}
	}
}

// --help must work without a connection string; the missing-conn-string check
// would otherwise mask it.
func TestHelpWorksWithoutAConnectionString(t *testing.T) {
	if _, err := loadConfig([]string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("error = %v, want flag.ErrHelp rather than a config complaint", err)
	}
}

func TestLoadConfigRequiresAConnectionString(t *testing.T) {
	_, err := loadConfig(nil)
	if err == nil || !strings.Contains(err.Error(), "no connection string") {
		t.Errorf("error = %v, want a missing-connection-string error", err)
	}
}

// Configuration arrives by flag and only by flag.
//
// This matters most to whoever is upgrading: a client config that used to set
// these variables still sets them, and silently honouring a stale
// MSSQL_READ_ONLY=false would start a server with writes enabled that the
// operator believes is read-only. Ignoring them outright is the only reading
// that cannot surprise anyone.
func TestLoadConfigIgnoresTheEnvironment(t *testing.T) {
	for name, value := range map[string]string{
		"MSSQL_CONNECTION_STRING": "server=fromenv",
		"MSSQL_TOOL_PREFIX":       "fromenv",
		"MSSQL_TOOL_DESCRIPTION":  "from the environment",
		"MSSQL_SERVER_LABEL":      "from the environment",
		"MSSQL_INSTRUCTIONS":      "from the environment",
		"MSSQL_MAX_ROWS":          "250",
		"MSSQL_QUERY_TIMEOUT":     "90s",
		"MSSQL_READ_ONLY":         "true",
		"MSSQL_MAX_OPEN_CONNS":    "8",
	} {
		t.Setenv(name, value)
	}

	cfg, err := loadConfig([]string{"--conn-string", "server=fromflag", "--tool-prefix", "sales"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.connString != "server=fromflag" || cfg.toolPrefix != "sales" {
		t.Errorf("got %+v, want the flag values", cfg)
	}
	if !hasDefaultBudget(cfg) || cfg.queryTimeout != 30*time.Second || cfg.readOnly || cfg.maxOpenConns != 4 {
		t.Errorf("got %+v, want the built-in defaults rather than the environment's values", cfg)
	}
	if !isBlank(cfg.queryDescription) || !isBlank(cfg.getMetadataDescription) ||
		!isBlank(cfg.listMetadataDescription) || !isBlank(cfg.serverLabel) || !isBlank(cfg.instructions) {
		t.Errorf("got %+v, want the identity fields left for newServer to default", cfg)
	}
}

// A variable that used to be a startup error is now not read at all, so it
// cannot fail a server that is correctly configured by flag.
func TestMalformedEnvironmentIsNotAStartupError(t *testing.T) {
	t.Setenv("MSSQL_MAX_ROWS", "1oo")
	t.Setenv("MSSQL_READ_ONLY", "yes")
	t.Setenv("MSSQL_QUERY_TIMEOUT", "half a minute")

	if _, err := loadConfig([]string{"--conn-string", "server=localhost", "--tool-prefix", "sales"}); err != nil {
		t.Errorf("loadConfig: %v, want the variables ignored rather than parsed", err)
	}
}

// Neither required setting can be supplied by its old variable: the server has
// to refuse to start rather than come up half-configured from a stale config.
func TestEnvironmentDoesNotSatisfyTheRequiredFlags(t *testing.T) {
	t.Setenv("MSSQL_CONNECTION_STRING", "server=localhost")
	t.Setenv("MSSQL_TOOL_PREFIX", "sales")

	_, err := loadConfig(nil)
	if err == nil || !strings.Contains(err.Error(), "no connection string") {
		t.Errorf("error = %v, want a missing-connection-string error", err)
	}

	_, err = loadConfig([]string{"--conn-string", "server=localhost"})
	if err == nil || !strings.Contains(err.Error(), "--tool-prefix") {
		t.Errorf("error = %v, want a missing-prefix error", err)
	}
}

func TestLoadConfigReadsFlags(t *testing.T) {
	cfg, err := loadConfig([]string{
		"--conn-string", "server=localhost", "--tool-prefix", "sales",
		"--max-rows", "250", "--query-timeout", "90s", "--read-only",
		"--max-open-conns", "8", "--query-fn-desc", "custom text",
		"--server-label", "Sales warehouse", "--instructions", "read the sales database",
	})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.connString != "server=localhost" || cfg.maxRows != 250 ||
		cfg.queryTimeout != 90*time.Second || !cfg.readOnly || cfg.maxOpenConns != 8 {
		t.Errorf("got %+v, want the flag values", cfg)
	}
	if cfg.toolPrefix != "sales" || cfg.queryDescription != "custom text" ||
		cfg.serverLabel != "Sales warehouse" || cfg.instructions != "read the sales database" {
		t.Errorf("got %+v, want the identity flag values", cfg)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig([]string{"--conn-string", "server=localhost", "--tool-prefix", "sales"})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !hasDefaultBudget(cfg) || cfg.queryTimeout != 30*time.Second || cfg.readOnly || cfg.maxOpenConns != 4 {
		t.Errorf("got %+v, want the built-in defaults", cfg)
	}
}

// hasDefaultBudget reports whether the three output limits are the built-in
// ones. They are checked together because they are one setting in three parts:
// a config with a row cap and no byte budget is not "the defaults with one
// change", it is a config that can still return an unbounded payload.
func hasDefaultBudget(cfg *config) bool {
	return cfg.maxRows == defaultMaxRows &&
		cfg.maxBytes == defaultMaxBytes &&
		cfg.maxCellBytes == defaultMaxCellBytes
}

func TestLoadConfigValidatesRanges(t *testing.T) {
	cases := [][]string{
		{"--max-rows", "-1"},
		{"--max-bytes", "-1"},
		{"--max-cell-bytes", "-1"},
		{"--query-timeout", "0s"},
		{"--query-timeout", "-5s"},
		{"--max-open-conns", "0"},
		{"--max-open-conns", "-2"},
	}
	for _, extra := range cases {
		args := append([]string{"--conn-string", "server=localhost", "--tool-prefix", "sales"}, extra...)
		if _, err := loadConfig(args); err == nil {
			t.Errorf("loadConfig(%v) = nil, want an error", extra)
		}
	}
}
