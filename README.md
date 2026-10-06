# mssql-mcp-toolkit

A configurable [Model Context Protocol](https://modelcontextprotocol.io) (MCP)
server for Microsoft SQL Server, written in Go and shipped as one binary. Point
it at a database and an LLM client can query it in two ways:

- **Ad hoc.** A built-in `<prefix>_query` tool takes one T-SQL statement and
  returns the result, with row, byte and per-value limits so one careless
  `SELECT *` cannot fill the model's context window.
- **Purpose-built.** A YAML file defines named, parameterized queries — a
  statement *you* wrote and tested — each exposed as its own tool. The model
  supplies parameter values and never writes SQL. Tools can be split into
  per-domain servers, and large results are kept in server memory under short
  *handles* that the model pages, passes between tools, and combines with
  set/filter/group/join operators, instead of copying ids from reply to reply.

**One server process serves exactly one database.** Register the binary a second
time, with its own connection string and `--tool-prefix`, for a second database
(see [Several databases](#several-databases)).

- Transports: stdio (the client launches the binary) or streamable HTTP on
  localhost
- SDK: [`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk)
- Driver: [`github.com/microsoft/go-mssqldb`](https://github.com/microsoft/go-mssqldb)

## Contents

- [Quickstart](#quickstart)
- [Command-line reference](#command-line-reference)
- [Execution modes](#execution-modes)
- [Built-in tools](#built-in-tools)
- [The tool file (YAML)](#the-tool-file-yaml)
- [Stored results: handles and operators](#stored-results-handles-and-operators)
- [Safety](#safety)
- [Several databases](#several-databases)
- [Testing](#testing)
- [Layout](#layout)

## Quickstart

### 1. Build

```bash
git clone https://github.com/akennis/mssql-mcp-toolkit
cd mssql-mcp-toolkit
go build -o mssql-mcp-toolkit .          # mssql-mcp-toolkit.exe on Windows
```

or `go install github.com/akennis/mssql-mcp-toolkit@latest`, which puts the
binary in `$(go env GOPATH)/bin`. Requires Go 1.25.7+. Cross-compile with, e.g.,
`GOOS=windows GOARCH=amd64 go build -o mssql-mcp-toolkit.exe .`

### 2. Create a least-privilege login

The SQL login is the real security boundary (see [Safety](#safety)). Give the
server an account that can read what the model should see and nothing else:

```sql
CREATE LOGIN mcp_reader WITH PASSWORD = 'use-a-long-random-password';
USE AdventureWorks;
CREATE USER mcp_reader FOR LOGIN mcp_reader;
ALTER ROLE db_datareader ADD MEMBER mcp_reader;
```

### 3. Smoke-test it

Every harness below runs the same command, so prove it works once by hand. This
launches the server over stdio, initializes, and calls the built-in query tool:

```bash
{ printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sales_query","arguments":{"query":"SELECT TOP 5 name FROM sys.tables"}}}'; sleep 2; } \
| ./mssql-mcp-toolkit --tool-prefix=sales --read-only \
    --conn-string='sqlserver://mcp_reader:PASSWORD@localhost:1433?database=AdventureWorks&encrypt=true&TrustServerCertificate=true'
```

Two flags are required: `--conn-string` and `--tool-prefix` (the prefix names
every built-in tool, so `--tool-prefix=sales` gives `sales_query`). Everything
else has a default. **The server reads no environment variables;** flags are the
only configuration, so put them in the client's `args`.

### 4. Connect your LLM harness

All of these run the same command: the binary, `--tool-prefix`, `--conn-string`,
and any other flags. Use an absolute path to the binary (on Windows, the full
path to the `.exe`, with doubled backslashes inside JSON). After saving, restart
or reload the client; the tools appear as `sales_query`, `sales_get_metadata`,
`sales_list_metadata`, plus anything from a `--query-tools` file.

#### Claude Desktop

Edit `claude_desktop_config.json` (macOS: `~/Library/Application Support/Claude/`,
Windows: `%APPDATA%\Claude\`) and restart Claude Desktop:

```json
{
  "mcpServers": {
    "adventureworks": {
      "command": "/usr/local/bin/mssql-mcp-toolkit",
      "args": [
        "--read-only",
        "--tool-prefix=sales",
        "--conn-string=sqlserver://mcp_reader:PASSWORD@sqlhost:1433?database=AdventureWorks&encrypt=true&TrustServerCertificate=true"
      ]
    }
  }
}
```

#### Claude Code

```bash
claude mcp add adventureworks -- /usr/local/bin/mssql-mcp-toolkit \
  --read-only --tool-prefix=sales \
  '--conn-string=sqlserver://mcp_reader:PASSWORD@sqlhost:1433?database=AdventureWorks&encrypt=true&TrustServerCertificate=true'
```

Everything after `--` is the server command. `claude mcp list` confirms it, and
`/mcp` inside a session shows its tools. A `--scope project` registration writes
`.mcp.json` at the repo root; that file is usually committed, so keep the
connection string out of it (see below).

#### VS Code (GitHub Copilot agent mode)

`.vscode/mcp.json` in the workspace, or the user-level MCP configuration:

```json
{
  "servers": {
    "adventureworks": {
      "type": "stdio",
      "command": "/usr/local/bin/mssql-mcp-toolkit",
      "args": [
        "--read-only",
        "--tool-prefix=sales",
        "--conn-string=sqlserver://mcp_reader:PASSWORD@sqlhost:1433?database=AdventureWorks&encrypt=true&TrustServerCertificate=true"
      ]
    }
  }
}
```

MCP tools are available in Copilot's **Agent** mode.

#### Cursor

`~/.cursor/mcp.json` (all projects) or `<repo>/.cursor/mcp.json`:

```json
{
  "mcpServers": {
    "adventureworks": {
      "command": "/usr/local/bin/mssql-mcp-toolkit",
      "args": [
        "--read-only",
        "--tool-prefix=sales",
        "--conn-string=sqlserver://mcp_reader:PASSWORD@sqlhost:1433?database=AdventureWorks&encrypt=true&TrustServerCertificate=true"
      ]
    }
  }
}
```

#### Continue.dev

`~/.continue/config.yaml`, or a block file at
`<repo>/.continue/mcpServers/sql-server.yaml`. [`config.yaml`](config.yaml) in
this repo is a complete, commented example with two databases:

```yaml
mcpServers:
  - name: AdventureWorks (sales)
    type: stdio
    command: /usr/local/bin/mssql-mcp-toolkit
    args:
      - --read-only
      - --tool-prefix=sales
      - "--conn-string=sqlserver://mcp_reader:PASSWORD@sqlhost:1433?database=AdventureWorks&encrypt=true&TrustServerCertificate=true"
    connectionTimeout: 20000
```

MCP tools are only available in Continue's **Agent** mode.

#### Open WebUI, and other clients that attach by URL

Clients that connect to a running server over HTTP need `--transport=http`. The
server listens on loopback only, and a shared secret keeps other local processes
out:

```bash
head -c 32 /dev/urandom | base64 > /etc/mssql-mcp-toolkit/token     # one line, no spaces
mssql-mcp-toolkit --transport=http --http-addr=127.0.0.1:8080 \
  --http-auth-token-file=/etc/mssql-mcp-toolkit/token \
  --user-header=X-OpenWebUI-User-Id \
  --tool-prefix=sales --read-only \
  --conn-string='sqlserver://mcp_reader:PASSWORD@sqlhost:1433?database=AdventureWorks&encrypt=true&TrustServerCertificate=true'
```

In Open WebUI (menu names vary by version), add the server under **Admin Settings → External Tools** as an
**MCP (Streamable HTTP)** connection: URL `http://127.0.0.1:8080/`, auth
**Bearer** with the token, and an **ID** of your choosing. Open WebUI names tools
`<ID>_<tool>` for the model, so also pass `--tool-call-name='{server}_{tool}'`
(`{server}` is the server's label, which must match the ID; see
[`--tool-call-name`](#tool-groups-one-server-per-domain)). `--user-header` keeps
each signed-in user's stored results private to them.

Two things trip this setup up:

- **Host header.** The server refuses a request whose `Host` is not a loopback
  name (`403`), a DNS-rebinding guard. A client in a container cannot reach it as
  `host.docker.internal`; use host networking, or a tunnel/reverse proxy that
  presents a loopback `Host`.
- **Clients that do not echo `Mcp-Session-Id`.** Add `--http-stateless`.

For any other client that speaks streamable HTTP, point it at the URL with the
bearer header. `curl` against the same URL is the quickest check:

```bash
curl -sS -X POST http://127.0.0.1:8080 \
  -H "Authorization: Bearer $(cat /etc/mssql-mcp-toolkit/token)" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}'
```

#### Keeping the password out of config files

The connection string is a command-line argument, so it is visible in `ps`
output to anyone on that host, and it sits in the client's config in plaintext.
Use a dedicated low-privilege login, restrict the config file's permissions
(`chmod 600`), and keep it out of version control. For a repo-level `.mcp.json`
or `.vscode/mcp.json` you intend to commit, put the connection string in a
wrapper script outside the repo and have the config call that script.

### 5. Add your own tools

Create `tools.yaml` — each entry is one tool the model can call with parameter
values:

```yaml
- name: orders_for_customer
  description: Orders for one customer on or after a date, newest first.
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
      type: date
      description: Orders on or after this date (yyyy-mm-dd, or an offset like -30d)
  outputFormat: csv
```

and add `--query-tools=/path/to/tools.yaml` to the server's args. Add
`--query-tools-only` to drop `<prefix>_query` and the metadata tools, so the model
can call *only* the queries you wrote. The full schema is in
[The tool file (YAML)](#the-tool-file-yaml);
[`query-tools.example.yaml`](query-tools.example.yaml) demonstrates every field.

### If something does not connect

Run the [smoke test](#3-smoke-test-it) first; if it works, the server is fine
and the problem is in the client's config (relative path, a leftover `env:` block
that does nothing, a missing `--tool-prefix`). Startup errors name the offending
flag. For HTTP clients, `--http-log` shows what arrived and what status it got;
see [When a client says it cannot connect](#when-a-client-says-it-cannot-connect).

## Command-line reference

**Flags are the only configuration input.** The server reads no environment
variables — not for the connection string, not for anything else. A setting that
can arrive from two places is a setting you have to check in two places when the
wrong database answers a question. Passing a flag with an empty value counts as
not passing it.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--conn-string` | *(required)* | SQL Server connection string, in `sqlserver://` URL or ADO (`key=value;`) form |
| `--tool-prefix` | *(required, except with `--query-tools-only`)* | Prefix for every built-in tool: `--tool-prefix=sales` gives `sales_query` |
| `--transport` | `stdio` | `stdio` (a client launches the binary) or `http` (listen on localhost) |
| `--http-addr` | `127.0.0.1:8080` | Loopback address for `--transport=http`. **Rejected under stdio** |
| `--http-stateless` | `false` | HTTP only: no per-session state, for clients that do not return `Mcp-Session-Id`. `GET`/`DELETE` then return 405 |
| `--http-log` | `false` | HTTP only: one log line per request (method, host, session, rpc method, status, timings, user) |
| `--http-log-headers` | `false` | HTTP only: log every header received with each tool call. Verbose; `Authorization`, `Proxy-Authorization` and `Cookie` are redacted |
| `--http-auth-token-file` | *(none)* | HTTP only: file holding a shared secret. Every request on every listener must carry `Authorization: Bearer <secret>` or gets `401` |
| `--user-header` | *(none)* | HTTP only: header carrying the calling user's id (Open WebUI: `X-OpenWebUI-User-Id`). Stored results are then per user |
| `--query-tools` | *(none)* | Path to a [tool file](#the-tool-file-yaml) |
| `--query-tools-only` | `false` | Expose only the `--query-tools` tools. Requires `--query-tools` |
| `--tool-call-name` | `{tool}` | How the client names tools to the model when it namespaces them by server: a template over `{server}` and `{tool}`. Must contain `{tool}` |
| `--read-only` | `false` | Reject anything but `SELECT`/`WITH` on `<prefix>_query`. See [Safety](#safety) for what it does not cover |
| `--max-rows` | `200` | Row cap per reply. `0` = no limit |
| `--max-bytes` | `262144` (256 KiB) | Cap on the total size of the returned rows. `0` = no limit |
| `--max-cell-bytes` | `4096` (4 KiB) | Cap on a single returned value; longer values are cut and marked in place. `0` = no limit |
| `--query-timeout` | `30s` | Per-query timeout |
| `--max-open-conns` | `4` | Connection pool size |
| `--result-store` | `true` | Keep every list result under a handle. `false` turns [handles](#stored-results-handles-and-operators) off |
| `--result-store-max-rows` | `100000` | Most rows one stored result keeps; a result cut here is marked truncated |
| `--result-store-max-bytes` | `1073741824` (1 GiB) | Memory the store may use for all users together; least recently used results are dropped past it |
| `--result-store-user-bytes` | `268435456` (256 MiB) | Memory one user's results may use. Cannot exceed `--result-store-max-bytes` |
| `--result-store-ttl` | `2h` | How long a stored result lives after it was last used |
| `--max-stored-cell-bytes` | `65536` (64 KiB) | Cap on a value kept in a stored result — what `show_field` can return. `0` = no limit |
| `--display-cell-chars` | `200` | Longest value, in characters, a reply over a stored result shows; a longer one ends `…[+N chars, row R]` and `show_field` returns the rest. `0` = show whole |
| `--calc-sql` | `false` | Also expose `<prefix>_calc_sql`, a read-only SQLite `SELECT` over stored results |
| `--metadata-dir` | *(none)* | Directory of `.md`/`.html` table documentation named `schema.table.md`. See [Metadata](#metadata-documentation-for-the-model) |
| `--metadata-dir-schema` | *(none)* | Schema assumed for files in `--metadata-dir` named `table.md`. Needs `--metadata-dir` |
| `--metadata-file` | *(none)* | `schema.table:path/to/file.md`; repeatable |
| `--descr-db` | *(database in `--conn-string`)* | The name descriptions and instructions use for this database |
| `--query-fn-desc` | *(names the database)* | Description the query tool advertises |
| `--get-metadata-fn-desc` | *(names the database)* | Description `get_metadata` advertises |
| `--list-metadata-fn-desc` | *(names the database)* | Description `list_metadata` advertises |
| `--server-label` | `mssql-mcp-toolkit-<prefix>` | The name this server reports at initialize, i.e. in the client's server list |
| `--instructions` | *(describes this database)* | Instructions delivered to the model once, at initialize |

The `--http-*` flags and `--user-header` are errors under `--transport=stdio`
rather than silently ignored: an ignored address leaves an operator with a port
written in a config file, a client that cannot connect, and no message saying
that nothing was ever listening.

### Connection strings

Either form supported by `go-mssqldb`:

```
sqlserver://user:password@host:1433?database=MyDb&encrypt=true&TrustServerCertificate=true
server=host,1433;user id=user;password=pass;database=MyDb;encrypt=true
server=localhost;database=MyDb;trusted_connection=yes        # Windows auth, Windows host
```

If the password contains URL-special characters, use the ADO form or
percent-encode it.

### Identity: `--tool-prefix` and friends

The prefix has no default. Every tool is named after it, and the composed name
must come out as 1–64 characters of letters, digits, `_` or `-`. A trailing
separator is normalised away (`sales_` and `sales` name the same tools), and a
prefix of only `_` or whitespace counts as absent. Keep prefixes short: some
clients namespace tool names again on top (`mcp__sales__sales_query`).

It is required rather than defaulted because the failure it guards against is
invisible at runtime. Two servers exposing a tool with the same name get handled
differently by different clients — namespaced, refused, or silently shadowed —
and in the last case every call lands on whichever database registered first,
with plausible answers and no diagnostic. If you start without a prefix, the
error suggests one parsed from your connection string.

The prefix solves collision; `--query-fn-desc` solves *selection*. It passes
through every client verbatim, while tool names get rewritten and namespaced.
Say what actually lives in the database — it is a far stronger signal to the
model than `sales_query` versus `hr_query`:

```
--query-fn-desc="Query the AdventureWorks sales database: orders, customers,
products and inventory. One T-SQL statement per call."
```

`--get-metadata-fn-desc` and `--list-metadata-fn-desc` do the same for the
metadata tools. `--descr-db` sets the single name all of those defaults are
written around — the catalog in the connection string (`SALES_DW_PRD01`) is often
not what anyone asking a question calls the database.

`--instructions` is delivered once, at initialize, and shapes the whole session.
The built-in text names the database, points the model at the metadata tools
before it guesses a table name, and asks for schema-qualified names, `TOP`, and
one statement per call. Whatever the instructions, each initialize ends them with
today's date (`Today is Thursday, 2026-09-24.`), taken from the clock at that
moment: left without it, a model works the date out from the data and can be
months out.

## Execution modes

`--transport` picks one of two, and they are alternatives rather than layers.

**`--transport=stdio`** (the default) is what an MCP client launches. The client
starts the binary and speaks JSON-RPC over stdin/stdout; the server lives and
dies with it. Nothing listens on any port.

**`--transport=http`** serves the streamable HTTP transport on localhost, for
clients that attach to an already-running server by URL, and for several clients
on one machine sharing a single connection pool (`--max-open-conns` is then the
ceiling for the machine). The address actually bound is logged at startup, so
`--http-addr=127.0.0.1:0` tells you which port the kernel chose.
`SIGINT`/`SIGTERM` drains requests in flight and exits.

### It really is localhost only

The host half of `--http-addr` must be `localhost` or a loopback IP. A bare
`:8080` is read as `127.0.0.1:8080`, and anything routable is refused at startup.
**Without `--http-auth-token-file` this server has no authentication of any
kind**: anything that can reach the port can read everything the SQL login can
read. The token closes the port to other local processes; it is a shared secret,
not per-person authentication, and it does not make a routable bind safe. To
reach the server from another machine, put it behind something that authenticates
— an SSH tunnel or a reverse proxy — rather than widening the bind. The SDK's
DNS-rebinding protection stays on: a request on loopback with a non-loopback
`Host` header gets `403`.

### Authentication and per-user results

- `--http-auth-token-file` reads a one-line secret from a file (a file, not a flag
  value, so `ps` does not show it). Every request on every listener — the base
  port and each group's — must carry `Authorization: Bearer <secret>`; anything
  else gets `401` before the MCP handler runs.
- `--user-header=X-OpenWebUI-User-Id` names the header the client stamps with the
  signed-in user's id. [Stored results](#stored-results-handles-and-operators) are
  then kept per user: a handle is readable only by the user whose call created it,
  through every entry point, and another user's handle gets exactly the error an
  unknown one does. A call without the header still gets its rows, but no handle.

The header is a claim, not a proof; it is trustworthy because only the holder of
the token can send it. `--user-header` without `--http-auth-token-file` logs a
startup warning, since any local process could then claim to be any user. Prefer
a stable id over a display name. Under stdio there is one caller and neither flag
applies; under HTTP without `--user-header`, every caller shares one set of
handles.

### When a client says it cannot connect

All the failure modes look identical from the client ("failed to connect"), and
`--http-log` tells them apart:

```
http POST / host=127.0.0.1:8890 session=- rpc=initialize -> 200 (answered in 2ms, held 2ms)
http POST / host=127.0.0.1:8890 session=P7VCVP… rpc=tools/list -> 200 (answered in 1ms, held 1ms)
```

| What you see | What it is | Fix |
| --- | --- | --- |
| A clean session, no non-2xx anywhere | Nothing failed here; the client rejected what it got afterwards | Look at how it consumes the tool schemas |
| `404` on a `POST` with a `session=` | The client is replaying a session this process never issued (or issued before a restart) | `--http-stateless` |
| `400` on a `GET` with `session=-` | The client opens a notification stream without a session id | `--http-stateless` |
| `401` | Missing or wrong bearer token | Check the `Authorization` header |
| `403`, any method | The rebinding guard: `host=` is not a loopback name | Reach the server as `127.0.0.1` |
| Nothing logged at all | The request never arrived: wrong port, or a container that cannot reach the host's loopback | Check networking |

When a header is rewritten on the way through a proxy, `--http-log-headers` dumps
every header received for each `tools/call` (credential headers redacted). It is
a debug aid; turn it off again afterwards.

## Built-in tools

| Tool | When it exists | What it does |
| --- | --- | --- |
| `<prefix>_query` | Unless `--query-tools-only`. Base server only | Run one T-SQL statement |
| `<prefix>_get_metadata`, `<prefix>_list_metadata` | Same | Read the documentation you supply with `--metadata-*` |
| `<prefix>_describe_tool` | When a `--query-tools` file defines tools | Full description, details, parameters and columns of a custom tool |
| `<prefix>_show`, `<prefix>_show_field` | Handles on (the default). Every server | Page a stored result; read a value that was shown cut |
| Operators (`set_union`, `filter`, `join`, …) | Handles on. One server | See [Stored results](#stored-results-handles-and-operators) |
| `<prefix>_calc_sql` | `--calc-sql` | Read-only SQLite `SELECT` over stored results |
| Your tools | `--query-tools` | See [The tool file](#the-tool-file-yaml) |

Without a prefix (`--query-tools-only` allows that), the unprefixed name is used.

### `<prefix>_query`

| Argument | Type | Required | Meaning |
| --- | --- | --- | --- |
| `query` | string | yes | One T-SQL statement |
| `max_rows` | integer | no | Cap the rows returned by this call; it can never raise the cap above `--max-rows` |

Only `query` is required — small models tend to leave `max_rows` out, and nothing
else is needed. Structured result:

```json
{
  "query": "SELECT TOP 1 OrderID, Customer, Total FROM dbo.Orders",
  "columns": ["OrderID", "Customer", "Total"],
  "rows": [ { "OrderID": 1, "Customer": "Acme", "Total": "199.99" } ],
  "row_count": 1,
  "rows_affected": -1,
  "truncated": false,
  "notes": [],
  "handle": "qx4",
  "total_rows": 1
}
```

`handle` and `total_rows` appear when [results are stored](#stored-results-handles-and-operators).
`query` restates the statement as received, so a log of the response carries the
SQL that produced it. The rows are also returned as text content, as a markdown
table (not JSON again, which would send every row twice to a client that honours
both channels); the row count and any notes follow the table. A statement with no
result set keeps the JSON form, where `rows_affected` and the notes are the whole
answer.

**The output budget.** Every result is read by a model with a finite context
window. Three limits bound it, because a result overflows in three different
ways and a row cap only catches the first:

| Limit | Default | Catches |
| --- | --- | --- |
| `--max-rows` | 200 rows | A million small rows |
| `--max-bytes` | 256 KiB | A few hundred very wide rows |
| `--max-cell-bytes` | 4 KiB | One `NVARCHAR(MAX)` or base64'd blob |

Whichever is reached first stops the result: `truncated` is `true` and a note says
which limit and what to do about it. An oversized value is cut on a rune boundary
and marked in place (`…[truncated, 1.2 MiB total]`). **The first row always comes
back**, even if it alone exceeds `--max-bytes`. `0` means "no limit" for all
three — something to ask for, not to get by forgetting a setting. There is no
per-call equivalent of the byte budgets: they protect the model's context, which
is not the caller's to spend.

**Behaviour worth knowing:**

- **Values.** `DECIMAL`/`NUMERIC`/`MONEY` come back as strings so precision
  survives JSON; `DATETIME*` as RFC 3339; `UNIQUEIDENTIFIER` as a GUID string;
  binary as base64. `NULL` is JSON `null`, and spelled `NULL` in the text table.
- **One result set.** Only the first set that has columns is returned; the rest
  are reported in `notes`.
- **Write statements.** Unless `--read-only` is set, `INSERT`/`UPDATE`/`DELETE`/DDL
  run and report `rows_affected`. A statement with an `OUTPUT` clause runs as a
  query, so those rows are returned.
- **Errors.** A SQL error (bad object name, syntax, timeout) comes back as an MCP
  tool error with the server's message, so the model can correct itself, with the
  failing statement restated on a second line (`sql: SELECT ...`).
- **Duplicate column names.** A repeated name is suffixed (`id`, `id_2`) and
  unnamed columns become `column_N`; no value is lost.
- **Session state.** `USE` and `SET` run on whichever pooled connection is free,
  so their effect does not carry to the next call; the result says so. Qualify
  names across databases (`OtherDb.dbo.Orders`) and put any `SET` in the same
  batch as the query that needs it.
- **Cancellation.** A call that exceeds `--query-timeout` is reported as a
  timeout; one the client cancels is reported as a cancellation, so the model
  does not rewrite a query that was never slow.

### Metadata: documentation for the model

`<prefix>_list_metadata` lists the tables and views you have documented, and
`<prefix>_get_metadata` returns one document by qualified name (`dbo.orders`;
case-insensitive). The documentation is files you write — Markdown or HTML —
supplied two ways, and the two combine:

```bash
--metadata-dir=/etc/mssql-mcp-toolkit/docs                 # dbo.orders.md, dbo.customers.html, ...
--metadata-dir-schema=dbo                                  # lets orders.md answer for dbo.orders
--metadata-file=sales.invoices:/etc/docs/invoices.md       # an explicit pair; repeatable
```

A file in the directory must be named `schema.table.md` / `.html`, or
`table.md` when `--metadata-dir-schema` is set (without a schema, an unqualified
file is skipped, because there is nothing to qualify it with). `--metadata-file`
wins over the directory for the same name. With no metadata configured,
`list_metadata` says so.

## The tool file (YAML)

`--query-tools=<path>` loads a YAML file that defines extra tools. YAML is the
format so the SQL can go in as a `|` block scalar: a statement you have already
written and tested pastes in verbatim, with no escaping and no collapsing onto
one line. **The loader rejects any key it does not know**, and every mistake below
is a startup error naming the tool and the problem, so a typo cannot silently
change behaviour.

### File shapes

The file may be any of:

| Shape | Use it for |
| --- | --- |
| A **list** of tools | The simplest form: one file, all tools |
| A **single tool** (a mapping with `name:`) | One tool per file, in a split tree |
| A **mapping** with any of `groups:`, `tools:`, `include:`, `sharedParameters:` | Per-port server groups, a split tree, shared parameter text |

```yaml
# Mapping form: every key is optional.
groups:               # logical servers, one port each under --transport=http
  <group-name>: { …see Group fields… }
sharedParameters:     # describe recurring parameters once
  - { name: …, description: …, … }
tools:                # tool definitions (same records as the list form)
  - { name: …, … }
include:              # pull more tools in from other files and directories
  - path/relative/to/this/file
```

A file that defines no tools is a startup error.

### Tool fields

| Field | Required | Meaning |
| --- | --- | --- |
| `name` | yes | The tool name, used verbatim: 1–64 characters of letters, digits, `_`, `-`. Must not collide with a built-in or another tool |
| `description` | yes | What the model reads when it picks the tool. One or two sentences on *when* to use it |
| `details` | no | The long form: column meanings, defaults, caveats, the sibling that fits better. Not advertised; returned by `<prefix>_describe_tool` on request. See [Keeping definitions small](#keeping-definitions-small) |
| `query` | yes | One T-SQL statement. Bind parameters are written `@name` |
| `outputFormat` | yes | `csv`, `md` (markdown table), or `scalar` (one value) |
| `parameters` | no | The parameters the client supplies; see below |
| `connectionString` | no | The database this query runs against. Omitted or blank uses `--conn-string` |
| `resultColumn` | no | `scalar` only: which column the value comes from. Required when the query returns more than one column |
| `columns` | no | `csv`/`md` only: the columns that reach the client, in this order. See [`columns` and `pickRecord`](#columns-and-pickrecord) |
| `pickRecord` | no | `csv`/`md` only: `true` or `optional`. Reduce the result to one record, chosen by the user. See below |
| `requireAnyOf` | no | Optional parameters of which a call must supply at least one non-blank value. See [`requireAnyOf`](#requireanyof) |
| `requireHint` | no | Text appended to the `requireAnyOf` error: where to go when the question is wider than the filters the caller has |
| `group` | no | Puts the tool on a logical server of its own. See [Tool groups](#tool-groups-one-server-per-domain) |

`outputFormat` also accepts the aliases `markdown`, `md-table`, `table` (for `md`)
and `scalar-value`, `value` (for `scalar`).

Rules the loader enforces: `columns`/`pickRecord` do not apply to `scalar`;
`resultColumn` applies only to `scalar`; a name in `columns` may not be empty or
repeated; and names are case-sensitive for tools but case-insensitive for
parameters and columns.

### Parameter fields

| Field | Required | Meaning |
| --- | --- | --- |
| `name` | yes | A SQL identifier, bound to `@name` in the query. Not `Columns`, `RequireSingle`, `SaveAs` or `AllowPartial` (reserved) |
| `description` | no | Advertised to the model. May be blank on a parameter that `sharedParameters:` describes |
| `type` | no | `string` (default), `int`, `number`, `bool`, `date`. Aliases: `integer`; `float`, `decimal`, `double`; `boolean`; `str`, `text`. A native value and the same value quoted as a string are both accepted |
| `required` | no | Defaults to `true`. An absent optional parameter is bound as SQL `NULL` |
| `batch` | no | Marks a comma-separated id list that also accepts `@handle.Column`. Inferred when the query passes the parameter to `STRING_SPLIT`; `false` opts out, `true` marks a list the query splits another way. `string` parameters only |
| `literal` | no | On a batch parameter, also accept a typed-out comma list. Without it a batch parameter is handle-only: a literal value is refused before the query runs, so every id a tool joins on came from a stored result |
| `accepts` | no | On a batch parameter, the extra column names, besides one spelled like the parameter, that it takes as `@handle.Column`. Any other column is refused, which stops an id from one id space being passed where another is wanted. The first entry is the default column |

Rules the loader enforces: a parameter the query never mentions as `@name` is an
error (the value would be accepted and silently dropped); the reverse is allowed,
so a query may use `@@ROWCOUNT` or `DECLARE` its own locals; and a parameter name
may not repeat.

**Values are always sent as bound parameters, never substituted into the SQL**, so
a parameter cannot inject SQL.

**Dates.** A `date` parameter takes an absolute `yyyy-mm-dd` or an offset from
today: `today`, `yesterday`, `tomorrow`, or `[+-]N` followed by `d`, `w`, `m` or
`y` (`-7d`, `-2w`, `-1m`). Month and year offsets clamp to the end of the target
month (`-1m` from March 31 is February 28). The offset form exists because small
models rarely know today's date and get calendar arithmetic wrong; the server
resolves it against its own clock and binds a SQL `date`. The loader advertises a
matching `pattern` and states the forms once in the initialize instructions, so a
description need not repeat them.

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
      type: date
      description: Orders on or after this date
  outputFormat: csv

- name: customer_balance
  description: A single customer's current account balance.
  query: SELECT Balance FROM dbo.Accounts WHERE CustomerID = @customer
  parameters:
    - name: customer
      type: int
  outputFormat: scalar
```

The `--max-rows` / `--max-bytes` / `--max-cell-bytes` budget applies to these
results as it does to `<prefix>_query`. [`query-tools.example.yaml`](query-tools.example.yaml)
is a worked example (and is exercised by the test suite, so it stays loadable): all
three output formats, typed and optional parameters, `resultColumn`, `columns`,
`pickRecord`, `requireAnyOf`, and a tool with its own `connectionString`.

### Output formats

- **`csv` / `md`** return a list. With handles on (the default) the whole result
  is stored and the reply leads with a handle; see
  [Stored results](#stored-results-handles-and-operators).
- **`scalar`** returns one value as the tool's text content (and as `value` in the
  structured output). One column is taken as the value; more than one needs
  `resultColumn`. One row gives its value; no rows is an error. More than one row
  cannot be resolved on its own, so the server asks: it sends an MCP
  **elicitation** whose form is a picker with one option per row, and takes the
  value from the record the person chooses. A client without the elicitation
  capability gets an error telling it to narrow the query. Scalar results get no
  handle.

### `columns` and `pickRecord`

`columns` trims and reorders the fields a `csv`/`md` tool returns. The query
selects whatever it needs (joins, `SELECT *`, a view you do not control); the
client only ever sees the listed columns. On its own it does not change the row
count. **There is no `Columns` argument on a custom tool:** a model has to format
a comma-separated list correctly, small models often cannot, and a failed call
costs a round trip. To keep a reply small, narrow the *rows* (filters, a page
size). A client that sends `Columns` anyway is not refused — the argument is
ignored and the reply says so.

`pickRecord: true` narrows the *rows* to one:

- **One row** is returned directly, trimmed to `columns`.
- **No rows** is an error.
- **Several rows** trigger an **elicitation**: a picker with one option per
  record, showing the values of **every column** the query selected (so select the
  fields that distinguish records, not just the ids `columns` will return). The
  tool then returns just the chosen record and just the `columns` fields. A client
  without elicitation gets an error asking it to narrow the query.

The review happens in the elicitation prompt, which the model never sees, so what
lands in the model's context is one projected row rather than the candidate set.

`pickRecord: optional` keeps the tool a list tool but advertises one more
argument, a boolean **`RequireSingle`**, which the initialize instructions tell
the model to set when the user means one particular record. With
`RequireSingle: true` it behaves as above, except that no rows is an ordinary
empty result. Without it, the full list comes back. Sending `RequireSingle` to a
tool that does not offer it is an unrecognized-parameter error.

Because the person picks from the rows the query returned, a small page can hide
the one they want: the instructions steer the model toward a large `PageSize`, and
when a tool has a parameter named `PageSize` and the page came back full, the
picker says so. (`PageSize` is only a naming convention; the loader gives it no
other meaning.)

### `requireAnyOf`

A search tool usually makes every filter optional so the caller can combine them
— `WHERE (@LastName IS NULL OR …) AND (@FirstName IS NULL OR …)`. The cost is
that a call supplying none of them is still valid and returns the whole table.

```yaml
requireAnyOf: [LastNameContains, FirstNameContains]
requireHint: For a whole-department count use department_headcount instead.
```

Each name must be a declared parameter that is itself `required: false`. A call is
rejected before the query runs unless at least one of them has a real value —
absent, `null` and whitespace-only all count as missing, since a blank filter
would bind fine and then match everything. The error names the parameters that
would have satisfied it. This matters because some MCP clients validate calls
against the schema and drop unknown keys: a model that guesses `SearchLastName`
for a filter named `LastNameContains` would otherwise reach the server with no
filter at all and get a quiet, unfiltered page.

### Keeping definitions small

Every tool's description and input schema sits in the model's context on every
turn, whether or not the tool is called. With sixty tools that is the largest
thing in the prompt, and a small model starts picking wrong tools or none.

**`description` is short, `details` is long.** Put *when to use it* in
`description`; put everything else in `details`, which is served by
`<prefix>_describe_tool` (the initialize instructions tell the model to call it
before using a tool for the first time). It accepts the served name or the
client's call name, and answers for every tool in the file, saying which server a
sibling's tool lives on.

**`sharedParameters` describes recurring parameters once.** A file whose tools
mostly take the same `PageSize`, `PageNumber`, customer id and date window need not
repeat those descriptions on every tool. Each entry needs a `name` and a
`description`; `type`, `required`, `batch`, `literal` and `accepts` are inherited
by a tool's parameter of the same name:

```yaml
sharedParameters:
  - name: PageSize
    type: int
    description: Rows per page. Pair with PageNumber to page through a large result.
  - name: FiscalYear
    type: string
    description: Fiscal year. One year, or several comma-separated to cover a span.
```

A tool's parameter of that name then needs only its `name:`; it is advertised
**without a description**, and the shared text goes into the server's initialize
instructions once, listing only the shared parameters that server's tools use. A
tool that does write a description keeps it — that is the override. The block
belongs in the root file; an included file that carries one is rejected.

**Conventions move to the instructions too.** The accepted date forms and what
`RequireSingle` does are each stated once in the initialize text, only when a tool
on that server has a date parameter or `pickRecord: optional`.

### Exposing only the custom tools: `--query-tools-only`

By default the file's tools are added *alongside* `<prefix>_query` and the
metadata tools. `--query-tools-only` drops those built-ins, so the model can only
call the queries you wrote and never composes SQL of its own. It requires
`--query-tools`. `--tool-prefix` is then optional (it only ever named the
built-ins); give one anyway to label the server `mssql-mcp-toolkit-<prefix>` in the
client's list, and it is still validated.

### Tool groups: one server per domain

A small local model pays for every tool schema in its list whether or not it calls
the tool. Groups split a large file into logical MCP servers, one per port under
`--transport=http`, so a client wires up only the groups an assistant needs.
Tools opt in with `group:`; the root's `groups:` block is optional metadata:

```yaml
groups:
  orders:
    label: sales_orders        # the server's name at initialize
    port: 8083                 # an absolute TCP port …
    # order: 3                 # … or an offset added to the --http-addr port
    description: >-            # folded into the initialize instructions; this is what
      Daily order history,     # a model reads to choose the server
      per-customer summaries and regional rollups.
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

Group fields:

| Field | Meaning |
| --- | --- |
| `label` | The name this server reports at initialize. Blank falls back to `<server-label>-<group>` |
| `description` | A sentence or two on what the group covers, folded into the initialize instructions |
| `instructions` | If set, replaces the generated initialize text outright |
| `port` | Absolute TCP port (1–65535) under `--transport=http` |
| `order` | Offset (≥ 0) added to the `--http-addr` port. Set `port` or `order`, not both. With neither, the group takes the next free port above the base, in the order the group first appears |
| `prefix` | Written in front of every member tool's *served* name: with `prefix: cust`, `flag_summary` is served as `cust_flag_summary` |
| `operators` | `true` puts the [operator tools](#stored-results-handles-and-operators) on this group's server. Such a group needs no query tools of its own. At most one group may set it |

- **Ports.** The `--http-addr` port serves the built-ins plus any tool with no
  `group:`. Two groups on one port, or a group on the base port, is a startup
  error. Under **stdio** there is one stream and so one server: groups are inert
  and every tool is served together, so the same file works both ways.
- **Typos are caught.** A tool may name a `group:` absent from `groups:` (it gets a
  derived label and generic instructions), but an entry in `groups:` that no tool
  references is a startup error — unless it sets `operators: true`.
- **Cross-references are written the way the model must call them.** A small model
  pairs a tool with the wrong sibling server whenever a description says "use
  `flag_summary` for the quick yes/no" and nothing says where that tool is. The
  loader rewrites every mention of a tool in tool, parameter and group descriptions
  (never in the SQL) to the form the model must emit, and follows the first mention
  of a tool from another server with "(on the `<label>` server)". Keep writing the
  declared names.
- **`--tool-call-name` matches the client's namespacing.** Some clients show a tool
  to the model as `<server>_<tool>` — Open WebUI does, with the connection's ID as
  `<server>`. `--tool-call-name='{server}_{tool}'` tells the loader that, with
  `{server}` the group's `label`, so the mentions and the initialize text show the
  exact string to copy (`cust_flag_summary`). The default `{tool}` is for clients
  that use names as served.
- **`prefix:` is for clients that do not namespace.** Do not combine it with a
  namespacing client — the model would see `cust_cust_flag_summary`. A prefix that
  makes two tools serve under one name, or a served name that collides with a
  built-in, is a startup error.

Client side, one entry per port. With Continue.dev:

```yaml
mcpServers:
  - name: Sales orders
    type: streamable-http
    url: http://127.0.0.1:8083/
  - name: Sales returns
    type: streamable-http
    url: http://127.0.0.1:8084/
```

### Splitting the file: `include:`

One file per tool gets unwieldy fast. `include:` lets the root be thin:

```yaml
groups:
  lookups:   { label: ref,  port: 8081, description: … }
  customers: { label: cust, port: 8082, description: … }

include:
  - tools/lookups          # every *.yaml / *.yml in the directory, in filename order
  - tools/customers
```

```
tools.yaml                 # the root: groups: + include:
tools/
  lookups/
    find_region.yaml       # one tool per file; the file IS the tool mapping
    find_product.yaml
  customers/
    find_customer_by_name.yaml
```

- Each entry is resolved **relative to the file that lists it**. A directory
  contributes every `*.yaml` / `*.yml` file directly inside it, in filename order,
  skipping names that start with `.` or `_`; a file contributes just itself.
- An included file is any of the shapes above — a single tool, a list, or another
  mapping with its own `tools:` / `include:`. So the tree can nest. An include
  cycle is a startup error.
- A tool loaded from a directory **inherits its `group:` from the directory's
  name**. An explicit `group:` is checked against the directory; a mismatch is a
  startup error.
- `groups:` and `sharedParameters:` belong in the root. Defining a group in more
  than one file, or a tool name twice anywhere in the tree, is a startup error.

### Validation at a glance

Startup fails, naming the tool, on: a missing `name`, `description`, `query` or
`outputFormat`; an invalid or duplicate tool name; a name that collides with a
built-in; an unknown `outputFormat`, `type` or `pickRecord` value; a declared
parameter the query never uses; a reserved parameter name; `resultColumn` on a
non-scalar or `columns`/`pickRecord` on a scalar; `requireAnyOf` naming an
unknown or already-required parameter; `batch` on a non-`string` parameter;
`accepts` on a non-batch parameter; an unknown YAML key; and every
group, port and include problem listed above.

## Stored results: handles and operators

A model is good at choosing the next step and bad at carrying hundreds of ids
from one reply into the next call, or at counting, intersecting and subtracting
sets in its head. Stored results move that work into the server.

**Every list result gets a handle.** A `csv`/`md` tool (and `<prefix>_query`) runs
its statement for the whole result — up to `--result-store-max-rows` — stores it,
and leads its reply with the handle:

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

- A result of 50 rows or fewer shows every row. A larger one shows at most 5 —
  from the start of the page asked for — and is marked partial twice (on the handle
  line and in a trailer after the last row, which is what a model reads last), since
  small models otherwise read the rows as the whole result. Only `show` pages at the
  size asked for (20 by default).
- The **profile** gives the columns with one value in every row on one line, then
  value breakdowns of low-cardinality columns (where a wrong scope shows up), date
  and number ranges, distinct counts of id columns, and the always-NULL columns. It
  is deterministic and capped at 2 KiB.
- The handle line shows how to pass the result on, using its own id columns,
  because small models copy an example more reliably than they apply a rule.
- The capture asks the statement for page 1 at one row past the store's limit
  (binding `PageSize`/`PageNumber` itself), so a result cut at the limit is marked
  `truncated`. The reply's own `--max-rows` / `--max-bytes` budget still caps what
  is shown.
- Scalar tools and a single record picked with `pickRecord` are answers, not
  lists, and get no handle.
- Notes flag what the model should check before building on a result: no rows
  matched; some `@handle` ids found no rows; rows carry a year or an id other than
  the one asked for.

Handles are short: two letters chosen at startup plus a per-user counter (`qx1`,
`qx2`, …). The letters make a handle from before a restart *unknown* instead of
silently naming a new result. Results live in memory only — never on disk, never
written to SQL Server — and are dropped after `--result-store-ttl` unused, or
least-recently-used first past the byte limits (per user first, then overall).
`--result-store=false` turns all of this off and restores plain replies.

**Handles as input.** A batch parameter takes `@handle.Column` instead of ids:
`CustomerID=@qx4.CustomerID`, or `@qx4` when the column has the parameter's name.
The `@` may be left off (`CustomerID=qx4.CustomerID`, as small models tend to
write it) when the value starts with this run's two handle letters; a bare value
with other letters stays a literal. The server expands the reference to the
column's distinct non-null values; a list over 1000 ids runs in chunks and the
results are merged into one handle. `@qx4.CustomerID[0]`, `[-1]` and `[0:5]` take a
position from a stored result. A truncated handle is refused unless the call passes
`AllowPartial=true`; `SaveAs="east customers"` labels the new handle. Both are
accepted by every list tool but explained once in the initialize instructions
rather than in every schema.

On a `literal` batch parameter, a typed id list that is exactly the ids a partial
reply *showed* of a larger result — the model copied the sample instead of passing
the handle — is refused with the handle to pass instead. The check ignores lists of
fewer than three ids, and `AllowPartial=true` lets through a subset that is really
meant.

**Ids are shielded from the client.** A tool's `columns:` menu is what its reply
shows; the whole query result is stored. A foreign key or record id left out of
`columns:` is therefore never displayed, yet the handle line names it (`pass on as
@qx4.CustomerID · also stored: CustomerID, RegionID`) and it passes on as
`@handle.Column`. Derived results inherit the hidden columns of their inputs, and
`show` will not display one on request (it leaves it out and says where it is).
Design `columns:` so the human-facing identifiers are visible and the internal keys
are not.

**The tools.** Each is published as `<prefix>_<name>`, reads only the caller's own
handles, and never touches SQL Server:

| Tool | What it does |
| --- | --- |
| `show` | Page a handle (`PageSize`, `PageNumber`, `OrderBy`, `Columns`), or `ProfileOnly=true` for its profile and lineage. On every server |
| `show_field` | Read the whole text of one value a reply showed cut (`…[+N chars, row R]`): `Handle`, `Row`, `Column`, and `Offset` to read on. On every server that carries `show` |
| `set_union`, `set_intersect`, `set_difference` | Set logic over two or more handles (`Handle=qx3,qx5`) on a `Key` column list; the first handle's full rows come back (`Carry=false` for the key alone) |
| `filter` | `Where`: `= <> < <= > >=`, `IN (…)`, `LIKE`, `BETWEEN`, `IS [NOT] NULL`, `AND`/`OR`/`NOT`, parentheses; `[Bracket Names]` for columns with spaces. A date column accepts the same dates as a date parameter, offsets included (`Birthdate <= '-15y'`), and a timestamp compared with a plain day compares by day |
| `group_aggregate` | `By` columns; `Aggregates` from `count()`, `count(Col)`, `count_distinct`, `sum`, `avg`, `min`, `max`, `count_if(condition)`, `sum_if(Col, condition)`, each `AS name`; optional `Having` |
| `join` | `inner`, `left`, `semi` or `anti` on `On` (`CustomerID`, or `Left = Right`); a join that would exceed the store's row limit is refused before it runs |
| `project` | Keep and rename columns (`Col AS Name`), optionally `Distinct` |
| `sort_limit` | `OrderBy` and `Limit`, for top-N questions |
| `calc` | Arithmetic and statistics: `+ - * /`, `round`, `abs`, `min`, `max`, `percent(part, whole)`, `ratio`, `days_between('d1','d2')`, and `@qx4.row_count`, `@qx4.count(Col)`, `count_distinct`, `sum`, `avg`, `min`, `max`, `median`, `stdev`, `percentile(Col, p)`; several expressions separated by `;` |
| `calc_sql` | Only with `--calc-sql`: one read-only SQLite `SELECT`/`WITH` over handles written `@qx4`, run in a private in-memory database holding only those handles |

A derived result's reply says where it came from (`from: qx3 = … → qx4 = …`).
"Customers who ordered only Widgets" becomes: fetch the orders for the region's
customers into a handle, then

```
group_aggregate Handle=qx6 By=CustomerID
  Aggregates="count() AS products, count_if(ProductName LIKE '%Widget%') AS widgets"
  Having="products = widgets"
```

The operators run in an embedded SQLite database (pure Go, in memory). Every
literal in a condition is a bound parameter and every column name is resolved
against the handle's real columns and quoted, so a condition cannot carry SQL.
Text compares case-insensitively, as SQL Server's default collation does; decimals
compare as numbers and dates as dates.

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

Every server carries `show` and `show_field`. Without an operators group the
operators go on the base server (`--http-addr`), and under stdio on the one
server. Each server's initialize instructions explain handles and name the
operators in the exact form the model calls them; under
`--tool-call-name='{server}_{tool}'` that is `<label>_<prefix>_filter`.

### When a model gets stuck in a loop

A small model that is refused tends to send the identical call again, or to try
call after call until the client's iteration cap ends the turn with no reply. The
server counts consecutive failing tool calls per caller and, once the streak
reaches a limit, appends an order to the failing reply:

- **2 identical failing calls in a row:** "STOP: this exact call has now failed N
  times… Do not send it again. Change the arguments as the error says, use a
  different tool, or write your reply…"
- **3 failing calls in a row, whatever they were:** "STOP: 3 calls in a row have
  failed. Do not call another tool for this step: write your reply…"

A successful call resets the streak. There is nothing to configure.

## Safety

**The real control is the SQL login.** Give the server a dedicated account with
`SELECT` on only what the model should see (`db_datareader` at most). Everything
else here is defence in depth.

- **`--read-only` guards `<prefix>_query` only.** It strips comments and string
  literals, then requires the statement to start with `SELECT` or `WITH` and to
  contain no write keywords. It stops accidents, but it is a convenience, not a
  security boundary. It also marks the tool read-only in its MCP annotations and
  says so in its description. Without the flag, `<prefix>_query` runs writes and
  DDL.
- **Custom tools run the SQL in the file, unchecked.** `--read-only` does not
  inspect a `--query-tools` statement, so review those statements yourself — or
  use `--query-tools-only` and a read-only login — before pointing them at data you
  care about. Parameters are always bound, never concatenated, so a parameter value
  cannot change the statement.
- **`--transport=http` is pinned to loopback.** Without `--http-auth-token-file`
  there is no authentication in front of it, so reachability *is* access. See
  [It really is localhost only](#it-really-is-localhost-only).
- **Stored results hold whatever the tools returned** — for a customer system,
  customer records — in the server's memory. They are never written to disk or to
  the database, are dropped when unused for `--result-store-ttl`, and with
  `--user-header` are readable only by the user who created them.
  `--result-store=false` turns them off.
- **The connection string is an argument:** visible in `ps` and in the client's
  config file. Restrict that file's permissions and keep it out of version control.

## Several databases

One process, one connection string, one database. A second database is a second
registration of the same binary, differing in its prefix and connection string —
which is why the prefix exists, and why nothing here takes a `database` argument
per call. It keeps the connection string, the read-only setting and the SQL login
one-to-one with the data they govern. (A custom tool may name its own
`connectionString`, but that is a fixed choice in a file you wrote, not something
the model selects.)

`USE` does not help: connections are pooled, so the switch does not survive to the
next call, and the result says so. Register another server instead:

```json
{
  "mcpServers": {
    "sales": { "command": "mssql-mcp-toolkit", "args": ["--tool-prefix=sales", "--conn-string=…database=AdventureWorks…"] },
    "hr":    { "command": "mssql-mcp-toolkit", "args": ["--tool-prefix=hr",    "--conn-string=…database=HRWarehouse…"] }
  }
}
```

## Testing

```bash
go test ./...
```

Everything runs against a stub database driver, so no SQL Server is needed. The
suite covers the SQL text guards, value conversion, flag parsing, tool naming and
per-database identity, the output budget and markdown rendering, multi-result-set
handling, timeout versus cancellation, the tool-file loader (groups, includes,
shared parameters, validation), stored results and the operator engine, the
failure-loop breaker, and end-to-end MCP round trips over stdio and over a real
loopback socket, including the refusal to bind anywhere but localhost.

## Layout

| File | Contents |
| --- | --- |
| `main.go` | Flags, config loading, server construction, the `<prefix>_query` tool |
| `transport.go` | The two execution modes: stdio wiring and the localhost HTTP listener |
| `identity.go` | Tool prefix, server label, per-database descriptions and instructions, `--tool-call-name` |
| `query.go`, `sqltext.go`, `rowlimit.go` | Query execution and value conversion; comment/literal stripping and the read-only guard; the `max_rows` argument |
| `budget.go`, `render.go`, `cells.go` | The row/byte/cell limits; the markdown table; display truncation and `show_field` |
| `metadata.go` | `get_metadata` and `list_metadata` |
| `querytools.go` | The tool-file loader: shapes, groups, includes, validation, and tool registration |
| `describe.go` | The describe tool and the notes appended to the initialize instructions |
| `caller.go` | The bearer-token check and the per-call user identity |
| `store.go`, `handles.go`, `profile.go` | The per-user result store, handle capture and `@handle` arguments, result profiles |
| `handletools.go` | `show` and the operator tools |
| `calc.go`, `calcparse.go`, `calcexpr.go` | The in-memory SQLite engine, the condition and aggregate grammar, `calc` arithmetic |
| `loopbreak.go` | The consecutive-failure STOP notice |
| `config.yaml` | A commented Continue.dev configuration, two databases |
| `query-tools.example.yaml` | A worked `--query-tools` file; kept loadable by the tests |

## License

MIT. See [LICENSE](LICENSE).
