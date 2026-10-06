package main

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Stored results.
//
// Every list result a tool returns is also kept here, whole (up to the
// store's limits), under a short handle such as qx4. The reply still shows
// rows — the model has to read what it resolved — but the next call no longer
// needs the model to copy hundreds of ids out of one reply and into another
// call's arguments: it passes @qx4.PersonID and the server expands it, or it
// hands qx4 to an operator that intersects, filters or counts it exactly.
//
// Results are kept per caller (see callerOf). A handle is only unique within
// its owner's results, which is why it can be short: nobody can read another
// user's handle, so there is nothing to gain by guessing one. The two letters
// in front are chosen when the process starts, so after a restart an old
// conversation's qx4 is unknown rather than silently some other result.
//
// Nothing here is written to disk, and nothing here writes to the database.

// repeatNoteWindow is how soon after a result was stored a repeat of the call
// is worth pointing out. Results are kept per caller, not per conversation, so
// an older match is often another chat's, and warning of a repeat then is wrong.
const repeatNoteWindow = 10 * time.Minute

const (
	defaultStoreMaxRows   = 100000
	defaultStoreMaxBytes  = 1 << 30
	defaultStoreUserBytes = 256 << 20
	defaultStoreTTL       = 2 * time.Hour
)

// provenance is how a stored result came to be: the tool that produced it,
// the arguments it was called with (as given, so a @handle argument reads as
// one), and the handles it was derived from.
type provenance struct {
	Tool    string         `json:"tool"`
	Args    map[string]any `json:"args,omitempty"`
	Parents []string       `json:"parents,omitempty"`
}

// storedResult is one result set held under a handle. Its rows are never
// modified after the store takes it, so a reader may use them without a lock.
type storedResult struct {
	ID    string
	Label string
	// Columns and Types are parallel: Types holds each column's SQL Server type
	// name as the driver reported it, or a synthetic one for derived results
	// ("BIGINT", "FLOAT", "NVARCHAR"), or "" when unknown.
	Columns []string
	Types   []string
	Rows    []map[string]any
	// Truncated is set when the capture stopped at a store limit, so Rows is
	// not the whole result. TruncNote says which limit.
	Truncated bool
	TruncNote string
	// Notes are the capture's other notes (a shortened cell, extra result
	// sets), carried so a later page of the result still says so.
	Notes []string
	Prov  provenance
	// lineage is every ancestor's one-line summary, oldest first, so a
	// derived result can say where it came from without the ancestors still
	// being in the store.
	lineage []string
	// hidden names the stored columns no reply shows: the record ids a tool
	// keeps only so they can be passed on as @handle.Column. A derived result
	// inherits the hidden columns of its inputs.
	hidden []string
	// display, when set, is the columns the query call asked to see; the reply
	// to a filter or sort of it shows these, not every non-hidden column.
	display []string
	// origRow, set only on a re-ordered view made by show, is each row's
	// 1-based place in the stored result the view came from. See
	// storedRowNumber.
	origRow []int

	owner string
	bytes int
	used  time.Time
	// made is when the store took the result; a repeat of the call long after
	// that is likelier a new conversation than the model repeating itself.
	made time.Time
	// shown is every [start, end) range of rows a reply has shown the model,
	// guarded by the store's lock. See copiedFrom.
	shown [][2]int
}

// column resolves a column name case-insensitively to its stored spelling.
func (r *storedResult) column(name string) (string, bool) {
	name = strings.TrimSpace(name)
	for _, c := range r.Columns {
		if strings.EqualFold(c, name) {
			return c, true
		}
	}
	return "", false
}

// isHidden reports whether a reply leaves the column out.
func (r *storedResult) isHidden(col string) bool {
	for _, h := range r.hidden {
		if h == col {
			return true
		}
	}
	return false
}

// visibleColumns is the stored columns a reply shows when nothing narrows it.
func (r *storedResult) visibleColumns() []string {
	var out []string
	for _, c := range r.Columns {
		if !r.isHidden(c) {
			out = append(out, c)
		}
	}
	return out
}

// inheritHidden marks the columns of r that any of the inputs hides.
func (r *storedResult) inheritHidden(inputs ...*storedResult) {
	for _, c := range r.Columns {
		for _, in := range inputs {
			if in.isHidden(c) {
				r.hidden = append(r.hidden, c)
				break
			}
		}
	}
}

// typeOf is the type recorded for a column, or "".
func (r *storedResult) typeOf(col string) string {
	for i, c := range r.Columns {
		if c == col && i < len(r.Types) {
			return r.Types[i]
		}
	}
	return ""
}

// typeMap is typeOf for every column at once.
func (r *storedResult) typeMap() map[string]string {
	m := make(map[string]string, len(r.Columns))
	for i, c := range r.Columns {
		if i < len(r.Types) {
			m[c] = r.Types[i]
		}
	}
	return m
}

// summary is the one-line description of how this result was produced, used
// in lineage.
func (r *storedResult) summary() string {
	var b strings.Builder
	b.WriteString(r.ID)
	b.WriteString(" = ")
	b.WriteString(r.Prov.Tool)
	b.WriteByte('(')
	keys := make([]string, 0, len(r.Prov.Args))
	for k := range r.Prov.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%s", k, clip(scalarString(r.Prov.Args[k]), 60))
	}
	fmt.Fprintf(&b, ") · %d rows", len(r.Rows))
	if r.Label != "" {
		fmt.Fprintf(&b, " · %q", r.Label)
	}
	return b.String()
}

// userResults is one owner's handles.
type userResults struct {
	next  int
	byID  map[string]*storedResult
	bytes int
}

// resultStore holds every caller's stored results, bounded by row count per
// result, bytes per user and bytes overall, and an idle time-to-live.
type resultStore struct {
	mu        sync.Mutex
	tag       string
	maxRows   int
	maxBytes  int
	userBytes int
	ttl       time.Duration
	now       func() time.Time
	users     map[string]*userResults
	total     int
	// onEvict is told about every result that leaves the store, so the
	// operator engine can drop the table it built for it.
	onEvict func(*storedResult)
}

func newResultStore(maxRows, maxBytes, userBytes int, ttl time.Duration) *resultStore {
	if maxRows <= 0 {
		maxRows = defaultStoreMaxRows
	}
	if maxBytes <= 0 {
		maxBytes = defaultStoreMaxBytes
	}
	if userBytes <= 0 || userBytes > maxBytes {
		userBytes = min(defaultStoreUserBytes, maxBytes)
	}
	if ttl <= 0 {
		ttl = defaultStoreTTL
	}
	return &resultStore{
		tag:       newHandleTag(),
		maxRows:   maxRows,
		maxBytes:  maxBytes,
		userBytes: userBytes,
		ttl:       ttl,
		now:       time.Now,
		users:     map[string]*userResults{},
	}
}

// handleTagLetters leaves out i, l and o, which read as digits.
const handleTagLetters = "abcdefghjkmnpqrstuvwxyz"

// newHandleTag picks the two letters every handle of this process starts
// with.
func newHandleTag() string {
	var b [2]byte
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(handleTagLetters))))
		if err != nil {
			// crypto/rand does not fail on supported platforms; a fixed tag
			// only loses the restart protection, not correctness.
			b[i] = handleTagLetters[i]
			continue
		}
		b[i] = handleTagLetters[n.Int64()]
	}
	return string(b[:])
}

// errUnknownHandle is the one answer for a handle that is not the caller's to
// read, whatever the reason: never issued, expired, dropped for space, from
// an earlier run, or someone else's. Telling those apart would tell a caller
// which handles exist.
func errUnknownHandle(id string) error {
	return fmt.Errorf("unknown handle %q: it may have expired, been dropped to make room, come from before the server restarted, or never existed; run the query that produced it again", id)
}

// normalizeHandle strips the @ a caller may write in front of a handle and
// lowercases it.
func normalizeHandle(id string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(id), "@"))
}

// put stores r for owner and returns its new handle. A result that on its own
// exceeds the per-user budget is not stored.
func (s *resultStore) put(owner string, r *storedResult) (string, error) {
	size := resultBytes(r)
	if size > s.userBytes {
		return "", fmt.Errorf("the result is %s, more than the %s one user's stored results may hold, so it was not stored", formatBytes(size), formatBytes(s.userBytes))
	}

	s.mu.Lock()
	var evicted []*storedResult
	defer func() {
		s.mu.Unlock()
		s.notifyEvicted(evicted)
	}()

	now := s.now()
	evicted = append(evicted, s.sweepLocked(now)...)
	u := s.users[owner]
	if u == nil {
		u = &userResults{byID: map[string]*storedResult{}}
		s.users[owner] = u
	}
	// Make room: the owner's own least recently used results first, then
	// anyone's, so one busy user cannot drain the store for the rest.
	for u.bytes+size > s.userBytes {
		if victim := lruOf(u.byID); victim != nil {
			evicted = append(evicted, s.dropLocked(victim))
			continue
		}
		break
	}
	for s.total+size > s.maxBytes {
		victim := s.globalLRULocked()
		if victim == nil {
			break
		}
		evicted = append(evicted, s.dropLocked(victim))
	}

	u.next++
	r.ID = s.tag + strconv.Itoa(u.next)
	r.owner = owner
	r.bytes = size
	r.used = now
	r.made = now
	u.byID[r.ID] = r
	u.bytes += size
	s.total += size
	return r.ID, nil
}

// findSame returns owner's stored result made by exactly this tool call (same
// tool, same arguments, same input handles), or nil. Stored rows never change,
// so re-running the call would only produce a second copy under a new handle.
func (s *resultStore) findSame(owner string, prov provenance) *storedResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.users[owner]
	if u == nil {
		return nil
	}
	var best *storedResult
	for _, r := range u.byID {
		if r.Prov.Tool != prov.Tool || !reflect.DeepEqual(r.Prov.Args, prov.Args) || !slices.Equal(r.Prov.Parents, prov.Parents) {
			continue
		}
		if best == nil || r.used.After(best.used) {
			best = r
		}
	}
	if best != nil {
		best.used = s.now()
	}
	return best
}

// recentlyMade reports whether r was stored within repeatNoteWindow.
func (s *resultStore) recentlyMade(r *storedResult) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now().Sub(r.made) <= repeatNoteWindow
}

// get returns owner's result under id, refreshing its idle clock.
func (s *resultStore) get(owner, id string) (*storedResult, error) {
	key := normalizeHandle(id)
	s.mu.Lock()
	var evicted []*storedResult
	defer func() {
		s.mu.Unlock()
		s.notifyEvicted(evicted)
	}()
	now := s.now()
	evicted = s.sweepLocked(now)
	u := s.users[owner]
	if u == nil {
		return nil, errUnknownHandle(key)
	}
	r := u.byID[key]
	if r == nil {
		return nil, errUnknownHandle(key)
	}
	r.used = now
	return r, nil
}

// sweepLocked drops every result idle for longer than the TTL.
func (s *resultStore) sweepLocked(now time.Time) []*storedResult {
	var out []*storedResult
	for _, u := range s.users {
		for _, r := range u.byID {
			if now.Sub(r.used) > s.ttl {
				out = append(out, s.dropLocked(r))
			}
		}
	}
	return out
}

func (s *resultStore) dropLocked(r *storedResult) *storedResult {
	u := s.users[r.owner]
	if u == nil || u.byID[r.ID] != r {
		return r
	}
	delete(u.byID, r.ID)
	u.bytes -= r.bytes
	s.total -= r.bytes
	return r
}

func (s *resultStore) globalLRULocked() *storedResult {
	var victim *storedResult
	for _, u := range s.users {
		if r := lruOf(u.byID); r != nil && (victim == nil || r.used.Before(victim.used)) {
			victim = r
		}
	}
	return victim
}

func lruOf(m map[string]*storedResult) *storedResult {
	var victim *storedResult
	for _, r := range m {
		if victim == nil || r.used.Before(victim.used) || (r.used.Equal(victim.used) && r.ID < victim.ID) {
			victim = r
		}
	}
	return victim
}

func (s *resultStore) notifyEvicted(rs []*storedResult) {
	if s.onEvict == nil {
		return
	}
	for _, r := range rs {
		s.onEvict(r)
	}
}

// resultBytes estimates what a result costs to hold: the text of every value
// plus a fixed overhead per value and per row. It only has to be consistent,
// not exact — it is what the byte limits are counted in.
func resultBytes(r *storedResult) int {
	n := 256
	for _, c := range r.Columns {
		n += len(c) + 16
	}
	for _, row := range r.Rows {
		n += 64
		for k, v := range row {
			n += len(k) + 16
			switch x := v.(type) {
			case string:
				n += len(x)
			case []byte:
				n += len(x)
			default:
				n += 8
			}
		}
	}
	return n
}

// cleanLabel tidies a SaveAs label: printable, single-line, at most 40
// characters.
func cleanLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.TrimSpace(s))
	s = strings.Join(strings.Fields(s), " ")
	return clip(s, 40)
}

// clip shortens s to n runes, marking the cut.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// markShown records that a reply showed rows [start, end) of r.
func (s *resultStore) markShown(r *storedResult, start, end int) {
	if end <= start {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rg := range r.shown {
		if rg == [2]int{start, end} {
			return
		}
	}
	r.shown = append(r.shown, [2]int{start, end})
}

// copiedMatch is a literal id list that turned out to be exactly the rows of
// a stored result the model was shown. sample is true when the result holds
// more than what was given (a partial reply's sample stood in for the
// whole set); it is false when the given list is exactly the whole,
// complete result (the model typed out ids it could have passed as a
// handle instead, even though it did see everything).
type copiedMatch struct {
	result          *storedResult
	column          string
	given, distinct int
	sample          bool
}

// minCopied is the shortest list copiedFrom looks at: a user naming two or
// three people who happen to sit on a shown page is ordinary.
const minCopied = 3

// copiedFrom looks through owner's stored results for one whose column col
// holds every value in values, where values are exactly the distinct values
// of the rows a reply showed — a page, or everything shown so far, whether
// that result was truncated or came back complete. That is the signature of
// a model copying ids out of a reply instead of passing the handle on. It
// returns nil otherwise.
func (s *resultStore) copiedFrom(owner, col string, values []string) *copiedMatch {
	want := map[string]bool{}
	for _, v := range values {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			want[v] = true
		}
	}
	if len(want) < minCopied {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.users[owner]
	if u == nil {
		return nil
	}
	for _, r := range u.byID {
		c, ok := r.column(col)
		if !ok || len(r.shown) == 0 {
			continue
		}
		distinctOf := func(rows []map[string]any) map[string]bool {
			out := map[string]bool{}
			for _, row := range rows {
				if v := row[c]; v != nil {
					out[strings.ToLower(strings.TrimSpace(scalarString(v)))] = true
				}
			}
			return out
		}
		all := distinctOf(r.Rows)
		if len(all) < len(want) {
			continue
		}
		// Each page shown, and the union of them.
		var union []map[string]any
		candidates := make([]map[string]bool, 0, len(r.shown)+1)
		for _, rg := range r.shown {
			page := r.Rows[rg[0]:min(rg[1], len(r.Rows))]
			union = append(union, page...)
			candidates = append(candidates, distinctOf(page))
		}
		candidates = append(candidates, distinctOf(union))
		for _, seen := range candidates {
			if sameSet(seen, want) {
				return &copiedMatch{result: r, column: c, given: len(want), distinct: len(all), sample: len(all) > len(want)}
			}
		}
	}
	return nil
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
