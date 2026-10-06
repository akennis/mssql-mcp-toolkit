package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// How a stored result reaches the model: what a reply shows, how a later
// call names a stored result as input, and how a query tool's result is
// captured whole before a page of it is shown.

const (
	// inlineMaxRows is the largest result shown in full with no profile. Every
	// resolver's answer is under it, so resolution reads exactly as it did
	// before handles existed.
	inlineMaxRows = 50
	// sampleRows is the most rows a large result shows in the reply that
	// produced it, whatever PageSize asked for. A handful of rows cannot pass
	// for a whole class list, which twenty could; the profile carries the
	// counts, and show pages through the rest.
	sampleRows = 5
	// batchChunk is the most ids one statement is sent at once when a
	// @handle expands a batch parameter; a longer list is run in chunks and
	// the results merged.
	batchChunk = 1000
)

// The synthetic arguments stored results add to a tool's schema. Like
// Columns, they never bind into the SQL text, so the names are reserved.
const (
	saveAsParamName       = "SaveAs"
	allowPartialParamName = "AllowPartial"
)

// ensureRuntime creates the process-wide result store and operator engine the
// first time a server is built, so every server of this process shares them.
func (cfg *config) ensureRuntime() {
	if !cfg.resultStore {
		return
	}
	if cfg.results == nil {
		cfg.results = newResultStore(cfg.storeMaxRows, cfg.storeMaxBytes, cfg.storeUserBytes, cfg.storeTTL)
	}
	if cfg.calc == nil {
		cfg.calc = newCalcEngine()
		cfg.results.onEvict = cfg.calc.drop
	}
}

// handlesOn reports whether stored results are in use.
func (cfg *config) handlesOn() bool { return cfg.resultStore && cfg.results != nil }

// showToolName is the show tool's name: the server's prefix and "show", or
// the bare suffix when there is no prefix.
func showToolName(cfg *config) string { return prefixedName(cfg, showToolSuffix) }

// prefixedName composes a built-in tool name the way describeToolName does.
func prefixedName(cfg *config, suffix string) string {
	if isBlank(cfg.toolPrefix) {
		return suffix
	}
	return toolName(cfg.toolPrefix, suffix)
}

// callNameOn is how a model on the server named server calls tool.
func callNameOn(cfg *config, server, tool string) string {
	return toolCallName(cfg.toolCallName, server, tool)
}

// serverOfGroup is the {server} a group's tools are called under: its
// label, or "" for the base server.
func serverOfGroup(cfg *config, group string) string {
	for _, g := range cfg.queryToolGroups {
		if g.Name == group {
			return groupServerName(g)
		}
	}
	return ""
}

// --- @handle arguments ---

// A handle reference is qx4, or qx4.Column, with an optional leading @ (see
// handleArg) and an optional row position (see parseHandleRef). The column may
// be written in brackets for a name with spaces: qx4.[Course Name].

// cleanHandleText strips the markup a model wraps around an argument it is
// writing as prose - **bold**, `code`, quotes, a trailing comma or period - so
// "@md18.SchCourseScheduleID**" still reads as a handle reference.
func cleanHandleText(s string) string {
	s = strings.Trim(s, " \t\r\n*`\"'")
	s = strings.TrimLeft(s, "(")
	return strings.TrimRight(s, ".,;:)*`\"'")
}

// handleRef is a parsed handle argument.
type handleRef struct {
	id, column string
	// sel is the rows picked by position (h1.Col[0], h1[2:5].Col), or nil for
	// every row.
	sel *rowSel
}

// text is the reference written one way: id, column and position.
func (r handleRef) text() string {
	s := r.id
	if r.column != "" {
		s += "." + r.column
	}
	if r.sel != nil {
		s += "[" + r.sel.text + "]"
	}
	return s
}

// handleHeadPattern is the front of a handle reference: an optional @ and the
// handle id. What follows is read by parseHandleRef.
var handleHeadPattern = regexp.MustCompile(`^(@?)([A-Za-z]{2}[0-9]+)(.*)$`)

// bracketTailPattern and bracketHeadPattern find a [...] at the end or the
// start of the text after the handle id.
var (
	bracketTailPattern = regexp.MustCompile(`^(.+?)\[([^\[\]]*)\]$`)
	bracketHeadPattern = regexp.MustCompile(`^\[([^\[\]]*)\](.*)$`)
)

// parseHandleRef reads a handle reference: id, id.Column, and either with a
// row position written Python style before the column (h1[0].Col) or after it
// (h1.Col[0:5]). at reports whether it was written with the leading @.
func parseHandleRef(s string) (ref handleRef, at, ok bool) {
	m := handleHeadPattern.FindStringSubmatch(cleanHandleText(s))
	if m == nil {
		return handleRef{}, false, false
	}
	rest := m[3]
	var sel *rowSel
	if hm := bracketHeadPattern.FindStringSubmatch(rest); hm != nil {
		if sel, ok = parseRowSel(hm[1]); !ok {
			return handleRef{}, false, false
		}
		rest = hm[2]
	}
	col := ""
	if rest != "" {
		if !strings.HasPrefix(rest, ".") || len(rest) == 1 {
			return handleRef{}, false, false
		}
		col = rest[1:]
		if tm := bracketTailPattern.FindStringSubmatch(col); tm != nil {
			if tsel, isSel := parseRowSel(tm[2]); isSel {
				if sel != nil {
					return handleRef{}, false, false
				}
				sel, col = tsel, tm[1]
			}
		}
		col = strings.TrimSpace(col)
		col = strings.TrimSuffix(strings.TrimPrefix(col, "["), "]")
	}
	return handleRef{id: strings.ToLower(m[2]), column: col, sel: sel}, m[1] == "@", true
}

// rowSel is a Python-style pick of rows by position: one index (h1.Col[0],
// h1.Col[-1]) or a slice (h1.Col[2:5], [:3], [-3:], [::2]). Positions count
// from 0 in the order the result was stored.
type rowSel struct {
	text  string
	index bool
	i     int
	// start, stop and step of a slice; nil is left out.
	start, stop, step *int
}

var (
	rowIndexPattern = regexp.MustCompile(`^\s*(-?\d+)\s*$`)
	rowSlicePattern = regexp.MustCompile(`^\s*(-?\d*)\s*:\s*(-?\d*)\s*(?::\s*(-?\d*)\s*)?$`)
)

// parseRowSel reads what sits between the brackets.
func parseRowSel(s string) (*rowSel, bool) {
	if m := rowIndexPattern.FindStringSubmatch(s); m != nil {
		i, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, false
		}
		return &rowSel{text: strings.TrimSpace(s), index: true, i: i}, true
	}
	m := rowSlicePattern.FindStringSubmatch(s)
	if m == nil {
		return nil, false
	}
	sel := &rowSel{text: strings.Join(strings.Fields(s), "")}
	for k, dst := range []**int{&sel.start, &sel.stop, &sel.step} {
		if m[k+1] == "" {
			continue
		}
		n, err := strconv.Atoi(m[k+1])
		if err != nil {
			return nil, false
		}
		*dst = &n
	}
	return sel, true
}

// positions are the row positions the selection picks out of n rows, in
// order, with Python's rules: a negative position counts from the end and a
// slice runs past either end without error.
func (s *rowSel) positions(n int) ([]int, error) {
	if s.index {
		i := s.i
		if i < 0 {
			i += n
		}
		if i < 0 || i >= n {
			return nil, fmt.Errorf("it has %d rows; use a position from 0 to %d (a negative position counts from the end)", n, n-1)
		}
		return []int{i}, nil
	}
	step := 1
	if s.step != nil {
		step = *s.step
	}
	if step == 0 {
		return nil, errors.New("a slice step cannot be 0")
	}
	lo, hi := 0, n // the bounds a clamped position may take
	if step < 0 {
		lo, hi = -1, n-1
	}
	clamp := func(p int) int {
		if p < 0 {
			p += n
			if p < 0 {
				p = lo
			}
		} else if p >= n {
			p = hi
		}
		return p
	}
	var start, stop int
	switch {
	case s.start != nil:
		start = clamp(*s.start)
	case step < 0:
		start = n - 1
	}
	switch {
	case s.stop != nil:
		stop = clamp(*s.stop)
	case step < 0:
		stop = -1
	default:
		stop = n
	}
	var out []int
	if step > 0 {
		for i := start; i < stop; i += step {
			out = append(out, i)
		}
	} else {
		for i := start; i > stop; i += step {
			out = append(out, i)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("it selects no rows of the %d it has; use a position or range inside 0 to %d", n, n-1)
	}
	return out, nil
}

// handleArg reports whether a list parameter's value is a handle reference.
// Written with @ it always is. Without it — a model that drops the @ and
// writes SchCourseID=qk3 — it is taken as one when it starts with this run's
// handle letters, since no id list holds a value of that shape; any other
// bare value (a school-assigned id such as AB123) stays literal.
func handleArg(cfg *config, s string) (handleRef, bool) {
	ref, at, ok := parseHandleRef(s)
	if !ok {
		return handleRef{}, false
	}
	if at {
		return ref, true
	}
	if cfg.handlesOn() && strings.HasPrefix(ref.id, cfg.results.tag) {
		return ref, true
	}
	return handleRef{}, false
}

// expandedArg is one batch parameter whose value came from a stored result.
type expandedArg struct {
	param  string
	ref    handleRef
	source *storedResult
	values []string
}

// columnValues is the distinct, non-null values of col in r, in the order
// they first appear, as the text a comma-separated id list is written in.
func columnValues(r *storedResult, col string) ([]string, error) {
	return columnValuesOf(r, r.Rows, col)
}

// columnValuesOf is columnValues over a chosen set of r's rows.
func columnValuesOf(r *storedResult, rows []map[string]any, col string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, row := range rows {
		v := row[col]
		if v == nil {
			continue
		}
		s := strings.TrimSpace(scalarString(v))
		if s == "" || seen[s] {
			continue
		}
		if strings.Contains(s, ",") {
			return nil, fmt.Errorf("%s.%s holds %q, which contains a comma and cannot be passed in a comma-separated list", r.ID, col, s)
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, nil
}

// expandHandleArgs replaces every @handle argument of a batch parameter with
// the handle's values, in place in args. It returns what it expanded, so the
// caller can chunk a long list and check the result against its input.
func expandHandleArgs(cfg *config, spec *queryToolSpec, owner string, identified bool, args map[string]any) ([]*expandedArg, error) {
	var out []*expandedArg
	allowPartial, err := boolArg(args, allowPartialParamName)
	if err != nil {
		return nil, err
	}
	for _, p := range spec.Parameters {
		raw, ok := args[p.Name].(string)
		if !ok {
			if v := args[p.Name]; v != nil && p.batch && !p.Literal {
				return nil, errTypedIDs(cfg, spec, p, fmt.Sprint(v))
			}
			continue
		}
		ref, at, isRef := parseHandleRef(raw)
		if !isRef {
			if err := literalIDs(cfg, spec, p, owner, identified, allowPartial, raw); err != nil {
				return nil, err
			}
			continue
		}
		if !p.batch {
			// Only a reference written with @ is an error here; a bare value
			// on a literal parameter is just a value.
			if !at {
				continue
			}
			return nil, fmt.Errorf("parameter %q takes a literal value, not a handle; the parameters that take handle.Column are: %s",
				p.Name, strings.Join(batchParamNames(spec), ", "))
		}
		if !at {
			if ref, isRef = handleArg(cfg, raw); !isRef {
				if err := literalIDs(cfg, spec, p, owner, identified, allowPartial, raw); err != nil {
					return nil, err
				}
				continue
			}
		}
		if !cfg.handlesOn() {
			return nil, fmt.Errorf("parameter %q: stored results are turned off on this server, so %s cannot be read", p.Name, ref.id)
		}
		if !identified {
			return nil, fmt.Errorf("parameter %q: %s", p.Name, noIdentityNote(cfg))
		}
		r, err := cfg.results.get(owner, ref.id)
		if err != nil {
			return nil, fmt.Errorf("parameter %q: %w", p.Name, err)
		}
		colName := ref.column
		if colName != "" && !p.takesColumn(colName) {
			return nil, errWrongIDColumn(cfg, spec, p, colName)
		}
		if colName == "" {
			colName = p.Name
			if _, has := r.column(colName); !has && len(p.Accepts) > 0 {
				colName = p.Accepts[0]
			}
		}
		col, ok := r.column(colName)
		if !ok {
			return nil, errWrongIDColumn(cfg, spec, p, "")
		}
		if r.Truncated && !allowPartial {
			return nil, fmt.Errorf("parameter %q: %s is truncated (%s), so its %s values are not the whole set; narrow the query that produced it, or pass %s=true to use the partial set knowingly",
				p.Name, r.ID, r.TruncNote, col, allowPartialParamName)
		}
		rows := r.Rows
		if ref.sel != nil {
			pos, err := ref.sel.positions(len(r.Rows))
			if err != nil {
				return nil, fmt.Errorf("parameter %q: %s %w", p.Name, r.ID, err)
			}
			rows = make([]map[string]any, len(pos))
			for k, i := range pos {
				rows[k] = r.Rows[i]
			}
		}
		values, err := columnValuesOf(r, rows, col)
		if err != nil {
			return nil, fmt.Errorf("parameter %q: %w", p.Name, err)
		}
		if len(values) == 0 {
			return nil, fmt.Errorf("parameter %q: %s.%s has no values, so there is nothing to look up; the answer over an empty set is empty", p.Name, r.ID, col)
		}
		ref.column = col
		args[p.Name] = strings.Join(values, ",")
		out = append(out, &expandedArg{param: p.Name, ref: ref, source: r, values: values})
	}
	chunked := 0
	for _, e := range out {
		if len(e.values) > batchChunk {
			chunked++
		}
	}
	if chunked > 1 {
		return nil, fmt.Errorf("more than one handle argument holds over %d values; narrow one of them first", batchChunk)
	}
	return out, nil
}

// filterToolCallName is the name a model calls the filter operator by.
func filterToolCallName(cfg *config) string {
	name := prefixedName(cfg, opFilter)
	if g := operatorsGroup(cfg); g != nil {
		return callNameOn(cfg, groupServerName(g), name)
	}
	return name
}

// errWrongIDColumn is the refusal for a batch parameter given a column that is
// not its own. It names the kind of id that was passed and, when another
// parameter of the same tool takes it, which one: a model told only where the
// right id comes from tends to resend the same wrong column, since it never
// learns the column was the wrong kind.
func errWrongIDColumn(cfg *config, spec *queryToolSpec, p queryToolParam, given string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "parameter %q is filled from the %s column of a result", p.Name, p.idColumn())
	if given = strings.TrimSpace(given); given != "" {
		fmt.Fprintf(&b, "; the %s column you passed is a different kind of id", given)
		var owners []string
		for _, q := range spec.Parameters {
			if q.batch && q.Name != p.Name && q.takesColumn(given) {
				owners = append(owners, q.Name)
			}
		}
		if len(owners) > 0 {
			fmt.Fprintf(&b, " and belongs to this tool's %s parameter", strings.Join(owners, " / "))
		} else {
			fmt.Fprintf(&b, " and is not used by this tool, so this tool may not be the right one for the question")
		}
	}
	return fmt.Errorf("%s. %s", b.String(), recoveryClause(cfg, spec, p))
}

// maxSourcesListed caps the tools a refusal names. An id that dozens of tools
// return (PersonID) would bury the instruction in names; idSources puts the
// tools an id starts from first, so the ones shown are the ones to start with.
const maxSourcesListed = 12

// rowPositionNote says how to take some rows of a handle by position. One
// text serves the server instructions, the describe tool and the prompts.
const rowPositionNote = "To take only some rows of a handle, add a Python-style position in square brackets after the column: qx4.PersonID[0] is the first row, qx4.PersonID[-1] the last, qx4.PersonID[0:5] the first five, qx4.PersonID[-3:] the last three, qx4.PersonID[::2] every second row. Every table's first column, Row, is each row's position, counted from 0 (the same number as in cut-value markers and show_field); a range may run past the end. Use positions for the first, last or top rows of a result already in the order wanted (sort it first with the sort tool); to pick a person or rows matching a condition, filter the handle instead."

// recoveryClause is the tail every id-parameter refusal ends on: every tool
// that returns the parameter's id, then the exact way to pass it on.
func recoveryClause(cfg *config, spec *queryToolSpec, p queryToolParam) string {
	var b strings.Builder
	if src := idSources(cfg, spec, p); len(src) > 0 {
		if len(src) > maxSourcesListed {
			fmt.Fprintf(&b, "Tools that return it include: %s, and %d more. Run one of them", strings.Join(src[:maxSourcesListed], ", "), len(src)-maxSourcesListed)
		} else {
			fmt.Fprintf(&b, "Tools that return it: %s. Run one of them", strings.Join(src, ", "))
		}
	} else {
		b.WriteString("Run a tool that returns one")
	}
	fmt.Fprintf(&b, ", then pass %s=<newhandle>.%s.", p.Name, p.idColumn())
	return b.String()
}

// outputAliasPattern finds the names a query gives its output columns.
var outputAliasPattern = regexp.MustCompile(`(?i)\bAS\s+\[?([A-Za-z_][A-Za-z0-9_]*)\]?`)

// returnsColumn reports whether a tool's results carry col: it is a declared
// column or a name the query selects, which includes ids stored under the
// handle but left out of the display.
func returnsColumn(s *queryToolSpec, col string) bool {
	for _, c := range s.Columns {
		if strings.EqualFold(c, col) {
			return true
		}
	}
	for _, m := range outputAliasPattern.FindAllStringSubmatch(s.Query, -1) {
		if strings.EqualFold(m[1], col) {
			return true
		}
	}
	return false
}

// idSources lists, by the name a model calls each by, every tool whose results
// carry a column the parameter reads, other than the tool being called. Tools
// that need no such id themselves come first: they are where the id starts.
func idSources(cfg *config, self *queryToolSpec, p queryToolParam) []string {
	cols := append([]string{p.Name}, p.Accepts...)
	var starts, rest []string
	for _, s := range cfg.queryTools {
		if s == self {
			continue
		}
		found := false
		for _, c := range cols {
			if returnsColumn(s, c) {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		takes := false
		for _, q := range s.Parameters {
			if q.batch && q.takesColumn(p.idColumn()) {
				takes = true
			}
		}
		if takes {
			rest = append(rest, queryToolCallName(cfg, s))
		} else {
			starts = append(starts, queryToolCallName(cfg, s))
		}
	}
	return append(starts, rest...)
}

// handleLikePattern finds a whole-word handle id inside text that is not a
// clean reference, so a refusal can name the reference that was probably
// meant. The word boundaries keep a course code such as AML2-AH from yielding
// a made-up "ml2" handle.
var handleLikePattern = regexp.MustCompile(`\b[A-Za-z]{1,2}[0-9]+\b`)

// errTypedIDs is the refusal for an id typed out where only a reference to a
// stored result is taken. It does not repeat the value received: a small model
// that sees its own mistake echoed tends to try it again. got is read only to
// find a handle id in it, which the example then spells correctly.
func errTypedIDs(cfg *config, spec *queryToolSpec, p queryToolParam, got string) error {
	var msg strings.Builder
	if id := handleLikePattern.FindString(got); id != "" {
		fmt.Fprintf(&msg, "parameter %q takes only a reference to a stored result, written as plain text like %s=%s.%s (no ** or quotes around it).",
			p.Name, p.Name, strings.ToLower(id), p.Name)
	} else {
		fmt.Fprintf(&msg, "parameter %q takes only a reference to a stored result, written as plain text like %s=<handle>.%s where <handle> is the handle a previous tool call printed.",
			p.Name, p.Name, p.Name)
	}
	msg.WriteString(" " + recoveryClause(cfg, spec, p))
	fmt.Fprintf(&msg, " To use just one row, first run %s on the handle you hold (Where=\"LastName='X' AND FirstName='Y'\"), then pass <newhandle>.%s. To take the first, last or top rows instead, add a position: <handle>.%s[0], <handle>.%s[-1], <handle>.%s[0:5].",
		prefixedName(cfg, opFilter), p.Name, p.Name, p.Name, p.Name)
	return errors.New(msg.String())
}

// literalIDs handles a batch parameter's value that is not a handle. An id
// list is refused outright; a literal list (school years, a typed-in lookup
// key) is only checked against copying a stored result.
func literalIDs(cfg *config, spec *queryToolSpec, p queryToolParam, owner string, identified, allowPartial bool, raw string) error {
	if p.batch && !p.Literal {
		if strings.TrimSpace(raw) == "" {
			return nil
		}
		return errTypedIDs(cfg, spec, p, raw)
	}
	return checkCopiedIDs(cfg, p, owner, identified, allowPartial, raw)
}

// checkCopiedIDs refuses a literal id list that is exactly the rows of a
// stored result the model was shown, whether that result was a partial
// sample of a larger set or came back complete: either way the model typed
// out ids it already holds under a handle instead of passing the handle on.
// The error names the handle to pass instead; AllowPartial=true says the
// list is meant literally.
func checkCopiedIDs(cfg *config, p queryToolParam, owner string, identified, allowPartial bool, raw string) error {
	if !p.batch || !identified || allowPartial || !cfg.handlesOn() {
		return nil
	}
	m := cfg.results.copiedFrom(owner, p.Name, strings.Split(raw, ","))
	if m == nil {
		return nil
	}
	if m.sample {
		return fmt.Errorf("parameter %q lists exactly the %d %s values of %s that were shown, but %s holds %d of them: the rows shown were only a sample. Pass %s=%s.%s to use all of them, or %s=true if you really mean only these %d",
			p.Name, m.given, m.column, m.result.ID, m.result.ID, m.distinct, p.Name, m.result.ID, m.column, allowPartialParamName, m.given)
	}
	return fmt.Errorf("parameter %q lists exactly the %d %s values that %s holds. Pass %s=%s.%s instead of typing ids out of a result you already hold, or %s=true if this exact list is coincidental and not meant as %s",
		p.Name, m.given, m.column, m.result.ID, p.Name, m.result.ID, m.column, allowPartialParamName, m.result.ID)
}

// batchParamNames lists a spec's batch parameters.
func batchParamNames(spec *queryToolSpec) []string {
	var names []string
	for _, p := range spec.Parameters {
		if p.batch {
			names = append(names, p.Name)
		}
	}
	return names
}

// boolArg reads an optional boolean synthetic argument.
func boolArg(args map[string]any, name string) (bool, error) {
	v, ok := args[name]
	if !ok || v == nil {
		return false, nil
	}
	conv, err := coerceParam(queryToolParam{Name: name, Type: "bool"}, v)
	if err != nil {
		return false, fmt.Errorf("parameter %q: %v", name, err)
	}
	b, _ := conv.(bool)
	return b, nil
}

// stringArg reads an optional string synthetic argument.
func stringArg(args map[string]any, name string) string {
	v, ok := args[name]
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(scalarString(v))
}

// --- capture ---

// captureQueryTool runs a csv/md tool's statement for the whole result, up to
// the store's row limit, instead of the one page the caller asked for. A
// tool that pages with PageSize/PageNumber is asked for page 1 at one row
// past the limit, which is how a result that stops exactly at the limit is
// told apart from one that was cut there. A long @handle list is run in
// chunks and merged.
func captureQueryTool(ctx context.Context, cfg *config, pool *connPool, spec *queryToolSpec, args map[string]any, expanded []*expandedArg) (*queryResult, error) {
	db, err := pool.get(spec.ConnectionString)
	if err != nil {
		return nil, fmt.Errorf("opening the connection for %q: %v", spec.Name, err)
	}
	limit := cfg.results.maxRows
	b := budget{rows: limit, bytes: cfg.results.userBytes, cellBytes: cfg.maxStoredCellBytes}

	// The chunked parameter, if any, and its chunks.
	chunkParam := ""
	chunks := [][]string{nil}
	for _, e := range expanded {
		if len(e.values) > batchChunk {
			chunkParam = e.param
			chunks = nil
			for i := 0; i < len(e.values); i += batchChunk {
				chunks = append(chunks, e.values[i:min(i+batchChunk, len(e.values))])
			}
		}
	}

	var merged *queryResult
	for _, chunk := range chunks {
		named, err := bindArgs(spec, args, func(p queryToolParam, v any) any {
			switch {
			case strings.EqualFold(p.Name, "PageSize"):
				return int64(limit + 1)
			case strings.EqualFold(p.Name, "PageNumber"):
				return int64(1)
			case chunkParam != "" && p.Name == chunkParam:
				return strings.Join(chunk, ",")
			}
			return v
		})
		if err != nil {
			return nil, err
		}
		qctx, cancel := context.WithTimeout(ctx, cfg.queryTimeout)
		res, err := runQuery(qctx, db, spec.Query, b, named...)
		if err != nil {
			err = describeQueryError(qctx, cfg, err)
			cancel()
			return nil, withQuery(err, spec.Query)
		}
		cancel()
		if merged == nil {
			merged = res
			continue
		}
		merged.Rows = append(merged.Rows, res.Rows...)
		merged.rowsCut = merged.rowsCut || res.rowsCut
		if merged.rowsCutNote == "" {
			merged.rowsCutNote = res.rowsCutNote
		}
		for _, n := range res.Notes {
			if !containsString(merged.Notes, n) {
				merged.Notes = append(merged.Notes, n)
			}
		}
	}
	if len(chunks) > 1 {
		// Each chunk came back in the tool's order; put the merged rows back
		// in order of the chunked id where the result carries it, so a batch
		// still reads one id's rows together.
		if col, ok := resultColumn(merged, chunkParam); ok {
			sortRowsBy(merged.Rows, col)
		}
		if len(merged.Rows) > limit {
			merged.Rows = merged.Rows[:limit]
			merged.rowsCut = true
			merged.rowsCutNote = fmt.Sprintf("result truncated at %d rows", limit)
		}
	}
	merged.RowCount = len(merged.Rows)
	return merged, nil
}

// describeQueryError distinguishes a timeout from a cancellation from a SQL
// error, as the query tools always have.
func describeQueryError(qctx context.Context, cfg *config, err error) error {
	switch {
	case errors.Is(qctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("query timed out after %s: %w", cfg.queryTimeout, err)
	case errors.Is(qctx.Err(), context.Canceled):
		return fmt.Errorf("query cancelled by the client: %w", err)
	default:
		return fmt.Errorf("query failed: %w", err)
	}
}

func resultColumn(res *queryResult, name string) (string, bool) {
	for _, c := range res.Columns {
		if strings.EqualFold(c, name) {
			return c, true
		}
	}
	return "", false
}

// sortRowsBy stable-sorts rows by one column, numerically when both values
// are numbers.
func sortRowsBy(rows []map[string]any, col string) {
	sort.SliceStable(rows, func(i, j int) bool {
		return lessValue(rows[i][col], rows[j][col])
	})
}

// lessValue orders two cell values: NULL first, numbers numerically, the rest
// as text.
func lessValue(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b != nil
	}
	as, bs := scalarString(a), scalarString(b)
	af, aerr := strconv.ParseFloat(as, 64)
	bf, berr := strconv.ParseFloat(bs, 64)
	if aerr == nil && berr == nil {
		return af < bf
	}
	return as < bs
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// newStoredResult turns a captured result into a stored one, not yet in the
// store.
func newStoredResult(cfg *config, res *queryResult, prov provenance, label string, lineage []string) *storedResult {
	r := &storedResult{
		Label:   label,
		Columns: res.Columns,
		Types:   res.types,
		Rows:    res.Rows,
		Prov:    prov,
		lineage: lineage,
	}
	for _, n := range res.Notes {
		if n != res.rowsCutNote {
			r.Notes = append(r.Notes, n)
		}
	}
	if res.rowsCut {
		r.Truncated = true
		r.TruncNote = fmt.Sprintf("stopped at the store's limit of %d rows or %s", cfg.results.maxRows, formatBytes(cfg.results.userBytes))
	}
	return r
}

// storeFor puts r in the store for the caller, if the call carries an
// identity, and returns why it has no handle when it does not get one: no
// identity, or a result too large to keep. Either way the reply can still
// show it.
func storeFor(cfg *config, owner string, identified bool, r *storedResult) string {
	if !identified {
		return noIdentityNote(cfg)
	}
	if _, err := cfg.results.put(owner, r); err != nil {
		return "no handle: " + err.Error()
	}
	return ""
}

// provenanceArgs is the part of a call's arguments that says what the
// result is: everything but paging and the display-only switches, which do
// not change what is stored.
func provenanceArgs(args map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range args {
		switch {
		case strings.EqualFold(k, "PageSize"), strings.EqualFold(k, "PageNumber"),
			k == columnsParamName, k == saveAsParamName, k == allowPartialParamName, k == requireSingleParamName:
			continue
		}
		out[k] = v
	}
	return out
}

// lineageOf is the lineage a result derived from parents carries: each
// parent's own lineage, then the parent itself, without repeats.
func lineageOf(parents ...*storedResult) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, p := range parents {
		for _, l := range p.lineage {
			add(l)
		}
		add(p.summary())
	}
	return out
}

// --- plausibility notes (G1) ---

// plausibilityNotes are the checks a result is put through before the model
// builds on it: nothing matched, ids that went in and matched nothing, and
// rows outside the scope the caller asked for.
func plausibilityNotes(spec *queryToolSpec, args map[string]any, expanded []*expandedArg, r *storedResult) []string {
	var notes []string
	if len(r.Rows) == 0 {
		notes = append(notes, "no rows matched. A wrong school year, building or id gives an empty result too, so check those before concluding there is nothing")
		return notes
	}
	for _, e := range expanded {
		col, ok := r.column(e.param)
		if !ok {
			continue
		}
		present := map[string]bool{}
		for _, row := range r.Rows {
			if v := row[col]; v != nil {
				present[strings.TrimSpace(scalarString(v))] = true
			}
		}
		missing := 0
		for _, v := range e.values {
			if !present[v] {
				missing++
			}
		}
		if missing > 0 {
			notes = append(notes, fmt.Sprintf("%d of the %d %s values from %s have no rows here", missing, len(e.values), e.param, e.ref.text()))
		}
	}
	for _, p := range spec.Parameters {
		if !(isIDColumn(p.Name) || strings.EqualFold(p.Name, "SchoolYear")) {
			continue
		}
		if expandedParam(expanded, p.Name) {
			continue
		}
		raw, ok := args[p.Name]
		if !ok || raw == nil {
			continue
		}
		col, ok := r.column(p.Name)
		if !ok {
			continue
		}
		want := map[string]bool{}
		for _, part := range strings.Split(scalarString(raw), ",") {
			if part = strings.TrimSpace(part); part != "" {
				want[strings.ToLower(part)] = true
			}
		}
		if len(want) == 0 {
			continue
		}
		outside := map[string]int{}
		for _, row := range r.Rows {
			v := row[col]
			if v == nil {
				continue
			}
			if s := strings.TrimSpace(scalarString(v)); !want[strings.ToLower(s)] {
				outside[s]++
			}
		}
		if len(outside) > 0 {
			keys := make([]string, 0, len(outside))
			for k := range outside {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("%s (%d)", k, outside[k]))
			}
			notes = append(notes, fmt.Sprintf("some rows have a %s other than the one asked for: %s", col, clip(strings.Join(parts, ", "), 200)))
		}
	}
	return notes
}

func expandedParam(expanded []*expandedArg, name string) bool {
	for _, e := range expanded {
		if strings.EqualFold(e.param, name) {
			return true
		}
	}
	return false
}

// --- replies ---

// replyOpts is how one reply over a stored result is laid out.
type replyOpts struct {
	// tool names the tool in the structured output.
	tool string
	// server is the {server} tool names in the text are written under.
	server string
	format outputFormat
	// cols is the display projection; empty shows every column.
	cols []string
	// pageSize/pageNumber pick the page shown; 0 means the default: the
	// whole result when it is small, a sample of it otherwise.
	pageSize, pageNumber int
	// browse is set by show, the one tool that pages a large result at the
	// size asked for; every other reply shows a large result as a sample.
	browse bool
	// profileOnly shows the profile and no rows.
	profileOnly bool
	// notes are extra notes (expansion checks, why there is no handle).
	notes []string
}

// storedReply renders a stored (or, with an empty ID, unstored) result: the
// handle line, the lineage of a derived result, the profile of a large one,
// the page shown, and the notes.
func storedReply(cfg *config, r *storedResult, o replyOpts) (*mcp.CallToolResult, error) {
	cols := o.cols
	if len(cols) == 0 {
		cols = r.visibleColumns()
	}
	total := len(r.Rows)

	start, end := 0, total
	switch {
	case o.profileOnly:
		end = 0
	case o.pageSize > 0:
		pn := max(o.pageNumber, 1)
		start = min((pn-1)*o.pageSize, total)
		end = min(start+o.pageSize, total)
	}
	if !o.browse && total > inlineMaxRows {
		end = min(end, start+sampleRows)
	}
	page := r.Rows[start:end]

	// A stored result keeps every value whole; the reply shows the first part
	// of a long one, and says where the rest is.
	var shortened int
	var shortenedCols []string
	if r.ID != "" && !o.profileOnly {
		page, shortened, shortenedCols = shortenCells(page, cols, cfg.displayCellChars, func(i int) int { return r.storedRowNumber(start + i) })
	}

	// The reply's own budget still holds: the page is what reaches the
	// model, so it is what the row and byte caps are counted against.
	b := budgetFor(cfg)
	kept := len(page)
	var displayNote string
	if b.rows > 0 && kept > b.rows {
		kept = b.rows
		displayNote = fmt.Sprintf("the reply is capped at %d rows", b.rows)
	}
	if b.bytes > 0 {
		used := 0
		for i := 0; i < kept; i++ {
			size := rowSize(reduceRow(page[i], cols))
			if i > 0 && used+size > b.bytes {
				kept = i
				displayNote = fmt.Sprintf("the reply is capped at %s of data", formatBytes(b.bytes))
				break
			}
			used += size
		}
	}
	page = page[:kept]
	end = start + kept

	partial := !o.profileOnly && (start > 0 || end < total)
	if r.ID != "" && kept > 0 && cfg.handlesOn() {
		cfg.results.markShown(r, start, end)
	}

	var pre strings.Builder
	show := callNameOn(cfg, o.server, showToolName(cfg))
	if r.ID != "" {
		fmt.Fprintf(&pre, "handle: %s", r.ID)
		if r.Label != "" {
			fmt.Fprintf(&pre, " %q", r.Label)
		}
		switch {
		case !partial:
			fmt.Fprintf(&pre, " · %d rows", total)
		case start == 0:
			fmt.Fprintf(&pre, " · SHOWING %d OF %d ROWS", kept, total)
		default:
			fmt.Fprintf(&pre, " · SHOWING ROWS %d-%d OF %d", start, end-1, total)
		}
		if r.Truncated {
			fmt.Fprintf(&pre, " (truncated: %s)", r.TruncNote)
		}
		if use := handleUsage(r); use != "" {
			fmt.Fprintf(&pre, " · %s", use)
		}
		if hidden := hiddenColumns(r.Columns, cols); len(hidden) > 0 {
			fmt.Fprintf(&pre, " · also stored: %s", strings.Join(hidden, ", "))
		}
		pre.WriteByte('\n')
		if len(r.Prov.Parents) > 0 {
			lines := r.lineage
			if len(lines) > 3 {
				lines = lines[len(lines)-3:]
			}
			fmt.Fprintf(&pre, "from: %s\n", strings.Join(append(append([]string{}, lines...), r.summary()), " → "))
		}
	}
	var prof *resultProfile
	if o.profileOnly || total > inlineMaxRows {
		prof = buildProfile(r)
		pre.WriteString(prof.text())
	}

	shown := withRowNumbers(&queryResult{Columns: cols, Rows: page, RowCount: len(page)}, func(i int) int { return r.storedRowNumber(start + i) })
	notes := append([]string{}, r.Notes...)
	notes = append(notes, o.notes...)
	if displayNote != "" {
		notes = append(notes, displayNote+"; the rest of the page is in the stored result")
	}
	if shortened > 0 {
		notes = append(notes, fmt.Sprintf("%d value(s) in %s are cut to %d characters. To read one whole, call %s with Handle=%s, Row=<the row in its marker>, Column=<its column>",
			shortened, strings.Join(shortenedCols, ", "), cfg.displayCellChars, callNameOn(cfg, o.server, showFieldToolName(cfg)), r.ID))
	}

	var table string
	if len(cols) == 0 && !o.profileOnly {
		// Every column is an id kept for @handle use: there is nothing to
		// show but the count, which the handle line already gives.
		notes = append(notes, "the rows hold only ids, kept for passing on as the handle line shows")
	} else if !o.profileOnly {
		if o.format == formatMD {
			// A partial table ends with the trailer below, not a row count
			// that reads like the size of the result.
			if partial {
				table = markdownRows(shown)
			} else {
				table = markdownTable(shown)
			}
		} else {
			var err error
			if table, err = csvText(shown); err != nil {
				return toolErrorf("encoding CSV: %v", err)
			}
		}
	}

	var text strings.Builder
	text.WriteString(pre.String())
	if pre.Len() > 0 && table != "" {
		text.WriteByte('\n')
	}
	text.WriteString(table)
	// The trailer sits right after the last row shown, which is what the
	// model reads last before it answers: a line above the table is easy to
	// read past, and the rows alone look like the whole result.
	if partial {
		text.WriteString(partialTrailer(r, start, end, total, o.browse, show))
	}
	for _, n := range notes {
		fmt.Fprintf(&text, "Note: %s\n", n)
	}

	format := "csv"
	if o.format == formatMD {
		format = "md"
	}
	out := &queryToolOutput{
		Tool:      o.tool,
		Format:    format,
		Columns:   shown.Columns,
		RowCount:  len(page),
		Truncated: r.Truncated || displayNote != "",
		Notes:     notes,
		Handle:    r.ID,
		Label:     r.Label,
		TotalRows: total,
		Partial:   partial,
		Profile:   prof,
	}
	if len(page) > 0 {
		out.FirstRow = &start
	}
	if o.format == formatMD {
		out.Markdown = table
	} else {
		out.CSV = table
	}
	return toolResult(text.String(), out)
}

// partialTrailer is the line after the last row of a partial reply: how much
// is missing, that the rows are not the answer, and where the whole result
// is.
func partialTrailer(r *storedResult, start, end, total int, browse bool, show string) string {
	shown, more := end-start, total-(end-start)
	var b strings.Builder
	switch {
	case shown == 0:
		fmt.Fprintf(&b, "… no rows on this page: the result has %d rows.", total)
	case start == 0 && !browse:
		fmt.Fprintf(&b, "… %d more rows not shown. The %d above are a sample: do not count, list or conclude from them.", more, shown)
	default:
		fmt.Fprintf(&b, "… rows %d-%d of %d shown; %d more not shown. Do not count, list or conclude from these rows alone.", start, end-1, total, more)
	}
	if r.ID == "" {
		b.WriteString(" The full result was not stored (see the note below).\n")
		return b.String()
	}
	fmt.Fprintf(&b, " The full result is %s: counts are in its profile", r.ID)
	if use := handleUsage(r); use != "" {
		fmt.Fprintf(&b, ", %s", use)
	}
	fmt.Fprintf(&b, ", or page it with %s (Handle=%s, Columns, PageSize, PageNumber).\n", show, r.ID)
	return b.String()
}

// handleUsage shows how to pass a result on, written out with its own id
// columns ("pass on as @qk3.SchCourseID"): a small model copies a worked
// example far more reliably than it applies the @handle.Column rule. At most
// three id columns are named; a result with none gets no example.
func handleUsage(r *storedResult) string {
	person := false
	for _, c := range r.Columns {
		if c == "PersonID" && isPassableColumn(c) {
			person = true
			break
		}
	}
	// A result of people is passed on by PersonID, listed first. The building,
	// level and grade ids are scope filters, not keys between two sets of
	// students, and advertising them led a model to join on them.
	cols := r.Columns
	if person {
		cols = []string{"PersonID"}
		for _, c := range r.Columns {
			if c != "PersonID" && !isScopeColumn(c) {
				cols = append(cols, c)
			}
		}
	}
	var refs []string
	for _, c := range cols {
		if isIDColumn(c) && isPassableColumn(c) {
			col := c
			if strings.ContainsAny(c, " .") {
				col = "[" + c + "]"
			}
			refs = append(refs, r.ID+"."+col)
			if len(refs) == 3 {
				break
			}
		}
	}
	if len(refs) == 0 {
		return ""
	}
	return "pass on as " + strings.Join(refs, " or ")
}

// isScopeColumn reports whether col is an id that scopes a query (a building,
// school level or grade) rather than identifies a person.
func isScopeColumn(col string) bool {
	switch col {
	case "BuildingSchoolLevelID", "BuildingID", "SchoolLevelID", "GradeID":
		return true
	}
	return false
}

var (
	passableMu   sync.RWMutex
	passableCols map[string]bool
)

// setPassableColumns records every column name some batch id parameter reads
// (its own name or an accepts entry), so a "pass on as" example never offers a
// column no tool would take, such as a course code.
func setPassableColumns(specs []*queryToolSpec) {
	set := map[string]bool{}
	for _, spec := range specs {
		for _, p := range spec.Parameters {
			if !p.batch || p.Literal {
				continue
			}
			set[strings.ToLower(p.Name)] = true
			for _, a := range p.Accepts {
				set[strings.ToLower(a)] = true
			}
		}
	}
	passableMu.Lock()
	passableCols = set
	passableMu.Unlock()
}

// isPassableColumn reports whether a tool reads col as @handle.col. Before any
// tools are registered every column counts.
func isPassableColumn(col string) bool {
	passableMu.RLock()
	defer passableMu.RUnlock()
	return passableCols == nil || passableCols[strings.ToLower(col)]
}

// hiddenColumns is the stored columns the display leaves out.
func hiddenColumns(all, shown []string) []string {
	var out []string
	for _, c := range all {
		found := false
		for _, s := range shown {
			if s == c {
				found = true
				break
			}
		}
		if !found {
			out = append(out, c)
		}
	}
	return out
}

// csvText renders res as CSV.
func csvText(res *queryResult) (string, error) {
	var b strings.Builder
	w := csv.NewWriter(&b)
	_ = w.Write(res.Columns)
	rec := make([]string, len(res.Columns))
	for _, row := range res.Rows {
		for i, name := range res.Columns {
			rec[i] = scalarString(row[name])
		}
		_ = w.Write(rec)
	}
	w.Flush()
	return b.String(), w.Error()
}

// --- instructions (G2) ---

// handleNotes is the initialize text on stored results for one server: what
// a handle is, how to pass one on, and which tools do the exact work. Empty
// when handles are off or the server carries none of their tools.
func handleNotes(cfg *config, build serverBuild) string {
	if !cfg.handlesOn() || !(build.show || build.operators) {
		return ""
	}
	// Where the operators are called from this server.
	where, place := build.server, "on this server"
	if !build.operators {
		if g := operatorsGroup(cfg); g != nil {
			where = groupServerName(g)
			place = fmt.Sprintf("on the %s server", where)
		} else {
			where = ""
		}
	}
	op := func(suffix string) string { return callNameOn(cfg, where, prefixedName(cfg, suffix)) }
	lists := build.builtins || len(build.specs) > 0

	var b strings.Builder
	b.WriteString("About stored results (handles):\n")
	if lists {
		b.WriteString("- Every list result is also stored whole under a short handle, named on its first line (handle: qx4). The rows still come back: read what you resolved - school years, buildings, courses, people - before building on it.\n")
		fmt.Fprintf(&b, "- A result over %d rows says SHOWING 5 OF N ROWS and shows only a few rows, with a profile (distinct ids, value breakdowns, date ranges). Those rows are a sample: never count, list or conclude from them. Counts come from the profile or the calc tools; the whole result is the handle. Check the profile before the next step: a wrong year, building or course shows up there.\n", inlineMaxRows)
		b.WriteString("- To pass a result's ids to another tool, write handle.Column as the value of a list parameter (PersonID=qx4.PersonID), as the result's \"pass on as\" example shows, instead of copying ids out of the rows - even when the result came back complete and you saw every row. A list that is exactly a stored result's ids, partial sample or complete, is refused. SaveAs gives a result a label; AllowPartial=true accepts a handle marked truncated, or a literal list of ids you really mean on its own.\n")
		b.WriteString("- " + rowPositionNote + "\n")
	}
	if build.show {
		fmt.Fprintf(&b, "- %s pages through a handle, sorted if you like, or re-reads its profile.\n", callNameOn(cfg, build.server, showToolName(cfg)))
		fmt.Fprintf(&b, "- A long value is shown cut, ending …[+N chars, row R]. %s (Handle, Row=R, Column) returns the rest of that one value; never guess or complete a cut value.\n", callNameOn(cfg, build.server, showFieldToolName(cfg)))
	}
	fmt.Fprintf(&b, "- Never count, compare, combine or compute over results yourself; the operators %s do it exactly: %s, %s, %s (set logic on a key column), %s, %s (conditions, counts per group, 'only X' checks), %s, %s, %s (joins, columns, top N) and %s (arithmetic, percentages, medians).\n",
		place, op(opUnion), op(opIntersect), op(opDifference), op(opFilter), op(opGroup), op(opJoin), op(opProject), op(opSort), op(opCalc))
	b.WriteString("- In the answer, name the school year, building and other entities the result covers.\n")
	b.WriteString("- Handles are yours alone, expire after a while unused, and do not survive a server restart; an unknown handle means run the query again.\n")
	return b.String()
}

// operatorsGroupInstructions is the initialize text for a group that carries
// only the operator tools.
func operatorsGroupInstructions(cfg *config, g *queryToolGroup, database string) string {
	subject := "the database"
	if database != "" {
		subject = database
	}
	var b strings.Builder
	fmt.Fprintf(&b, "This server works on results already fetched from %s by the sibling servers in your list. It never queries the database itself.\n", subject)
	if d := strings.TrimSpace(g.Description); d != "" {
		b.WriteString(d)
		if !strings.HasSuffix(d, ".") {
			b.WriteByte('.')
		}
		b.WriteByte('\n')
	}
	b.WriteString("Every list result on those servers is stored under a short handle (qx4). Pass handles here to intersect, subtract, filter, group, count, join or calculate over them exactly; each operator stores its own result under a new handle.\n")
	return b.String()
}

// --- the built-in query tool ---

// handleQueryFor is handleQuery with stored results: the whole result set,
// up to the store's limits, is kept under a handle, and the reply shows the
// first rows within the call's cap, as it always has.
func handleQueryFor(ctx context.Context, cfg *config, db *sql.DB, req *mcp.CallToolRequest, in queryInput) (*mcp.CallToolResult, *queryResult, error) {
	if !cfg.handlesOn() || req == nil {
		return handleQuery(ctx, cfg, db, in)
	}
	if isBlank(in.Query) {
		return nil, nil, errors.New("query is empty")
	}
	if cfg.readOnly {
		if err := checkReadOnly(in.Query); err != nil {
			return nil, nil, withQuery(err, in.Query)
		}
	}
	shown := budgetFor(cfg)
	if requested := int(in.MaxRows); requested > 0 && (shown.rows <= 0 || requested < shown.rows) {
		shown.rows = requested
	}
	capture := budget{rows: cfg.results.maxRows, bytes: cfg.results.userBytes, cellBytes: cfg.maxStoredCellBytes}

	qctx, cancel := context.WithTimeout(ctx, cfg.queryTimeout)
	defer cancel()
	res, err := runQuery(qctx, db, in.Query, capture)
	if err != nil {
		return nil, nil, withQuery(describeQueryError(qctx, cfg, err), in.Query)
	}
	if len(res.Columns) == 0 {
		text, err := resultText(res)
		if err != nil {
			return nil, nil, withQuery(fmt.Errorf("encoding result: %w", err), in.Query)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, res, nil
	}

	owner, identified := callerOf(cfg, req)
	name := toolName(cfg.toolPrefix, queryToolSuffix)
	r := newStoredResult(cfg, res, provenance{Tool: name, Args: map[string]any{"query": in.Query}}, "", nil)
	why := storeFor(cfg, owner, identified, r)

	// The reply: the first rows, within the call's cap, noted as before.
	out := &queryResult{
		Query:        in.Query,
		Columns:      res.Columns,
		RowsAffected: -1,
		Notes:        append([]string{}, r.Notes...),
		Handle:       r.ID,
		TotalRows:    len(r.Rows),
	}
	used := 0
	for i, row := range r.Rows {
		if shown.rows > 0 && i >= shown.rows {
			out.truncate(fmt.Sprintf("result truncated at %d rows; refine the query (for example with TOP or a WHERE clause) to see more", shown.rows))
			break
		}
		if shown.bytes > 0 {
			size := rowSize(row)
			if i > 0 && used+size > shown.bytes {
				out.truncate(fmt.Sprintf("result truncated at %s of data after %d row(s); the payload budget was reached before the row limit, so select fewer columns or narrow the rows to see more",
					formatBytes(used), i))
				break
			}
			used += size
		}
		out.Rows = append(out.Rows, row)
	}
	if out.Rows == nil {
		out.Rows = []map[string]any{}
	}
	out.RowCount = len(out.Rows)
	if r.Truncated {
		out.truncate("the stored result " + r.TruncNote)
	}
	if why != "" {
		out.Notes = append(out.Notes, why)
	}

	var pre strings.Builder
	if r.ID != "" {
		if out.RowCount < len(r.Rows) {
			fmt.Fprintf(&pre, "handle: %s · SHOWING %d OF %d ROWS", r.ID, out.RowCount, len(r.Rows))
			out.Notes = append(out.Notes, fmt.Sprintf("only the first %d of %d rows are shown: do not count, list or conclude from them alone; the full result is %s (page it with %s)",
				out.RowCount, len(r.Rows), r.ID, callNameOn(cfg, "", showToolName(cfg))))
		} else {
			fmt.Fprintf(&pre, "handle: %s · %d rows", r.ID, len(r.Rows))
		}
		if out.RowCount > 0 {
			cfg.results.markShown(r, 0, out.RowCount)
		}
		if use := handleUsage(r); use != "" {
			fmt.Fprintf(&pre, " · %s", use)
		}
		pre.WriteByte('\n')
	}
	if len(r.Rows) > inlineMaxRows {
		pre.WriteString(buildProfile(r).text())
	}
	text, err := resultText(withRowNumbers(out, ownPositions))
	if err != nil {
		return nil, nil, withQuery(fmt.Errorf("encoding result: %w", err), in.Query)
	}
	if pre.Len() > 0 {
		text = pre.String() + "\n" + text
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
}
