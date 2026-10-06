package main

import (
	"strings"
	"testing"
)

func TestLoopBreakerStreaks(t *testing.T) {
	b := newLoopBreaker()
	if w := b.record("u", "a {}", true); w != "" {
		t.Errorf("first failure warned: %s", w)
	}
	if w := b.record("u", "a {}", true); !strings.Contains(w, "STOP: this exact call has now failed 2 times") {
		t.Errorf("identical repeat: %q", w)
	}
	// A success ends the streak; three different failures then trip the other limit.
	b.record("u", "ok {}", false)
	b.record("u", "a {}", true)
	b.record("u", "b {}", true)
	if w := b.record("u", "c {}", true); !strings.Contains(w, "3 calls in a row have failed") {
		t.Errorf("three different failures: %q", w)
	}
	// Another caller is unaffected.
	if w := b.record("v", "a {}", true); w != "" {
		t.Errorf("other caller warned: %s", w)
	}
}

func TestZeroRowFilterHintAndRepeat(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	people := handleOf(t, callQueryTool(t, cs, "roster", nil))
	sched := handleOf(t, callQueryTool(t, cs, "schedules", nil))

	// The text is in another column: the hint points at it.
	res := callQueryTool(t, cs, "sales_filter", map[string]any{"Handle": people, "Where": "first = 'analyst'"})
	if text := contentText(res); !strings.Contains(text, "0 rows") || !strings.Contains(text, "does occur in title") {
		t.Errorf("hint for text in another column:\n%s", text)
	}
	// Nowhere: the hint shows what the named column holds.
	res = callQueryTool(t, cs, "sales_filter", map[string]any{"Handle": sched, "Where": "CourseName = 'Nope'"})
	text := contentText(res)
	if !strings.Contains(text, "CourseName holds values such as") || !strings.Contains(text, "Algebra") {
		t.Errorf("hint for a value that is nowhere:\n%s", text)
	}
	// The same filter again reuses the handle and says it found nothing twice.
	first := handleOf(t, res)
	again := callQueryTool(t, cs, "sales_filter", map[string]any{"Handle": sched, "Where": "CourseName = 'Nope'"})
	if handleOf(t, again) != first || !strings.Contains(contentText(again), "same call as "+first) || !strings.Contains(contentText(again), "0 rows both times") {
		t.Errorf("repeat filter:\n%s", contentText(again))
	}
}

func TestWrongIDColumnNamesTheKind(t *testing.T) {
	cs, _ := handleClient(t, handleTools, nil)
	people := handleOf(t, callQueryTool(t, cs, "roster", nil))
	res := callQueryTool(t, cs, "echo", map[string]any{"PersonID": "@" + people + ".title"})
	text := contentText(res)
	if !res.IsError || !strings.Contains(text, "the title column you passed is a different kind of id") || !strings.Contains(text, "not used by this tool") {
		t.Errorf("wrong id column:\n%s", text)
	}
}

func TestRestoreBits(t *testing.T) {
	r := &storedResult{
		Columns: []string{"n", "flag"},
		Types:   []string{"INT", "BIT"},
		Rows:    []map[string]any{{"n": int64(1), "flag": int64(1)}, {"n": int64(0), "flag": int64(0)}},
	}
	restoreBits(r)
	if r.Rows[0]["flag"] != true || r.Rows[1]["flag"] != false || r.Rows[0]["n"] != int64(1) {
		t.Errorf("restoreBits: %v", r.Rows)
	}
}
