package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func testConfig() *config {
	return &config{queryTimeout: 5 * time.Second, maxOpenConns: 1}
}

// A query that outlives the deadline is a timeout; one the client cancelled is
// not. Reporting a cancellation as a timeout tells the model to rewrite a
// query that was never slow.
func TestHandleQueryDistinguishesTimeoutFromCancellation(t *testing.T) {
	db := openStub(t)

	cfg := testConfig()
	cfg.queryTimeout = 50 * time.Millisecond
	_, _, err := handleQuery(context.Background(), cfg, db, queryInput{Query: "WAITFOR DELAY '00:10:00'"})
	if err == nil || !strings.Contains(err.Error(), "timed out after 50ms") {
		t.Errorf("deadline: error = %v, want a timeout", err)
	}

	cfg = testConfig()
	cfg.queryTimeout = time.Hour // far away, so only the cancel can fire
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, _, err = handleQuery(ctx, cfg, db, queryInput{Query: "WAITFOR DELAY '00:10:00'"})
	if err == nil || !strings.Contains(err.Error(), "cancelled by the client") {
		t.Errorf("cancellation: error = %v, want a cancellation", err)
	}
	if err != nil && strings.Contains(err.Error(), "timed out") {
		t.Errorf("cancellation was reported as a timeout: %v", err)
	}
}

// A SQL error is neither, and must not be dressed up as one.
func TestHandleQueryReportsSQLErrorAsFailure(t *testing.T) {
	_, _, err := handleQuery(context.Background(), testConfig(), openStub(t), queryInput{Query: "SELECT * FROM dbo.Nope"})
	if err == nil || !strings.Contains(err.Error(), "query failed") {
		t.Errorf("error = %v, want a plain query failure", err)
	}
	if !strings.Contains(err.Error(), "invalid object name") {
		t.Errorf("error = %v, want the driver's message preserved", err)
	}
}

func TestHandleQueryRejectsEmptyQuery(t *testing.T) {
	for _, q := range []string{"", "   ", "\n\t "} {
		if _, _, err := handleQuery(context.Background(), testConfig(), openStub(t), queryInput{Query: q}); err == nil {
			t.Errorf("handleQuery(%q) = nil, want an error", q)
		}
	}
}

// The server cap and the per-call cap both use 0 for "no limit", so the
// effective limit is the smaller of the two that are actually set.
func TestHandleQueryAppliesSmallerOfTheTwoCaps(t *testing.T) {
	cases := []struct {
		name      string
		serverCap int
		callCap   rowLimit
		wantRows  int
	}{
		{"neither set", 0, 0, 2},
		{"server cap only", 1, 0, 1},
		{"call cap only", 0, 1, 1},
		{"call cap is smaller", 5, 1, 1},
		{"server cap is smaller", 1, 5, 1},
		{"both above the row count", 9, 9, 2},
	}
	for _, c := range cases {
		cfg := testConfig()
		cfg.maxRows = c.serverCap
		_, res, err := handleQuery(context.Background(), cfg, openStub(t),
			queryInput{Query: "SELECT id, name, price FROM dbo.Widget", MaxRows: c.callCap})
		if err != nil {
			t.Errorf("%s: handleQuery: %v", c.name, err)
			continue
		}
		if res.RowCount != c.wantRows {
			t.Errorf("%s: row_count = %d, want %d", c.name, res.RowCount, c.wantRows)
		}
	}
}

func TestHandleQueryReadOnlyMode(t *testing.T) {
	cfg := testConfig()
	cfg.readOnly = true

	if _, _, err := handleQuery(context.Background(), cfg, openStub(t), queryInput{Query: "DELETE FROM dbo.Widget"}); err == nil ||
		!strings.Contains(err.Error(), "read-only mode") {
		t.Errorf("error = %v, want a read-only rejection", err)
	}
	if _, _, err := handleQuery(context.Background(), cfg, openStub(t),
		queryInput{Query: "SELECT id, name, price FROM dbo.Widget"}); err != nil {
		t.Errorf("SELECT was rejected in read-only mode: %v", err)
	}
}

// A failure is as loggable as a success: the statement that failed is restated
// in the error, verbatim and after the message the caller matches on.
func TestHandleQueryErrorsRestateTheStatement(t *testing.T) {
	roCfg := testConfig()
	roCfg.readOnly = true

	for _, c := range []struct {
		name  string
		cfg   *config
		query string
	}{
		{"sql error", testConfig(), "SELECT * FROM dbo.Nope -- no such table"},
		{"read-only rejection", roCfg, "DROP TABLE dbo.Widget"},
	} {
		_, _, err := handleQuery(context.Background(), c.cfg, openStub(t), queryInput{Query: c.query})
		if err == nil {
			t.Fatalf("%s: error = nil, want a failure", c.name)
		}
		if !strings.Contains(err.Error(), "\nsql: "+c.query) {
			t.Errorf("%s: error = %q, want it to restate %q", c.name, err, c.query)
		}
	}
}
