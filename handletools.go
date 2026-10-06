package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The tools that work on stored results rather than on the database: show,
// which pages a handle, and the operators, which combine handles into new
// ones. None of them reaches SQL Server; they read the caller's own stored
// results and nothing else.

// The operator tools' name suffixes. Like the built-ins, each is published as
// <prefix>_<suffix>.
const (
	opUnion      = "set_union"
	opIntersect  = "set_intersect"
	opDifference = "set_difference"
	opFilter     = "filter"
	opGroup      = "group_aggregate"
	opJoin       = "join"
	opProject    = "project"
	opSort       = "sort_limit"
	opCalc       = "calc"
	opCalcSQL    = "calc_sql"
)

// operatorSuffixes is every operator, in the order they are registered.
var operatorSuffixes = []string{opUnion, opIntersect, opDifference, opFilter, opGroup, opJoin, opProject, opSort, opCalc, opCalcSQL}

// handleToolNames is every tool name the stored-result tools can take on this
// server, so a --query-tools file cannot define one that shadows them.
func handleToolNames(cfg *config) map[string]bool {
	names := map[string]bool{showToolName(cfg): true, showFieldToolName(cfg): true}
	for _, s := range operatorSuffixes {
		names[prefixedName(cfg, s)] = true
	}
	return names
}

// --- arguments ---

// toolArgs is a call's decoded arguments.
type toolArgs map[string]any

// readArgs decodes a call's arguments and rejects any key not in known, for
// the same reason the query tools do: a misspelled key would otherwise read
// as "not given".
func readArgs(req *mcp.CallToolRequest, known ...string) (toolArgs, error) {
	args := toolArgs{}
	if raw := req.Params.Arguments; len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("could not read the tool arguments: %v", err)
		}
	}
	ok := map[string]bool{}
	for _, k := range known {
		ok[k] = true
	}
	var unknown []string
	for k := range args {
		if !ok[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("unrecognized parameter(s) %s; expected one of: %s", strings.Join(unknown, ", "), strings.Join(known, ", "))
	}
	return args, nil
}

func (a toolArgs) str(name string) string { return stringArg(a, name) }

func (a toolArgs) required(name string) (string, error) {
	s := a.str(name)
	if s == "" {
		return "", fmt.Errorf("missing required parameter %q", name)
	}
	return s, nil
}

func (a toolArgs) boolean(name string) (bool, error) { return boolArg(a, name) }

func (a toolArgs) integer(name string) (int, error) {
	v, ok := a[name]
	if !ok || v == nil {
		return 0, nil
	}
	conv, err := coerceParam(queryToolParam{Name: name, Type: "int"}, v)
	if err != nil {
		return 0, fmt.Errorf("parameter %q: %v", name, err)
	}
	n, _ := conv.(int64)
	if n < 0 {
		return 0, fmt.Errorf("parameter %q must not be negative", name)
	}
	return int(n), nil
}

// provArgs is the arguments recorded in a derived result's provenance.
func (a toolArgs) provArgs() map[string]any {
	out := map[string]any{}
	for k, v := range a {
		if k == saveAsParamName || k == allowPartialParamName {
			continue
		}
		out[k] = v
	}
	return out
}

// prop is one property of a tool's input schema.
type prop struct {
	name, typ, desc string
	required        bool
}

func objectSchema(props ...prop) *jsonschema.Schema {
	s := &jsonschema.Schema{
		Type:                 "object",
		Properties:           map[string]*jsonschema.Schema{},
		AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
	}
	for _, p := range props {
		s.Properties[p.name] = &jsonschema.Schema{Type: p.typ, Description: p.desc}
		if p.required {
			s.Required = append(s.Required, p.name)
		}
	}
	return s
}

var (
	propSaveAs       = prop{name: saveAsParamName, typ: "string", desc: "Optional label for the new handle."}
	propAllowPartial = prop{name: allowPartialParamName, typ: "boolean", desc: "True to accept a truncated input handle."}
)

// --- getting the caller's handles ---

// callerHandles resolves handle names for the calling user, refusing a
// truncated one unless allowPartial.
func callerHandles(cfg *config, req *mcp.CallToolRequest, allowPartial bool, ids ...string) ([]*storedResult, error) {
	owner, ok := callerOf(cfg, req)
	if !ok {
		return nil, fmt.Errorf("%s", noIdentityNote(cfg))
	}
	out := make([]*storedResult, 0, len(ids))
	for _, id := range ids {
		if ref, _, ok := parseHandleRef(id); ok {
			if ref.sel != nil {
				return nil, fmt.Errorf("this argument takes a whole handle, written %s; row positions go on an id parameter of a query tool, or use the filter or sort tool here", ref.id)
			}
			id = ref.id
		}
		r, err := cfg.results.get(owner, id)
		if err != nil {
			return nil, err
		}
		if r.Truncated && !allowPartial {
			return nil, fmt.Errorf("%s is truncated (%s), so it is not the whole set and an exact answer over it would be wrong; narrow the query that produced it, or pass %s=true to work on the partial set knowingly",
				r.ID, r.TruncNote, allowPartialParamName)
		}
		out = append(out, r)
	}
	return out, nil
}

// splitList splits a comma-separated argument into its trimmed, non-empty
// parts.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// --- show ---

// registerShowTool adds the show tool to a server. server is the {server}
// this server's tools are called under, for the text of its replies.
func registerShowTool(mcpServer *mcp.Server, cfg *config, server string) {
	name := showToolName(cfg)
	mcpServer.AddTool(&mcp.Tool{
		Name:  name,
		Title: "Show a stored result",
		Description: "Page through a stored result by its handle (the qx4 on a result's first line), or re-read its profile. " +
			"Use it to see more of a large result than the first reply showed, sorted if you like, without running the query again.",
		InputSchema: objectSchema(
			prop{name: "Handle", typ: "string", desc: "The handle, as shown on the result (qx4).", required: true},
			prop{name: columnsParamName, typ: "string", desc: "Columns to return, comma-separated, as named in the result's profile or its 'also stored' list. Ask only for what you need: fewer columns, less data.", required: true},
			prop{name: "PageSize", typ: "integer", desc: "Rows per page; default: all of a small result, 20 of a large one."},
			prop{name: "PageNumber", typ: "integer", desc: "1-based page number."},
			prop{name: "OrderBy", typ: "string", desc: "Sort before paging: Column [DESC], comma-separated."},
			prop{name: "ProfileOnly", typ: "boolean", desc: "True for the profile and lineage only, no rows."},
		),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: boolPtr(false)},
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return handleShow(cfg, req, server, name)
	})
}

func handleShow(cfg *config, req *mcp.CallToolRequest, server, name string) (*mcp.CallToolResult, error) {
	args, err := readArgs(req, "Handle", columnsParamName, "PageSize", "PageNumber", "OrderBy", "ProfileOnly")
	if err != nil {
		return toolErrorf("%v", err)
	}
	id, err := args.required("Handle")
	if err != nil {
		return toolErrorf("%v", err)
	}
	rs, err := callerHandles(cfg, req, true, id)
	if err != nil {
		return toolErrorf("%v", err)
	}
	r := rs[0]
	var cols, idNotes []string
	profileOnly, err := args.boolean("ProfileOnly")
	if err != nil {
		return toolErrorf("%v", err)
	}
	// Columns is required, but a profile-only call shows no rows, so it has
	// nothing to project.
	if src := args.str(columnsParamName); src != "" {
		items, err := compileColumnList(src, columnsParamName, r.visibleColumns(), false)
		if err != nil {
			return toolErrorf("%v", err)
		}
		for _, it := range items {
			cols = append(cols, it.name)
		}
	} else if !profileOnly {
		return toolErrorf("%s is required: name the columns you need, comma-separated, from: %s", columnsParamName, strings.Join(r.visibleColumns(), ", "))
	}
	pageSize, err := args.integer("PageSize")
	if err != nil {
		return toolErrorf("%v", err)
	}
	pageNumber, err := args.integer("PageNumber")
	if err != nil {
		return toolErrorf("%v", err)
	}
	// show pages at the size asked for; with none, a large result comes in
	// pages of twenty.
	if pageSize == 0 && len(r.Rows) > inlineMaxRows {
		pageSize = 20
	}

	view := r
	if ob := args.str("OrderBy"); ob != "" {
		order, err := compileOrderList(ob, "OrderBy", r.Columns)
		if err != nil {
			return toolErrorf("%v", err)
		}
		sorted := *r
		sorted.Rows = append([]map[string]any(nil), r.Rows...)
		// Rows are numbered by their place in the stored result, not in this
		// order, because that is the number show_field takes.
		sorted.origRow = make([]int, len(sorted.Rows))
		for i := range sorted.origRow {
			sorted.origRow[i] = i
		}
		sort.Stable(&rowOrder{rows: sorted.Rows, orig: sorted.origRow, less: func(x, y map[string]any) bool {
			for _, o := range order {
				a, b := x[o.col], y[o.col]
				if lessValue(a, b) {
					return !o.desc
				}
				if lessValue(b, a) {
					return o.desc
				}
			}
			return false
		}})
		view = &sorted
	}
	notes := idNotes
	if profileOnly && len(r.lineage) > 0 {
		notes = append(notes, "lineage: "+strings.Join(append(append([]string{}, r.lineage...), r.summary()), " → "))
	}
	return storedReply(cfg, view, replyOpts{
		tool:        name,
		server:      server,
		format:      formatCSV,
		cols:        cols,
		pageSize:    pageSize,
		pageNumber:  pageNumber,
		browse:      true,
		profileOnly: profileOnly,
		notes:       notes,
	})
}

// --- operators ---

// opSpec is one operator tool's registration.
type opSpec struct {
	suffix, title, desc string
	props               []prop
	// aliases are argument names accepted but not advertised.
	aliases []string
	run     func(ctx context.Context, cfg *config, req *mcp.CallToolRequest, args toolArgs) (*opOutput, error)
}

// opOutput is what an operator produced: a result set to store, or (calc) a
// finished reply.
type opOutput struct {
	rows   *calcRows
	inputs []*storedResult
	// typeSources, when set, is where the output columns look up their types
	// before the inputs: project's renamed columns.
	typeSources []*storedResult
	notes       []string
	scalar      *mcp.CallToolResult
	// keepDisplay carries the single input's display columns to the output
	// (filter, sort_limit: same columns, fewer or reordered rows).
	keepDisplay bool
}

// registerOperatorTools adds the operator tools to a server.
func registerOperatorTools(mcpServer *mcp.Server, cfg *config, server string) {
	for _, op := range operatorSpecs(cfg, server) {
		op := op
		name := prefixedName(cfg, op.suffix)
		props := append([]prop{}, op.props...)
		known := make([]string, 0, len(props)+len(op.aliases))
		for _, p := range props {
			known = append(known, p.name)
		}
		known = append(known, op.aliases...)
		mcpServer.AddTool(&mcp.Tool{
			Name:        name,
			Title:       op.title,
			Description: op.desc,
			InputSchema: objectSchema(props...),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: boolPtr(false)},
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args, err := readArgs(req, known...)
			if err != nil {
				return toolErrorf("%v", err)
			}
			out, err := op.run(ctx, cfg, req, args)
			if err != nil {
				return toolErrorf("%v", err)
			}
			if out.scalar != nil {
				return out.scalar, nil
			}
			return storeOperatorResult(cfg, req, server, name, args, out)
		})
	}
}

// storeOperatorResult stores an operator's output under a new handle and
// replies with it.
func storeOperatorResult(cfg *config, req *mcp.CallToolRequest, server, name string, args toolArgs, out *opOutput) (*mcp.CallToolResult, error) {
	owner, identified := callerOf(cfg, req)
	// The same operator call again (same tool, arguments and input handles) has
	// the same rows: hand back the handle already held, and say so, rather than
	// a copy under a new handle.
	if label := cleanLabel(args.str(saveAsParamName)); identified && label == "" {
		prov := provenance{Tool: name, Args: args.provArgs()}
		for _, in := range out.inputs {
			prov.Parents = append(prov.Parents, in.ID)
		}
		if same := cfg.results.findSame(owner, prov); same != nil {
			notes := []string{fmt.Sprintf("this is the same call as %s, so that handle is reused: no need to run it again", same.ID)}
			if len(same.Rows) == 0 {
				notes = append(notes, "it returned 0 rows both times; change the condition instead of repeating it")
			}
			return storedReply(cfg, same, replyOpts{tool: name, server: server, format: formatCSV, cols: same.display, notes: notes})
		}
	}
	r := &storedResult{
		Label:   cleanLabel(args.str(saveAsParamName)),
		Columns: out.rows.cols,
		Types:   resultTypes(out.rows, append(append([]*storedResult{}, out.typeSources...), out.inputs...)...),
		Rows:    out.rows.rows,
		Notes:   out.notes,
		Prov:    provenance{Tool: name, Args: args.provArgs()},
		lineage: lineageOf(out.inputs...),
	}
	r.inheritHidden(out.inputs...)
	restoreBits(r)
	if out.keepDisplay && len(out.inputs) == 1 {
		r.display = out.inputs[0].display
	}
	for _, in := range out.inputs {
		r.Prov.Parents = append(r.Prov.Parents, in.ID)
		if in.Truncated {
			r.Truncated = true
			r.TruncNote = "computed from truncated input " + in.ID
		}
	}
	if out.rows.cut {
		r.Truncated = true
		r.TruncNote = fmt.Sprintf("stopped at the store's limit of %d rows", cfg.results.maxRows)
	}
	var notes []string
	if _, err := cfg.results.put(owner, r); err != nil {
		r.ID = ""
		notes = append(notes, "no handle: "+err.Error())
	}
	return storedReply(cfg, r, replyOpts{tool: name, server: server, format: formatCSV, cols: r.display, notes: notes})
}

// restoreBits turns the 0/1 SQLite hands back for a BIT column into the
// true/false the column showed before the operator, so a filtered result reads
// like the result it came from.
func restoreBits(r *storedResult) {
	for i, c := range r.Columns {
		if i >= len(r.Types) || r.Types[i] != "BIT" {
			continue
		}
		for _, row := range r.Rows {
			if n, ok := row[c].(int64); ok && (n == 0 || n == 1) {
				row[c] = n == 1
			}
		}
	}
}

// withTables runs fn with the engine locked and every input materialized.
func withTables(ctx context.Context, cfg *config, inputs []*storedResult, fn func(tables []string) error) error {
	e := cfg.calc
	if err := e.open(); err != nil {
		return fmt.Errorf("starting the operator engine: %w", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	tables := make([]string, len(inputs))
	for i, in := range inputs {
		t, err := e.tableLocked(ctx, in)
		if err != nil {
			return err
		}
		tables[i] = t
	}
	return fn(tables)
}

// affinity groups SQL types by how SQLite will compare them.
func affinity(t string) string {
	switch sqliteDecl(t) {
	case " INTEGER", " REAL":
		return "number"
	case "":
		return ""
	default:
		return "text"
	}
}

func operatorSpecs(cfg *config, server string) []opSpec {
	show := callNameOn(cfg, server, showToolName(cfg))
	specs := []opSpec{
		setOperator(cfg, opUnion, "Union of stored results",
			"Rows whose Key appears in any of the handles, each once. Example: Handle=qx3,qx5 Key=PersonID for everyone in either set.", "UNION"),
		setOperator(cfg, opIntersect, "Intersection of stored results",
			"Rows whose Key appears in every handle. Example: Handle=qx3,qx5 Key=PersonID for students on both lists; the first handle's full rows come back (Carry=false for the key alone).", "INTERSECT"),
		setOperator(cfg, opDifference, "Difference of stored results",
			"Rows whose Key is in the first handle and in none of the others. Example: Handle=qx3,qx5 Key=PersonID for students in qx3 but not qx5; the first handle's full rows come back (Carry=false for the key alone).", "EXCEPT"),
		{
			suffix: opFilter, title: "Filter a stored result",
			desc: "Keep the rows of a stored result that match a condition, exactly. Where uses column names with =, <>, <, <=, >, >=, IN (...), LIKE '%x%', BETWEEN a AND b, IS [NOT] NULL, AND, OR, NOT. " +
				"A date column compares with yyyy-mm-dd or an offset from today (today, -7d, -2w, -3m, -1y), counted by the server. " +
				"Examples: Handle=qx4 Where=\"Grade IN ('09','10') AND Average < 65\"; Where=\"Birthdate <= '-15y'\" for everyone aged 15 or over.",
			props: []prop{
				{name: "Handle", typ: "string", desc: "The handle to filter.", required: true},
				{name: "Where", typ: "string", desc: "The condition.", required: true},
				propSaveAs, propAllowPartial,
			},
			run: runFilter,
		},
		{
			suffix: opGroup, title: "Group and count a stored result",
			desc: "Group a stored result's rows and count, sum or average them exactly - the way to answer how many, how much, or per-X questions. " +
				"Aggregates: count(), count(Col), count_distinct(Col), sum(Col), avg(Col), min(Col), max(Col), count_if(condition), sum_if(Col, condition), each optionally AS name. " +
				"Group by a readable column (Building, Grade), not an id column: id columns are hidden, so the output would show only the aggregates, unlabeled. count() with no By gives one total row. To compare per-building or per-grade numbers, prefer a rollup tool that already returns them. " +
				"sum_if takes the column first, then the condition; to count rows that match a condition use count_if(condition) alone. Example (score bands from a grades handle): Handle=qx8 Aggregates=\"count() AS total, count_if(NumericScore >= 90) AS A, count_if(NumericScore >= 80 AND NumericScore < 90) AS B, count_if(NumericScore < 65) AS Below65\". " +
				"Example (students scheduled into Homeroom and nothing else): Handle=qx6 By=PersonID Aggregates=\"count() AS courses, count_if(CourseName LIKE '%Homeroom%') AS homerooms\" Having=\"courses = homerooms\".",
			props: []prop{
				{name: "Handle", typ: "string", desc: "The handle to group.", required: true},
				{name: "By", typ: "string", desc: "Columns to group by, comma-separated; omit for one row of totals."},
				{name: "Aggregates", typ: "string", desc: "The aggregates, comma-separated.", required: true},
				{name: "Having", typ: "string", desc: "Optional condition on the output columns, same syntax as filter."},
				propSaveAs, propAllowPartial,
			},
			run: runGroup,
		},
		{
			suffix: opJoin, title: "Join two stored results",
			desc: "Combine two stored results on matching columns. Type is inner (default), left, semi (left rows that have a match) or anti (left rows with no match). " +
				"Parameters are Left and Right (two separate handles); an inner join may instead take both as Handle=\"a,b\". On is a column both sides have (PersonID), or LeftCol = RightCol; the two columns must hold the same kind of value (id to id, name to name). Example: Left=qx4 Right=qx7 On=PersonID Type=anti.",
			props: []prop{
				{name: "Left", typ: "string", desc: "The left handle. Give Left and Right, or Handle."},
				{name: "Right", typ: "string", desc: "The right handle."},
				{name: "Handle", typ: "string", desc: "Inner joins only: the two handles comma-separated (\"xj3,xj6\"), in place of Left and Right. Other types need Left and Right, because which side is which changes the result."},
				{name: "On", typ: "string", desc: "Join columns, comma-separated.", required: true},
				{name: "Type", typ: "string", desc: "inner, left, semi or anti."},
				propSaveAs, propAllowPartial,
			},
			run: runJoin,
		},
		{
			suffix: opProject, title: "Choose columns of a stored result",
			desc: "Keep, reorder, rename (Col AS Name) and optionally de-duplicate existing columns of a stored result. Column names only: no expressions, no concatenation (||), no Name = expr, no functions - it cannot build new values or divide one column by another. Rates per building or grade come from a rollup tool that returns them. " +
				"A new name (after AS) must be one word of letters, digits and underscores, with no hyphen, space or slash: DaysAbsent AS Absences_25_26, never Absences25-26. " +
				"Example: Handle=qx4 Columns=\"PersonID, LastName AS Surname\" Distinct=true.",
			props: []prop{
				{name: "Handle", typ: "string", desc: "The handle.", required: true},
				{name: columnsParamName, typ: "string", desc: "Existing column names to keep, comma-separated, each optionally AS a new name (letters, digits, underscore only: Absences_25_26, not Absences25-26). Not expressions.", required: true},
				{name: "Distinct", typ: "boolean", desc: "True to keep each distinct row once."},
				propSaveAs, propAllowPartial,
			},
			run: runProject,
		},
		{
			suffix: opSort, title: "Sort and cut a stored result",
			desc: "Sort a stored result and keep the first Limit rows - top-N questions. Example: Handle=qx4 OrderBy=\"Average DESC\" Limit=10.",
			props: []prop{
				{name: "Handle", typ: "string", desc: "The handle.", required: true},
				{name: "OrderBy", typ: "string", desc: "Column [DESC], comma-separated.", required: true},
				{name: "Limit", typ: "integer", desc: "Rows to keep; all when omitted."},
				propSaveAs, propAllowPartial,
			},
			run: runSort,
		},
		{
			suffix: opCalc, title: "Calculate",
			desc: "Exact arithmetic and statistics - use it instead of working numbers out yourself. " +
				"Numbers, + - * / ( ), round(x, n), abs, min, max, percent(part, whole), ratio(a, b), days_between('2025-09-01', '2026-06-20'), and stored-result statistics " +
				"@qx4.row_count, @qx4.count(Col), @qx4.count_distinct(Col), @qx4.sum(Col), @qx4.avg(Col), @qx4.min(Col), @qx4.max(Col), @qx4.median(Col), @qx4.stdev(Col), @qx4.percentile(Col, 90). " +
				"Several expressions separated by ; are evaluated together. Example: Expression=\"percent(@qx5.row_count, @qx2.row_count)\".",
			props: []prop{
				{name: "Expression", typ: "string", desc: "The expression(s).", required: true},
				propAllowPartial,
			},
			run: runCalc,
		},
	}
	if cfg.calcSQL {
		specs = append(specs, opSpec{
			suffix: opCalcSQL, title: "SQL over stored results",
			desc: "Run one read-only SQLite SELECT over stored results, each referenced as @handle (SELECT Grade, COUNT(*) FROM @qx4 GROUP BY Grade). " +
				"Only when the other operators cannot express the question; the result is a new handle. Page it with " + show + ".",
			props: []prop{
				{name: "Query", typ: "string", desc: "One SELECT or WITH statement.", required: true},
				propSaveAs, propAllowPartial,
			},
			run: runCalcSQL,
		})
	}
	return specs
}

// setOperator builds one of the three set operators.
func setOperator(cfg *config, suffix, title, desc, compound string) opSpec {
	props := []prop{
		{name: "Handle", typ: "string", desc: "Two or more handles, comma-separated (qx3,qx5); for a difference, the first minus the rest.", required: true},
		{name: "Key", typ: "string", desc: "Column(s) compared, comma-separated, present in every handle.", required: true},
	}
	if compound != "UNION" {
		props = append(props, prop{name: "Carry", typ: "boolean", desc: "Defaults to true: return the first handle's full rows (names, hidden ids) for the matches. False returns the key column alone."})
	}
	props = append(props, propSaveAs, propAllowPartial)
	return opSpec{
		suffix: suffix, title: title, desc: desc, props: props,
		// The parameter was called Handles until every operator took Handle;
		// the old name still works for a call written against it.
		aliases: []string{"Handles"},
		run: func(ctx context.Context, cfg *config, req *mcp.CallToolRequest, args toolArgs) (*opOutput, error) {
			return runSetOp(ctx, cfg, req, args, compound)
		},
	}
}

func runSetOp(ctx context.Context, cfg *config, req *mcp.CallToolRequest, args toolArgs, compound string) (*opOutput, error) {
	given := args.str("Handle")
	if given == "" {
		given = args.str("Handles")
	}
	ids := splitList(given)
	if len(ids) < 2 {
		got := "none"
		if len(ids) == 1 {
			got = "only " + ids[0]
		}
		return nil, fmt.Errorf("Handle needs two or more handles, comma-separated, and got %s; pass Handle=qx3,qx5", got)
	}
	keySpec := splitList(args.str("Key"))
	if len(keySpec) == 0 {
		return nil, fmt.Errorf("missing required parameter %q", "Key")
	}
	allowPartial, err := args.boolean(allowPartialParamName)
	if err != nil {
		return nil, err
	}
	// Carry defaults to on for an intersection or difference: a key-only
	// result loses the names and the hidden ids, and cannot be used further.
	carry := compound != "UNION"
	if v, given := args["Carry"]; given && v != nil {
		if carry, err = args.boolean("Carry"); err != nil {
			return nil, err
		}
	}
	inputs, err := callerHandles(cfg, req, allowPartial, ids...)
	if err != nil {
		return nil, err
	}
	// Resolve the key in every input, and compare as text when the inputs
	// disagree about a key's type (an id stored as a number in one result
	// and as text in another).
	keys := make([][]string, len(inputs))
	for i, in := range inputs {
		for _, k := range keySpec {
			c, ok := in.column(k)
			if !ok {
				return nil, fmt.Errorf("%s has no column %q (columns: %s)", in.ID, k, strings.Join(in.Columns, ", "))
			}
			keys[i] = append(keys[i], c)
		}
	}
	castText := make([]bool, len(keySpec))
	for k := range keySpec {
		first := affinity(inputs[0].typeOf(keys[0][k]))
		for i := range inputs {
			if affinity(inputs[i].typeOf(keys[i][k])) != first {
				castText[k] = true
			}
		}
	}
	keyExpr := func(alias string, i, k int) string {
		e := quoteIdent(keys[i][k])
		if alias != "" {
			e = alias + "." + e
		}
		if castText[k] {
			e = "CAST(" + e + " AS TEXT)"
		}
		return e
	}

	var out *opOutput
	err = withTables(ctx, cfg, inputs, func(tables []string) error {
		var parts []string
		nulls := 0
		for i, t := range tables {
			sel := make([]string, len(keySpec))
			notNull := make([]string, len(keySpec))
			for k := range keySpec {
				sel[k] = keyExpr("", i, k) + " AS " + quoteIdent(keys[0][k])
				notNull[k] = quoteIdent(keys[i][k]) + " IS NOT NULL"
			}
			parts = append(parts, "SELECT "+strings.Join(sel, ", ")+" FROM "+quoteIdent(t)+" WHERE "+strings.Join(notNull, " AND "))
			var n int
			isNull := make([]string, len(keySpec))
			for k := range keySpec {
				isNull[k] = quoteIdent(keys[i][k]) + " IS NULL"
			}
			if err := cfg.calc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdent(t)+" WHERE "+strings.Join(isNull, " OR ")).Scan(&n); err != nil {
				return err
			}
			nulls += n
		}
		body := strings.Join(parts, " "+compound+" ")
		var q string
		if carry && compound != "UNION" {
			lhs := make([]string, len(keySpec))
			for k := range keySpec {
				lhs[k] = keyExpr("t", 0, k)
			}
			tuple := lhs[0]
			if len(lhs) > 1 {
				tuple = "(" + strings.Join(lhs, ", ") + ")"
			}
			q = "SELECT t.* FROM " + quoteIdent(tables[0]) + " AS t WHERE " + tuple + " IN (" + body + ") ORDER BY t.rowid"
		} else {
			order := make([]string, len(keySpec))
			for k := range keySpec {
				order[k] = fmt.Sprint(k + 1)
			}
			q = "SELECT * FROM (" + body + ") ORDER BY " + strings.Join(order, ", ")
		}
		rows, err := queryRows(ctx, cfg.calc.db, cfg.results.maxRows, q)
		if err != nil {
			return err
		}
		out = &opOutput{rows: rows, inputs: inputs}
		keyList := strings.Join(keySpec, "+")
		if carry && compound != "UNION" {
			out.notes = append(out.notes, fmt.Sprintf("each row is a row of %s (its own columns, so a %s can repeat); distinct %s counts are in the profile", inputs[0].ID, keyList, keyList))
		} else {
			out.notes = append(out.notes, fmt.Sprintf("each row is one distinct %s, not a record: the row count is a count of %s values", keyList, keyList))
		}
		if nulls > 0 {
			out.notes = append(out.notes, fmt.Sprintf("%d input row(s) with a NULL %s were left out", nulls, strings.Join(keySpec, "/")))
		}
		if slicesContain(castText, true) {
			out.notes = append(out.notes, "the inputs store the key with different types, so it was compared as text")
		}
		return nil
	})
	return out, err
}

func slicesContain(bs []bool, want bool) bool {
	for _, b := range bs {
		if b == want {
			return true
		}
	}
	return false
}

// singleInput resolves the Handle argument.
func singleInput(cfg *config, req *mcp.CallToolRequest, args toolArgs, param string) (*storedResult, error) {
	id, err := args.required(param)
	if err != nil {
		return nil, err
	}
	allowPartial, err := args.boolean(allowPartialParamName)
	if err != nil {
		return nil, err
	}
	rs, err := callerHandles(cfg, req, allowPartial, id)
	if err != nil {
		return nil, err
	}
	return rs[0], nil
}

func runFilter(ctx context.Context, cfg *config, req *mcp.CallToolRequest, args toolArgs) (*opOutput, error) {
	in, err := singleInput(cfg, req, args, "Handle")
	if err != nil {
		return nil, err
	}
	where, err := args.required("Where")
	if err != nil {
		return nil, err
	}
	cond, params, err := compilePredicate(where, "Where", in.Columns, in.typeMap())
	if err != nil {
		return nil, err
	}
	var out *opOutput
	err = withTables(ctx, cfg, []*storedResult{in}, func(tables []string) error {
		rows, err := queryRows(ctx, cfg.calc.db, cfg.results.maxRows,
			"SELECT * FROM "+quoteIdent(tables[0])+" WHERE "+cond+" ORDER BY rowid", params...)
		out = &opOutput{rows: rows, inputs: []*storedResult{in}, keepDisplay: true}
		if err == nil && len(rows.rows) == 0 {
			out.notes = append(out.notes, zeroRowFilterHint(in, where)...)
		}
		return err
	})
	return out, err
}

var quotedLiteralPattern = regexp.MustCompile(`'([^']*)'`)

// zeroRowFilterHint explains a filter that kept nothing, so a model reads the
// cause rather than trying the same condition on a new handle: where each text
// the condition looks for does occur in the input, and what the filtered
// columns hold.
func zeroRowFilterHint(in *storedResult, where string) []string {
	if len(in.Rows) == 0 {
		return []string{fmt.Sprintf("0 rows: %s itself has 0 rows, so there is nothing to filter; find out why that result is empty", in.ID)}
	}
	cols := in.visibleColumns()
	var notes []string
	for _, m := range quotedLiteralPattern.FindAllStringSubmatch(where, -1) {
		lit := strings.ToLower(strings.Trim(m[1], "%"))
		if lit == "" {
			continue
		}
		var found []string
		for _, c := range cols {
			if wordIn(where, c) {
				continue // the condition already looks here
			}
			for _, row := range in.Rows {
				v := strings.TrimSpace(fmt.Sprint(row[c]))
				if v != "" && strings.Contains(strings.ToLower(v), lit) {
					found = append(found, fmt.Sprintf("%s (for example %q)", c, v))
					break
				}
			}
		}
		if len(found) > 0 {
			if len(found) > 3 {
				found = found[:3]
			}
			notes = append(notes, fmt.Sprintf("0 rows: %q does not match in the column(s) the condition names, but it does occur in %s. Filter that column instead.", m[1], strings.Join(found, "; ")))
		}
	}
	if len(notes) == 0 {
		var named []string
		for _, c := range cols {
			if !wordIn(where, c) {
				continue
			}
			var vals []string
			seen := map[string]bool{}
			for _, row := range in.Rows {
				v := strings.TrimSpace(fmt.Sprint(row[c]))
				if v == "" || seen[v] {
					continue
				}
				seen[v] = true
				vals = append(vals, fmt.Sprintf("%q", v))
				if len(vals) == 5 {
					break
				}
			}
			named = append(named, fmt.Sprintf("%s holds values such as %s", c, strings.Join(vals, ", ")))
		}
		if len(named) > 0 {
			notes = append(notes, fmt.Sprintf("0 rows from %d: %s. Check the condition against these values; do not repeat it.", len(in.Rows), strings.Join(named, "; ")))
		} else {
			notes = append(notes, fmt.Sprintf("0 rows from %d: the condition matched nothing. Check the column names and values in %s; do not repeat it.", len(in.Rows), in.ID))
		}
	}
	return notes
}

// wordIn reports whether name appears in text as a whole word, ignoring case.
func wordIn(text, name string) bool {
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(name) + `\b`).MatchString(text)
}

func runGroup(ctx context.Context, cfg *config, req *mcp.CallToolRequest, args toolArgs) (*opOutput, error) {
	in, err := singleInput(cfg, req, args, "Handle")
	if err != nil {
		return nil, err
	}
	aggSrc, err := args.required("Aggregates")
	if err != nil {
		return nil, err
	}
	var by []selectItem
	if b := args.str("By"); b != "" {
		if by, err = compileColumnList(b, "By", in.Columns, false); err != nil {
			return nil, err
		}
	}
	aggs, params, err := compileAggregates(aggSrc, in.Columns, in.typeMap())
	if err != nil {
		return nil, err
	}
	sel := make([]string, 0, len(by)+len(aggs))
	groupBy := make([]string, 0, len(by))
	outCols := make([]string, 0, len(by)+len(aggs))
	outTypes := map[string]string{} // for Having's date comparisons
	for _, b := range by {
		sel = append(sel, b.sql)
		groupBy = append(groupBy, b.sql)
		outCols = append(outCols, b.name)
		outTypes[b.name] = in.typeOf(b.name)
	}
	for _, a := range aggs {
		sel = append(sel, a.sql+" AS "+quoteIdent(a.name))
		outCols = append(outCols, a.name)
		outTypes[a.name] = a.typ
	}
	if err := checkUniqueNames(namesOf(outCols), "By and Aggregates"); err != nil {
		return nil, err
	}
	var out *opOutput
	err = withTables(ctx, cfg, []*storedResult{in}, func(tables []string) error {
		q := "SELECT " + strings.Join(sel, ", ") + " FROM " + quoteIdent(tables[0])
		if len(groupBy) > 0 {
			q += " GROUP BY " + strings.Join(groupBy, ", ") + " ORDER BY " + strings.Join(groupBy, ", ")
		}
		if h := args.str("Having"); h != "" {
			cond, hp, err := compilePredicate(h, "Having", outCols, outTypes)
			if err != nil {
				return err
			}
			q = "SELECT * FROM (" + q + ") WHERE " + cond
			params = append(params, hp...)
		}
		rows, err := queryRows(ctx, cfg.calc.db, cfg.results.maxRows, q, params...)
		out = &opOutput{rows: rows, inputs: []*storedResult{in}}
		return err
	})
	return out, err
}

func namesOf(cols []string) []selectItem {
	out := make([]selectItem, len(cols))
	for i, c := range cols {
		out[i] = selectItem{name: c}
	}
	return out
}

func runJoin(ctx context.Context, cfg *config, req *mcp.CallToolRequest, args toolArgs) (*opOutput, error) {
	if args.str("Left") == "" && args.str("Right") == "" {
		if two := splitList(args.str("Handle")); len(two) == 2 {
			if k := strings.ToLower(args.str("Type")); k != "" && k != "inner" {
				return nil, fmt.Errorf("a %s join is not symmetric, so name the sides: use Left=%s Right=%s (swap them if the other result is the one to keep), On=<column in both, or LeftCol = RightCol> Type=%s", k, two[0], two[1], k)
			}
			// An inner join gives the same rows whichever side is first.
			delete(args, "Handle")
			args["Left"], args["Right"] = two[0], two[1]
		}
	}
	if args.str("Left") == "" || args.str("Right") == "" {
		return nil, fmt.Errorf("a join takes two handles, Left and Right (or, for an inner join, Handle=\"a,b\"), plus On and an optional Type. Example: Left=md19 Right=md11 On=\"PersonID = StudentPersonID\" Type=left")
	}
	leftID, err := args.required("Left")
	if err != nil {
		return nil, err
	}
	rightID, err := args.required("Right")
	if err != nil {
		return nil, err
	}
	onSrc, err := args.required("On")
	if err != nil {
		return nil, err
	}
	allowPartial, err := args.boolean(allowPartialParamName)
	if err != nil {
		return nil, err
	}
	kind := strings.ToLower(args.str("Type"))
	if kind == "" {
		kind = "inner"
	}
	switch kind {
	case "inner", "left", "semi", "anti":
	default:
		return nil, fmt.Errorf("Type must be inner, left, semi or anti, got %q", args.str("Type"))
	}
	inputs, err := callerHandles(cfg, req, allowPartial, leftID, rightID)
	if err != nil {
		return nil, err
	}
	l, r := inputs[0], inputs[1]
	pairs, err := compileJoinOn(onSrc, l.Columns, r.Columns)
	if err != nil {
		return nil, err
	}
	conds := make([]string, len(pairs))
	for i, p := range pairs {
		le, re := "l."+quoteIdent(p.left), "r."+quoteIdent(p.right)
		if affinity(l.typeOf(p.left)) != affinity(r.typeOf(p.right)) {
			le, re = "CAST("+le+" AS TEXT)", "CAST("+re+" AS TEXT)"
		}
		conds[i] = le + " = " + re
	}
	on := strings.Join(conds, " AND ")

	// The right side's columns, minus a join column of the same name, and
	// renamed where they would collide with a left column.
	var rightSel []string
	prefix := r.ID
	if r.Label != "" {
		prefix = strings.Join(strings.Fields(r.Label), "_")
	}
	for _, c := range r.Columns {
		skip := false
		for _, p := range pairs {
			if p.right == c && strings.EqualFold(p.left, c) {
				skip = true
			}
		}
		if skip {
			continue
		}
		name := c
		if _, clash := l.column(c); clash {
			name = prefix + "_" + c
		}
		rightSel = append(rightSel, "r."+quoteIdent(c)+" AS "+quoteIdent(name))
	}

	var out *opOutput
	err = withTables(ctx, cfg, inputs, func(tables []string) error {
		from := quoteIdent(tables[0]) + " AS l"
		var q string
		switch kind {
		case "semi", "anti":
			not := ""
			if kind == "anti" {
				not = "NOT "
			}
			q = "SELECT l.* FROM " + from + " WHERE " + not + "EXISTS (SELECT 1 FROM " + quoteIdent(tables[1]) + " AS r WHERE " + on + ") ORDER BY l.rowid"
		default:
			join := " JOIN "
			if kind == "left" {
				join = " LEFT JOIN "
			}
			tail := from + join + quoteIdent(tables[1]) + " AS r ON " + on
			var n int
			if err := cfg.calc.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tail).Scan(&n); err != nil {
				return err
			}
			if n > cfg.results.maxRows {
				return fmt.Errorf("the join would produce %d rows, more than the %d a stored result may hold: each %s matches many rows on the other side. Narrow one side first, or join on more columns", n, cfg.results.maxRows, onSrc)
			}
			sel := "l.*"
			if len(rightSel) > 0 {
				sel += ", " + strings.Join(rightSel, ", ")
			}
			q = "SELECT " + sel + " FROM " + tail + " ORDER BY l.rowid, r.rowid"
		}
		rows, err := queryRows(ctx, cfg.calc.db, cfg.results.maxRows, q)
		out = &opOutput{rows: rows, inputs: inputs}
		if err == nil && len(rows.rows) == 0 && kind != "anti" && len(l.Rows) > 0 && len(r.Rows) > 0 {
			out.notes = append(out.notes, fmt.Sprintf("0 rows: no value of the Left column(s) equals any value of the Right column(s) in On=%q. The columns probably hold different things (a name against an id, say). Look at both handles and pick the pair that hold the same values; do not carry on with this empty result", onSrc))
		}
		return err
	})
	return out, err
}

func runProject(ctx context.Context, cfg *config, req *mcp.CallToolRequest, args toolArgs) (*opOutput, error) {
	in, err := singleInput(cfg, req, args, "Handle")
	if err != nil {
		return nil, err
	}
	src, err := args.required(columnsParamName)
	if err != nil {
		return nil, err
	}
	items, err := compileColumnList(src, columnsParamName, in.Columns, true)
	if err != nil {
		return nil, err
	}
	distinct, err := args.boolean("Distinct")
	if err != nil {
		return nil, err
	}
	sel := make([]string, len(items))
	exprs := make([]string, len(items))
	for i, it := range items {
		sel[i] = it.sql + " AS " + quoteIdent(it.name)
		exprs[i] = it.sql
	}
	var out *opOutput
	err = withTables(ctx, cfg, []*storedResult{in}, func(tables []string) error {
		q := "SELECT " + strings.Join(sel, ", ") + " FROM " + quoteIdent(tables[0])
		if distinct {
			// Grouping keeps each distinct row once, in the order it first
			// appears.
			q += " GROUP BY " + strings.Join(exprs, ", ") + " ORDER BY MIN(rowid)"
		} else {
			q += " ORDER BY rowid"
		}
		rows, err := queryRows(ctx, cfg.calc.db, cfg.results.maxRows, q)
		out = &opOutput{rows: rows, inputs: []*storedResult{in}}
		return err
	})
	if err == nil {
		// A renamed column keeps the type it had under its old name.
		out.typeSources = []*storedResult{renamedView(in, items)}
	}
	return out, err
}

// renamedView is in with its columns renamed as items say, so the output
// columns find their types.
func renamedView(in *storedResult, items []selectItem) *storedResult {
	v := &storedResult{ID: in.ID}
	for _, it := range items {
		src := strings.Trim(it.sql, `"`)
		src = strings.ReplaceAll(src, `""`, `"`)
		v.Columns = append(v.Columns, it.name)
		v.Types = append(v.Types, in.typeOf(src))
	}
	return v
}

func runSort(ctx context.Context, cfg *config, req *mcp.CallToolRequest, args toolArgs) (*opOutput, error) {
	in, err := singleInput(cfg, req, args, "Handle")
	if err != nil {
		return nil, err
	}
	obSrc, err := args.required("OrderBy")
	if err != nil {
		return nil, err
	}
	order, err := compileOrderList(obSrc, "OrderBy", in.Columns)
	if err != nil {
		return nil, err
	}
	limit, err := args.integer("Limit")
	if err != nil {
		return nil, err
	}
	terms := make([]string, len(order))
	for i, o := range order {
		terms[i] = quoteIdent(o.col)
		if o.desc {
			terms[i] += " DESC"
		}
	}
	var out *opOutput
	err = withTables(ctx, cfg, []*storedResult{in}, func(tables []string) error {
		q := "SELECT * FROM " + quoteIdent(tables[0]) + " ORDER BY " + strings.Join(terms, ", ") + ", rowid"
		var params []any
		if limit > 0 {
			q += " LIMIT ?"
			params = append(params, limit)
		}
		rows, err := queryRows(ctx, cfg.calc.db, cfg.results.maxRows, q, params...)
		out = &opOutput{rows: rows, inputs: []*storedResult{in}, keepDisplay: true}
		return err
	})
	return out, err
}

func runCalc(_ context.Context, cfg *config, req *mcp.CallToolRequest, args toolArgs) (*opOutput, error) {
	src, err := args.required("Expression")
	if err != nil {
		return nil, err
	}
	allowPartial, err := args.boolean(allowPartialParamName)
	if err != nil {
		return nil, err
	}
	resolve := func(h, fn, col string, arg float64, hasArg bool) (float64, error) {
		rs, err := callerHandles(cfg, req, allowPartial, h)
		if err != nil {
			return 0, err
		}
		return columnStat(rs[0], fn, col, arg, hasArg)
	}
	type calcResult struct {
		Expression string  `json:"expression"`
		Value      float64 `json:"value"`
		Text       string  `json:"text"`
	}
	var results []calcResult
	var text strings.Builder
	for _, expr := range strings.Split(src, ";") {
		expr = strings.TrimSpace(expr)
		if expr == "" {
			continue
		}
		v, err := evalExpression(expr, resolve)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", expr, err)
		}
		s := formatNumber(v)
		results = append(results, calcResult{Expression: expr, Value: v, Text: s})
		fmt.Fprintf(&text, "%s = %s\n", expr, s)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("missing required parameter %q", "Expression")
	}
	return &opOutput{scalar: &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: text.String()}},
		StructuredContent: map[string]any{"results": results},
	}}, nil
}

// calcSQLForbidden are the words calc_sql refuses outside string literals:
// anything that writes, or reaches beyond the handles it was given.
var calcSQLForbidden = regexp.MustCompile(`(?i)\b(ATTACH|DETACH|PRAGMA|INSERT|UPDATE|DELETE|REPLACE|CREATE|DROP|ALTER|VACUUM|REINDEX|ANALYZE|TRIGGER|load_extension|sqlite_master|sqlite_schema|sqlite_temp_master)\b`)

// calcSQLHandle is a @handle reference inside calc_sql text.
var calcSQLHandle = regexp.MustCompile(`@([A-Za-z]{2}[0-9]+)\b`)

func runCalcSQL(ctx context.Context, cfg *config, req *mcp.CallToolRequest, args toolArgs) (*opOutput, error) {
	src, err := args.required("Query")
	if err != nil {
		return nil, err
	}
	allowPartial, err := args.boolean(allowPartialParamName)
	if err != nil {
		return nil, err
	}
	code := stripSQLiteLiterals(src)
	trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(code), ";"))
	if strings.Contains(trimmed, ";") {
		return nil, fmt.Errorf("Query must be a single statement")
	}
	lead := strings.ToUpper(leadingKeyword(trimmed))
	if lead != "SELECT" && lead != "WITH" {
		return nil, fmt.Errorf("Query must be a SELECT or WITH statement")
	}
	if m := calcSQLForbidden.FindString(code); m != "" {
		return nil, fmt.Errorf("Query may not use %s", m)
	}
	var ids []string
	seen := map[string]bool{}
	for _, m := range calcSQLHandle.FindAllStringSubmatch(code, -1) {
		id := strings.ToLower(m[1])
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("Query references no stored result; write each one as @handle (FROM @qx4)")
	}
	inputs, err := callerHandles(cfg, req, allowPartial, ids...)
	if err != nil {
		return nil, err
	}

	// A private database per call, holding only this caller's inputs: the
	// shared engine holds every user's tables, and free-form SQL must not be
	// able to name one.
	db, err := openMemoryDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	names := map[string]string{}
	for i, in := range inputs {
		name := fmt.Sprintf("t%d", i+1)
		if err := materialize(ctx, db, name, in); err != nil {
			return nil, err
		}
		names[in.ID] = quoteIdent(name)
	}
	query := replaceOutsideLiterals(src, func(code string) string {
		return calcSQLHandle.ReplaceAllStringFunc(code, func(m string) string {
			return names[strings.ToLower(m[1:])]
		})
	})
	if _, err := db.ExecContext(ctx, "PRAGMA query_only = 1"); err != nil {
		return nil, err
	}
	rows, err := queryRows(ctx, db, cfg.results.maxRows, query)
	if err != nil {
		return nil, fmt.Errorf("query failed: %w", err)
	}
	return &opOutput{rows: rows, inputs: inputs}, nil
}

// stripSQLiteLiterals empties string literals and drops comments, so a
// keyword check does not trip on text.
func stripSQLiteLiterals(s string) string {
	return rewriteSQLiteCode(s, func(code string) string { return code }, func(string) string { return "''" })
}

// replaceOutsideLiterals applies fn to the code between string literals and
// comments, leaving literals intact (and comments dropped).
func replaceOutsideLiterals(s string, fn func(string) string) string {
	return rewriteSQLiteCode(s, fn, func(lit string) string { return lit })
}

// rewriteSQLiteCode walks SQLite text, passing the code between literals
// through code and each quoted literal through literal; comments become a
// space.
func rewriteSQLiteCode(s string, fn func(string) string, literal func(string) string) string {
	var out, code strings.Builder
	flush := func() {
		out.WriteString(fn(code.String()))
		code.Reset()
	}
	r := []rune(s)
	for i := 0; i < len(r); i++ {
		switch {
		case r[i] == '\'':
			flush()
			j := i + 1
			for j < len(r) {
				if r[j] == '\'' {
					if j+1 < len(r) && r[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			out.WriteString(literal(string(r[i:min(j+1, len(r))])))
			i = j
		case r[i] == '-' && i+1 < len(r) && r[i+1] == '-':
			flush()
			for i < len(r) && r[i] != '\n' {
				i++
			}
			out.WriteByte(' ')
		case r[i] == '/' && i+1 < len(r) && r[i+1] == '*':
			flush()
			i += 2
			for i+1 < len(r) && !(r[i] == '*' && r[i+1] == '/') {
				i++
			}
			i++
			out.WriteByte(' ')
		default:
			code.WriteRune(r[i])
		}
	}
	flush()
	return out.String()
}
