package main

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func sampleResult(n int) *storedResult {
	r := &storedResult{Columns: []string{"id", "name"}, Types: []string{"INT", "NVARCHAR"}, Prov: provenance{Tool: "t"}}
	for i := range n {
		r.Rows = append(r.Rows, map[string]any{"id": int64(i + 1), "name": fmt.Sprintf("name-%d", i+1)})
	}
	return r
}

func TestStorePutGetAndHandleFormat(t *testing.T) {
	s := newResultStore(100, 1<<20, 1<<20, time.Hour)
	id1, err := s.put("alice", sampleResult(3))
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := s.put("alice", sampleResult(3))
	idB, _ := s.put("bob", sampleResult(3))
	if !regexp.MustCompile(`^[a-z]{2}1$`).MatchString(id1) || id2 != s.tag+"2" {
		t.Errorf("handles %q, %q; want the startup tag and a per-user counter", id1, id2)
	}
	// Counters are per user, so bob's first handle has the same text as
	// alice's first — and still only reads bob's result.
	if idB != id1 {
		t.Errorf("bob's first handle = %q, want %q", idB, id1)
	}
	for _, spelled := range []string{id1, "@" + id1, strings.ToUpper(id1), " " + id1 + " "} {
		r, err := s.get("alice", spelled)
		if err != nil || r.ID != id1 || r.owner != "alice" {
			t.Errorf("get(alice, %q) = %v, %v", spelled, r, err)
		}
	}
}

// One user can never read another's result, and the error is the same one an
// unknown handle gets, so it does not tell anyone the handle exists.
func TestStoreIsolatesUsers(t *testing.T) {
	s := newResultStore(100, 1<<20, 1<<20, time.Hour)
	s.put("alice", sampleResult(1))
	s.put("alice", sampleResult(1))
	id, _ := s.put("alice", sampleResult(1)) // alice's third; bob has none
	_, errOther := s.get("bob", id)
	_, errNever := s.get("alice", s.tag+"99")
	if errOther == nil {
		t.Fatal("bob read alice's handle")
	}
	if strings.ReplaceAll(errOther.Error(), id, "X") != strings.ReplaceAll(errNever.Error(), s.tag+"99", "X") {
		t.Errorf("errors differ:\n%v\n%v", errOther, errNever)
	}
}

// A handle from before a restart has the old tag, so the new process does not
// resolve it to one of its own results.
func TestStoreRejectsHandlesFromAnEarlierRun(t *testing.T) {
	before := newResultStore(100, 1<<20, 1<<20, time.Hour)
	before.tag = "qx"
	old, _ := before.put("alice", sampleResult(1))
	after := newResultStore(100, 1<<20, 1<<20, time.Hour)
	after.tag = "rk"
	after.put("alice", sampleResult(1))
	if _, err := after.get("alice", old); err == nil {
		t.Errorf("%s from the earlier run resolved after the restart", old)
	}
}

func TestStoreTTL(t *testing.T) {
	now := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	s := newResultStore(100, 1<<20, 1<<20, time.Hour)
	s.now = func() time.Time { return now }
	var evicted []string
	s.onEvict = func(r *storedResult) { evicted = append(evicted, r.ID) }
	a, _ := s.put("alice", sampleResult(1))
	b, _ := s.put("alice", sampleResult(1))
	now = now.Add(50 * time.Minute)
	if _, err := s.get("alice", a); err != nil { // a is used, b is not
		t.Fatal(err)
	}
	now = now.Add(20 * time.Minute)
	if _, err := s.get("alice", b); err == nil {
		t.Errorf("%s outlived its TTL", b)
	}
	if _, err := s.get("alice", a); err != nil {
		t.Errorf("%s expired although it was used: %v", a, err)
	}
	if len(evicted) != 1 || evicted[0] != b {
		t.Errorf("evicted %v, want [%s]", evicted, b)
	}
}

func TestStoreEvictsByBytes(t *testing.T) {
	one := resultBytes(sampleResult(10))
	// Room for two results per user and three overall.
	s := newResultStore(100, 3*one+10, 2*one+10, time.Hour)
	clock := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { clock = clock.Add(time.Second); return clock }

	a1, _ := s.put("alice", sampleResult(10))
	a2, _ := s.put("alice", sampleResult(10))
	a3, _ := s.put("alice", sampleResult(10)) // alice is over her share: a1 goes
	if _, err := s.get("alice", a1); err == nil {
		t.Errorf("%s survived alice exceeding her own budget", a1)
	}
	for _, id := range []string{a2, a3} {
		if _, err := s.get("alice", id); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
	// Bob's first result fits the total; his second does not, and the least
	// recently used result overall (a2, read before a3) makes room.
	s.put("bob", sampleResult(10))
	s.put("bob", sampleResult(10))
	if _, err := s.get("alice", a2); err == nil {
		t.Errorf("%s survived the store exceeding its total", a2)
	}
	if s.total > s.maxBytes {
		t.Errorf("store holds %d bytes, over its %d", s.total, s.maxBytes)
	}

	// A result bigger than a user's whole share is refused outright.
	if _, err := s.put("carol", sampleResult(100)); err == nil {
		t.Error("an oversized result was stored")
	}
}

func TestStoreConcurrentUse(t *testing.T) {
	s := newResultStore(100, 1<<24, 1<<22, time.Hour)
	var wg sync.WaitGroup
	for u := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner := fmt.Sprintf("user%d", u)
			for range 50 {
				id, err := s.put(owner, sampleResult(5))
				if err != nil {
					t.Error(err)
					return
				}
				if r, err := s.get(owner, id); err != nil || r.owner != owner {
					t.Errorf("get(%s, %s) = %v", owner, id, err)
				}
			}
		}()
	}
	wg.Wait()
	for u := range 8 {
		if n := s.users[fmt.Sprintf("user%d", u)].next; n != 50 {
			t.Errorf("user%d issued %d handles, want 50", u, n)
		}
	}
}

func TestCleanLabel(t *testing.T) {
	if got := cleanLabel("  hr\tstudents\n2025  "); got != "hr students 2025" {
		t.Errorf("cleanLabel = %q", got)
	}
	if got := cleanLabel(strings.Repeat("x", 60)); len([]rune(got)) != 40 {
		t.Errorf("cleanLabel kept %d runes, want 40", len([]rune(got)))
	}
}
