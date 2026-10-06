package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Everything one instance uses to tell itself apart from its siblings.
//
// This binary is registered into the LLM client once per database, so
// configuration is the only thing distinguishing two copies of it — for the
// person reading the client's server list, and for the model choosing which of
// N otherwise-identical tools to call. The protocol does not help: a tool's
// wire object carries no server field, and server identity travels in a
// different message (initialize) entirely, so a client aggregating several of
// these servers has nothing to pair the two with. Clients that hit a duplicate
// tool name split three ways — namespace it, refuse to start, or silently let
// one shadow the other — and it is the third that this file exists to prevent,
// because it misroutes queries to the wrong database and produces
// plausible-looking answers with no diagnostic.

// The suffixes every tool name is composed from. A suffix added here widens
// the prefix length check below, so a prefix accepted today can be rejected
// once a longer tool name joins the set.
const (
	queryToolSuffix        = "query"
	getMetadataToolSuffix  = "get_metadata"
	listMetadataToolSuffix = "list_metadata"
	describeToolSuffix     = "describe_tool"
	showToolSuffix         = "show"
	showFieldToolSuffix    = "show_field"
)

var toolSuffixes = append([]string{
	queryToolSuffix,
	getMetadataToolSuffix,
	listMetadataToolSuffix,
	describeToolSuffix,
	showToolSuffix,
	showFieldToolSuffix,
}, operatorSuffixes...)

// toolName composes one tool's name from this server's prefix.
func toolName(prefix, suffix string) string { return prefix + "_" + suffix }

// defaultToolCallName is the --tool-call-name for a client that calls tools
// by the names the server lists.
const defaultToolCallName = "{tool}"

// checkToolCallName rejects a --tool-call-name that could not name a tool.
func checkToolCallName(template string) error {
	if !strings.Contains(template, "{tool}") {
		return fmt.Errorf("--tool-call-name %q must contain {tool}", template)
	}
	return nil
}

// toolCallName is the name the model calls a tool by under the given
// --tool-call-name template. A tool with no group (or a client that does not
// namespace) is called by its published name alone: {server} has no value on
// the base server, whose label is decided elsewhere.
func toolCallName(template, server, tool string) string {
	if server == "" || template == "" {
		return tool
	}
	return strings.NewReplacer("{server}", server, "{tool}", tool).Replace(template)
}

// normalizePrefix drops the trailing separator an operator naturally writes
// when they think of the prefix as a stem, so that --tool-prefix=sales_ and
// --tool-prefix=sales name the same tools rather than differing by an
// invisible underscore.
func normalizePrefix(prefix string) string {
	return strings.TrimRight(strings.TrimSpace(prefix), "_-")
}

// checkToolPrefix rejects a prefix that composes a name no client will accept.
// It is the composed name that is checked, not the prefix: one comfortably
// inside the 64-character limit can still push <prefix>_list_metadata past
// it, and a client that namespaces again (mcp__sales__sales_query) eats more
// of the budget still, so short prefixes are worth the nagging.
func checkToolPrefix(prefix string) error {
	// The longest offending name is the one reported: it is the binding
	// constraint, and naming a shorter one would have the operator shorten the
	// prefix, restart, and hit the same error again.
	worst := ""
	for _, suffix := range toolSuffixes {
		if name := toolName(prefix, suffix); !toolNamePattern.MatchString(name) && len(name) > len(worst) {
			worst = name
		}
	}
	if worst == "" {
		return nil
	}
	return fmt.Errorf("--tool-prefix %q composes the tool name %q, which must be 1-64 characters of letters, digits, _ or -", prefix, worst)
}

// missingPrefixError is the startup failure when no prefix is configured.
//
// The failure a prefix guards against — a client silently shadowing one
// server's tool with another's — is invisible at runtime, so it is made loud
// here instead. That is also why there is no derived default: one would paper
// over exactly the misconfiguration worth shouting about.
func missingPrefixError(connString string) error {
	const base = "no tool prefix: pass --tool-prefix. " +
		"Every tool this server exposes is named after it (<prefix>_query), so that two databases registered in the same client cannot shadow each other"
	if suggestion := suggestPrefix(databaseName(connString)); suggestion != "" {
		return fmt.Errorf("%s; try --tool-prefix=%s", base, suggestion)
	}
	return errors.New(base)
}

// suggestPrefix turns a database name into a prefix worth suggesting, or ""
// when nothing usable comes out of it. Only what a tool name admits survives;
// anything else collapses to a single separator, because a suggestion the
// operator cannot paste verbatim is worse than no suggestion at all.
func suggestPrefix(database string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(database)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
			continue
		}
		if s := b.String(); s != "" && !strings.HasSuffix(s, "_") {
			b.WriteByte('_')
		}
	}
	prefix := normalizePrefix(b.String())
	if prefix == "" || checkToolPrefix(prefix) != nil {
		return ""
	}
	return prefix
}

// databaseName reads the database out of a connection string. It returns ""
// when the string names none, which is legitimate: a trusted_connection string
// need not carry one, and the login's default database applies.
//
// All three spellings are handled, since the caller writes whichever form
// their password and authentication scheme pushed them towards: the URL form's
// ?database=, and the ADO form's database= and Initial Catalog=.
func databaseName(connString string) string {
	s := strings.TrimSpace(connString)
	if u, err := url.Parse(s); err == nil && strings.EqualFold(u.Scheme, "sqlserver") {
		for key, values := range u.Query() {
			if strings.EqualFold(key, "database") && len(values) > 0 {
				return strings.TrimSpace(values[0])
			}
		}
		return ""
	}
	for _, part := range strings.Split(s, ";") {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "database", "initial catalog":
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// displayDatabase is the database name this server calls itself by in the text
// a model reads. --descr-db is what the people asking the questions call the
// database, which is often not what the connection string calls it: a catalog
// spelled SALES_DW_PRD01 is "the sales warehouse" to everyone who asks it
// anything, and it is the second name a model has any chance of matching a
// question against. The connection string's own name is the fallback, so an
// operator who has nothing to correct sets nothing.
func displayDatabase(cfg *config) string {
	if name := strings.TrimSpace(cfg.descrDatabase); name != "" {
		return name
	}
	return databaseName(cfg.connString)
}

// toolRef names one of this server's tools for use inside a description. It
// falls back to the bare suffix rather than composing "_query" when no prefix
// is configured, so that a description built without one still reads.
func toolRef(prefix, suffix string) string {
	p := normalizePrefix(prefix)
	if p == "" {
		return suffix
	}
	return toolName(p, suffix)
}

// genericToolDescription describes the query tool when there is no database
// name to describe — neither --descr-db nor the connection string gives one.
// It is the fallback, not the norm.
const genericToolDescription = "Run a single T-SQL query against the configured Microsoft SQL Server database and return its result set. " +
	"Call list_metadata to see which tables and views are documented, and get_metadata for one object's documentation, rather than guessing table names or querying for them. " +
	"Schema-qualify object names (dbo.Orders), pass one statement per call, and prefer TOP or a WHERE clause to asking for a whole table. " +
	"Only the query argument is required; max_rows is optional and is normally left out."

// defaultQueryToolDescription names the database in the text the model reads
// when it decides which tool to call.
//
// This carries more weight than the prefix does. The prefix solves collision;
// the description solves selection — which database the model reaches for —
// and it is the more reliable of the two signals, passing through verbatim in
// every client while names get rewritten or namespaced. "sales_query" against
// "hr_query" is a thin thing to route on; naming the database and what lives
// in it is not.
func defaultQueryToolDescription(database, prefix string) string {
	if database == "" {
		return genericToolDescription
	}
	// return fmt.Sprintf("Query the %s database on Microsoft SQL Server: send one T-SQL statement and get its result set back. "+
	// 	"Use this tool for any question whose answer lives in %s — its tables, views and reference data. "+
	// 	"Call %s to see which tables and views in %s are documented, and %s for one object's documentation, rather than guessing table names or querying for them. "+
	// 	"Schema-qualify object names (dbo.Orders), pass one statement per call, and prefer TOP or a WHERE clause to asking for a whole table. "+
	// 	"Only the query argument is required; max_rows is optional and is normally left out.",
	// 	database, database,
	// 	toolRef(prefix, listMetadataToolSuffix), database, toolRef(prefix, getMetadataToolSuffix))
	return fmt.Sprintf("Query the %s data on Microsoft SQL Server: send one T-SQL statement and get its result set back", database)
}

// The metadata tools serve the documentation an operator wrote for this
// database's tables and views. Like the query tool's description, the default
// names the database whenever one is known, so a model with several servers in
// its list can tell whose documentation it is reaching for.
const (
	genericGetMetadataToolDescription = "Retrieve custom markdown or html metadata/documentation for a specific database table or view by its qualified name (schema.table or schema.view)."

	genericListMetadataToolDescription = "List all database tables and views that have associated custom metadata/documentation files (markdown or html)."
)

// defaultGetMetadataToolDescription names the database in the get_metadata
// tool's description, on the same reasoning as
// defaultQueryToolDescription: selection between servers is decided on this
// text more reliably than on the tool name.
func defaultGetMetadataToolDescription(database string) string {
	if database == "" {
		return genericGetMetadataToolDescription
	}
	return fmt.Sprintf("Retrieve custom markdown or html metadata/documentation for a specific table or view in the %s database, by its qualified name (schema.table or schema.view). "+
		"Read it before querying an unfamiliar table: it is what the operator wrote about that object, which INFORMATION_SCHEMA cannot tell you.",
		database)
}

// defaultListMetadataToolDescription is the list_metadata counterpart of
// defaultGetMetadataToolDescription.
func defaultListMetadataToolDescription(database, prefix string) string {
	if database == "" {
		return genericListMetadataToolDescription
	}
	return fmt.Sprintf("List the tables and views in the %s database that have custom metadata/documentation (markdown or html) available. "+
		"Takes no arguments; use it to find out what documentation exists before asking for one object's with %s.",
		database, toolRef(prefix, getMetadataToolSuffix))
}

// defaultInstructions is delivered once at initialize and shapes the whole
// session, which makes it the strongest per-database lever available: it is
// the one place to say which database this instance is and how to approach it,
// before the model has chosen a tool or written a statement.
func defaultInstructions(cfg *config, database string) string {
	if cfg.queryToolsOnly {
		return queryToolsOnlyInstructions(cfg, database)
	}

	subject := "one Microsoft SQL Server database"
	if database != "" {
		subject = fmt.Sprintf("the %s database on Microsoft SQL Server", database)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "This server exposes %s. You can query it using the %s tool.\n",
		subject, toolName(cfg.toolPrefix, queryToolSuffix))
	fmt.Fprintf(&b, "You can also retrieve custom table/view documentation using %s and %s.\n\n",
		toolName(cfg.toolPrefix, listMetadataToolSuffix), toolName(cfg.toolPrefix, getMetadataToolSuffix))
	b.WriteString("How to work with it:\n")
	fmt.Fprintf(&b, "- Find your tables through the metadata tools, not by querying for them. %s lists the documented tables and views, and %s returns what the operator wrote about one of them. Guessing a name usually costs a turn to \"Invalid object name\".\n",
		toolName(cfg.toolPrefix, listMetadataToolSuffix), toolName(cfg.toolPrefix, getMetadataToolSuffix))
	b.WriteString("- Schema-qualify object names (dbo.Orders, Sales.SalesOrderHeader). An unqualified name resolves against the login's default schema, which is rarely the one you want.\n")
	b.WriteString("- One statement per call.\n")
	b.WriteString("- Prefer TOP and a WHERE clause to fetching a whole table; an oversized result set is truncated and the rest is lost.\n")
	if cfg.readOnly {
		b.WriteString("- This server is read-only: only SELECT and WITH statements are accepted.\n")
	}
	if database != "" {
		fmt.Fprintf(&b, "- This server reaches %s and nothing else. Another database means another server in your tool list, under its own prefix; USE does not carry over between calls.\n", database)
	} else {
		b.WriteString("- This server reaches one database and nothing else. Another database means another server in your tool list, under its own prefix; USE does not carry over between calls.\n")
	}
	return b.String()
}

// queryToolsOnlyInstructions is the initialize text for a server started with
// --query-tools-only: the built-in query and metadata tools are gone, so the
// model has only the operator-defined tools and cannot compose SQL of its own.
func queryToolsOnlyInstructions(cfg *config, database string) string {
	subject := "one Microsoft SQL Server database"
	if database != "" {
		subject = fmt.Sprintf("the %s database on Microsoft SQL Server", database)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "This server exposes %s through a fixed set of purpose-built tools.\n", subject)
	b.WriteString("Each tool runs a query the operator has already written and tested; you supply its parameter values and nothing else.\n\n")
	b.WriteString("How to work with it:\n")
	b.WriteString("- Choose the tool whose description matches the question. You cannot write or run arbitrary SQL through this server.\n")
	b.WriteString("- If no tool fits the question, say so rather than forcing an answer out of one that does not.\n")
	if database != "" {
		fmt.Fprintf(&b, "- This server reaches %s and nothing else.\n", database)
	}
	return b.String()
}

// groupInstructions is the initialize text for one tool group's server. Like
// --query-tools-only, the model has only a fixed set of purpose-built tools;
// unlike it, the rest of the database's tools are a call away on sibling
// servers, so the text says to look there before giving up.
func groupInstructions(cfg *config, g *queryToolGroup, database string) string {
	if s := strings.TrimSpace(g.Instructions); s != "" {
		return s
	}
	if g.Operators && len(specsInGroup(cfg.queryTools, g.Name)) == 0 {
		return operatorsGroupInstructions(cfg, g, database)
	}

	subject := "one Microsoft SQL Server database"
	if database != "" {
		subject = fmt.Sprintf("the %s database on Microsoft SQL Server", database)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "This server exposes the %q tools for %s.\n", g.Name, subject)
	if d := strings.TrimSpace(g.Description); d != "" {
		b.WriteString(d)
		if !strings.HasSuffix(d, ".") {
			b.WriteByte('.')
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nHow to work with it:\n")
	b.WriteString("- Each tool runs a query the operator has already written and tested; you supply its parameter values and nothing else.\n")
	b.WriteString("- Choose the tool whose description matches the question. You cannot write or run arbitrary SQL through this server.\n")
	b.WriteString("- The rest of this database's tools are grouped onto sibling servers in your list. If nothing here fits, look there before saying it cannot be answered.\n")
	// Name the pattern the model must reproduce, with a real example: a
	// small model copies an example far more reliably than it applies a
	// rule. The stem is whatever the names share — the client's server
	// namespace, the group prefix, or both.
	if stem := groupCallStem(cfg, g); stem != "" {
		fmt.Fprintf(&b, "- Every tool on this server is called %s…", stem)
		if specs := specsInGroup(cfg.queryTools, g.Name); len(specs) > 0 {
			fmt.Fprintf(&b, " (for example %s)", toolCallName(cfg.toolCallName, groupServerName(g), specs[0].Name))
		}
		b.WriteString("; a tool whose name starts differently belongs to a sibling server, and a description that says \"(on the X server)\" after a tool name is telling you which one.\n")
	}
	return b.String()
}

// groupCallStem is the leading text every call name on a group's server
// shares under the configured --tool-call-name: the template's text before
// {tool} with {server} filled in, then the group's prefix. Empty when nothing
// in the name identifies the server.
func groupCallStem(cfg *config, g *queryToolGroup) string {
	head, _, _ := strings.Cut(cfg.toolCallName, "{tool}")
	stem := strings.ReplaceAll(head, "{server}", groupServerName(g))
	if g.Prefix != "" {
		stem += g.Prefix + "_"
	}
	return stem
}

// serverLabelFor names this instance in the client's server list. Left to the
// hardcoded program name, N registrations of one binary show the same name N
// times and the person reading the list cannot tell which is which.
func serverLabelFor(cfg *config) string {
	if label := strings.TrimSpace(cfg.serverLabel); label != "" {
		return label
	}
	if cfg.toolPrefix != "" {
		return serverName + "-" + cfg.toolPrefix
	}
	return serverName
}
