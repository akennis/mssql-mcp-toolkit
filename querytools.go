package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-sql/civil"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"
)

// --query-tools turns one YAML file into a set of extra MCP tools. Each record
// is a named, parameterized query with its own connection string and an output
// format: the client supplies the parameter values, the server runs the query
// under the record's connection string, and replies as CSV, a markdown table,
// or a single scalar value. YAML is the file format so a tested statement can
// be pasted in verbatim as a block scalar instead of being escaped onto one
// line.
//
// The built-in <prefix>_query tool is the general one: any statement, always
// the same shape of result. These are the opposite — a fixed statement an
// operator has already written and tested, exposed under its own name so a
// model can call it without composing SQL. The two coexist; --query-tools adds
// tools, it does not replace anything.

// outputFormat is how a query tool renders its result set.
type outputFormat string

const (
	formatCSV    outputFormat = "csv"
	formatMD     outputFormat = "md"
	formatScalar outputFormat = "scalar"
)

// parseOutputFormat accepts the spellings an operator is likely to write for
// each of the three formats.
func parseOutputFormat(s string) (outputFormat, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "csv":
		return formatCSV, nil
	case "md", "markdown", "md-table", "table":
		return formatMD, nil
	case "scalar", "scalar-value", "value":
		return formatScalar, nil
	case "":
		return "", errors.New(`outputFormat is required (one of "csv", "md", "scalar")`)
	default:
		return "", fmt.Errorf(`unknown outputFormat %q (one of "csv", "md", "scalar")`, s)
	}
}

// paramType is the JSON type a query-tool parameter accepts, and decides how a
// received value is coerced before it is bound to the statement.
type paramType string

const (
	paramString paramType = "string"
	paramInt    paramType = "int"
	paramNumber paramType = "number"
	paramBool   paramType = "bool"
	// paramDate takes an absolute yyyy-mm-dd or an offset from today (-7d,
	// -2w, -1m, -1y, today, yesterday, tomorrow) and binds a date. The offset
	// form exists for small models: they rarely know today's date and get
	// calendar arithmetic wrong, so the server does the subtraction instead.
	paramDate paramType = "date"
)

func parseParamType(s string) (paramType, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "string", "str", "text":
		return paramString, nil
	case "int", "integer":
		return paramInt, nil
	case "number", "float", "decimal", "double":
		return paramNumber, nil
	case "bool", "boolean":
		return paramBool, nil
	case "date":
		return paramDate, nil
	default:
		return "", fmt.Errorf(`unknown type %q (one of "string", "int", "number", "bool", "date")`, s)
	}
}

// mustParamType is parseParamType for a value validation has already accepted.
func mustParamType(s string) paramType {
	t, err := parseParamType(s)
	if err != nil {
		return paramString
	}
	return t
}

// queryToolParam is one named parameter a query tool takes.
type queryToolParam struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	// Type is one of string (the default), int, number, bool, date. It decides
	// the JSON Schema type advertised to the model and the Go type the value
	// is bound as.
	Type string `json:"type,omitempty" yaml:"type,omitempty"`
	// Required defaults to true: a parameter is only optional if it says so.
	// An absent optional parameter is bound as SQL NULL.
	Required *bool `json:"required,omitempty" yaml:"required,omitempty"`
	// Batch marks a parameter that takes a comma-separated list of ids, and so
	// also accepts @handle.Column: the values of a stored result's column,
	// expanded by the server. Left unset, it is inferred from the query — a
	// parameter the statement passes to STRING_SPLIT is a list. Set false to
	// opt a list parameter out, or true for a list the statement splits some
	// other way.
	Batch *bool `json:"batch,omitempty" yaml:"batch,omitempty"`
	// Literal lets a batch parameter also take a value typed out as a comma
	// list. Left false, a batch parameter is an id list that takes only
	// @handle.Column — a reference to a stored result — and a literal value is
	// rejected before the query runs, so every id a tool joins on came from a
	// result the caller was shown. Set it for a list that is not an id read
	// from another result: school years, or a value a person types in as a
	// lookup key.
	Literal bool `json:"literal,omitempty" yaml:"literal,omitempty"`
	// Accepts names the columns, besides one spelled like the parameter, that
	// a batch parameter takes as @handle.Column. A column with any other name
	// is refused before the query runs, which keeps an id from one id space
	// (a SchCourseID) from being passed where another is wanted (a
	// FacultyID). The first entry is the column used when a bare @handle has
	// none spelled like the parameter, and the one a refusal points to.
	Accepts []string `json:"accepts,omitempty" yaml:"accepts,omitempty"`

	// shared is set by inheritSharedParams when the root file's
	// `sharedParameters:` block describes a parameter of this name.
	shared bool
	// batch is Batch resolved against the query, filled in by validate.
	batch bool
}

func (p queryToolParam) required() bool { return p.Required == nil || *p.Required }

// takesColumn reports whether a batch parameter accepts col, written as
// @handle.col: a column spelled like the parameter or one it lists in Accepts.
func (p queryToolParam) takesColumn(col string) bool {
	col = strings.TrimSpace(col)
	if strings.EqualFold(col, p.Name) {
		return true
	}
	for _, a := range p.Accepts {
		if strings.EqualFold(col, a) {
			return true
		}
	}
	return false
}

// idColumn is the column a refusal points a caller to for this parameter.
func (p queryToolParam) idColumn() string {
	if len(p.Accepts) > 0 {
		return p.Accepts[0]
	}
	return p.Name
}

// queryToolSpec is one record of the --query-tools file.
type queryToolSpec struct {
	Name string `json:"name" yaml:"name"`
	// Description is the text advertised with the tool — what every model
	// reads for every tool on the server, on every turn, so it is kept to a
	// sentence or two on when to use the tool. It is required.
	Description string `json:"description" yaml:"description"`
	// Details is the rest: what each returned column means, defaults, edge
	// cases, why a sibling tool might be the better choice. It is not
	// advertised; the describe tool returns it on request, so a model pays
	// for the long text only for the tool it is about to call.
	Details    string           `json:"details,omitempty" yaml:"details,omitempty"`
	Query      string           `json:"query" yaml:"query"`
	Parameters []queryToolParam `json:"parameters,omitempty" yaml:"parameters,omitempty"`
	// ConnectionString is the database this tool's query runs against. Blank
	// falls back to the server's --conn-string.
	ConnectionString string `json:"connectionString,omitempty" yaml:"connectionString,omitempty"`
	OutputFormat     string `json:"outputFormat" yaml:"outputFormat"`
	// ResultColumn names the column a scalar result is taken from. It is only
	// meaningful when OutputFormat is scalar, and only needed when the query
	// returns more than one column.
	ResultColumn string `json:"resultColumn,omitempty" yaml:"resultColumn,omitempty"`
	// Columns, when set, is the subset of result-set columns the tool actually
	// returns, in this order. It applies to the csv and md formats. The query
	// can select whatever it likes (or the tool can wrap a view it does not
	// control); only these columns reach the client. A name not in the result
	// set is a call-time error. Matching is case-insensitive.
	Columns []string `json:"columns,omitempty" yaml:"columns,omitempty"`
	// PickRecord makes a csv/md tool return a single record. One row is
	// returned as-is; several rows trigger an elicitation that shows the whole
	// result set — every column, so the user can review — and asks which record
	// to return. The reply carries only that record, and only the Columns
	// fields, which is the point: the client's context holds one projected row
	// instead of the whole set. `true` makes that the tool's only behaviour;
	// `optional` instead advertises a RequireSingle argument and applies it
	// only on the calls that ask, so the same tool can still list.
	PickRecord pickRecordMode `json:"pickRecord,omitempty" yaml:"pickRecord,omitempty"`
	// RequireAnyOf lists optional parameters of which a call must supply at
	// least one with a non-blank value. It is for a filter tool whose
	// parameters are individually optional so a caller can combine them
	// freely, but which is meaningless — or returns the whole table — with
	// none of them: every name here is optional on its own, and the call is
	// rejected before the query runs when all of them are absent, null, or
	// whitespace.
	RequireAnyOf []string `json:"requireAnyOf,omitempty" yaml:"requireAnyOf,omitempty"`
	// RequireHint is appended to the error a call gets for missing every
	// RequireAnyOf parameter: where to go instead when the question is wider
	// than the ids the caller has (a whole-building total, say).
	RequireHint string `json:"requireHint,omitempty" yaml:"requireHint,omitempty"`
	// Group, when set, puts this tool on a separate logical MCP server made up
	// of every tool that names the same group. Under --transport=http each
	// group listens on its own port (see the file's `groups:` block and
	// buildServers); under stdio there is one stream and so one server, and
	// the field has no effect.
	Group string `json:"group,omitempty" yaml:"group,omitempty"`

	// format is OutputFormat parsed, filled in by validate.
	format outputFormat
}

// pickRecordMode is the parsed `pickRecord:` setting. YAML `true` (and the
// spelled-out "always") means every call reduces to one record; "optional"
// means the tool takes a RequireSingle argument and reduces only when the
// caller sets it; absent / `false` means never.
type pickRecordMode string

const (
	pickRecordOff      pickRecordMode = ""
	pickRecordAlways   pickRecordMode = "always"
	pickRecordOptional pickRecordMode = "optional"
)

// always reports whether every call of the tool reduces to one record.
func (m pickRecordMode) always() bool { return m == pickRecordAlways }

// optional reports whether the tool offers RequireSingle per call.
func (m pickRecordMode) optional() bool { return m == pickRecordOptional }

// UnmarshalYAML accepts the boolean spelling the key has always had alongside
// the two mode names, so an existing `pickRecord: true` keeps working.
func (m *pickRecordMode) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("pickRecord must be true, false, or %q", pickRecordOptional)
	}
	switch strings.ToLower(strings.TrimSpace(node.Value)) {
	case "", "false", "no", "off":
		*m = pickRecordOff
	case "true", "yes", "on", string(pickRecordAlways):
		*m = pickRecordAlways
	case string(pickRecordOptional):
		*m = pickRecordOptional
	default:
		return fmt.Errorf("pickRecord: unknown value %q; want true, false, or %q", node.Value, pickRecordOptional)
	}
	return nil
}

// queryToolGroup is one logical MCP server carved out of the --query-tools
// file: every spec whose `group:` names it is served together, on a port of
// its own, so a model choosing among a handful of small servers never loads
// every tool's schema at once.
type queryToolGroup struct {
	// Name is the token tools carry in their `group:` field. It is the key in
	// the file's `groups:` mapping and is filled in from there.
	Name string `yaml:"-"`
	// Label is the name this group's server reports at initialize. Blank falls
	// back to "<server-label>-<name>".
	Label string `yaml:"label,omitempty"`
	// Description is a sentence or two on what the group's tools cover. It is
	// folded into the server's initialize instructions — the text a model
	// reads when it decides which server to call.
	Description string `yaml:"description,omitempty"`
	// Instructions, when set, replaces the generated initialize text outright.
	Instructions string `yaml:"instructions,omitempty"`
	// Prefix, when set, is written in front of every member tool's published
	// name (see publishedName), so the name a client shows the model carries
	// the group it belongs to: prefix st_dir on st_student_profile publishes
	// st_dir_student_profile. It exists for small models, which pair a tool
	// with the wrong sibling server when nothing in the tool's own name says
	// which one owns it. Cross-references in descriptions are rewritten to the
	// published names, so a file keeps using the declared ones.
	Prefix string `yaml:"prefix,omitempty"`
	// Port is the absolute TCP port this group's server listens on under
	// --transport=http. Order is an offset added to the --http-addr port
	// instead. Set at most one; a group that sets neither takes the next free
	// port above the base one, in the order its name first appears among the
	// tools.
	Port  int `yaml:"port,omitempty"`
	Order int `yaml:"order,omitempty"`
	// Operators puts the operator tools — the set, filter, grouping, join and
	// calc tools that work on stored results — on this group's server. Such a
	// group needs no query tools of its own. See handletools.go.
	Operators bool `yaml:"operators,omitempty"`

	// resolvedPort is Port/Order worked out against the running --http-addr,
	// filled in by resolveGroupPorts. Zero until then.
	resolvedPort int
}

// queryToolFile is a parsed --query-tools file: the tool specs, and the groups
// they are partitioned into. A file with no group declarations yields no
// groups and behaves exactly as it did before groups existed.
type queryToolFile struct {
	Specs  []*queryToolSpec
	Groups []*queryToolGroup
	// SharedParams is the root file's `sharedParameters:` block: parameters
	// that many tools take under the same name and meaning (PageSize, a
	// student id, a date window). A tool's parameter of the same name that
	// leaves its own description blank is advertised without one, and the
	// shared text goes into the server's initialize instructions once, instead
	// of into every tool's schema. See queryToolNotes.
	SharedParams []queryToolParam
	// rewrite is applyGroupNaming's mention rewriter, kept so text rendered
	// later for a particular server — the shared parameter notes — can have
	// its tool mentions qualified the same way the descriptions were.
	rewrite mentionRewriter
}

// mentionRewriter rewrites the tool names mentioned in text as a model on
// fromGroup's server has to call them; see applyGroupNaming.
type mentionRewriter func(text, fromGroup string) string

// groupNamePattern is what a group name may contain: the same character set as
// a tool name, since it ends up in a server label the client displays.
var groupNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// sqlParamName is what a bound-parameter name has to look like: a SQL
// identifier, so it can be written as @name in the statement.
var sqlParamName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// columnsParamName is the synthetic runtime parameter a csv/md tool gets for
// free whenever it declares a `columns:` menu: a comma-separated subset of
// that menu, letting the caller ask for only the fields it needs instead of
// the whole row. It is required whenever the menu exists — the caller must
// name the columns it wants, every call. It is not one of spec.Parameters —
// it never binds into the SQL text — so the name is reserved and an operator
// cannot declare an ordinary parameter under it.
const columnsParamName = "Columns"

// requireSingleParamName is the synthetic boolean parameter a `pickRecord:
// optional` tool advertises. Like Columns it never binds into the SQL text
// and its name is reserved. A call that sets it true gets pick-record
// behaviour for that call only: several matches are put to the person via
// elicitation and one comes back, so the model's context never holds the
// candidates it did not want.
const requireSingleParamName = "RequireSingle"

// loadQueryTools reads and validates the --query-tools file and returns its
// tool specs. It is parseQueryToolsFile without the group partition, kept for
// the callers that only ever wanted the tools.
func loadQueryTools(path string, reserved map[string]bool) ([]*queryToolSpec, error) {
	f, err := parseQueryToolsFile(path, reserved)
	if err != nil {
		return nil, err
	}
	return f.Specs, nil
}

// parseQueryToolsFile reads and validates the --query-tools file. reserved is
// the set of tool names the server already uses, so a record cannot shadow
// one.
//
// A file is one of:
//
//   - a bare list of tool specs (the original format);
//   - a single tool spec (a mapping with a `name:` key) — the shape a per-tool
//     file in a split tree uses;
//   - a mapping with any of `groups:`, `tools:` and `include:`.
//
// `include:` is a list of paths resolved against the including file's own
// directory. A path to a directory pulls in every *.yaml / *.yml file directly
// inside it, in filename order, skipping entries whose name starts with "." or
// "_"; a path to a file pulls in just that file. Each included file is parsed
// by this same routine, so the tree can be nested to any depth. A tool loaded
// from a directory that does not set its own `group:` inherits the directory's
// base name as its group; a `group:` it does set must match that name.
func parseQueryToolsFile(path string, reserved map[string]bool) (*queryToolFile, error) {
	return parseQueryToolsFileAs(path, reserved, defaultToolCallName)
}

// parseQueryToolsFileAs is parseQueryToolsFile for a client that namespaces
// tools by server: callName is the --tool-call-name template, and the
// cross-references in descriptions are written in that form.
func parseQueryToolsFileAs(path string, reserved map[string]bool, callName string) (*queryToolFile, error) {
	if err := checkToolCallName(callName); err != nil {
		return nil, err
	}
	specs, groupDefs, shared, err := collectQueryToolsShared(path, "", map[string]bool{})
	if err != nil {
		return nil, err
	}

	if len(specs) == 0 {
		return nil, fmt.Errorf("--query-tools: %s defines no tools", path)
	}

	sharedByName, err := validateSharedParams(shared)
	if err != nil {
		return nil, fmt.Errorf("--query-tools: %w", err)
	}

	seen := make(map[string]bool, len(specs))
	for i, spec := range specs {
		spec.inheritSharedParams(sharedByName)
		if err := spec.validate(reserved, seen); err != nil {
			label := spec.Name
			if label == "" {
				label = fmt.Sprintf("#%d", i+1)
			}
			return nil, fmt.Errorf("--query-tools: tool %s: %w", label, err)
		}
		seen[spec.Name] = true
	}

	groups, err := buildGroups(specs, groupDefs)
	if err != nil {
		return nil, fmt.Errorf("--query-tools: %w", err)
	}
	rewrite, err := applyGroupNaming(specs, groups, reserved, callName)
	if err != nil {
		return nil, fmt.Errorf("--query-tools: %w", err)
	}
	return &queryToolFile{Specs: specs, Groups: groups, SharedParams: shared, rewrite: rewrite}, nil
}

// validateSharedParams checks the root file's `sharedParameters:` block and
// indexes it by lowercased name. Each entry needs a name and a description —
// the description is the whole point — and a type the loader knows.
func validateSharedParams(shared []queryToolParam) (map[string]*queryToolParam, error) {
	byName := make(map[string]*queryToolParam, len(shared))
	for i := range shared {
		p := &shared[i]
		p.Name = strings.TrimSpace(p.Name)
		p.Description = strings.TrimSpace(p.Description)
		switch {
		case p.Name == "":
			return nil, fmt.Errorf("sharedParameters[%d]: name is required", i)
		case !sqlParamName.MatchString(p.Name):
			return nil, fmt.Errorf("sharedParameters: %q: name must be a SQL identifier", p.Name)
		case p.Description == "":
			return nil, fmt.Errorf("sharedParameters: %q: description is required", p.Name)
		case byName[strings.ToLower(p.Name)] != nil:
			return nil, fmt.Errorf("sharedParameters: %q is listed more than once", p.Name)
		}
		if _, err := parseParamType(p.Type); err != nil {
			return nil, fmt.Errorf("sharedParameters: %q: %w", p.Name, err)
		}
		byName[strings.ToLower(p.Name)] = p
	}
	return byName, nil
}

// inheritSharedParams fills in what a tool's parameter leaves blank from the
// shared parameter of the same name: the type, and the required flag. The
// description is deliberately not copied — a blank one stays blank in the
// schema, and the shared text reaches the model through the initialize
// instructions instead. A description the tool does write is an override
// for that tool alone.
func (s *queryToolSpec) inheritSharedParams(shared map[string]*queryToolParam) {
	for i := range s.Parameters {
		p := &s.Parameters[i]
		sp := shared[strings.ToLower(strings.TrimSpace(p.Name))]
		if sp == nil {
			continue
		}
		p.shared = true
		if strings.TrimSpace(p.Type) == "" {
			p.Type = sp.Type
		}
		if p.Required == nil && sp.Required != nil {
			p.Required = sp.Required
		}
		if p.Batch == nil && sp.Batch != nil {
			p.Batch = sp.Batch
		}
		if sp.Literal {
			p.Literal = true
		}
		if len(p.Accepts) == 0 {
			p.Accepts = sp.Accepts
		}
	}
}

// collectQueryTools reads one --query-tools file and every file it pulls in
// through `include:`, returning the flattened tool specs (in the order they
// were encountered) and the merged `groups:` definitions. inheritedGroup is the
// group a tool falls back to when it does not name one itself — the base name
// of the directory the file came from, or "" for the file --query-tools points
// at directly. visiting holds the absolute paths currently on the include
// stack, so a cycle is reported instead of recursing forever.
func collectQueryTools(path, inheritedGroup string, visiting map[string]bool) ([]*queryToolSpec, map[string]*queryToolGroup, error) {
	specs, groups, _, err := collectQueryToolsShared(path, inheritedGroup, visiting)
	return specs, groups, err
}

// collectQueryToolsShared is collectQueryTools plus the `sharedParameters:`
// block. The block is only read from the file --query-tools names; an
// included file that carries one is rejected, for the same reason a second
// `groups:` block is.
func collectQueryToolsShared(path, inheritedGroup string, visiting map[string]bool) ([]*queryToolSpec, map[string]*queryToolGroup, []queryToolParam, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("--query-tools: %w", err)
	}
	if visiting[abs] {
		return nil, nil, nil, fmt.Errorf("--query-tools: include cycle through %s", path)
	}
	visiting[abs] = true
	defer delete(visiting, abs)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("--query-tools: %w", err)
	}

	var probe yaml.Node
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return nil, nil, nil, fmt.Errorf("--query-tools: reading %s: %w", path, err)
	}
	root := &probe
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	dir := filepath.Dir(path)
	var specs []*queryToolSpec
	var shared []queryToolParam
	groupDefs := map[string]*queryToolGroup{}

	switch {
	case root.Kind == yaml.MappingNode && !mappingHasKey(root, "name"):
		var wrapper struct {
			Groups  map[string]*queryToolGroup `yaml:"groups"`
			Include []string                   `yaml:"include"`
			Tools   []*queryToolSpec           `yaml:"tools"`
			Shared  []queryToolParam           `yaml:"sharedParameters"`
		}
		if err := dec.Decode(&wrapper); err != nil && !errors.Is(err, io.EOF) {
			return nil, nil, nil, fmt.Errorf("--query-tools: reading %s: %w", path, err)
		}
		for name, g := range wrapper.Groups {
			groupDefs[name] = g
		}
		shared = wrapper.Shared
		for _, s := range wrapper.Tools {
			if err := applyInheritedGroup(s, inheritedGroup, path); err != nil {
				return nil, nil, nil, err
			}
			specs = append(specs, s)
		}
		for _, inc := range wrapper.Include {
			incSpecs, incGroups, err := loadInclude(dir, inc, visiting)
			if err != nil {
				return nil, nil, nil, err
			}
			specs = append(specs, incSpecs...)
			if err := mergeGroupDefs(groupDefs, incGroups); err != nil {
				return nil, nil, nil, err
			}
		}
	case root.Kind == yaml.MappingNode: // a single tool spec
		var s queryToolSpec
		if err := dec.Decode(&s); err != nil && !errors.Is(err, io.EOF) {
			return nil, nil, nil, fmt.Errorf("--query-tools: reading %s: %w", path, err)
		}
		if err := applyInheritedGroup(&s, inheritedGroup, path); err != nil {
			return nil, nil, nil, err
		}
		specs = append(specs, &s)
	case root.Kind == yaml.SequenceNode:
		if err := dec.Decode(&specs); err != nil && !errors.Is(err, io.EOF) {
			return nil, nil, nil, fmt.Errorf("--query-tools: reading %s: %w", path, err)
		}
		for _, s := range specs {
			if err := applyInheritedGroup(s, inheritedGroup, path); err != nil {
				return nil, nil, nil, err
			}
		}
	default:
		// An empty file, or a scalar — nothing to serve either way.
		return nil, nil, nil, fmt.Errorf("--query-tools: %s defines no tools", path)
	}

	return specs, groupDefs, shared, nil
}

// loadInclude resolves one `include:` entry against baseDir and returns its
// tools and group definitions. A directory entry contributes every *.yaml /
// *.yml file directly inside it, in filename order, and hands each its own base
// name as the inherited group; a file entry contributes just that file, with
// its parent directory's base name as the inherited group.
func loadInclude(baseDir, inc string, visiting map[string]bool) ([]*queryToolSpec, map[string]*queryToolGroup, error) {
	p := inc
	if !filepath.IsAbs(p) {
		p = filepath.Join(baseDir, inc)
	}
	info, err := os.Stat(p)
	if err != nil {
		return nil, nil, fmt.Errorf("--query-tools: include %s: %w", inc, err)
	}

	if !info.IsDir() {
		specs, groups, shared, err := collectQueryToolsShared(p, filepath.Base(filepath.Dir(p)), visiting)
		if err == nil && len(shared) > 0 {
			err = fmt.Errorf("--query-tools: include %s: `sharedParameters:` belongs in the root file", inc)
		}
		return specs, groups, err
	}

	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, nil, fmt.Errorf("--query-tools: include %s: %w", inc, err)
	}
	group := filepath.Base(p)
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_") {
			continue
		}
		switch strings.ToLower(filepath.Ext(n)) {
		case ".yaml", ".yml":
			names = append(names, n)
		}
	}
	sort.Strings(names)

	var specs []*queryToolSpec
	groupDefs := map[string]*queryToolGroup{}
	for _, n := range names {
		s, g, shared, err := collectQueryToolsShared(filepath.Join(p, n), group, visiting)
		if err != nil {
			return nil, nil, err
		}
		if len(shared) > 0 {
			return nil, nil, fmt.Errorf("--query-tools: include %s: `sharedParameters:` belongs in the root file", filepath.Join(inc, n))
		}
		specs = append(specs, s...)
		if err := mergeGroupDefs(groupDefs, g); err != nil {
			return nil, nil, err
		}
	}
	return specs, groupDefs, nil
}

// mergeGroupDefs folds src into dst, rejecting a group name defined in more
// than one file — the `groups:` block belongs in the root, and a second
// definition elsewhere is almost certainly a mistake.
func mergeGroupDefs(dst, src map[string]*queryToolGroup) error {
	for name, g := range src {
		if _, dup := dst[name]; dup {
			return fmt.Errorf("--query-tools: group %q is defined in more than one file", name)
		}
		dst[name] = g
	}
	return nil
}

// mappingHasKey reports whether a YAML mapping node has the given key. It is
// how a single-tool file (a mapping with `name:`) is told apart from a wrapper
// mapping (`groups:` / `tools:` / `include:`).
func mappingHasKey(n *yaml.Node, key string) bool {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return true
		}
	}
	return false
}

// applyInheritedGroup gives a spec the group its directory implies when it does
// not name one, and rejects a spec whose explicit group contradicts its
// directory — so a file's location and its contents cannot disagree.
func applyInheritedGroup(s *queryToolSpec, inherited, path string) error {
	if s == nil || inherited == "" {
		return nil
	}
	switch g := strings.TrimSpace(s.Group); g {
	case "":
		s.Group = inherited
	case inherited:
		// consistent; nothing to do
	default:
		return fmt.Errorf("--query-tools: %s: tool %s sets group %q but sits under directory %q", path, s.Name, g, inherited)
	}
	return nil
}

// buildGroups partitions specs by their group field and pairs each group name
// with its definition from the `groups:` block, synthesising a bare one for a
// group a tool names but the block omits. The result is ordered by where each
// group first appears among the specs — the order resolveGroupPorts hands out
// the default ports in.
func buildGroups(specs []*queryToolSpec, defs map[string]*queryToolGroup) ([]*queryToolGroup, error) {
	for name, g := range defs {
		if !groupNamePattern.MatchString(name) {
			return nil, fmt.Errorf("group %q: name must be 1-64 characters of letters, digits, _ or -", name)
		}
		if g == nil {
			g = &queryToolGroup{}
			defs[name] = g
		}
		g.Name = name
		if g.Port != 0 && g.Order != 0 {
			return nil, fmt.Errorf("group %q: set port or order, not both", name)
		}
		if g.Port != 0 && (g.Port < 1 || g.Port > 65535) {
			return nil, fmt.Errorf("group %q: port must be between 1 and 65535, got %d", name, g.Port)
		}
		if g.Order < 0 {
			return nil, fmt.Errorf("group %q: order must be 0 or positive, got %d", name, g.Order)
		}
	}

	var ordered []*queryToolGroup
	index := make(map[string]*queryToolGroup)
	for _, spec := range specs {
		g := strings.TrimSpace(spec.Group)
		spec.Group = g
		if g == "" {
			continue
		}
		if !groupNamePattern.MatchString(g) {
			return nil, fmt.Errorf("tool %s: group %q must be 1-64 characters of letters, digits, _ or -", spec.Name, g)
		}
		if _, ok := index[g]; ok {
			continue
		}
		grp := defs[g]
		if grp == nil {
			grp = &queryToolGroup{Name: g}
		}
		index[g] = grp
		ordered = append(ordered, grp)
	}

	// An operators group carries built-in tools, not query tools, so it is
	// the one kind of group no tool has to name. Such groups follow the
	// others, in name order.
	var opGroups []string
	for name, g := range defs {
		if _, ok := index[name]; ok {
			continue
		}
		if !g.Operators {
			return nil, fmt.Errorf("group %q is defined in `groups:` but no tool declares it", name)
		}
		opGroups = append(opGroups, name)
	}
	sort.Strings(opGroups)
	for _, name := range opGroups {
		ordered = append(ordered, defs[name])
	}
	operators := 0
	for _, g := range ordered {
		if g.Operators {
			operators++
		}
	}
	if operators > 1 {
		return nil, errors.New("more than one group sets `operators: true`; the operator tools belong on one server")
	}
	return ordered, nil
}

// applyGroupNaming is the last step of loading: it gives every grouped tool
// its published name (declared name behind the group's prefix) and rewrites
// the tool names mentioned in descriptions to match. A mention of a tool that
// lives in a different group is also qualified, once per description, with the
// name of the server that owns it — "cust_flag_summary (on the
// customers server)" — because a description that says "use X for
// the quick yes/no" without saying where X is leaves a small model to guess
// the server, and it guesses the one it is already on. The rewrite covers tool
// and parameter descriptions and the group descriptions that go into
// initialize instructions; it never touches the SQL.
//
// The mention is written the way the model has to call the tool: through the
// callName template (--tool-call-name), so a client that namespaces tools by
// server (Open WebUI: <server id>_<tool>) has its descriptions say
// "st_srvc_st_student_iep", the exact string to emit, rather than the served
// name the model would then have to compose a prefix onto — and small models
// do not compose, they copy.
//
// It runs even when no group sets a prefix, so cross-group mentions are
// qualified regardless.
func applyGroupNaming(specs []*queryToolSpec, groups []*queryToolGroup, reserved map[string]bool, callName string) (mentionRewriter, error) {
	groupsByName := make(map[string]*queryToolGroup, len(groups))
	for _, g := range groups {
		g.Prefix = normalizePrefix(g.Prefix)
		if g.Prefix != "" && !toolNamePattern.MatchString(g.Prefix) {
			return nil, fmt.Errorf("group %q: prefix %q must be 1-64 characters of letters, digits, _ or -", g.Name, g.Prefix)
		}
		groupsByName[g.Name] = g
	}

	published := make(map[string]string, len(specs)) // declared name -> published name
	owner := make(map[string]*queryToolGroup, len(specs))
	taken := make(map[string]string, len(specs)) // published name -> declared name
	for _, s := range specs {
		g := groupsByName[s.Group] // nil for an ungrouped tool
		name := s.Name
		if g != nil {
			name = publishedName(g.Prefix, s.Name)
		}
		switch {
		case !toolNamePattern.MatchString(name):
			return nil, fmt.Errorf("tool %s: published name %q must be 1-64 characters of letters, digits, _ or -", s.Name, name)
		case reserved[name]:
			return nil, fmt.Errorf("tool %s: published name %q is already one of this server's built-in tools", s.Name, name)
		case taken[name] != "":
			return nil, fmt.Errorf("tools %s and %s both publish as %q", taken[name], s.Name, name)
		}
		taken[name] = s.Name
		published[s.Name] = name
		owner[s.Name] = g
	}

	// asCalled is what a description writes for a mention: the published
	// name as the client presents it to the model.
	asCalled := make(map[string]string, len(specs))
	for declared, name := range published {
		server := ""
		if g := owner[declared]; g != nil {
			server = groupServerName(g)
		}
		asCalled[declared] = toolCallName(callName, server, name)
	}
	mention := toolMentionPattern(published)
	namespaced := strings.Contains(callName, "{server}")
	rewrite := func(text, fromGroup string) string {
		return qualifyToolMentions(mention, text, fromGroup, asCalled, owner, namespaced)
	}
	for _, s := range specs {
		s.Description = rewrite(s.Description, s.Group)
		s.Details = rewrite(s.Details, s.Group)
		s.RequireHint = rewrite(s.RequireHint, s.Group)
		for i := range s.Parameters {
			s.Parameters[i].Description = rewrite(s.Parameters[i].Description, s.Group)
		}
	}
	for _, g := range groups {
		g.Description = rewrite(g.Description, g.Name)
	}
	for _, s := range specs {
		s.Name = published[s.Name]
	}
	return rewrite, nil
}

// publishedName is the name a grouped tool is registered under: the group's
// prefix, an underscore, the declared name. A leading run of `_`-separated
// segments the two share is written once, so a file whose tools already carry
// a stem does not double it: prefix st_dir and name st_student_profile give
// st_dir_student_profile, while prefix dir and name find_person give
// dir_find_person. An empty prefix leaves the name alone.
func publishedName(prefix, name string) string {
	if prefix == "" {
		return name
	}
	rest := name
	for _, seg := range strings.Split(prefix, "_") {
		if seg == "" || !strings.HasPrefix(rest, seg+"_") {
			break
		}
		rest = rest[len(seg)+1:]
	}
	return prefix + "_" + rest
}

// toolMentionPattern matches any declared tool name standing on its own in
// prose — bounded by non-identifier characters, longest names first so
// st_student_iep does not swallow the front of st_student_iep_history.
func toolMentionPattern(published map[string]string) *regexp.Regexp {
	names := make([]string, 0, len(published))
	for n := range published {
		names = append(names, regexp.QuoteMeta(n))
	}
	sort.Slice(names, func(i, j int) bool {
		if len(names[i]) != len(names[j]) {
			return len(names[i]) > len(names[j])
		}
		return names[i] < names[j]
	})
	return regexp.MustCompile(`\b(?:` + strings.Join(names, "|") + `)\b`)
}

// qualifiedSuffix is the text qualifyToolMentions appends after a mention of a
// tool on another server; a mention the operator already qualified by hand in
// this form is left as it is.
const qualifiedSuffix = " (on the "

// qualifyToolMentions rewrites every declared tool name in text to the name
// the model calls it by (published[name], already in call form) and tags the first mention of a tool that lives outside
// fromGroup with the server that owns it. When the call name already carries
// the server — the group has a prefix, or the client namespaces (namespaced)
// — one tag per group per description is enough: every later mention of a
// sibling starts the same way, and "a (on the X server) / b / c" reads better
// than three identical tags. Otherwise nothing in the name says where a tool
// lives, so each tool is tagged on its first mention. A tool with no group is
// on the base server, whose label is not known here, so it is renamed but not
// tagged.
func qualifyToolMentions(mention *regexp.Regexp, text, fromGroup string, published map[string]string, owner map[string]*queryToolGroup, namespaced bool) string {
	spans := mention.FindAllStringIndex(text, -1)
	if len(spans) == 0 {
		return text
	}
	var b strings.Builder
	tagged := map[string]bool{}
	last := 0
	for _, span := range spans {
		name := text[span[0]:span[1]]
		b.WriteString(text[last:span[0]])
		b.WriteString(published[name])
		last = span[1]
		g := owner[name]
		if g == nil || g.Name == fromGroup {
			continue
		}
		key := name
		if g.Prefix != "" || namespaced {
			key = "group:" + g.Name
		}
		if tagged[key] || strings.HasPrefix(text[last:], qualifiedSuffix) {
			continue
		}
		tagged[key] = true
		fmt.Fprintf(&b, "%s%s server)", qualifiedSuffix, groupServerName(g))
	}
	b.WriteString(text[last:])
	return b.String()
}

// groupServerName is what a description calls the server a group runs on: its
// configured label, else the group name — which the default label
// "<server-label>-<name>" contains, so either way the model can find it.
func groupServerName(g *queryToolGroup) string {
	if s := strings.TrimSpace(g.Label); s != "" {
		return s
	}
	return g.Name
}

// validate checks one spec and fills in its derived fields.
func (s *queryToolSpec) validate(reserved, seen map[string]bool) error {
	s.Name = strings.TrimSpace(s.Name)
	switch {
	case s.Name == "":
		return errors.New("name is required")
	case !toolNamePattern.MatchString(s.Name):
		return fmt.Errorf("name %q must be 1-64 characters of letters, digits, _ or -", s.Name)
	case reserved[s.Name]:
		return fmt.Errorf("name %q is already one of this server's built-in tools", s.Name)
	case seen[s.Name]:
		return fmt.Errorf("name %q is defined more than once", s.Name)
	}

	if strings.TrimSpace(s.Description) == "" {
		return errors.New("description is required")
	}
	if strings.TrimSpace(s.Query) == "" {
		return errors.New("query is required")
	}

	format, err := parseOutputFormat(s.OutputFormat)
	if err != nil {
		return err
	}
	s.format = format

	s.ResultColumn = strings.TrimSpace(s.ResultColumn)
	if s.ResultColumn != "" && format != formatScalar {
		return errors.New(`resultColumn only applies when outputFormat is "scalar"`)
	}

	colSeen := make(map[string]bool, len(s.Columns))
	for i, c := range s.Columns {
		c = strings.TrimSpace(c)
		if c == "" {
			return fmt.Errorf("columns[%d] is empty", i)
		}
		if colSeen[strings.ToLower(c)] {
			return fmt.Errorf("column %q is listed more than once", c)
		}
		colSeen[strings.ToLower(c)] = true
		s.Columns[i] = c
	}
	if len(s.Columns) > 0 && format == formatScalar {
		return errors.New(`columns applies to outputFormat "csv" and "md"; for "scalar" use resultColumn`)
	}
	if s.PickRecord != pickRecordOff && format == formatScalar {
		return errors.New(`pickRecord applies to outputFormat "csv" and "md"; "scalar" already selects a single record`)
	}

	refs := referencedParams(s.Query)
	paramSeen := make(map[string]bool, len(s.Parameters))
	for i := range s.Parameters {
		p := &s.Parameters[i]
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" {
			return fmt.Errorf("parameter #%d: name is required", i+1)
		}
		if !sqlParamName.MatchString(p.Name) {
			return fmt.Errorf("parameter %q: name must be a SQL identifier so it can be written as @%s", p.Name, p.Name)
		}
		if strings.EqualFold(p.Name, columnsParamName) {
			return fmt.Errorf("parameter %q: the name %q is reserved (it is ignored if a client sends it)", p.Name, columnsParamName)
		}
		if strings.EqualFold(p.Name, requireSingleParamName) {
			return fmt.Errorf("parameter %q: the name %q is reserved for record selection when `pickRecord: optional` is set", p.Name, requireSingleParamName)
		}
		if strings.EqualFold(p.Name, saveAsParamName) || strings.EqualFold(p.Name, allowPartialParamName) {
			return fmt.Errorf("parameter %q: the name is reserved for stored results", p.Name)
		}
		low := strings.ToLower(p.Name)
		if paramSeen[low] {
			return fmt.Errorf("parameter %q is declared more than once", p.Name)
		}
		paramSeen[low] = true
		if _, err := parseParamType(p.Type); err != nil {
			return fmt.Errorf("parameter %q: %w", p.Name, err)
		}
		// A declared parameter that never appears in the statement is a
		// mistake worth catching now: the value would be accepted from the
		// client and silently dropped. The reverse (an @name with no
		// declaration) is left to SQL Server, since a statement may legitimately
		// use @@ROWCOUNT or DECLARE a local of its own.
		if !refs[low] {
			return fmt.Errorf("parameter %q is declared but the query never uses @%s", p.Name, p.Name)
		}
		switch {
		case p.Batch != nil:
			p.batch = *p.Batch
		default:
			p.batch = splitsParam(s.Query, p.Name)
		}
		for j, a := range p.Accepts {
			if strings.TrimSpace(a) == "" {
				return fmt.Errorf("parameter %q: accepts[%d] is empty", p.Name, j)
			}
			if !p.batch {
				return fmt.Errorf("parameter %q: accepts applies to a batch parameter", p.Name)
			}
		}
		if p.batch && mustParamType(p.Type) != paramString {
			return fmt.Errorf("parameter %q: batch applies to a string parameter holding a comma-separated list, not a %s", p.Name, mustParamType(p.Type))
		}
	}

	// requireAnyOf names declared parameters, each spelled as declared so the
	// call-time lookup and the error message match the advertised schema. A
	// required parameter has no business here: it is always present, which
	// would make the group's check trivially true.
	anySeen := make(map[string]bool, len(s.RequireAnyOf))
	for i, name := range s.RequireAnyOf {
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("requireAnyOf[%d] is empty", i)
		}
		p := s.param(name)
		if p == nil {
			return fmt.Errorf("requireAnyOf names %q, which is not a declared parameter", name)
		}
		if p.required() {
			return fmt.Errorf("requireAnyOf names %q, which is already required on its own", p.Name)
		}
		if anySeen[strings.ToLower(p.Name)] {
			return fmt.Errorf("requireAnyOf lists %q more than once", p.Name)
		}
		anySeen[strings.ToLower(p.Name)] = true
		s.RequireAnyOf[i] = p.Name
	}
	return nil
}

// param finds a declared parameter by name, case-insensitively, or nil.
func (s *queryToolSpec) param(name string) *queryToolParam {
	for i := range s.Parameters {
		if strings.EqualFold(s.Parameters[i].Name, name) {
			return &s.Parameters[i]
		}
	}
	return nil
}

// referencedParams is the set of @name tokens a statement uses, lowercased and
// without the @. Comments and string literals are stripped first, and @@NAME
// system references are skipped.
func referencedParams(query string) map[string]bool {
	code := []rune(stripNoise(query))
	out := make(map[string]bool)
	for i := 0; i < len(code); i++ {
		if code[i] != '@' {
			continue
		}
		if i+1 < len(code) && code[i+1] == '@' { // @@ROWCOUNT and friends
			i++
			continue
		}
		j := i + 1
		for j < len(code) && isParamRune(code[j]) {
			j++
		}
		if j > i+1 {
			out[strings.ToLower(string(code[i+1:j]))] = true
		}
		i = j - 1
	}
	return out
}

// splitsParam reports whether the statement passes @name to STRING_SPLIT,
// which is how every list parameter in the house style is read.
func splitsParam(query, name string) bool {
	re := regexp.MustCompile(`(?i)\bSTRING_SPLIT\s*\(\s*@` + regexp.QuoteMeta(name) + `\b`)
	return re.MatchString(stripNoise(query))
}

func isParamRune(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// inputSchema is the JSON Schema advertised for this tool: one property per
// declared parameter, typed, with the required ones listed, plus — when the
// tool declares a `columns:` menu — the synthetic Columns property that lets
// a caller narrow the response to the fields it actually needs, and — when
// it is `pickRecord: optional` — the synthetic RequireSingle switch.
//
// The stored-result switches a list tool accepts (SaveAs, AllowPartial; see
// knownArgNames) are deliberately not in it: every property here is paid for
// on every turn by every tool on the server, and these two are rarely used,
// so — like the shared parameters — they are explained once in the
// initialize instructions instead.
// batchParamHint is what an id-list parameter's schema says about taking a
// list. The schema is the one place every model reads before calling, so the
// hint sits there even on a shared parameter whose own text is only in the
// initialize instructions: a model that reads "given PersonID" as one id
// otherwise loops over a roster one call at a time. Every character here is
// paid on every tool that has such a parameter (see the per-tool budget tests
// for a large tool file), so it is terse.
const batchParamHint = "handle only."

// advertisedParamDescription is a parameter's schema description: its own
// text, plus batchParamHint for an id-list parameter whose text does not
// already say it takes a list. SchoolYear is split too, but it is not an id
// and its shared text already says several may be given.
func advertisedParamDescription(p queryToolParam) string {
	d := p.Description
	switch {
	case !p.batch || p.Literal:
		return d
	case d == "":
		return batchParamHint
	case !strings.Contains(d, "handle"):
		return d + " " + batchParamHint
	}
	return d
}

func (s *queryToolSpec) inputSchema() *jsonschema.Schema {
	schema := &jsonschema.Schema{Type: "object"}
	if len(s.Parameters) == 0 && len(s.Columns) == 0 && !s.PickRecord.optional() {
		return schema
	}
	schema.Properties = make(map[string]*jsonschema.Schema, len(s.Parameters)+2)
	var required []string
	for _, p := range s.Parameters {
		ps := &jsonschema.Schema{Description: advertisedParamDescription(p)}
		switch mustParamType(p.Type) {
		case paramInt:
			ps.Type = "integer"
		case paramNumber:
			ps.Type = "number"
		case paramBool:
			ps.Type = "boolean"
		case paramDate:
			// The accepted forms are spelled out once in the initialize
			// instructions (see queryToolNotes) rather than on every date
			// parameter; the pattern lets a grammar-constrained client refuse
			// anything else at decode time.
			ps.Type = "string"
			ps.Pattern = dateArgPattern
		default:
			ps.Type = "string"
		}
		schema.Properties[p.Name] = ps
		if p.required() {
			required = append(required, p.Name)
		}
	}
	if s.PickRecord.optional() {
		schema.Properties[requireSingleParamName] = &jsonschema.Schema{
			Type:        "boolean",
			Description: "Set true when exactly one record is wanted; several matches are then put to the user to choose from.",
		}
	}
	schema.Required = required
	// Reject any argument key beyond the ones declared above. Without this a
	// validating client would let a misspelled parameter name through as an
	// ordinary extra property, and handleQueryTool's exact-key lookup would
	// then silently treat it as "not provided" rather than erroring.
	schema.AdditionalProperties = &jsonschema.Schema{Not: &jsonschema.Schema{}}
	return schema
}

// connPool owns the database handles the query tools reach through. The handle
// from --conn-string is the primary and is opened and closed by run(); every
// other connection string named by a spec is opened here on first use and kept
// for the life of the process.
type connPool struct {
	cfg     *config
	primary *sql.DB

	mu    sync.Mutex
	extra map[string]*sql.DB
}

func newConnPool(cfg *config, primary *sql.DB) *connPool {
	return &connPool{cfg: cfg, primary: primary, extra: make(map[string]*sql.DB)}
}

// dialConn opens a pooled database for one query tool's connection string. It
// is a variable so tests can point it at the stub driver.
var dialConn = func(cfg *config, connString string) (*sql.DB, error) {
	c := *cfg
	c.connString = connString
	return openDB(&c)
}

// get returns the database for connString, opening it on first use. A blank
// string, or the server's own --conn-string, resolves to the already-open
// primary handle.
func (p *connPool) get(connString string) (*sql.DB, error) {
	connString = strings.TrimSpace(connString)
	if connString == "" || connString == strings.TrimSpace(p.cfg.connString) {
		return p.primary, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if db, ok := p.extra[connString]; ok {
		return db, nil
	}
	db, err := dialConn(p.cfg, connString)
	if err != nil {
		return nil, err
	}
	p.extra[connString] = db
	return db, nil
}

// closeExtra closes every handle the pool opened itself. The primary is not
// among them: it belongs to run().
func (p *connPool) closeExtra() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, db := range p.extra {
		db.Close()
		delete(p.extra, k)
	}
}

// registerQueryTools adds one MCP tool per spec. specs is a subset of
// cfg.queryTools — every tool for a plain server, or one group's worth for a
// grouped one.
func registerQueryTools(server *mcp.Server, cfg *config, pool *connPool, specs []*queryToolSpec) {
	// The passable set is process-wide and a result's id can be read by a tool
	// on another group's server, so build it from every tool, not this group's.
	if len(cfg.queryTools) > 0 {
		setPassableColumns(cfg.queryTools)
	} else {
		setPassableColumns(specs)
	}
	for _, spec := range specs {
		spec := spec
		server.AddTool(&mcp.Tool{
			Name:        spec.Name,
			Description: spec.Description,
			InputSchema: spec.inputSchema(),
			Annotations: &mcp.ToolAnnotations{
				// These tools run a fixed statement the operator wrote; they are
				// meant for reads, and the login they run as is expected to be
				// read-only.
				ReadOnlyHint:  true,
				OpenWorldHint: boolPtr(true),
			},
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleQueryTool(ctx, cfg, pool, spec, req)
		})
	}
}

// queryToolOutput is the structured result of a query tool. Which of csv /
// markdown / value is populated follows the tool's configured format.
type queryToolOutput struct {
	Tool        string   `json:"tool"`
	Format      string   `json:"format"`
	Columns     []string `json:"columns,omitempty"`
	RowCount    int      `json:"row_count"`
	Truncated   bool     `json:"truncated,omitempty"`
	Notes       []string `json:"notes,omitempty"`
	CSV         string   `json:"csv,omitempty"`
	Markdown    string   `json:"markdown,omitempty"`
	Value       any      `json:"value,omitempty"`
	ValueColumn string   `json:"value_column,omitempty"`
	// ValueRow is the 1-based record the scalar value came from when the caller
	// (or the user, through elicitation) picked one out of several.
	ValueRow int `json:"value_row,omitempty"`
	// Handle, when set, names the stored copy of the whole result, of which
	// the rows here are one page; TotalRows is its size and FirstRow the
	// 0-based position of the first row shown. Profile summarizes a large
	// result. See store.go.
	Handle    string `json:"handle,omitempty"`
	Label     string `json:"label,omitempty"`
	TotalRows int    `json:"total_rows,omitempty"`
	FirstRow  *int   `json:"first_row,omitempty"`
	// Partial is set when the rows here are only some of the stored result.
	Partial bool           `json:"partial,omitempty"`
	Profile *resultProfile `json:"profile,omitempty"`
}

func handleQueryTool(ctx context.Context, cfg *config, pool *connPool, spec *queryToolSpec, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// A tool that asked the user which record to use is being retried with
	// their answer. Everything it needs is in the request state; there is no
	// reason to run the query again.
	if er, ok := elicitationAnswer(req); ok {
		owner, identified := callerOf(cfg, req)
		return resumeRecordPick(cfg, spec, req, er, owner, identified)
	}

	args := map[string]any{}
	if raw := req.Params.Arguments; len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return toolErrorf("could not read the tool arguments: %v", err)
		}
	}

	// A parameter is looked up by exact key below, and an optional one that's
	// simply absent binds as SQL NULL with no complaint — that's how a caller
	// skips a filter on purpose. A misspelled or wrong-case key looks exactly
	// the same as "absent" to that lookup, so without this check a typoed
	// filter name would silently stop filtering instead of erroring, and the
	// query would quietly return an unfiltered (or wrongly filtered) result.
	if unknown := unknownArgs(spec, args, cfg.handlesOn()); len(unknown) > 0 {
		known := knownArgNames(spec, cfg.handlesOn())
		return toolErrorf("unrecognized parameter(s) %s for tool %q%s; expected one of: %s",
			strings.Join(unknown, ", "), spec.Name, didYouMean(unknown, known), strings.Join(known, ", "))
	}

	if len(spec.RequireAnyOf) > 0 && !anyArgPresent(spec.RequireAnyOf, args) {
		hint := ""
		if spec.RequireHint != "" {
			hint = " " + spec.RequireHint
		}
		return toolErrorf("tool %q needs at least one of %s (non-blank); none was given. Calling it again without one fails the same way. Pass one as handle.Column from a tool that lists them (a roster tool for students, st_find_building / st_list_school_levels for a school or program), or ask the user which students, school or program to cover.%s",
			spec.Name, strings.Join(spec.RequireAnyOf, ", "), hint)
	}

	single := false
	if spec.PickRecord.optional() {
		var err error
		if single, err = requireSingleArg(args); err != nil {
			return toolErrorf("%v", err)
		}
	}

	// A batch parameter written as @handle.Column is replaced by that stored
	// result's values before anything binds. The original arguments are kept
	// for the new result's provenance, where the reference reads better than
	// the ids it stood for.
	given := make(map[string]any, len(args))
	for k, v := range args {
		given[k] = v
	}
	owner, identified := callerOf(cfg, req)
	expanded, err := expandHandleArgs(cfg, spec, owner, identified, args)
	if err != nil {
		return toolErrorf("%v", err)
	}

	cols, colNotes, err := effectiveColumns(spec, args)
	if err != nil {
		return toolErrorf("%v", err)
	}

	// A list result is captured whole and stored; the reply shows a page of
	// it. A scalar or a single picked record is the answer itself and has
	// nothing to store.
	storing := cfg.handlesOn() && spec.format != formatScalar && !spec.PickRecord.always() && !single
	if storing {
		return handleStoredQueryTool(ctx, cfg, pool, spec, args, given, expanded, cols, colNotes, owner, identified)
	}

	named, err := bindArgs(spec, args, nil)
	if err != nil {
		return toolErrorf("%v", err)
	}

	db, err := pool.get(spec.ConnectionString)
	if err != nil {
		return toolErrorf("opening the connection for %q: %v", spec.Name, err)
	}

	qctx, cancel := context.WithTimeout(ctx, cfg.queryTimeout)
	defer cancel()

	res, err := runQuery(qctx, db, spec.Query, budgetFor(cfg), named...)
	if err != nil {
		return toolErrorf("%v", withQuery(describeQueryError(qctx, cfg, err), spec.Query))
	}

	// The record a resolver returns is stored under a handle like any list, so
	// the id it carries can reach the next tool as @handle.Column.
	var keep *pickKeep
	if cfg.handlesOn() && spec.format != formatScalar {
		keep = &pickKeep{cfg: cfg, owner: owner, identified: identified, args: provenanceArgs(given)}
	}
	if spec.PickRecord.always() {
		return withNotes(renderPickRecord(req, spec, cols, res, pickRequested{keep: keep}))(colNotes)
	}
	if single {
		return withNotes(renderPickRecord(req, spec, cols, res, pickRequested{opted: true, pageSize: pageSizeArg(spec, args), keep: keep}))(colNotes)
	}
	switch spec.format {
	case formatCSV:
		return withNotes(renderCSV(spec, cols, res))(colNotes)
	case formatMD:
		return withNotes(renderMarkdown(spec, cols, res))(colNotes)
	case formatScalar:
		return renderScalar(req, spec, res)
	default:
		return toolErrorf("tool %q has an unknown output format %q", spec.Name, spec.format)
	}
}

// withNotes appends notes to the text of a reply that has none of its own
// (an unstored one), so a note about the call reaches the model on any path.
func withNotes(res *mcp.CallToolResult, err error) func(notes []string) (*mcp.CallToolResult, error) {
	return func(notes []string) (*mcp.CallToolResult, error) {
		if err != nil || res == nil || res.IsError || len(notes) == 0 || len(res.Content) == 0 {
			return res, err
		}
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			for _, n := range notes {
				tc.Text += "Note: " + n + "\n"
			}
		}
		return res, err
	}
}

// handleStoredQueryTool is a csv/md call with stored results on: capture the
// whole result, store it under a handle, and reply with the page asked for
// (or, for a large result and no page, a profile and a sample).
func handleStoredQueryTool(ctx context.Context, cfg *config, pool *connPool, spec *queryToolSpec, args, given map[string]any, expanded []*expandedArg, cols, colNotes []string, owner string, identified bool) (*mcp.CallToolResult, error) {
	prov := provenance{Tool: spec.Name, Args: provenanceArgs(given)}
	var parents []*storedResult
	for _, e := range expanded {
		prov.Parents = append(prov.Parents, e.source.ID)
		parents = append(parents, e.source)
		// Write the reference one way, so "md18" and "@md18.SchCourseScheduleID"
		// count as the same call.
		prov.Args[e.param] = e.ref.text()
	}
	label := cleanLabel(stringArg(args, saveAsParamName))

	// The same call again (same tool, arguments and input handles) has the
	// same rows, so hand back the handle already held instead of a copy.
	var r *storedResult
	notes := append([]string(nil), colNotes...)
	if identified && label == "" {
		if same := cfg.results.findSame(owner, prov); same != nil {
			r = same
			if cfg.results.recentlyMade(r) {
				notes = append(notes, fmt.Sprintf("this is the same call as %s, so that handle is reused: no need to run it again", r.ID))
			}
			if len(cols) > 0 {
				var err error
				if cols, err = resolveColumns(cols, r.Columns, "in the result set"); err != nil {
					return toolErrorf("%v", err)
				}
			}
		}
	}
	if r == nil {
		res, err := captureQueryTool(ctx, cfg, pool, spec, args, expanded)
		if err != nil {
			return toolErrorf("%v", err)
		}
		if len(cols) > 0 {
			if cols, err = resolveColumns(cols, res.Columns, "in the result set"); err != nil {
				return toolErrorf("%v", err)
			}
		}
		menu, _ := resolveColumns(spec.Columns, res.Columns, "in the result set")
		r = newStoredResult(cfg, res, prov, label, lineageOf(parents...))
		r.hidden = hiddenColumns(res.Columns, menu)
		r.display = cols
		notes = plausibilityNotes(spec, given, expanded, r)
		if why := storeFor(cfg, owner, identified, r); why != "" {
			notes = append(notes, why)
		}
	}

	pageSize := pageSizeArg(spec, args)
	pageNumber := 0
	if pageSize > 0 {
		pageNumber = pageNumberArg(spec, args)
	}
	return storedReply(cfg, r, replyOpts{
		tool:       spec.Name,
		server:     serverOfGroup(cfg, spec.Group),
		format:     spec.format,
		cols:       cols,
		pageSize:   pageSize,
		pageNumber: pageNumber,
		notes:      notes,
	})
}

// bindArgs turns a call's arguments into the named values bound to the
// statement: each declared parameter coerced to its type, or SQL NULL when an
// optional one is absent. override, when given, may replace any parameter's
// value after coercion (nil for an absent one) — the capture uses it to ask
// for the whole result instead of the caller's page.
func bindArgs(spec *queryToolSpec, args map[string]any, override func(p queryToolParam, v any) any) ([]any, error) {
	named := make([]any, 0, len(spec.Parameters))
	for _, p := range spec.Parameters {
		var conv any
		v, ok := args[p.Name]
		if !ok || v == nil {
			if p.required() {
				return nil, fmt.Errorf("missing required parameter %q", p.Name)
			}
		} else if p.required() && blankArg(v) {
			// A blank required filter would bind fine and quietly match
			// nothing (or everything), so it counts as missing.
			return nil, fmt.Errorf("missing required parameter %q (a blank value doesn't count)", p.Name)
		} else {
			var err error
			if conv, err = coerceParam(p, v); err != nil {
				return nil, fmt.Errorf("parameter %q: %v", p.Name, err)
			}
		}
		if override != nil {
			conv = override(p, conv)
		}
		named = append(named, sql.Named(p.Name, conv))
	}
	return named, nil
}

// pageNumberArg is the call's PageNumber, or 1.
func pageNumberArg(spec *queryToolSpec, args map[string]any) int {
	for _, p := range spec.Parameters {
		if !strings.EqualFold(p.Name, "PageNumber") {
			continue
		}
		conv, err := coerceParam(queryToolParam{Name: p.Name, Type: "int"}, args[p.Name])
		if err != nil {
			return 1
		}
		if n, ok := conv.(int64); ok && n > 0 {
			return int(n)
		}
	}
	return 1
}

// knownArgNames lists the argument keys a tool call may use: its declared
// parameters, in declaration order, plus the synthetic Columns key. Columns
// is never advertised, but a stale prompt or a small model may still send it,
// so it is accepted here and ignored (effectiveColumns says so in a note).
func knownArgNames(spec *queryToolSpec, handles bool) []string {
	names := make([]string, 0, len(spec.Parameters)+2)
	for _, p := range spec.Parameters {
		names = append(names, p.Name)
	}
	names = append(names, columnsParamName)
	// RequireSingle, by contrast, is only known where it is advertised: a tool
	// that can't honour it must reject it rather than quietly return the whole
	// set the caller asked not to see.
	if spec.PickRecord.optional() {
		names = append(names, requireSingleParamName)
	}
	// The stored-result switches. Like Columns they are accepted without
	// being in every schema; the initialize instructions describe them.
	if handles && spec.format != formatScalar && !spec.PickRecord.always() {
		names = append(names, saveAsParamName)
		if len(batchParamNames(spec)) > 0 {
			names = append(names, allowPartialParamName)
		}
	}
	return names
}

// unknownArgs reports the keys in args that aren't among a tool's declared
// parameters (or its Columns key). See the call site in handleQueryTool for
// why these can't just be ignored.
func unknownArgs(spec *queryToolSpec, args map[string]any, handles bool) []string {
	known := make(map[string]bool, len(spec.Parameters)+1)
	for _, name := range knownArgNames(spec, handles) {
		known[name] = true
	}
	var unknown []string
	for k := range args {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	return unknown
}

// didYouMean returns " (did you mean X?)" for each unknown key that differs
// from a known name only in letter case — the commonest slip of a small model
// (SearchFirstname for SearchFirstName) — or "" when there is none.
func didYouMean(unknown, known []string) string {
	var hints []string
	for _, u := range unknown {
		for _, k := range known {
			if strings.EqualFold(u, k) {
				hints = append(hints, fmt.Sprintf("%s is spelled %s", u, k))
				break
			}
		}
	}
	if len(hints) == 0 {
		return ""
	}
	return " (parameter names are case-sensitive: " + strings.Join(hints, "; ") + ")"
}

// anyArgPresent reports whether at least one of names is in args with a
// value that isn't null or a blank string. It backs `requireAnyOf:` — a
// whitespace-only filter would bind fine and then match everything, which is
// exactly the unfiltered result the setting exists to prevent.
func anyArgPresent(names []string, args map[string]any) bool {
	for _, name := range names {
		if v, ok := args[name]; ok && v != nil && !blankArg(v) {
			return true
		}
	}
	return false
}

// blankArg reports whether v is a string holding nothing but whitespace.
func blankArg(v any) bool {
	str, isStr := v.(string)
	return isStr && strings.TrimSpace(str) == ""
}

// requireSingleArg reads the RequireSingle argument. Absent or null is false;
// anything else has to be a boolean (or a string one, like "true").
func requireSingleArg(args map[string]any) (bool, error) {
	v, ok := args[requireSingleParamName]
	if !ok || v == nil {
		return false, nil
	}
	conv, err := coerceParam(queryToolParam{Name: requireSingleParamName, Type: "bool"}, v)
	if err != nil {
		return false, fmt.Errorf("parameter %q: %v", requireSingleParamName, err)
	}
	b, _ := conv.(bool)
	return b, nil
}

// pageSizeArg is the value of a tool's PageSize parameter on this call, or 0
// when the tool has no such parameter or the caller left it out. PageSize is
// only a convention — nothing in the loader gives the name meaning — but a
// RequireSingle pick made from one page of a larger set is a pick made
// blind, so when the convention is in use the elicitation says so.
func pageSizeArg(spec *queryToolSpec, args map[string]any) int {
	for _, p := range spec.Parameters {
		if !strings.EqualFold(p.Name, "PageSize") {
			continue
		}
		conv, err := coerceParam(queryToolParam{Name: p.Name, Type: "int"}, args[p.Name])
		if err != nil {
			return 0
		}
		if n, ok := conv.(int64); ok && n > 0 {
			return int(n)
		}
		return 0
	}
	return 0
}

// effectiveColumns returns the columns a csv/md call shows: every column of
// the tool's declared `columns:` list, always. Columns is not a parameter of
// any query tool (a small model formats a comma-separated list badly, and a
// failed call costs a round trip), so a client that sends one anyway is not
// refused; the argument is ignored and the reply says so. A tool with no
// declared list returns nil, and every result-set column passes through.
func effectiveColumns(spec *queryToolSpec, args map[string]any) ([]string, []string, error) {
	if len(spec.Columns) == 0 {
		return nil, nil, nil
	}
	var notes []string
	if raw, ok := args[columnsParamName]; ok && raw != nil && !blankArg(raw) {
		notes = append(notes, "Columns is not a parameter of this tool, so it was ignored: every column is always returned")
	}
	return spec.Columns, notes, nil
}

func containsFold(list []string, s string) bool {
	for _, l := range list {
		if strings.EqualFold(l, s) {
			return true
		}
	}
	return false
}

// coerceParam turns a JSON-decoded argument into the Go value bound to the
// statement. Both the native JSON type and the same value quoted as a string
// are accepted, since MCP clients disagree about how they render tool
// arguments.
func coerceParam(p queryToolParam, v any) (any, error) {
	switch mustParamType(p.Type) {
	case paramInt:
		switch x := v.(type) {
		case float64:
			if x != math.Trunc(x) {
				return nil, fmt.Errorf("%v is not a whole number", x)
			}
			return int64(x), nil
		case string:
			n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%q is not an integer", x)
			}
			return n, nil
		default:
			return nil, fmt.Errorf("expected an integer, got %T", v)
		}
	case paramNumber:
		switch x := v.(type) {
		case float64:
			return x, nil
		case string:
			f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
			if err != nil {
				return nil, fmt.Errorf("%q is not a number", x)
			}
			return f, nil
		default:
			return nil, fmt.Errorf("expected a number, got %T", v)
		}
	case paramBool:
		switch x := v.(type) {
		case bool:
			return x, nil
		case string:
			b, err := strconv.ParseBool(strings.TrimSpace(x))
			if err != nil {
				return nil, fmt.Errorf("%q is not a boolean", x)
			}
			return b, nil
		default:
			return nil, fmt.Errorf("expected a boolean, got %T", v)
		}
	case paramDate:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("expected a date string, got %T", v)
		}
		t, err := parseDateArg(s, time.Now())
		if err != nil {
			return nil, err
		}
		// civil.Date binds as SQL `date`. A bare time.Time would go over as
		// datetimeoffset, which outranks the date/datetime columns it is
		// compared to and makes SQL Server convert the column side instead.
		return civil.DateOf(t), nil
	default: // string
		if s, ok := v.(string); ok {
			return s, nil
		}
		return scalarString(v), nil
	}
}

// dateArgHint is how the initialize instructions state the accepted date
// forms, and dateArgPattern is the JSON Schema pattern advertised on every
// date parameter; the two must agree with parseDateArg.
const (
	dateArgHint    = "Accepts yyyy-mm-dd, or an offset from today: today, yesterday, -7d, -2w, -3m, -1y."
	dateArgPattern = `^(\d{4}-\d{2}-\d{2}|[Tt]oday|[Yy]esterday|[Tt]omorrow|[+-]?\d+[dDwWmMyY])$`
)

var relativeDateRe = regexp.MustCompile(`^([+-]?)(\d+)([dwmy])$`)

// parseDateArg accepts an absolute date (yyyy-mm-dd) or an offset from now:
// today, yesterday, tomorrow, or [+-]N followed by d/w/m/y for calendar days,
// weeks, months or years. Relative values resolve against now's calendar day,
// so -7d is midnight seven days ago. The grammar is deliberately small — each
// extra form is one more thing a small model can get half right.
func parseDateArg(s string, now time.Time) (time.Time, error) {
	in := strings.ToLower(strings.TrimSpace(s))
	if t, err := time.Parse("2006-01-02", in); err == nil {
		return t, nil
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch in {
	case "today":
		return today, nil
	case "yesterday":
		return today.AddDate(0, 0, -1), nil
	case "tomorrow":
		return today.AddDate(0, 0, 1), nil
	}
	m := relativeDateRe.FindStringSubmatch(in)
	if m == nil {
		return time.Time{}, fmt.Errorf("%q is not a date: use yyyy-mm-dd, today, yesterday, tomorrow, or an offset like -7d, -2w, -3m, -1y", s)
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return time.Time{}, fmt.Errorf("%q: offset is too large", s)
	}
	if m[1] == "-" {
		n = -n
	}
	switch m[3] {
	case "d":
		return today.AddDate(0, 0, n), nil
	case "w":
		return today.AddDate(0, 0, 7*n), nil
	case "m":
		return addMonthsClamped(today, n), nil
	default: // y
		return addMonthsClamped(today, 12*n), nil
	}
}

// addMonthsClamped moves t by n months, pinning the day to the target month's
// last day when the source day does not exist there — Mar 31 minus one month
// is Feb 28, not (as time.AddDate normalizes it) Mar 3.
func addMonthsClamped(t time.Time, n int) time.Time {
	first := time.Date(t.Year(), t.Month()+time.Month(n), 1, 0, 0, 0, 0, t.Location())
	last := first.AddDate(0, 1, -1).Day()
	day := t.Day()
	if day > last {
		day = last
	}
	return time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, t.Location())
}

// resolveColumns maps a requested column list onto the names have actually
// offers (case-insensitively, preserving have's spelling so the value can
// still be looked up). An empty want list passes have through unchanged.
// haveNoun names what have is, for the error message — "in the result set"
// when have is a query's output columns, "one of this tool's columns" when
// have is a tool's configured menu.
func resolveColumns(want, have []string, haveNoun string) ([]string, error) {
	if len(want) == 0 {
		return have, nil
	}
	out := make([]string, 0, len(want))
	for _, w := range want {
		match := ""
		for _, h := range have {
			if strings.EqualFold(h, w) {
				match = h
				break
			}
		}
		if match == "" {
			return nil, fmt.Errorf("column %q is not %s (columns: %s)",
				w, haveNoun, strings.Join(have, ", "))
		}
		out = append(out, match)
	}
	return out, nil
}

// projected returns res narrowed to cols, or res unchanged when cols is
// empty. The rows keep their full maps; only Columns — which every renderer
// iterates — is rewritten.
func projected(cols []string, res *queryResult) (*queryResult, error) {
	if len(cols) == 0 {
		return res, nil
	}
	resolved, err := resolveColumns(cols, res.Columns, "in the result set")
	if err != nil {
		return nil, err
	}
	clone := *res
	clone.Columns = resolved
	return &clone, nil
}

func renderCSV(spec *queryToolSpec, cols []string, res *queryResult) (*mcp.CallToolResult, error) {
	res, err := projected(cols, res)
	if err != nil {
		return toolErrorf("%v", err)
	}
	res = withRowNumbers(res, ownPositions)
	text, err := csvText(res)
	if err != nil {
		return toolErrorf("encoding CSV: %v", err)
	}
	out := baseOutput(spec, res, "csv")
	out.CSV = text
	return toolResult(text, out)
}

func renderMarkdown(spec *queryToolSpec, cols []string, res *queryResult) (*mcp.CallToolResult, error) {
	res, err := projected(cols, res)
	if err != nil {
		return toolErrorf("%v", err)
	}
	res = withRowNumbers(res, ownPositions)
	text := markdownTable(res)
	out := baseOutput(spec, res, "md")
	out.Markdown = text
	return toolResult(text, out)
}

// renderTable dispatches to the csv or md renderer.
func renderTable(spec *queryToolSpec, cols []string, res *queryResult) (*mcp.CallToolResult, error) {
	if spec.format == formatMD {
		return renderMarkdown(spec, cols, res)
	}
	return renderCSV(spec, cols, res)
}

// recordPickID is the input-request ID under which the "which record?"
// elicitation and its answer travel.
const recordPickID = "pick_record"

// recordChoice is what renderScalar / renderPickRecord stash in
// CallToolResult.RequestState so the retried call can produce the answer
// without re-running the query. It carries only the fields that will actually
// be returned — for a pick-record tool that is the projected subset, not the
// whole result set. The client echoes it back verbatim; a tampered or
// truncated value only ever selects a different already-returned candidate,
// and the index is bounds-checked on the way back in.
type recordChoice struct {
	// Columns is the output column order for a pick-record tool; empty for a
	// scalar one.
	Columns []string `json:"columns,omitempty"`
	// Rows are the candidate records, each already reduced to the fields that
	// will be returned.
	Rows []map[string]any `json:"rows"`
	// ScalarColumn is set only for the scalar path; it names the field in each
	// row that holds the value.
	ScalarColumn string `json:"scalarColumn,omitempty"`
	// Args are the arguments of the call that found the candidates, kept so
	// the chosen record can be stored with its provenance.
	Args map[string]any `json:"args,omitempty"`
	// AllColumns is the result's every column when the chosen record is to be
	// stored, of which Columns are the ones shown.
	AllColumns []string `json:"allColumns,omitempty"`
}

func renderScalar(req *mcp.CallToolRequest, spec *queryToolSpec, res *queryResult) (*mcp.CallToolResult, error) {
	col, err := scalarColumn(spec, res)
	if err != nil {
		return toolErrorf("%v", err)
	}

	switch len(res.Rows) {
	case 0:
		return toolErrorf("scalar output: the query returned no rows, so there is no value to return")
	case 1:
		return scalarResult(spec, res, col, res.Rows[0][col], 0)
	}

	// More than one row: the tool cannot decide which value is "the" value, so
	// the user is asked.
	state := recordChoice{ScalarColumn: col, Rows: make([]map[string]any, len(res.Rows))}
	for i, row := range res.Rows {
		state.Rows[i] = map[string]any{col: row[col]}
	}
	message := fmt.Sprintf(
		"%q returned %d rows, but it is configured to return a single %s value. Choose the record to take the value from.",
		spec.Name, len(res.Rows), col)
	return elicitRecordChoice(req, "scalar output", recordLabels(res), message, state)
}

// pickRequested says how a pick-record render came about. The zero value is
// the `pickRecord: true` tool, where reducing to one record is all the tool
// ever does. opted marks a `pickRecord: optional` call that set RequireSingle,
// which changes two edges: no rows is an ordinary empty result rather than an
// error (the requirement was "at most one", not "exactly one"), and the
// no-elicitation error points at the switch the caller can drop. pageSize,
// when known, lets the elicitation warn that a full page may not be the
// whole candidate set.
type pickRequested struct {
	opted    bool
	pageSize int
	// keep, when set, stores the chosen record under a handle.
	keep *pickKeep
}

// pickKeep is what storing a chosen record needs: the store, whose call it
// was, and the arguments that say what the record is.
type pickKeep struct {
	cfg        *config
	owner      string
	identified bool
	args       map[string]any
}

// storedPick stores one chosen record as a result of its own and replies with
// it and its handle.
func storedPick(keep *pickKeep, spec *queryToolSpec, cols, all []string, row map[string]any) (*mcp.CallToolResult, error) {
	res := &queryResult{Columns: all, Rows: []map[string]any{row}, RowCount: 1}
	r := newStoredResult(keep.cfg, res, provenance{Tool: spec.Name, Args: keep.args}, "", lineageOf())
	r.hidden = hiddenColumns(all, cols)
	var notes []string
	if why := storeFor(keep.cfg, keep.owner, keep.identified, r); why != "" {
		notes = append(notes, why)
	}
	return storedReply(keep.cfg, r, replyOpts{
		tool:   spec.Name,
		server: serverOfGroup(keep.cfg, spec.Group),
		format: spec.format,
		cols:   cols,
		notes:  notes,
	})
}

// renderPickRecord returns one record out of a csv/md tool's result set. A
// single row comes straight back; several rows are shown in full for the user
// to review, and only the chosen one — projected to cols — is returned.
func renderPickRecord(req *mcp.CallToolRequest, spec *queryToolSpec, cols []string, res *queryResult, how pickRequested) (*mcp.CallToolResult, error) {
	outCols, err := resolveColumns(cols, res.Columns, "in the result set")
	if err != nil {
		return toolErrorf("pick-record: %v", err)
	}

	failNoun := "pick-record"
	if how.opted {
		failNoun = requireSingleParamName
	}

	switch len(res.Rows) {
	case 0:
		if how.opted {
			return renderTable(spec, cols, res)
		}
		return toolErrorf("pick-record: the query returned no rows")
	case 1:
		if how.keep != nil {
			return storedPick(how.keep, spec, outCols, res.Columns, res.Rows[0])
		}
		return renderChosenRecord(spec, outCols, res.Rows[0])
	}

	state := recordChoice{Columns: outCols, Rows: make([]map[string]any, len(res.Rows))}
	if how.keep != nil {
		// The whole row is kept, ids and all, so the chosen record can be
		// stored with the ids a handle passes on; only outCols are shown.
		state.Args = how.keep.args
		state.AllColumns = res.Columns
		copy(state.Rows, res.Rows)
	} else {
		for i, row := range res.Rows {
			state.Rows[i] = reduceRow(row, outCols)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%q returned %d rows. Choose the record to return; only these fields come back: %s.",
		spec.Name, len(res.Rows), strings.Join(outCols, ", "))
	if res.Truncated {
		b.WriteString(" The result set was cut off at the server's row budget, so the one you want may not be listed.")
	}
	if how.pageSize > 0 && len(res.Rows) >= how.pageSize {
		fmt.Fprintf(&b, " This is one full page of %d, so there may be more matches than are listed. If the one you want is missing, cancel and ask for the search to be repeated with a larger PageSize.", how.pageSize)
	}
	if how.opted && !clientCanElicit(req) {
		return toolErrorf(
			"%s: the query returned %d rows and this client cannot be asked which one to use; either narrow the search or call again without %s to receive the full list",
			failNoun, len(res.Rows), requireSingleParamName)
	}
	return elicitRecordChoice(req, failNoun, recordLabels(res), b.String(), state)
}

// recordLabels is one picker label per row: its 1-based number and its
// non-null values in result-set order, joined by a separator, clipped so a
// long row stays a readable option. The labels are all the person sees of the
// candidates — the message deliberately doesn't repeat them — so every column
// the query selected goes in, and a tool that wants a good picker selects
// the fields that tell its records apart.
func recordLabels(res *queryResult) []string {
	const maxLabel = 160
	labels := make([]string, len(res.Rows))
	for n, row := range res.Rows {
		parts := make([]string, 0, len(res.Columns))
		for _, c := range res.Columns {
			v := row[c]
			if v == nil {
				continue
			}
			if str := strings.TrimSpace(fmt.Sprint(v)); str != "" {
				parts = append(parts, str)
			}
		}
		label := fmt.Sprintf("%d — %s", n+1, strings.Join(parts, " · "))
		if r := []rune(label); len(r) > maxLabel {
			label = string(r[:maxLabel-1]) + "…"
		}
		labels[n] = label
	}
	return labels
}

// reduceRow copies just cols out of a row.
func reduceRow(row map[string]any, cols []string) map[string]any {
	out := make(map[string]any, len(cols))
	for _, c := range cols {
		out[c] = row[c]
	}
	return out
}

// renderChosenRecord renders one record in the tool's table format, narrowed to
// cols. Projection inside the renderer is a no-op here — the row already
// carries exactly these fields.
func renderChosenRecord(spec *queryToolSpec, cols []string, row map[string]any) (*mcp.CallToolResult, error) {
	return renderTable(spec, cols, &queryResult{
		Columns:  cols,
		Rows:     []map[string]any{reduceRow(row, cols)},
		RowCount: 1,
	})
}

// elicitRecordChoice builds the input-required result that asks the user to
// pick one record out of several. message is the one-line framing shown above
// the form; labels, one per row, become the options of a titled-enum picker
// whose values are the 1-based record numbers; state is echoed back on the
// retry. The SDK carries the round-trip out — as a client
// request on the current protocol, or a server-issued elicitation on older
// ones.
func elicitRecordChoice(req *mcp.CallToolRequest, failNoun string, labels []string, message string, state recordChoice) (*mcp.CallToolResult, error) {
	n := len(labels)
	if !clientCanElicit(req) {
		return toolErrorf(
			"%s: the query returned %d rows and this client cannot be asked which one to use; narrow the query so it returns a single row",
			failNoun, n)
	}
	blob, err := json.Marshal(state)
	if err != nil {
		return toolErrorf("%s: preparing the record choice: %v", failNoun, err)
	}
	// A titled enum (oneOf of const+title) so a client renders the candidates
	// as a list to choose from. The elicitation spec only allows that shape
	// on a string property, so the values are the record numbers as strings;
	// pickedRow reads them back.
	options := make([]*jsonschema.Schema, n)
	for i, label := range labels {
		options[i] = &jsonschema.Schema{Const: anyPtr(strconv.Itoa(i + 1)), Title: label}
	}
	pick := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"row": {
				Type:        "string",
				Description: fmt.Sprintf("the record to use, 1 to %d", n),
				OneOf:       options,
			},
		},
		Required: []string{"row"},
	}
	return &mcp.CallToolResult{
		RequestState: string(blob),
		InputRequests: mcp.InputRequestMap{
			recordPickID: &mcp.ElicitParams{Message: message, RequestedSchema: pick},
		},
	}, nil
}

// elicitationAnswer returns the reply to the record-choice elicitation, if this
// call is the retry that carries one.
func elicitationAnswer(req *mcp.CallToolRequest) (*mcp.ElicitResult, bool) {
	if req.Params == nil || len(req.Params.InputResponses) == 0 {
		return nil, false
	}
	er, ok := req.Params.InputResponses[recordPickID].(*mcp.ElicitResult)
	return er, ok
}

// clientCanElicit reports whether the client declared the elicitation
// capability at initialize. Without it, neither the client-side nor the
// server-side round-trip can gather the record choice.
func clientCanElicit(req *mcp.CallToolRequest) bool {
	ip := req.Session.InitializeParams()
	return ip != nil && ip.Capabilities != nil && ip.Capabilities.Elicitation != nil
}

// resumeRecordPick produces the answer once the user has chosen a record. The
// full result set is gone by now; the candidates were carried in the request
// state.
func resumeRecordPick(cfg *config, spec *queryToolSpec, req *mcp.CallToolRequest, er *mcp.ElicitResult, owner string, identified bool) (*mcp.CallToolResult, error) {
	var state recordChoice
	dec := json.NewDecoder(strings.NewReader(req.Params.RequestState))
	dec.UseNumber()
	if err := dec.Decode(&state); err != nil {
		return toolErrorf("the record choice could not be resumed: %v", err)
	}
	if er.Action != "accept" || er.Content == nil {
		return toolErrorf("no record was chosen (%s), so there is no result to return", er.Action)
	}
	n, err := pickedRow(er.Content["row"], len(state.Rows))
	if err != nil {
		return toolErrorf("%v", err)
	}
	row := state.Rows[n-1]

	if state.ScalarColumn != "" {
		val := row[state.ScalarColumn]
		return toolResult(scalarString(val), &queryToolOutput{
			Tool:        spec.Name,
			Format:      "scalar",
			RowCount:    len(state.Rows),
			Value:       val,
			ValueColumn: state.ScalarColumn,
			ValueRow:    n,
		})
	}
	if cfg.handlesOn() && spec.format != formatScalar {
		keep := &pickKeep{cfg: cfg, owner: owner, identified: identified, args: state.Args}
		return storedPick(keep, spec, state.Columns, state.AllColumns, row)
	}
	return renderChosenRecord(spec, state.Columns, row)
}

// scalarColumn decides which column a scalar value is read from: the configured
// one if set, otherwise the sole column. More than one column and nothing
// configured is an error, per the spec.
func scalarColumn(spec *queryToolSpec, res *queryResult) (string, error) {
	if spec.ResultColumn != "" {
		for _, c := range res.Columns {
			if strings.EqualFold(c, spec.ResultColumn) {
				return c, nil
			}
		}
		return "", fmt.Errorf("scalar output: the configured result column %q is not in the result set (columns: %s)",
			spec.ResultColumn, strings.Join(res.Columns, ", "))
	}
	if len(res.Columns) == 1 {
		return res.Columns[0], nil
	}
	return "", fmt.Errorf("scalar output: the query returned %d columns (%s) and no result column is configured; set \"resultColumn\" on this tool",
		len(res.Columns), strings.Join(res.Columns, ", "))
}

func scalarResult(spec *queryToolSpec, res *queryResult, col string, val any, rowNo int) (*mcp.CallToolResult, error) {
	out := &queryToolOutput{
		Tool:        spec.Name,
		Format:      "scalar",
		RowCount:    res.RowCount,
		Truncated:   res.Truncated,
		Notes:       res.Notes,
		Value:       val,
		ValueColumn: col,
		ValueRow:    rowNo,
	}
	return toolResult(scalarString(val), out)
}

func baseOutput(spec *queryToolSpec, res *queryResult, format string) *queryToolOutput {
	return &queryToolOutput{
		Tool:      spec.Name,
		Format:    format,
		Columns:   res.Columns,
		RowCount:  res.RowCount,
		Truncated: res.Truncated,
		Notes:     res.Notes,
	}
}

func toolResult(text string, out *queryToolOutput) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: text}},
		StructuredContent: out,
	}, nil
}

// toolErrorf builds a tool-level error result: the model sees it and can
// correct itself, rather than the call failing at the protocol level.
func toolErrorf(format string, args ...any) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}, nil
}

// markdownTable renders a result set the way the built-in query tool's text
// channel does: a pipe table, then the row count.
func markdownTable(res *queryResult) string {
	if len(res.Columns) == 0 {
		return "(no result set)\n"
	}
	var b strings.Builder
	b.WriteString(markdownRows(res))
	fmt.Fprintf(&b, "\n%d row(s).\n", res.RowCount)
	for _, note := range res.Notes {
		fmt.Fprintf(&b, "Note: %s\n", note)
	}
	return b.String()
}

// markdownRows is the pipe table alone, without the row count and notes.
func markdownRows(res *queryResult) string {
	var b strings.Builder
	writeTableRow(&b, res.Columns)
	sep := make([]string, len(res.Columns))
	for i := range sep {
		sep[i] = "---"
	}
	writeTableRow(&b, sep)
	cells := make([]string, len(res.Columns))
	for _, row := range res.Rows {
		for i, name := range res.Columns {
			cells[i] = formatCell(row[name])
		}
		writeTableRow(&b, cells)
	}
	return b.String()
}

func pickedRow(v any, n int) (int, error) {
	var idx int
	switch x := v.(type) {
	case float64:
		if x != math.Trunc(x) {
			return 0, fmt.Errorf("%v is not a record number", x)
		}
		idx = int(x)
	case json.Number:
		i, err := x.Int64()
		if err != nil {
			return 0, fmt.Errorf("%v is not a record number", x)
		}
		idx = int(i)
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return 0, fmt.Errorf("%q is not a record number", x)
		}
		idx = i
	case int:
		idx = x
	default:
		return 0, errors.New("no record number was provided")
	}
	if idx < 1 || idx > n {
		return 0, fmt.Errorf("record number %d is out of range 1 to %d", idx, n)
	}
	return idx, nil
}

// scalarString renders one value as the plain text of a CSV cell or a scalar
// answer. NULL becomes an empty string, which is the CSV convention and is
// unambiguous for a single value.
func scalarString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case json.Number:
		return x.String()
	default:
		return fmt.Sprint(x)
	}
}
