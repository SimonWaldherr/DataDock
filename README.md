# DataDock

DataDock is a server-side web GUI starting point for a future standalone SQL
database manager. It was split from a tinySQL example, keeps the implementation
self-contained, and now includes a local connection layer so it can grow toward
a separate repository without broad changes to the parent project's `internal`
packages.

The near-term target is a browser-based database manager for tinySQL, SQLite,
PostgreSQL, MariaDB/MySQL, and Microsoft SQL Server with two credential modes:
administrator-managed shared connections or per-user credentials.

## Features

| Feature | Description |
|---|---|
| **Table/View Browser** | Sidebar lists available tables and views; click to open a paginated datasheet view |
| **Datasheet View** | View, sort, and page through table rows |
| **Record CRUD** | Add, edit, and delete records from any table with an `id INT` column |
| **Table Design** | Create new tables with a visual column designer (INT, FLOAT, TEXT, BOOL) |
| **Export** | Download whole tables/views or SQL query results as CSV, TSV, XLSX, JSON, or XML |
| **Drop Table** | Delete any table with a one-click confirmation |
| **SQL Editor** | Monaco-enhanced SQL editor with SQL syntax highlighting and textarea fallback; selected text can be executed with Run, export, Ctrl+Enter, or F5 |
| **Example Queries & Prompts** | One-click sample SQL queries and natural-language prompts against the demo dataset, so the editor and LLM assistant can be tried immediately with zero setup; picking an example query auto-imports the demo dataset first if it isn't loaded yet |
| **Local Query History** | Browser-local history for recently executed queries |
| **Shareable Queries** | Copy a browser URL that restores the editor SQL from a compact hash |
| **Quick Charts** | D3-powered first chart preview for numeric query results |
| **Connection Manager** | Register managed database connections and switch the active connection from the GUI |
| **Session-scoped Active Connection** | Each browser session can use a different active connection |
| **Table Migration** | Copy a table from one registered connection into another, with optional target table creation |
| **LLM Assistant** | Optional OpenAI-compatible assistant for SQL generation, schema context preview, and natural-language explanations |
| **Runtime Admin Settings** | Dialect, timeouts, LLM provider, page size, and default theme/density can be changed from the Admin UI or API without YAML/JSON config files |
| **Maintenance Mode** | Admin toggle that blocks writes (record edits, DDL, imports, migrations, DML) server-wide while keeping read-only SQL available |
| **Demo Dataset** | One-click demo data (departments/people/projects plus a small sales funnel and a 30-day metrics time series) with a sample scheduled job, and a one-click removal |
| **File Persistence** | Optionally read/write a `.gob` file on disk |

## Current Scope

DataDock starts with an embedded tinySQL database as the default connection and can
add managed connections for SQLite, PostgreSQL, MariaDB/MySQL, and Microsoft SQL
Server at runtime. The active connection drives the table/view browser, CRUD
views, SQL editor, exports, schema retrieval, and LLM context.

The minimal productive mode needs no external database, no LM Studio/OpenAI
server, and no YAML/JSON settings file. Running `go run .` starts DataDock with a
file-backed embedded tinySQL database (`datadock.db`). Runtime settings can then be
maintained through **Admin** and are persisted in the embedded tinySQL database
table `__datadock_settings`.

Credential handling is intentionally still simple: managed connections are
process-local and shared by all users, but the active connection is scoped to
the browser session. Authentication, secret storage, and per-user credentials
are the next larger product boundary.

## Quick Start

```bash
# File-backed by default (datadock.db)
go run .

# Same start through Make
make run

# Explicit in-memory mode (data lost on exit)
go run . -db :memory:

# Same in-memory start through Make
make run-memory

# Persist to a custom file
go run . -db mydata.db

# Custom port
go run . -addr :9090

# Build or test the standalone datadock command
make build
make test
```

Open your browser at **http://localhost:8080**.

## Command-line flags

| Flag | Default | Description |
|---|---|---|
| `-addr` | `:8080` | HTTP listen address |
| `-db` | `datadock.db` | Path to a `.gob` database file. Use `:memory:` or an empty value for in-memory mode. |
| `-tenant` | `default` | Tenant namespace within the database |
| `-dialect` | `$DATADOCK_SQL_DIALECT` or `tinysql` | SQL dialect profile for LLM guidance: `tinysql`, `sqlite`, `postgres`, `mysql`, `mariadb`, `mssql`. |
| `-llm-base-url` | `$DATADOCK_LLM_BASE_URL` | OpenAI-compatible API base URL. |
| `-llm-api-key` | `$DATADOCK_LLM_API_KEY` or `$OPENAI_API_KEY` | API key. Optional for local providers. |
| `-llm-model` | `$DATADOCK_LLM_MODEL` | Model name sent to the provider. |
| `-connect-timeout` | `$DATADOCK_CONNECT_TIMEOUT` or `10s` | Timeout for adding and pinging managed database connections. |
| `-query-timeout` | `$DATADOCK_QUERY_TIMEOUT` or `60s` | Default timeout for interactive SQL queries, browsing, and exports. |
| `-llm-timeout` | `$DATADOCK_LLM_TIMEOUT` or `45s` | Timeout for OpenAI-compatible LLM requests. |

Flags and environment variables are bootstrap defaults only. After startup, the
Admin settings page can change the active dialect, connection timeout, query
timeout, LLM timeout, LLM base URL, model, and API key for the running server.
Leaving the LLM base URL or model empty disables LLM support cleanly.

## Admin Settings

Open **Admin** to edit runtime settings without touching any config file. The
same settings are available for automation through:

| Endpoint | Method | Description |
|---|---|---|
| `/admin/settings` | `POST` | HTML form endpoint used by the Admin UI |
| `/api/admin/settings` | `GET` | Return the current runtime settings as JSON, with secrets masked |
| `/api/admin/settings` | `POST` | Apply runtime settings from JSON |

In addition to dialect/timeouts/LLM provider settings, Admin also controls:

| Setting | Description |
|---|---|
| `page_size` | Rows per page in the datasheet view (default `50`, max `1000`) |
| `default_theme` | Fallback UI theme for browsers without a saved preference (`workbench`, `midnight`, `forest`, `contrast`, `solaris`, `xp`, `classic2000`, `kde`) |
| `default_density` | Fallback table density for browsers without a saved preference (`comfortable`, `compact`) |
| `read_only_mode` | Maintenance mode: blocks record edits, table/DDL changes, imports, migrations, and non-`SELECT` SQL editor statements for every user until turned off |

The Admin page also has a **Demo Data** section to load or remove the built-in
demo dataset (`POST /demo-data`, `POST /demo-data/remove`) without needing an
empty database.

Example API update:

```bash
curl -X POST http://localhost:8080/api/admin/settings \
  -H 'Content-Type: application/json' \
  -d '{
    "dialect": "mssql",
    "connect_timeout": "5s",
    "query_timeout": "90s",
    "llm_base_url": "",
    "llm_model": "",
    "llm_timeout": "45s",
    "page_size": 50,
    "default_theme": "workbench",
    "default_density": "comfortable",
    "read_only_mode": false
  }'
```

## API Standards

datadock uses stable web and data interchange standards for external integrations:

- HTTP semantics follow RFC 9110 status/method behavior.
- API errors use RFC 9457 Problem Details (`application/problem+json`) and keep
  a compatibility `error` field for the current browser UI.
- JSON uses RFC 8259 (`application/json; charset=utf-8`).
- Timestamps use RFC 3339.
- CSV/TSV exports use RFC 4180-style CSV handling.
- XLSX exports use Office Open XML spreadsheet packages.
- tinySQL-facing error classes can use ISO/IEC 9075 SQLSTATE helpers exposed by
  the public tinySQL API.

## Connections

Open **Connections** in the top navigation to add and activate external
databases. Supported connection kinds:

| Kind | Driver | DSN examples |
|---|---|---|
| `sqlite` | `modernc.org/sqlite` | `./app.db`, `:memory:` |
| `postgres` | `lib/pq` | `postgres://user:pass@localhost:5432/db?sslmode=disable` |
| `mysql` / `mariadb` | `go-sql-driver/mysql` | `user:pass@tcp(localhost:3306)/db` |
| `mssql` | `go-mssqldb` | `sqlserver://user:pass@localhost:1433?database=db` |

Connections are tested on add with `PingContext`. The active connection is
remembered per browser session using an HTTP-only session cookie.

For remote database servers, use the host and port as seen from the DataDock
server, not from the user's browser. Typical DSNs:

```text
postgres://user:pass@db-server.example.net:5432/app?sslmode=require
user:pass@tcp(mariadb-server.example.net:3306)/app?tls=true&timeout=10s&readTimeout=60s&writeTimeout=60s
sqlserver://user:pass@mssql-server.example.net:1433?database=DWH&encrypt=true&TrustServerCertificate=false&connection+timeout=10
```

The connection timeout limits initial ping/add operations. The query timeout
limits interactive SQL, table browsing, and exports so an unreachable or slow
remote server does not block DataDock indefinitely.

## Migration

Open **Migration** to copy one table from a source connection into a target
connection. The first implementation supports:

- choosing any two registered connections,
- selecting a source table,
- optionally creating the target table,
- simple cross-dialect type mapping,
- streaming row-by-row inserts with dialect-specific placeholders.

It does not yet preserve indexes, foreign keys, triggers, views, generated
columns, or table-specific permissions.

## LLM Assistant

The SQL editor can call an OpenAI-compatible `/v1/chat/completions` endpoint to:

- turn a natural-language prompt into SQL,
- safely generate and run read-only result queries,
- explain query results,
- explain SQL/database errors.

Generated SQL is placed in the editor for review. It is not executed
automatically unless the user chooses **Ask & Run**. Automatic execution is
limited to result-producing SQL (`SELECT`, `WITH`, `SHOW`, `EXPLAIN`) and blocks
common DDL/DML operations.

DataDock teaches the LLM the active database shape and SQL dialect on every request
by sending a compact schema snapshot containing:

- the active dialect profile and syntax rules,
- the active LLM skill and output contract,
- retrieval metadata explaining which tables were selected,
- tables, columns, types, constraints, row counts, and sample values,
- known foreign-key relationships when available.

The dialect profile controls prompt guidance such as identifier quoting,
placeholder style, limit/pagination syntax, case-insensitive search operators,
and words blocked from automatic execution. This is context injection, not model
training.

The SQL editor exposes the same schema snapshot through **Schema** / `GET
/api/schema`, so users can inspect the exact compact context that will be sent
to the LLM.

### Skills And RAG

DataDock uses small task-specific LLM skills:

- `text_to_sql`: natural language to SQL, with `sql` or `clarify` JSON output.
- `result_explainer`: concise explanation of result rows.
- `sql_error_explainer`: plain-language error diagnosis and correction hints.

For retrieval-augmented generation, DataDock builds a lexical schema index from
table names, column names, constraints, relationships, and sample values. For
small databases the full schema is sent. For larger schemas, DataDock ranks tables
against the prompt/current SQL/error text and sends only the most relevant
tables plus retrieval metadata. This keeps prompts smaller and reduces accidental
joins against unrelated tables.

OpenAI-style configuration:

```bash
export OPENAI_API_KEY='...'
export DATADOCK_LLM_MODEL='<openai-model-name>'
go run . -llm-base-url https://api.openai.com/v1
```

LM Studio-style local configuration:

```bash
go run . \
  -llm-base-url http://127.0.0.1:1234/v1 \
  -llm-model local-model \
  -dialect tinysql
```

When LM Studio runs on another machine, configure LM Studio to listen on that
machine's LAN/VPN interface and point DataDock at that host from the DataDock server:

```bash
go run . \
  -addr :8080 \
  -llm-base-url http://lmstudio-host.example.net:1234/v1 \
  -llm-model local-model \
  -llm-timeout 60s
```

The **Test LLM** button in the SQL editor calls `GET /api/llm/health` from the
DataDock server. Browser CORS is not involved because LLM calls are made
server-to-provider, not directly from browser JavaScript to LM Studio.

## Architecture

```
cmd/datadock/
├── main.go          # Server setup, embed, flag parsing, template funcs
├── db.go            # App struct, table/record helpers, SQL execution
├── connections.go   # Managed sql.DB connections and dialect-aware helpers
├── dialect.go       # SQL dialect profiles for LLM guidance and adapters
├── settings.go      # Runtime settings stored in the embedded tinySQL DB
├── llm.go           # OpenAI-compatible assistant client and workflows
├── migration.go     # Table-copy migration between managed connections
├── rag.go           # Schema retrieval and result profiling for LLM context
├── handlers.go      # HTTP route handlers
├── main_test.go     # HTTP integration tests
├── static/
│   └── app.js       # Minimal client-side helpers
└── templates/
    ├── base.html    # Layout: top nav + sidebar (shared across pages)
    ├── index.html   # Empty-state landing page
    ├── table_view.html   # Datasheet with pagination + sort
    ├── record_form.html  # Create/edit record form
    ├── query.html   # SQL editor with async JSON API
    ├── admin.html   # Admin status and runtime settings
    ├── jobs.html    # Job overview and manual execution
    ├── connections.html  # Managed connection UI
    ├── migration.html    # Table migration UI
    └── create_table.html # Table design wizard
```

### HTTP Routes

| Method | Path | Description |
|---|---|---|
| `GET` | `/` | Redirect to first table, or empty-state |
| `GET` | `/t/{table}` | Datasheet view (query params: `page`, `sort`, `dir`) |
| `GET` | `/t/{table}/export?format=csv\|tsv\|xlsx\|json\|xml` | Download a full table/view export |
| `GET` | `/t/{table}/new` | New record form |
| `POST` | `/t/{table}/new` | Create record |
| `GET` | `/t/{table}/{id}/edit` | Edit record form |
| `POST` | `/t/{table}/{id}/edit` | Update record |
| `POST` | `/t/{table}/{id}/delete` | Delete record |
| `POST` | `/drop-table/{table}` | Drop table |
| `GET` | `/query` | SQL editor page |
| `POST` | `/api/query` | Execute SQL (JSON API) |
| `POST` | `/api/export` | Download SQL query results as CSV, TSV, XLSX, JSON, or XML |
| `GET` | `/api/schema` | Return the compact active-connection schema snapshot used for LLM context |
| `GET` | `/api/llm/health` | Test server-side connectivity to the configured LLM provider |
| `GET` | `/connections` | Connection manager |
| `POST` | `/connections` | Add a managed connection |
| `POST` | `/connections/active` | Switch active connection |
| `GET` | `/migrate` | Table migration wizard |
| `POST` | `/migrate` | Run table migration |
| `GET` | `/create-table` | Table designer |
| `POST` | `/create-table` | Create table |
| `POST` | `/demo-data` | Load (or reset) the built-in demo dataset plus a sample scheduled job |
| `POST` | `/demo-data/remove` | Drop every demo table and the sample demo job |
| `GET/POST` | `/static/*` | Static assets |

Every mutating route above (except `/connections`, `/connections/active`, and
`/admin/settings`) is blocked while **maintenance mode** is enabled in Admin,
returning `503 Service Unavailable`. `/query` and `/api/query` stay open for
read-only statements (`SELECT`, `WITH`, `SHOW`, `EXPLAIN`) even in maintenance
mode.

### JSON API

**POST /api/query**

Request:
```json
{ "sql": "SELECT * FROM my_table LIMIT 5" }
```

Response (SELECT):
```json
{
  "columns": ["id", "name"],
  "rows": [["1", "Alice"]],
  "elapsed_ms": 2
}
```

Response (DML):
```json
{ "affected": 1, "elapsed_ms": 1 }
```

Response (error):
```json
{
  "type": "about:blank",
  "title": "Query failed",
  "status": 500,
  "detail": "table not found",
  "instance": "/api/export",
  "error": "table not found"
}
```

API error responses use RFC 9457 Problem Details
(`application/problem+json`). The `error` member is kept as a compatibility
extension for the current browser UI and simple clients.

**POST /api/export**

Request:
```json
{ "sql": "SELECT * FROM my_table", "format": "xlsx" }
```

The endpoint accepts result-producing SQL (`SELECT`, `WITH`, `SHOW`, `EXPLAIN`) and returns an attachment in `csv`, `tsv`, `xlsx`, `json`, or `xml` format.

**GET /api/schema**

Returns the active connection's compact dialect/schema/context snapshot as
JSON. The query editor uses this for the schema preview, and the LLM assistant
uses the same shape for prompt grounding.

## Running Tests

```bash
go test ./...
```

## ChatSQL Notes

The current LLM workflow is informed by the static ChatSQL demo:

- structured LLM responses (`sql` or `clarify`),
- schema context with table/column metadata and samples,
- a guarded ask-run-explain flow,
- result/error explanations in natural language.
- F5 execution and "selected SQL if available" query execution.
- shareable query URLs via a compact hash.
- immediate schema-context preview for debugging prompt grounding.
- simple D3 visualizations that can switch from bars to time-series lines when
  a date-like dimension is detected.

Large query results are not sent to the LLM as raw rows. DataDock automatically
builds a compact pivot-style result profile with total row count, capped sample
rows, per-column null/distinct counts, top values, and numeric min/max/sum/avg
statistics. This gives the LLM enough shape to explain the result without
leaking or overloading thousands of rows.

DataDock implements those ideas server-side so API keys and database access do not
need to live in browser JavaScript.
