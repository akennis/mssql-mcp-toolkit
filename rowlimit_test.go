package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRowLimitAcceptsNumberAndString(t *testing.T) {
	cases := []struct {
		in   string
		want rowLimit
	}{
		{`5`, 5},
		{`"5"`, 5},
		{`" 5 "`, 5},
		{`500.0`, 500},
		{`"500.0"`, 500},
		{`5e2`, 500},
		{`null`, 0},
		{`""`, 0},
	}
	for _, c := range cases {
		var got rowLimit
		if err := json.Unmarshal([]byte(c.in), &got); err != nil {
			t.Errorf("Unmarshal(%s): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Unmarshal(%s) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestRowLimitRejectsNonNumbers(t *testing.T) {
	for _, in := range []string{`"ten"`, `"5 rows"`, `true`, `[5]`, `5.5`, `"5.5"`, `1e300`} {
		var got rowLimit
		if err := json.Unmarshal([]byte(in), &got); err == nil {
			t.Errorf("Unmarshal(%s) = %d, want an error", in, got)
		}
	}
}

// The whole point of the widened field is that a struct decode succeeds either
// way, so check it through queryInput rather than the scalar alone.
func TestQueryInputDecodesStringMaxRows(t *testing.T) {
	var in queryInput
	if err := json.Unmarshal([]byte(`{"query":"SELECT 1","max_rows":"25"}`), &in); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if in.Query != "SELECT 1" || in.MaxRows != 25 {
		t.Errorf("got %+v, want query=SELECT 1 max_rows=25", in)
	}
}

// A negative max_rows means the caller meant something, and "no cap" is
// almost certainly not it. Silently reading it as 0 is the worst of the
// available answers.
func TestRowLimitRejectsNegativeValues(t *testing.T) {
	for _, in := range []string{`-5`, `-5.0`, `-1`, `"-5"`, `"  -5 "`, `-5e2`, `"-5e2"`} {
		var got rowLimit
		err := json.Unmarshal([]byte(in), &got)
		if err == nil {
			t.Errorf("Unmarshal(%s) = %d, want an error", in, got)
			continue
		}
		if !strings.Contains(err.Error(), "negative") {
			t.Errorf("Unmarshal(%s) error = %v, want it to say the value is negative", in, err)
		}
	}
}

// Zero is the one non-positive value that is meaningful: it means "no cap".
func TestRowLimitAcceptsZeroAsNoCap(t *testing.T) {
	for _, in := range []string{`0`, `0.0`, `"0"`, `"0.0"`, `-0.0`} {
		var got rowLimit
		if err := json.Unmarshal([]byte(in), &got); err != nil {
			t.Errorf("Unmarshal(%s): %v", in, err)
			continue
		}
		if got != 0 {
			t.Errorf("Unmarshal(%s) = %d, want 0", in, got)
		}
	}
}

// Out-of-range values are rejected at both ends, each with its own message.
func TestRowLimitRejectsOutOfRangeValues(t *testing.T) {
	cases := map[string]string{
		`2147483648`:             "too large",
		`1e10`:                   "too large",
		`"99999999999999999999"`: "too large",
		`-2147483649`:            "negative",
		`-1e10`:                  "negative",
	}
	for in, want := range cases {
		var got rowLimit
		err := json.Unmarshal([]byte(in), &got)
		if err == nil {
			t.Errorf("Unmarshal(%s) = %d, want an error", in, got)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Unmarshal(%s) error = %v, want it to mention %q", in, err, want)
		}
	}
}

// maxRowLimit itself is the boundary, and is accepted.
func TestRowLimitAcceptsTheMaximum(t *testing.T) {
	var got rowLimit
	if err := json.Unmarshal([]byte(`2147483647`), &got); err != nil {
		t.Fatalf("Unmarshal(2147483647): %v", err)
	}
	if int(got) != maxRowLimit {
		t.Errorf("got %d, want %d", got, maxRowLimit)
	}
}
