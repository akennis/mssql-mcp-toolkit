package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The describe tool is the other half of keeping tool descriptions short.
// Every tool's description and schema is in the model's context on every
// turn, whether or not the tool is called, so each one is held to a sentence
// or two on when to use it. The rest — what the columns mean, defaults, the
// caveats, the shared parameters — is served here, on request, for the one
// tool the model is about to call. With 65 tools that is the difference
// between ~28K tokens of definitions and under 10K.

// describeToolName is the name the describe tool is registered under: the
// server's prefix and the suffix, or the bare suffix when there is no prefix
// (allowed under --query-tools-only).
func describeToolName(cfg *config) string {
	if isBlank(cfg.toolPrefix) {
		return describeToolSuffix
	}
	return toolName(cfg.toolPrefix, describeToolSuffix)
}

// describeInput is the describe tool's one argument.
type describeInput struct {
	Tool string `json:"tool" jsonschema:"the tool's name, as listed"`
}

// registerDescribeTool adds the describe tool to a server that carries query
// tools. It answers for every tool in the file, not just the ones on this
// server, and says which server a sibling's tool lives on.
func registerDescribeTool(server *mcp.Server, cfg *config, specs []*queryToolSpec) {
	name := describeToolName(cfg)
	inputSchema, err := jsonschema.For[describeInput](nil)
	if err != nil {
		panic(fmt.Sprintf("building %s input schema: %v", name, err))
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        name,
		Title:       "Describe a tool",
		Description: "Full notes on one of this server's tools: every parameter with its meaning and default, the columns it returns, and when a sibling tool fits better. Call it before using a tool for the first time.",
		InputSchema: inputSchema,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  boolPtr(false),
		},
	}, func(_ context.Context, _ *mcp.CallToolRequest, in describeInput) (*mcp.CallToolResult, any, error) {
		spec := findQueryTool(cfg, in.Tool)
		if spec == nil {
			names := make([]string, 0, len(specs))
			for _, s := range specs {
				names = append(names, s.Name)
			}
			res, err := toolErrorf("no tool named %q; the tools on this server are: %s", in.Tool, strings.Join(names, ", "))
			return res, nil, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: describeQueryTool(cfg, spec)}},
		}, nil, nil
	})
}

// findQueryTool looks a tool up by the name a model is likely to hand back:
// the published name, the client's call name for it (Open WebUI's
// <server>_<tool>), or either with a stray namespace in front. Matching is
// case-insensitive.
func findQueryTool(cfg *config, name string) *queryToolSpec {
	want := strings.ToLower(strings.TrimSpace(name))
	if want == "" {
		return nil
	}
	for _, s := range cfg.queryTools {
		if strings.EqualFold(s.Name, want) || strings.EqualFold(queryToolCallName(cfg, s), want) {
			return s
		}
	}
	for _, s := range cfg.queryTools {
		if strings.HasSuffix(want, "_"+strings.ToLower(s.Name)) {
			return s
		}
	}
	return nil
}

// queryToolGroupOf is the group a spec belongs to, or nil.
func queryToolGroupOf(cfg *config, s *queryToolSpec) *queryToolGroup {
	for _, g := range cfg.queryToolGroups {
		if g.Name == s.Group {
			return g
		}
	}
	return nil
}

// queryToolCallName is the name the model calls a spec by under the
// configured --tool-call-name.
func queryToolCallName(cfg *config, s *queryToolSpec) string {
	server := ""
	if g := queryToolGroupOf(cfg, s); g != nil {
		server = groupServerName(g)
	}
	return toolCallName(cfg.toolCallName, server, s.Name)
}

// sharedParam is the root file's shared parameter of this name, or nil.
func sharedParam(cfg *config, name string) *queryToolParam {
	for i := range cfg.querySharedParams {
		if strings.EqualFold(cfg.querySharedParams[i].Name, name) {
			return &cfg.querySharedParams[i]
		}
	}
	return nil
}

// describeQueryTool renders one tool's full notes as markdown.
func describeQueryTool(cfg *config, s *queryToolSpec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s", queryToolCallName(cfg, s))
	if g := queryToolGroupOf(cfg, s); g != nil && len(cfg.queryToolGroups) > 1 {
		fmt.Fprintf(&b, " (on the %s server)", groupServerName(g))
	}
	b.WriteString("\n\n")
	b.WriteString(strings.TrimSpace(s.Description))
	b.WriteString("\n")
	if d := strings.TrimSpace(s.Details); d != "" {
		b.WriteString("\n")
		b.WriteString(d)
		b.WriteString("\n")
	}

	b.WriteString("\nParameters:\n")
	for _, p := range s.Parameters {
		desc := strings.TrimSpace(p.Description)
		if desc == "" {
			if sp := sharedParam(cfg, p.Name); sp != nil {
				desc = sp.Description
			}
		}
		typ := string(mustParamType(p.Type))
		if typ == string(paramDate) {
			desc = strings.TrimSpace(desc + " " + dateArgHint)
		}
		if p.batch && cfg.handlesOn() {
			desc = strings.TrimSpace(desc + " Also takes handle.Column: a stored result's values for that column; handle.Column[0], [-1], [0:5] take only the first, last or first five rows (Python-style, from 0).")
		}
		fmt.Fprintf(&b, "- %s (%s, %s)", p.Name, typ, requiredWord(p.required()))
		if desc != "" {
			fmt.Fprintf(&b, ": %s", desc)
		}
		b.WriteString("\n")
	}
	if cfg.handlesOn() && s.format != formatScalar && !s.PickRecord.always() {
		fmt.Fprintf(&b, "- %s (string, optional): a label for this result's handle.\n", saveAsParamName)
		if len(batchParamNames(s)) > 0 {
			fmt.Fprintf(&b, "- %s (boolean, optional): true to accept a handle marked truncated, or a literal list of ids that exactly matches a stored result's column - partial sample or complete - when that literal list is really what you mean.\n", allowPartialParamName)
		}
	}
	if s.PickRecord.optional() {
		fmt.Fprintf(&b, "- %s (boolean, optional): true when exactly one record is wanted; several matches are put to the user to choose from, and only the chosen record is returned. Pair it with a large PageSize so every plausible match is on offer.\n", requireSingleParamName)
	}
	if len(s.RequireAnyOf) > 0 {
		fmt.Fprintf(&b, "\nAt least one of %s must be given.\n", strings.Join(s.RequireAnyOf, ", "))
	}
	if s.PickRecord.always() {
		b.WriteString("\nReturns exactly one record: several matches are put to the user to choose from.\n")
	}

	if len(s.Columns) > 0 {
		fmt.Fprintf(&b, "\nColumns: %s\n", strings.Join(s.Columns, ", "))
	}
	fmt.Fprintf(&b, "\nOutput: %s", s.format)
	if s.ResultColumn != "" {
		fmt.Fprintf(&b, " (the %s column)", s.ResultColumn)
	}
	b.WriteString("\n")
	return b.String()
}

func requiredWord(required bool) string {
	if required {
		return "required"
	}
	return "optional"
}

// queryToolNotes is the text appended to a server's initialize instructions
// for the query tools it carries: the describe tool, the shared parameters
// those tools use, and the conventions (date forms, Columns, RequireSingle)
// that would otherwise be repeated on every tool's schema. Empty when the
// server carries no query tools.
func queryToolNotes(cfg *config, specs []*queryToolSpec) string {
	if len(specs) == 0 {
		return ""
	}
	var (
		usedShared    = map[string]bool{}
		hasDate       bool
		hasPickSingle bool
	)
	for _, s := range specs {
		for _, p := range s.Parameters {
			if p.shared && strings.TrimSpace(p.Description) == "" {
				usedShared[strings.ToLower(p.Name)] = true
			}
			if mustParamType(p.Type) == paramDate {
				hasDate = true
			}
		}
		hasPickSingle = hasPickSingle || s.PickRecord.optional()
	}

	// Tool names in the shared text are written as this server's model
	// calls them, as the descriptions were at load time.
	rewrite := cfg.queryToolRewrite
	if rewrite == nil {
		rewrite = func(text, _ string) string { return text }
	}
	fromGroup := specs[0].Group

	var b strings.Builder
	b.WriteString("About the query tools:\n")
	fmt.Fprintf(&b, "- %s returns a tool's full notes: every parameter with its meaning and default, and the columns it returns. Call it before using a tool for the first time.\n",
		toolCallName(cfg.toolCallName, notesServerName(cfg, specs), describeToolName(cfg)))
	if len(usedShared) > 0 {
		b.WriteString("- These parameters mean the same thing on every tool that takes them, so the tools do not describe them again:\n")
		for _, sp := range cfg.querySharedParams {
			if !usedShared[strings.ToLower(sp.Name)] {
				continue
			}
			fmt.Fprintf(&b, "  - %s: %s\n", sp.Name, rewrite(sp.Description, fromGroup))
		}
	}
	if hasDate {
		fmt.Fprintf(&b, "- Every date parameter accepts yyyy-mm-dd, or an offset from today: today, yesterday, -7d, -2w, -3m, -1y. Use the offset form for \"last week\" and the like; the server does the calendar arithmetic.\n")
	}
	if hasPickSingle {
		fmt.Fprintf(&b, "- %s: set it true when the question is about one particular record. If several match, the user is asked to choose and only that record is returned; pair it with a large PageSize so every plausible match is on offer.\n", requireSingleParamName)
	}
	return b.String()
}

// notesServerName is the {server} the describe tool's call name takes on a
// server carrying these specs: the group's name when they are one group's
// worth, blank on the base server.
func notesServerName(cfg *config, specs []*queryToolSpec) string {
	if len(specs) == 0 || specs[0].Group == "" {
		return ""
	}
	if g := queryToolGroupOf(cfg, specs[0]); g != nil {
		return groupServerName(g)
	}
	return ""
}
