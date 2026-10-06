# sqlserver-mcp

An MCP (Model Context Protocol) server in Go that gives an LLM client one tool:
send a single T-SQL query to Microsoft SQL Server, get the result set back as
JSON. The connection string is supplied by the client as configuration, so the
same binary can be pointed at any database.

**One server process serves exactly one database.** Point it at a second
database by registering the binary a second time, with its own connection
string and its own `--tool-prefix`. See [Several databases](#several-databases).

- Transports: stdio by default (what VS Code extensions such as Continue.dev
  launch), or streamable HTTP on localhost with `--transport=http`. The two are
  exclusive; see [Execution modes](#execution-modes)
- SDK: [`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk)
- Driver: [`github.com/microsoft/go-mssqldb`](https://github.com/microsoft/go-mssqldb)

## Build

```bash
go build -o sqlserver-mcp .
# or install onto your PATH
go install .
```

Requires Go 1.25.7+. Cross-compile for another machine with, e.g.,
`GOOS=windows GOARCH=amd64 go build -o sqlserver-mcp.exe .`

## Configuration

**Command-line flags are the only configuration input.** The server reads no
environment variables at all — not for the connection string, not for anything
else. With one process per database, a setting that can arrive from two places
is a setting you have to check in two places when the wrong database answers a
question.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--conn-string` | *(required)* | SQL Server connection string |
| `--tool-prefix` | *(required, except with `--query-tools-only`)* | Prefix for every built-in tool this server exposes: `--tool-prefix=sales` gives `sales_query` |
| `--transport` | `stdio` | Execution mode: `stdio`, or `http` to listen on localhost instead |
| `--http-addr` | `127.0.0.1:8080` | Loopback address `--transport=http` listens on. **Rejected under `--transport=stdio`** |
| `--http-stateless` | `false` | With `--transport=http`, serve without per-session state, for clients that do not send `Mcp-Session-Id` back. `GET`/`DELETE` then return 405 |
| `--http-log` | `false` | With `--transport=http`, log one line per HTTP request: method, host, session id, status |
| `--http-log-headers` | `false` | With `--transport=http`, log every HTTP header received with each tool call, one line per header. Verbose; `Authorization`, `Proxy-Authorization` and `Cookie` values are logged as `[redacted]` |
| `--http-auth-token-file` | *(none)* | With `--transport=http`, a file holding a shared secret. Every request on every listener must then carry `Authorization: Bearer <secret>` or gets a `401`. See [Authentication and per-user results](#authentication-and-per-user-results) |
| `--user-header` | *(none)* | With `--transport=http`, the header carrying the calling user's id (Open WebUI: `X-OpenWebUI-User-Id`). Stored results are kept per user; a call without the header gets its rows but no handle |
| `--result-store` | `true` | Store every list result under a short handle for paging, `@handle.Column` arguments and the operator tools. `false` turns handles off and restores the plain replies. See [Stored results](#stored-results-handles-and-operators) |
| `--result-store-max-rows` | `100000` | Most rows one stored result keeps; a result cut here is marked truncated |
| `--result-store-max-bytes` | `1073741824` (1 GiB) | Memory the store may use for every user together; least recently used results are dropped past it |
| `--result-store-user-bytes` | `268435456` (256 MiB) | Memory one user's stored results may use |
| `--result-store-ttl` | `2h` | How long a stored result is kept after it was last used |
| `--calc-sql` | `false` | Also expose `<prefix>_calc_sql`, a read-only SQLite `SELECT` over stored results |
| `--max-rows` | `200` | Row cap per query; extra rows are dropped and flagged. `0` means no limit |
| `--max-bytes` | `262144` (256 KiB) | Cap on the total size of the returned rows, in bytes of JSON. `0` means no limit |
| `--max-cell-bytes` | `4096` (4 KiB) | Cap on a single returned value; longer values are cut and marked in place. `0` means no limit |
| `--max-stored-cell-bytes` | `65536` (64 KiB) | The same cap for a value kept in a stored result, which is what `show_field` can return. `0` means no limit |
| `--display-cell-chars` | `200` | Longest value, in characters, that any reply over a stored result shows. A longer one ends `…[+N chars, row R]`; `show_field` returns the rest. `0` shows values whole |
| `--query-timeout` | `30s` | Per-query timeout |
| `--read-only` | `false` | Reject anything that is not `SELECT`/`WITH` |
| `--max-open-conns` | `4` | Connection pool size |
| `--descr-db` | *(the database in `--conn-string`)* | The database name the descriptions and instructions use, for when people know this database by a different name than the connection string does |
| `--query-fn-desc` | *(names the database)* | Description the query tool advertises to the model |
| `--get-metadata-fn-desc` | *(names the database)* | Description the `get_metadata` tool advertises to the model |
| `--list-metadata-fn-desc` | *(names the database)* | Description the `list_metadata` tool advertises to the model |
| `--server-label` | `sqlserver-mcp-<prefix>` | Name this server reports at initialize, i.e. in the client's server list |
| `--instructions` | *(describes this database)* | Instructions delivered to the model once, at initialize |
| `--query-tools` | *(none)* | Path to a YAML file of extra, purpose-built tools. See [Custom query tools](#custom-query-tools---query-tools) |
| `--query-tools-only` | `false` | Expose only the `--query-tools` tools, dropping the built-in `<prefix>_query` and metadata tools. Requires `--query-tools` |

A leftover `MSSQL_*` variable in a client config is inert — it is not read, so
it cannot half-configure the server. Passing a flag with an empty value counts
as not passing it: `--tool-prefix=` fails the same way as omitting it, and
`--query-fn-desc=` falls back to the default text.

**The connection string is an argument, so it is visible in `ps` output** to
anyone with an account on that host. On a shared machine, give the server a
dedicated login with `db_datareader` and treat that credential as readable by
anyone who can run `ps` there.

### Identity: `--tool-prefix` and friends

The prefix is required and has no default. Every tool is named after it —
`--tool-prefix=sales` yields `sales_query` — and the composed name is what gets
validated, so it must come out as 1–64 characters of letters, digits, `_` or
`-`. A trailing separator is normalised away: `--tool-prefix=sales_` and
`--tool-prefix=sales` name the same tools, and a prefix of `_` or whitespace
counts as absent. Keep prefixes short; some clients namespace tool names again
on top (`mcp__sales__sales_query`).

It is required rather than defaulted because the failure it guards against is
invisible at runtime. Registering two of these servers means two tools that
differ only by configuration, and clients disagree about what to do with a
duplicate tool name: some namespace it, some refuse to start, and some silently
let one server shadow the other — after which every call lands on whichever
database registered first, with plausible-looking answers and no diagnostic. A
derived default would paper over exactly that. If you start without a prefix,
the error suggests one parsed from your connection string:

```
$ sqlserver-mcp --conn-string 'sqlserver://host?database=AdventureWorks'
sqlserver-mcp: no tool prefix: pass --tool-prefix. Every tool this server exposes
is named after it (<prefix>_query), so that two databases registered in the same
client cannot shadow each other; try --tool-prefix=adventureworks
```

`--query-fn-desc` is the other half, and the more important one for getting
the model to pick the right server: the prefix solves collision, the description
solves *selection*. It passes through every client verbatim, while tool names
get rewritten and namespaced. By default it names the database read from the
connection string (`?database=`, `database=` or `Initial Catalog=`), or the name
given by `--descr-db`; override it to say what actually lives in there, which is
a far stronger signal than `sales_query` versus `hr_query`:

```
--query-fn-desc="Query the AdventureWorks sales database: orders, customers,
products and inventory. One T-SQL statement per call."
```

The two metadata tools are described the same way, with
`--get-metadata-fn-desc` and `--list-metadata-fn-desc`. Their defaults name the
database as well, so a model with several of these servers registered can tell
whose documentation it is asking for; override them when the documentation is
worth describing in its own terms:

```
--list-metadata-fn-desc="List the tables in the AdventureWorks sales warehouse
that have a data dictionary entry."
```

`--descr-db` sets the one name all of those defaults are written around. The
catalog in the connection string is often not what anyone asking a question
calls the database — `SALES_DW_PRD01` is "the sales warehouse" to everyone who
uses it — and the name in the description is what a model matches a question
against. Set it to the canonical name and every default description and the
instructions use it; leave it out and the connection string's own database name
is used, as before.

```
--descr-db="AdventureWorks sales warehouse"
```

`--instructions` is delivered once, at initialize, and shapes the whole session
rather than one tool call. The built-in text names the database, points the
model at `list_metadata` and `get_metadata` before it guesses a table name, and
asks for schema-qualified names, `TOP`, and one statement per call. `--server-label` is
what the client shows in its list of servers; the default distinguishes each
registration (`sqlserver-mcp-sales`, `sqlserver-mcp-hr`) instead of showing the
program name N times.

Whatever the instructions, each initialize ends them with today's date
(`Today is Thursday, 2026-09-24.`), taken from the clock at that moment rather
than at startup. Left without it, a model works the date out from the data -
the fiscal year it just queried - and can be months out.

With `--read-only`, the read-only note is appended to whatever description you
supply and added to the instructions, since it describes how the server actually
behaves rather than being editorial copy.

Connection strings accept either form supported by `go-mssqldb`:

```
sqlserver://user:password@host:1433?database=MyDb&encrypt=true&TrustServerCertificate=true
server=host,1433;user id=user;password=pass;database=MyDb;encrypt=true
server=localhost;database=MyDb;trusted_connection=yes        # Windows auth, Windows host
```

If the password contains URL-special characters, use the ADO (`key=value;`)
form or percent-encode it.

## Execution modes

`--transport` picks one of two, and they are alternatives rather than layers: a
process serves one client over its standard streams, or it listens on a
loopback port. Never both.

**`--transport=stdio`** (the default) is what an MCP client launches. The client
starts the binary, speaks JSON-RPC over stdin/stdout, and the server lives and
dies with it. Nothing listens on any port. Every existing config keeps working
untouched.

**`--transport=http`** serves the streamable HTTP transport on localhost, for
clients that attach to an already-running server by URL rather than launching
one, and for several clients on the same machine sharing a single connection
pool (`--max-open-conns` is then the ceiling for the machine, not per client).
Start it in a terminal or under a service manager:

```bash
sqlserver-mcp --transport=http --http-addr=127.0.0.1:8080 \
  --tool-prefix=sales \
  --conn-string='sqlserver://host?database=AdventureWorks'
# sqlserver-mcp: listening for MCP over HTTP on http://127.0.0.1:8080
```

Point a client at `http://127.0.0.1:8080`. The address it actually bound is
logged at startup, so `--http-addr=127.0.0.1:0` tells you which port the kernel
handed out. `SIGINT`/`SIGTERM` drains the requests in flight and exits.

### `--http-addr` is exclusive to `--transport=http`

Passing it under stdio is a startup error, not a setting that is quietly
ignored:

```
$ sqlserver-mcp --tool-prefix=sales --conn-string=... --http-addr=127.0.0.1:8080
sqlserver-mcp: --http-addr applies to --transport=http only: a stdio server talks
over its standard streams and listens on nothing
```

An ignored address is worse than a rejected one. It leaves an operator with a
port written down in a config file, a client that cannot connect, and nothing
anywhere saying that nothing was ever listening.

### When a client says it cannot connect

All of the ways this can fail look identical from the client, which reports
"failed to connect" and nothing else. `--http-log` is how you tell them apart:

```
http POST / host=127.0.0.1:8890 session=- rpc=initialize -> 200 (answered in 2ms, held 2ms)
http POST / host=127.0.0.1:8890 session=P7VCVP… rpc=tools/list -> 200 (answered in 1ms, held 1ms)
http GET  / host=127.0.0.1:8890 session=P7VCVP… rpc=- -> 200 (answered in 0s, held 2.001s)
```

`rpc=` is the JSON-RPC method, read out of the request body, which is what
separates a discovery problem from an execution one. The two durations separate
a slow server from a held connection: the streamable transport answers early
and keeps the stream open, so a long `held` on its own means nothing, while a
long `answered in` is the server actually taking that time.

| What you see | What it is | Fix |
| --- | --- | --- |
| A clean session — `initialize`, `tools/list`, `DELETE`, no non-2xx anywhere | Nothing failed here. The client got what it asked for and rejected it afterwards; look at how it consumes the tool schemas | — |
| `404` on a `POST` with a `session=` | The client is replaying a session this process never issued, or issued before a restart | `--http-stateless` |
| `400` on a `GET` with `session=-` | The client is opening a notification stream without a session id | `--http-stateless` |
| `403`, any method | The rebinding guard: `host=` is not a loopback name | Reach the server as `127.0.0.1`, not by a routable name |
| Nothing logged at all | The request never arrived — wrong port, or a container that cannot route to the host's loopback | See the mode's networking notes above |

`--http-stateless` drops the session bookkeeping entirely: no `Mcp-Session-Id`
is issued and none is required, so every POST stands on its own. It is the
right setting for clients and proxies that do not carry a session id back
between calls. The cost is that `GET` and `DELETE` return 405, so server→client
notifications over a standalone stream are not available — which this server
does not use, since every tool call answers on its own POST.

### When a header is not arriving as sent

`--http-log` names the exchange, not the headers on it. When a call is routed or
authorized by a header a proxy or gateway rewrites on the way through,
`--http-log-headers` dumps every header the server actually received for each
`tools/call`, one line per header, just before the call runs:

```
tool-call header server=sqlserver-mcp-sales tool=sales_query Accept: application/json, text/event-stream
tool-call header server=sqlserver-mcp-sales tool=sales_query Content-Type: application/json
tool-call header server=sqlserver-mcp-sales tool=sales_query Mcp-Session-Id: P7VCVP…
tool-call header server=sqlserver-mcp-sales tool=sales_query X-Trace-Id: abc-123
```

It is a debug aid, off by default: it prints a line per header on every tool
call, verbatim except for the credential headers (`Authorization`,
`Proxy-Authorization`, `Cookie`), whose values are logged as `[redacted]`. Other
headers can still carry something sensitive, so turn it off again once you have
what you need. A call that did not arrive over HTTP (there are none under `--transport=http`,
but the guard is there) logs a single line saying so.

### It really is localhost only

The host half of `--http-addr` must be `localhost` or a loopback IP. A bare
`:8080` is read as `127.0.0.1:8080` rather than as every interface, and anything
routable is refused at startup:

```
$ sqlserver-mcp --transport=http --http-addr=0.0.0.0:8080 ...
sqlserver-mcp: --http-addr host must be a loopback address: "0.0.0.0" would expose
this database to the network, and this server has no authentication
```

That is the reason, in full. **Without `--http-auth-token-file` this server has
no authentication of any kind.** Anything that can reach the port can read
everything the SQL login can read, so a routable bind address publishes the
database to whoever is on that network. The token (below) closes the port to
other local processes; it is a shared secret, not per-person authentication,
and it does not make a routable bind safe.
If you need to reach it from another machine, put it behind something that
authenticates — an SSH tunnel (`ssh -L 8080:127.0.0.1:8080 host`) or a
reverse proxy that does the authenticating — rather than widening the bind.

The SDK's DNS-rebinding protection is left on: a request arriving on loopback
with a non-loopback `Host` header gets a `403`, so a web page in the operator's
browser cannot use the browser as a bridge to this port.

### Authentication and per-user results

Loopback is not one person: every process on the host can reach the port, and a
client such as Open WebUI serves many users through one connection. Two flags
close that gap.

- `--http-auth-token-file=/etc/sqlserver-mcp/token` reads a shared secret (one
  line, no spaces) from a file — a file, not a flag value, so `ps` does not show
  it. Every request on every listener, the base port and each group's, must then
  carry `Authorization: Bearer <secret>`; anything else gets `401` before the MCP
  handler runs. Give the same token to each MCP connection in the client.
- `--user-header=X-OpenWebUI-User-Id` names the header the client stamps with the
  signed-in user's id. [Stored results](#stored-results-handles-and-operators) are
  then kept per user: a handle is readable only by the user whose call created it,
  through every entry point (`@handle` arguments, `show`, the operators), and
  another user's handle gets exactly the error an unknown one does. A call without
  the header still gets its rows, but no handle, and cannot read one. With
  `--http-log` the user is logged on each request line (`user=…`).

The header is a claim, not a proof: it is trustworthy because only the holder of
the token can send it. `--user-header` without `--http-auth-token-file` logs a
startup warning, since any local process could then claim to be any user. Prefer
the user's id over their display name, so a rename does not orphan or merge
their results. Under stdio there is one caller and neither flag applies; under
`--transport=http` without `--user-header`, every caller shares one set of
handles.

## The tool

`<prefix>_query` — `sales_query` with `--tool-prefix=sales`. Arguments:

| Argument | Type | Required | Meaning |
| --- | --- | --- | --- |
| `query` | string | yes | One T-SQL statement |
| `max_rows` | integer | no | Cap the rows returned by this call; it can never raise the cap above `--max-rows`. Omit it and the server's cap applies |

Only `query` is required. Omitting `max_rows` is the normal case — small models
tend to leave it out, and nothing else is needed for the call to succeed.

Structured result:

```json
{
  "query": "SELECT TOP 1 OrderID, Customer, Total FROM dbo.Orders",
  "columns": ["OrderID", "Customer", "Total"],
  "rows": [
    { "OrderID": 1, "Customer": "Acme", "Total": "199.99" }
  ],
  "row_count": 1,
  "rows_affected": -1,
  "truncated": false,
  "notes": []
}
```

The rows are also returned as text content, so clients that ignore structured
tool output still see the data. That channel is a markdown table rather than
the same JSON again:

```
| OrderID | Customer | Total |
| --- | --- | --- |
| 1 | Acme | 199.99 |

1 row(s).
```

A client that honours both channels would otherwise be sent every row twice, in
the more expensive of the two encodings, since `MarshalIndent` spends a line and
a repeated key on every value. The row count and any notes follow the table, so
a text-only client still learns that a result was truncated. A statement with no
result set — an `INSERT`, or a batch that selected nothing — has no table to
draw and keeps the JSON form, where `rows_affected` and the notes are the whole
answer.

`query` restates the statement exactly as it was received, so a caller logging
the response has the SQL that produced it without correlating back to its own
request.

### The output budget

Every result is read by a model with a finite context window, and one
`SELECT * FROM dbo.Transactions` is enough to fill it. Three limits bound it,
because a result overflows in three different ways and a row cap only catches
the first:

| Limit | Default | Catches |
| --- | --- | --- |
| `--max-rows` | 200 rows | A million small rows |
| `--max-bytes` | 256 KiB | A few hundred very wide rows |
| `--max-cell-bytes` | 4 KiB | One `NVARCHAR(MAX)` or base64'd blob |

Whichever is reached first stops the result. `truncated` is then `true` and a
note says which limit it was and what to do about it — the flag is one flag for
all three because the caller's response to all three is the same, while the note
is what distinguishes them. An oversized value is cut on a rune boundary and
marked in place (`…[truncated, 1.2 MiB total]`), so a shortened value can never
be mistaken for a complete one.

Two details worth knowing:

- **The first row always comes back**, even if it alone exceeds `--max-bytes`.
  One oversized row is something the caller can read and act on; zero rows and a
  note about bytes reads like an empty table.
- **`0` means "no limit"** for all three, matching `--max-rows`. Turning a limit
  off is something to ask for, not something to get by forgetting to set it.

The per-call `max_rows` argument can only lower the row cap, never raise it, and
there is no per-call equivalent for the byte budgets: those protect the model's
context window, which is not the caller's to spend.

Behaviour worth knowing:

- **Values.** `DECIMAL`/`NUMERIC`/`MONEY` come back as strings so precision
  survives JSON; `DATETIME*` as RFC 3339; `UNIQUEIDENTIFIER` as a GUID string;
  binary types as base64. `NULL` is JSON `null`, and `NULL` in the text table —
  spelled out, because an empty cell is also what an empty string looks like.
- **One result set.** Only one result set of a batch is returned; the rest are
  reported in `notes`. It is the first set that actually has columns — a
  procedure that assigns before it selects emits an empty set first, and
  skipping it is noted rather than reported as "no rows".
- **Write statements.** `INSERT`/`UPDATE`/`DELETE`/DDL run through `Exec` and
  report `rows_affected`. If the statement has an `OUTPUT` clause it runs as a
  query instead, so those rows are returned.
- **Errors.** A SQL error (bad object name, syntax error, timeout) comes back
  as an MCP tool error with the server's message, so the model can correct
  itself; it is not a protocol failure. The failing statement is restated on a
  second line (`sql: SELECT ...`), so failures are as loggable as successes.
- **Duplicate column names.** Rows are JSON objects, so a repeated column name
  is suffixed (`id`, `id_2`) and unnamed columns become `column_N`. A suffix
  that would collide with a real column is bumped again, so no value is lost.
- **Session state.** `USE` and `SET` run on whichever pooled connection is free,
  so their effect does not carry over to the next call; the result carries a
  note saying so. Qualify names across databases (`OtherDb.dbo.Orders`) or point
  the connection string at the right database, and put any `SET` in the same
  batch as the query that needs it.
- **Cancellation.** A call that exceeds `--query-timeout` is reported as a
  timeout; one the client cancels is reported as a cancellation, so the model
  does not rewrite a query that was never slow.

## Custom query tools: `--query-tools`

`<prefix>_query` takes a statement the model writes. `--query-tools` is the
other direction: a statement *you* write, tested and named, exposed as its own
tool so the model only supplies parameter values.

```
--query-tools=/etc/sqlserver-mcp/tools.yaml
```

The file is a YAML list — each record is one tool — or, to put the tools on
separate ports, a mapping with a `groups:` block and a `tools:` list (see
[Tool groups](#tool-groups-one-server-per-domain)); that mapping can also carry
an `include:` list to split the tools across a directory tree (see
[Splitting the file](#splitting-the-file-include)). YAML is the format either
way so the `query` can go in as a `|` block scalar — a statement you have
already written and tested pastes in verbatim, no escaping and no collapsing
onto one line:

| Field | Required | Meaning |
| --- | --- | --- |
| `name` | yes | The tool name, used verbatim (1–64 chars of letters, digits, `_`, `-`). It must not collide with a built-in (`<prefix>_query`, `<prefix>_get_metadata`, `<prefix>_list_metadata`, `<prefix>_describe_tool`, `<prefix>_show` and the operator tools) or another record |
| `description` | yes | What the model reads when it picks the tool. Keep it to a sentence or two — see [Keeping definitions small](#keeping-definitions-small-details-sharedparameters-and-the-describe-tool) |
| `details` | no | The long form — column meanings, defaults, caveats, when a sibling tool fits better. Not advertised; served by `<prefix>_describe_tool` on request |
| `query` | yes | One T-SQL statement. Bind parameters are written `@name` |
| `parameters` | no | The named parameters the client supplies (see below) |
| `connectionString` | no | The database this query runs against. Omitted or blank falls back to `--conn-string` |
| `outputFormat` | yes | `csv`, `md` (a markdown table), or `scalar` (a single value) |
| `resultColumn` | no | `scalar` only: which column the value comes from |
| `columns` | no | `csv`/`md` only: the subset of result-set columns to return, always all of them, in this order. The query can `SELECT *` (or wrap a view you don't control) and only these reach the client. Case-insensitive; a name not in the result set is a call-time error. Setting it also gives the tool a runtime `Columns` parameter — see below |
| `pickRecord` | no | `csv`/`md` only: return a single record. `true`: one row comes straight back; several rows trigger an elicitation that shows the **whole** result set for review, and only the chosen row — trimmed to `columns` — is returned. `optional`: the same, but only on calls that set the advertised `RequireSingle` argument; otherwise the tool lists. See below |
| `requireAnyOf` | no | Names of optional parameters of which a call must supply at least one non-blank value. Absent, null, and whitespace-only all count as "not supplied"; a call with none of them is rejected before the query runs. For a multi-filter tool that would otherwise return the whole table. See below |
| `group` | no | Puts this tool on a separate logical MCP server made of every tool naming the same group. Under `--transport=http` each group listens on its own port; under stdio it has no effect. See [Tool groups](#tool-groups-one-server-per-domain) |

Each `parameters` entry:

| Field | Required | Meaning |
| --- | --- | --- |
| `name` | yes | A SQL identifier; it is bound to `@name` in the query |
| `description` | no | Advertised to the model. Leave it blank on a parameter the root file's `sharedParameters:` block describes (below) |
| `type` | no | `string` (default), `int`, `number`, `bool`, or `date`. The native value and the same value quoted as a string are both accepted. Blank inherits the shared parameter's type |
| `required` | no | Defaults to `true`, or to the shared parameter's setting. An absent optional parameter is bound as SQL `NULL` |
| `literal` | no | On a list parameter, lets it also take a comma list typed out. Left out, a list parameter is handle-only: it accepts `@handle.Column` and a literal value (or a JSON number) is refused before the query runs, so every id a tool joins on came from a stored result. Set it for school years and for a key a person types in (a student id, a BEDS code). Blank inherits the shared parameter's setting |
| `batch` | no | Marks a comma-separated id list, which then also accepts `@handle.Column` (see [Stored results](#stored-results-handles-and-operators)). Left out, it is inferred: a parameter the query passes to `STRING_SPLIT` is a list. `false` opts one out; `true` marks a list the query splits another way. Only `string` parameters can be lists. Blank inherits the shared parameter's setting |

A `date` parameter takes either an absolute `yyyy-mm-dd` or an offset from
today — `today`, `yesterday`, `tomorrow`, or `[+-]N` followed by `d`, `w`, `m`
or `y` for calendar days, weeks, months or years (`-7d`, `-2w`, `-1m`). Month
and year offsets clamp to the end of the target month, so `-1m` from March 31
is February 28. The offset form exists for small models, which rarely know
today's date and get calendar arithmetic wrong: the server resolves it against
its own clock at midnight and binds a SQL `date`. The loader advertises a
matching `pattern` on every date parameter and states the accepted forms once
in the server's initialize instructions, so a description should not repeat
the format.

A parameter that the query never mentions as `@name` is a startup error — the
value would be accepted and silently dropped. The reverse is allowed: a query
may use `@@ROWCOUNT` or `DECLARE` a local of its own.

Values are always sent as bound parameters, never string-substituted into the
SQL, so a parameter cannot inject SQL. The `--max-rows` / `--max-bytes` /
`--max-cell-bytes` budget applies to these results as it does to `<prefix>_query`.

```yaml
- name: orders_for_customer
  description: Every order for one customer in a date range, newest first.
  query: |
    SELECT OrderID, OrderDate, Total
    FROM dbo.Orders
    WHERE CustomerID = @customer AND OrderDate >= @since
    ORDER BY OrderDate DESC
  parameters:
    - name: customer
      type: int
      description: CustomerID
    - name: since
      type: string
      description: ISO 8601 date; orders on or after it
  outputFormat: csv

- name: customer_balance
  description: A single customer's current account balance.
  query: SELECT Balance FROM dbo.Accounts WHERE CustomerID = @customer
  parameters:
    - name: customer
      type: int
  outputFormat: scalar
```

[`query-tools.example.yaml`](query-tools.example.yaml) in this repo is a fuller
worked example: all three output formats, typed and optional parameters,
`resultColumn`, `columns`, `pickRecord`, and a tool with its own
`connectionString`.

### Keeping definitions small: `details`, `sharedParameters` and the describe tool

Every tool's description and input schema sits in the model's context on
every turn, whether or not the tool is called. With a few tools that is
nothing; with sixty it is the largest thing in the prompt, and a small model
starts picking wrong tools or none. Three things keep it down.

**`description` is short, `details` is long.** The advertised `description`
is a sentence or two on *when* to use the tool: what it returns, what it
needs, and the sibling that fits better ("use `customer_lifetime_value` for the
full-history figure"). Everything else — what each column means, the defaults, the
edge cases — goes in `details`, which is never advertised. It is returned by
**`<prefix>_describe_tool`** (`describe_tool` when there is no prefix), a
tool the loader registers beside the query tools on every server that has
any. Given a tool's name it returns the description, the details, every
parameter with its type, requirement and meaning, and the `columns` menu; the
initialize instructions tell the model to call it before using a tool for the
first time. It accepts the served name, the client's call name under
`--tool-call-name`, or either with a stray namespace in front, and answers
for every tool in the file, saying which server a sibling's tool lives on.

**`sharedParameters` describes the recurring ones once.** A file whose tools
mostly take the same `PageSize`, `PageNumber`, customer id and date window
repeats those descriptions on every tool. The root file can declare them
instead:

```yaml
sharedParameters:
- name: PageSize
  type: int
  description: Rows per page. Pair with PageNumber to page through a large result set.
- name: SchoolYear
  type: string
  description: School year as yyyy-yyyy. One year, or several comma-separated to cover a span
    in one call (example 2024-2025,2025-2026); rows then carry a SchoolYear column.
```

A tool's parameter of the same name then needs only its `name:`; it inherits
the type (and `required`, unless it sets its own) and is advertised **without
a description**. The shared text goes into the server's initialize
instructions once, under "About the query tools", listing only the shared
parameters the tools on that server actually use — a grouped server does not
carry text about parameters it never sees. A tool that does write a
description keeps it: that is the override, for the one tool where
`StartDate` means something different. The block belongs in the root file;
an included file that carries one is rejected, like a second `groups:`. Tool
names mentioned in the shared text are rewritten the way the descriptions
are (see [Tool groups](#tool-groups-one-server-per-domain)).

**The conventions move to the instructions too.** The accepted date forms,
and what `RequireSingle` does are each stated once in the initialize text —
again only when a tool on that server has a date parameter or
`pickRecord: optional` — rather than on every parameter that has them.

On a real 72-tool file, the three together cut the advertised definitions
from roughly 37K tokens to 21K, and an eight-way group split then puts 1–5K of
that on any one server.

### Exposing only the custom tools: `--query-tools-only`

By default the `--query-tools` tools are added *alongside* `<prefix>_query` and
the metadata tools. Pass `--query-tools-only` to drop those built-ins and expose
nothing but the tools from the YAML file — the model then can only call the
queries you wrote, never compose SQL of its own. The flag requires
`--query-tools`; without it the server would have no tools at all, and startup
fails. The initialize instructions change to match: they describe a fixed tool
set rather than the `<prefix>_query` workflow.

`--tool-prefix` is optional in this mode: it only ever named the built-in tools,
and those are gone. Give one anyway if you want the server's entry in the
client's list to read `sqlserver-mcp-<prefix>` rather than `sqlserver-mcp`; a
prefix that is supplied is still validated.

### Tool groups: one server per domain

A small local model with a short context window pays for every tool schema in
its list whether it calls the tool or not. Forty parameterized queries in one
server can crowd out the conversation. Tool groups split them up: each group
becomes its own logical MCP server on its own port, and the client wires up
only the groups a given assistant needs — or the model picks the server first
and only that group's schemas are ever loaded.

To use groups, the `--query-tools` file changes from a bare list to a mapping
with two keys:

```yaml
groups:
  orders:
    label: sales_orders                    # optional; the server's name at initialize
    # prefix: sales_orders               # optional; served tool names become sales_orders_… (see below)
    port: 8083                             # absolute TCP port …
    # order: 3                             # … or an offset added to the --http-addr port
    description: >-                        # folded into the server's initialize instructions —
      Daily order history, per-customer    # this is what a model reads to pick the server
      summaries and regional rollups.
  returns:
    label: sales-returns
    port: 8084
    description: Return history, single-return detail, and return-rate stats.

tools:
  - name: customer_daily_orders
    group: orders
    description: …
    query: |
      …
    outputFormat: csv
  - name: customer_returns
    group: returns
    # …
```

- **Ports.** Under `--transport=http`, the `--http-addr` port serves the
  built-in tools (`<prefix>_query` and the metadata tools) plus any tool with
  no `group:`. Each group listens on its own port: its `port:` (absolute) or
  `order:` (an offset from the `--http-addr` port), or — if it sets neither —
  the next free port above the base one, in the order the group first appears
  among the tools. Two groups on the same port, or a group on the base port,
  is a startup error.
- **stdio.** `--transport=stdio` has one stream and so one server: groups are
  inert and every tool is served together, exactly as an ungrouped file. The
  same file works both ways.
- **`groups:` is optional metadata.** A tool can name a `group:` with no entry
  in the `groups:` block; the group still gets its own port, just with a
  derived label (`<server-label>-<group>`) and generic instructions. An entry
  in `groups:` that no tool references is a startup error, to catch typos.
- **`--query-tools-only`** drops the built-ins as usual; the base port is then
  used only if some tool is left ungrouped, and otherwise skipped.
- Each group's server reports its own `label` and instructions at initialize,
  so the client's server list stays legible and the `description` steers tool
  selection.
- **Cross-references are written the way the model must call them.** A
  small model pairs a tool with the wrong sibling server whenever a
  description says "use flag_summary for the quick yes/no" and
  nothing says which server has it — it guesses the one it is on. So the
  loader rewrites every mention of a tool in tool, parameter and group
  descriptions (never in the SQL), and the first mention of a tool from
  another server is followed by "(on the `<label>` server)". Keep writing
  the declared names; the rewrite is automatic.
- **`--tool-call-name` matches the client's namespacing.** Some clients
  present a tool to the model as `<server>_<tool>` — Open WebUI does, with
  the MCP connection's *ID* field as `<server>` — and then the served name is
  not what the model has to emit. `--tool-call-name='{server}_{tool}'`
  tells the loader that, with `{server}` being the group's `label`, so the
  mentions and the initialize text show the exact string to copy
  (`cust_flag_summary`). Give each group a short label, register
  the Open WebUI connection with that label as its ID, and the model sees
  one consistent token everywhere. The default `{tool}` is for clients that
  use names as served.
- **`prefix:` is for clients that do not namespace.** With `prefix: cust`
  on the customers group, `flag_summary` is *served* as
  `cust_flag_summary`, so the pairing is in the name itself (a
  leading segment the prefix and the declared name share is written once).
  Do not combine it with a namespacing client — the model would see
  `cust_cust_flag_summary`. A prefix that makes two tools serve
  under one name, or a served name that collides with a built-in, is a
  startup error.

The client side is one `mcpServers` entry per port. With Continue.dev:

```yaml
mcpServers:
  - name: Sales orders
    type: streamable-http
    url: http://127.0.0.1:8083/
  - name: Sales returns
    type: streamable-http
    url: http://127.0.0.1:8084/
```

[`query-tools.example.yaml`](query-tools.example.yaml) in this repo is a small
worked example of the single-file form.

### Splitting the file: `include:`

One file per tool gets unwieldy fast. The mapping form takes a third key,
`include:`, so the file `--query-tools` points at can be a thin root that pulls
the tools in from a directory tree:

```yaml
groups:
  lookups: { label: ref, port: 8081, description: … }
  customers: { label: cust, port: 8082, description: … }
  # …

include:
  - tools/lookups                     # every *.yaml in the directory, in filename order
  - tools/customers
  # …
```

```
tools.yaml                             # the root: groups: + include:
tools/
  lookups/
    find_region.yaml                    # one tool per file; the file IS the tool mapping
    find_product.yaml
    …
  customers/
    find_customer_by_name.yaml
    …
```

- Each `include:` entry is resolved **relative to the file that lists it**. A
  **directory** contributes every `*.yaml` / `*.yml` file directly inside it, in
  filename order, skipping names that start with `.` or `_`. A **file**
  contributes just that file.
- An included file is any of the shapes above — a **single tool** (a mapping
  with `name:`), a **list** of tools, or another mapping with its own
  `groups:` / `tools:` / `include:`. So the tree can nest.
- A tool loaded from a directory **inherits its `group:` from that directory's
  name**. Keeping an explicit `group:` in the file is fine and is checked
  against the directory; a mismatch is a startup error.
- `groups:` still belongs in the root. Tool names must be unique across the
  whole tree, and an include cycle is a startup error.
- The single-file forms are unchanged — `include:` is purely additive.

[`scripts/split_query_tools.py`](scripts/split_query_tools.py) performs the
split deterministically: point it at a grouped-form file and it writes the root
plus one file per tool under `<name>/<group>/<tool>.yaml`, verifying the tree
parses back to the same tools before it rewrites the root.

### The `scalar` format

`scalar` returns one value as the tool's text content (and as `value` in the
structured output).

- **One column** is taken as the value. **More than one column** needs
  `resultColumn` naming which one; without it the call returns an error.
- **One row** gives its value directly. **No rows** is an error.
- **More than one row** cannot be resolved on its own, so the server asks. It
  sends an MCP **elicitation** whose form is a picker with one option per
  row (its number and its values) and waits for the person to choose; the
  value is then taken from that record. A client that did not offer the
  elicitation capability gets an error telling it to narrow the query instead.

### `columns` and `pickRecord`

`columns` trims and reorders the fields a `csv`/`md` tool returns. The query
selects whatever it needs (joins, `SELECT *`, a view); the client only ever
sees the listed columns. On its own it does not change how many rows come back.

There is no `Columns` parameter, on any tool. Every call returns every column
in the tool's `columns:` list (or, with no list, every column the query
selects). A model has to format a comma-separated column list correctly, and
small models often cannot; a failed call costs a round trip, and a narrower
reply saved little. To keep a reply small, narrow the *rows* (filters,
`PageSize`), not the columns. A client that sends `Columns` anyway (a stale
prompt, say) is not refused: the argument is ignored and the reply carries a
note saying so. The one place a column list is still an argument is
the calc `project` tool, whose whole job is to pick and rename columns of a stored
result.

`pickRecord: true` narrows the *rows* to one:

- **One row** — returned directly, trimmed to `columns`.
- **No rows** — an error.
- **Several rows** — an MCP **elicitation**. The form is a picker with one
  option per record: its number and the values of **every column** the query
  selected, so the person can tell the candidates apart (the message above
  the form is a one-line framing and does not repeat them — select the
  fields that distinguish records, not just the ids `columns` will return).
  The tool then returns just the chosen record, and just the `columns`
  fields. A client without the elicitation capability gets an error asking it
  to narrow the query.

The point of the pairing is context: the review happens in the elicitation
prompt (which the model never sees), and what lands back in the client is a
single projected row rather than the whole candidate set. `pickRecord` with no
`columns` returns the whole chosen row.

`pickRecord: optional` keeps the tool a list tool but advertises one more
argument, a boolean **`RequireSingle`**, which the initialize instructions
tell the model to set when the user means one particular record. A call with
`RequireSingle: true` behaves as above, with two differences aimed at its
opt-in nature: **no rows** is an ordinary empty result rather than an error,
and the no-elicitation error tells the caller it can drop `RequireSingle` to
receive the list instead. A call without it (or with `false`) returns the
full result set exactly as a plain `csv`/`md` tool would. Sending
`RequireSingle` to a tool that does not offer it is an unrecognized-parameter
error, never a silent list. The name is reserved like `Columns` is.

Paging and `RequireSingle` need a word: the candidates the person sees are
the rows the query returned, so a small page can hide the one they want. The
instructions steer the model toward a large `PageSize`, and when
the tool has a parameter named `PageSize` and the page came back full the
elicitation says so and suggests cancelling and searching again with a larger
page. (`PageSize` is only a naming convention here; the loader gives it no
other meaning.)

`columns`/`pickRecord` are for `csv` and `md`; `scalar` already reduces to one
value via `resultColumn` and its own multi-row elicitation.

### `requireAnyOf`

A search tool usually makes every filter optional so the caller can combine
them freely — `WHERE (@LastName IS NULL OR ...) AND (@FirstName IS NULL OR ...)`.
The cost is that a call supplying none of them is still valid and returns the
whole table. `requireAnyOf` closes that gap:

```yaml
requireAnyOf: [LastNameContains, FirstNameContains]
```

Each name must be a declared parameter that is itself `required: false`. A
call is rejected, before the query runs, unless at least one of them arrives
with a real value — absent, `null`, and a whitespace-only string all count as
missing, since a blank filter would bind fine and then match everything. The
error names the parameters that would have satisfied it.

This matters more than it looks because of how a misnamed argument behaves.
The loader rejects unrecognized argument names, but some MCP clients validate
a call against the tool's input schema and drop unknown keys before sending
it. A model that guesses `SearchLastName` for a tool whose filter is
`LastNameContains` then reaches the server with no name filter at all, and
without `requireAnyOf` the result is a quiet, unfiltered page.

## Stored results: handles and operators

A model is good at choosing the next step and bad at carrying hundreds of ids
from one reply into the next call, or at counting, intersecting and subtracting
sets in its head. Stored results move that work into the server.

**Every list result gets a handle.** A `csv`/`md` query tool (and `<prefix>_query`)
runs its statement for the whole result — up to `--result-store-max-rows` —
stores it, and leads its reply with the handle:

```
handle: qx4 · SHOWING 5 OF 612 ROWS · pass on as @qx4.CustomerID or @qx4.RegionID · also stored: Segment
Profile (612 rows):
- Same in every row: FiscalYear=2025
- Region: East (590), West (22)
- CustomerID: 598 distinct (some repeat across the 612 rows)
- Always NULL: Fax

CustomerID,FirstName,LastName
… (5 rows)
… 607 more rows not shown. The 5 above are a sample: do not count, list or conclude from them. The full result is qx4: counts are in its profile, pass on as @qx4.CustomerID or @qx4.RegionID, or page it with show (Handle=qx4, Columns, PageSize, PageNumber).
```

- A result of 50 rows or fewer shows every row, as before — every resolver's
  answer is that small, so entity resolution reads exactly as it always did.
- A larger one shows at most 5 rows — from the start of the page asked for,
  whatever `PageSize` says — and is marked partial twice: `SHOWING 5 OF 612 ROWS`
  on the handle line, and a trailer right after the last row, which is what a
  model reads last before it answers. Small models otherwise read the rows as
  the whole result. Only `show` pages at the size asked for (20 by default).
  It also carries a **profile**:
  the columns with one value in every row on one line, then value breakdowns of
  low-cardinality columns (fiscal year, region, product — where a wrong scope
  shows up), date and number ranges, distinct counts of id columns, and the
  always-NULL columns on one last line. It is deterministic and capped at 2 KiB.
- The handle line shows how to pass the result on, using its own id columns
  (`pass on as @qx4.CustomerID`), because small models copy an example more
  reliably than they apply a rule.
- The capture asks the statement for page 1 at one row past the store's limit
  (binding `PageSize`/`PageNumber` itself), so a result cut at the limit is
  marked `truncated` rather than passed off as complete. The reply's own
  `--max-rows` / `--max-bytes` budget still caps what is shown.
- Scalar tools, and a single record picked with `RequireSingle`/`pickRecord`, are
  answers rather than lists and get no handle.
- Notes flag what the model should check before building on a result: no rows
  matched; some `@handle` ids found no rows; rows carry a `FiscalYear` or an
  `…ID` other than the one asked for.

Handles are short: two letters chosen at startup plus a per-user counter
(`qx1`, `qx2`, …). The letters make a handle from before a restart unknown
instead of silently naming a new result. Results live in memory only — never
on disk, never written to SQL Server — and are dropped after `--result-store-ttl`
unused, or least-recently-used first past the byte limits (per user first, then
overall).

**Handles as input.** A list parameter (`batch`, above) takes `@handle.Column`
instead of ids, and — unless it sets `literal` — takes nothing else: `CustomerID=@qx4.CustomerID`, or `@qx4` when the column
has the parameter's name. The `@` may be left off (`CustomerID=qx4.CustomerID`, as
small models tend to write it) when the value starts with this run's two
handle letters; a bare value with other letters stays a literal id. The server expands it to the column's distinct
non-null values; a list over 1000 ids runs in chunks and the results are merged
into one handle. A truncated handle is refused unless the call passes
`AllowPartial=true`. `SaveAs="east customers"` labels the new handle.

**Ids are shielded from the client.** A tool's `columns:` menu is what its reply shows; the whole query result is stored. A foreign key or record id left out of `columns:` is therefore never displayed, yet the handle line names it (`pass on as @qx4.CustomerID · also stored: CustomerID, RegionID`) and it passes on as `@handle.Column`. Derived results (`st_calc_*`, `show`) inherit the hidden columns of their inputs, and `show` will not display one on request (it leaves it out and says where it is). A `Columns` list that names an id is treated the same way on a query tool: the id is dropped with a note rather than failing the call. Only human-facing identifiers (`SchoolID`, `StaffID`, `CourseID`, ...) are listed.

A resolver's record (`pickRecord`, including the record a user picked) is stored
under a handle like any other result, so the first id in a chain is a handle too.

On a `literal` list parameter, a literal id list that is exactly the ids a partial reply showed of a larger
result — the model copied the sample instead of passing the handle — is
refused with the handle to pass instead (`Pass CustomerID=@qx4.CustomerID …`).
The check compares the list with the rows each reply showed of the caller's
stored results, in the column named like the parameter, and ignores lists of
fewer than three ids. `AllowPartial=true` lets through a subset that is really
meant. The
derived result's reply says where it came from (`from: qx3 = … → qx4 = …`).
`SaveAs` and `AllowPartial` are accepted by every list tool but, like the shared
parameters, are explained once in the initialize instructions rather than in
every tool's schema.

**The tools.** Each is published as `<prefix>_<name>`, reads only the caller's
own handles, and never touches SQL Server:

| Tool | What it does |
| --- | --- |
| `show_field` | Read the whole text of one value a reply showed cut (`…[+N chars, row R]`): `Handle`, `Row` (R, the row's 0-based position in the stored result, the number in its `Row` column, also after `OrderBy`), `Column`, and `Offset` to read on past the 4000 characters one call returns. On every server that carries `show` |
| `show` | Page a handle (`PageSize`, `PageNumber`, `OrderBy`), or `ProfileOnly=true` for its profile and full lineage. On every server |
| `set_union`, `set_intersect`, `set_difference` | Set logic over two or more handles (`Handle=qx3,qx5`, the same parameter name every operator uses) on a `Key` column list; the first handle's full rows come back (`Carry=false` for the key alone); a repeat of an identical query-tool call reuses its handle |
| `filter` | `Where` condition: `= <> < <= > >=`, `IN (…)`, `LIKE`, `BETWEEN`, `IS [NOT] NULL`, `AND`/`OR`/`NOT`, parentheses; `[Bracket Names]` for columns with spaces. A date column takes the same dates as a date parameter, offsets from today included (`Birthdate <= '-15y'`), and a timestamp column compared with a plain day compares by day, so midnight on the day counts. `Having` and `count_if`/`sum_if` conditions do the same |
| `group_aggregate` | `By` columns, `Aggregates` from `count()`, `count(Col)`, `count_distinct`, `sum`, `avg`, `min`, `max`, `count_if(condition)`, `sum_if(Col, condition)`, each `AS name`, and an optional `Having` |
| `join` | `inner`, `left`, `semi` or `anti` on `On` (`CustomerID`, or `Left = Right`); a join that would exceed the store's row limit is refused before it runs |
| `project` | Keep and rename columns (`Col AS Name`), optionally `Distinct` |
| `sort_limit` | `OrderBy` and `Limit`, for top-N questions |
| `calc` | Arithmetic and statistics: `+ - * /`, `round`, `abs`, `min`, `max`, `percent(part, whole)`, `ratio`, `days_between('d1','d2')`, and `@qx4.row_count`, `@qx4.count(Col)`, `count_distinct`, `sum`, `avg`, `min`, `max`, `median`, `stdev`, `percentile(Col, p)`; several expressions separated by `;` |
| `calc_sql` | Only with `--calc-sql`: one read-only SQLite `SELECT`/`WITH` over handles written `@qx4`, run in a private in-memory database holding only those handles |

"Customers who ordered only Widgets" becomes: resolve the year
and region; fetch the orders for the region's customers into a handle;
then

```
group_aggregate Handle=qx6 By=CustomerID
  Aggregates="count() AS products, count_if(ProductName LIKE '%Widget%') AS widgets"
  Having="products = widgets"
```

The operators run in an embedded SQLite database (pure Go, in memory). Every
literal in a condition is a bound parameter and every column name is resolved
against the handle's real columns and quoted, so a condition cannot carry SQL.
Text columns compare case-insensitively, as SQL Server's default collation does;
decimals compare as numbers and dates as dates.

**Where the operators live.** In a grouped file, the group with `operators: true`
carries them — it needs no query tools of its own:

```yaml
groups:
  calc:
    label: sales_calc
    port: 8079
    operators: true
    description: Exact set logic, filtering, counting, grouping, joins and arithmetic over results the other servers returned.
```

Every server carries `show`. Without an operators group the operators go on the
base server (`--http-addr`), and under stdio on the one server. Each server's
initialize instructions explain handles and name the operators in the exact form
the model calls them (`sales_calc_filter (on the sales_calc server)` under
`--tool-call-name='{server}_{tool}'`).

## Safety

`--read-only` strips comments and string literals, then requires the statement
to start with `SELECT` or `WITH` and to contain no write keywords. It stops
accidents, but treat it as a convenience, not a security boundary: **the real
control is the SQL login**. Give the server a dedicated account with `SELECT`
on only what the model should see (`db_datareader` at most), and prefer that
over trusting the parser.

`--transport=http` is why the listener is pinned to loopback and refuses to
bind anywhere else: without `--http-auth-token-file` there is no authentication
in front of it, so reachability *is* access. See [It really is localhost only](#it-really-is-localhost-only)
and [Authentication and per-user results](#authentication-and-per-user-results).

Stored results hold whatever the tools returned — for a customer
management system, customer records — in the server's memory. They are never written to disk
or to the database, are dropped when unused for `--result-store-ttl`, and with
`--user-header` are readable only by the user who created them. `--result-store=false`
turns them off.

Also note that the connection string sits in the client's config file in
plaintext — restrict its permissions (`chmod 600`) and keep it out of version
control.

## Several databases

One process, one connection string, one database. A second database is a second
registration of the same binary, differing only in its prefix and its connection
string — which is why the prefix exists, and why nothing here takes a `database`
argument per call. It also keeps the connection string, the read-only setting and
the SQL login one-to-one with the data they govern: a per-call `database`
argument would let one set of credentials and one read-only decision span
several databases.

`USE` does not help. Connections are pooled, so the switch does not survive to
the next call — the result carries a note saying so. Register a server for the
other database instead.

## Integrating with Continue.dev (VS Code)

Two equivalent options; both are in `continue/`.

**Option A — add the blocks to your assistant config** (`~/.continue/config.yaml`
for all workspaces, or `<repo>/.continue/config.yaml` for one). Two databases,
one binary; the prefix and the connection string are what differ:

```yaml
name: sql-server-assistant
version: 0.0.1
schema: v1

mcpServers:
  - name: AdventureWorks (sales)
    type: stdio
    command: /usr/local/bin/sqlserver-mcp   # absolute path; .exe on Windows
    args:
      - --read-only         # drop to allow writes
      # - --max-rows=500    # raise or lower the 200-row default
      - --query-timeout=30s
      # Required. Names every tool this server exposes: sales_query.
      - --tool-prefix=sales
      - --conn-string=sqlserver://reporting_user:CHANGE_ME@sqlhost:1433?database=AdventureWorks&encrypt=true&TrustServerCertificate=true
      # What the model reads when choosing between this server and the next
      # one. Drop the line and the default names the database on its own.
      - >-
        --query-fn-desc=Query the AdventureWorks sales database on sqlhost
        with a single T-SQL statement and get the result set back as JSON.
        Use it for questions about orders, customers, products and inventory.
    connectionTimeout: 20000

  - name: HR warehouse
    type: stdio
    command: /usr/local/bin/sqlserver-mcp
    args:
      - --read-only
      - --tool-prefix=hr          # hr_query, disjoint from sales_query
      - --conn-string=sqlserver://reporting_user:CHANGE_ME@sqlhost:1433?database=HRWarehouse&encrypt=true&TrustServerCertificate=true
      - >-
        --query-fn-desc=Query the HR warehouse on sqlhost with a single
        T-SQL statement. Use it for questions about employees, departments,
        headcount and payroll.
    connectionTimeout: 20000
```

**Option B — a standalone block file** at
`<repo>/.continue/mcpServers/sql-server.yaml`, which Continue merges into the
active assistant without touching your main config:

```yaml
name: SQL Server MCP
version: 0.0.1
schema: v1

mcpServers:
  - name: AdventureWorks (sales)
    type: stdio
    command: /usr/local/bin/sqlserver-mcp
    args:
      - --read-only
      - --tool-prefix=sales
      - --conn-string=server=sqlhost,1433;user id=reporting_user;password=CHANGE_ME;database=AdventureWorks;encrypt=true
      - >-
        --query-fn-desc=Query the AdventureWorks sales database on sqlhost
        with a single T-SQL statement and get the result set back as JSON.
```

There is no `env:` block in either example, and adding one does nothing: the
server reads only `args:`.

To run from source without installing a binary, use
`command: go` with `args: [run, ., --read-only, --tool-prefix=sales]` — but note
that Continue launches the process from an unspecified working directory, so
prefer an absolute path:
`args: [run, /home/you/src/sql-server-mcp, --read-only, --tool-prefix=sales]`.

After saving, reload the VS Code window. The MCP server appears in Continue's
tool list as **AdventureWorks (sales) / sales_query**; agent mode will call it
when you ask questions like *"how many orders did we ship last week?"*. MCP
tools are only available in Continue's **Agent** mode, not plain Chat.

## Upgrading: `--tool-prefix` is required

This is a breaking change, deliberately. Existing configurations will not start
until they add a prefix, and the tool the model was calling is renamed from
`run_query` to `<prefix>_query`:

```yaml
args:
  - --tool-prefix=sales    # add this
```

A server that silently kept working under a colliding name is the bug being
fixed, so failing at startup is the intended outcome — the alternative is a
client quietly routing `run_query` to the wrong database. The startup error
suggests a prefix parsed from your connection string.

Three other things to update:

- **Every `MSSQL_*` environment variable is gone.** Flags are the only input
  now, so an `env:` block in a client config is dead weight — move each setting
  across, including the connection string:

  ```
  # before                            # after
  env:                                args:
    MSSQL_CONNECTION_STRING: "..."      - --conn-string=...
    MSSQL_READ_ONLY: "true"             - --read-only
    MSSQL_MAX_ROWS: "500"               - --max-rows=500
  ```

  Nothing warns you about a leftover variable, because nothing reads it. If the
  server starts and behaves as though a setting is missing, that setting is
  still in `env:`.
- `--tool-name` is **gone**. The prefix is now the only way to name tools, so
  `--tool-name=adventureworks_query` becomes `--tool-prefix=adventureworks`.
- Any saved prompt, rule or custom instruction that names `run_query` needs the
  new name.

## Testing

```bash
go test ./...
```

Everything runs against a stub database driver, so no SQL Server is needed. The
suite covers the SQL text guards, value conversion, `max_rows` decoding,
flag parsing, tool naming and per-database identity, the output budget and the
markdown rendering, multi-result-set handling, timeout versus cancellation, and
an end-to-end MCP round trip (client → `tools/call` → result). The execution
modes are covered too, including a real MCP session over a loopback socket and
the refusal to bind anywhere but localhost.

You can also drive the binary by hand — over stdio:

```bash
{ printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"master_query","arguments":{"query":"SELECT TOP 5 name FROM sys.tables"}}}'; sleep 2; } \
| ./sqlserver-mcp --tool-prefix=master \
    --conn-string='server=localhost;user id=sa;password=...;database=master;encrypt=disable' 
```

or against the localhost listener, with the server already running under
`--transport=http`:

```bash
curl -sS -X POST http://127.0.0.1:8080 \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}'
```

## Layout

| File | Contents |
| --- | --- |
| `main.go` | Config loading, MCP server and tool definition |
| `transport.go` | The two execution modes: stdio wiring, and the localhost HTTP listener |
| `identity.go` | Tool prefix, server label, per-database description and instructions |
| `describe.go` | The describe tool, and the query-tool notes appended to the initialize instructions |
| `query.go` | Query execution, row scanning, SQL→JSON value conversion |
| `sqltext.go` | Comment/literal stripping, statement classification, read-only guard |
| `rowlimit.go` | Decoding the optional `max_rows` argument in either the number or string form |
| `budget.go` | The row, payload and per-cell limits, and cutting a value that exceeds one |
| `render.go` | The markdown table the text channel carries |
| `caller.go` | The bearer-token check and the per-call user identity |
| `store.go` | The per-user result store: handles, limits, eviction |
| `handles.go` | Capturing results, `@handle` arguments, the reply layout, and the instructions on handles |
| `profile.go` | The profile of a large result |
| `handletools.go` | `show` and the operator tools |
| `cells.go` | Display truncation of long values (`shortenCells`) and `show_field` |
| `calc.go`, `calcparse.go`, `calcexpr.go` | The in-memory SQLite engine, the condition/aggregate grammar, and `calc`'s arithmetic |
| `continue/` | Ready-to-copy Continue.dev YAML |
