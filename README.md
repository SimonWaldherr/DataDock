# DataDock

DataDock is a SQL-first web workspace for browsing databases, importing messy
files, matching records, visualizing query results, and exporting data in
analysis-friendly formats. It starts with an embedded tinySQL database and can
connect to SQLite, PostgreSQL, MariaDB/MySQL, and Microsoft SQL Server.

The project is intentionally self-contained: the browser UI, import/export
pipeline, matching tools, local scheduler, and optional OpenAI-compatible LLM
assistant run from one Go process. This makes it useful as a local data dock for
CSV/JSON/office/geospatial files, while still leaving a path toward a standalone
database manager with administrator-managed or per-user credentials.

## Features

| Feature | Description |
|---|---|
| **Table/View Browser** | Sidebar lists available tables and views; click to open a paginated datasheet view; use the sidebar refresh button after external schema changes, while in-app DDL refreshes it automatically |
| **Datasheet View** | View, sort, and page through table rows |
| **Record CRUD** | Add, edit, and delete records from any table with an `id INT` column |
| **Table Design** | Create new tables with a visual column designer (INT, FLOAT, TEXT, BOOL) |
| **Broad File Import** | Import HTML tables, SQLite databases, MessagePack/CBOR/BSON, iCalendar, vCard, and file manifests for Parquet/Arrow/Feather/DuckDB |
| **Map Data Import** | Import GeoJSON, GeoPackage, GPX, KML, OSM XML/PBF, Shapefile ZIPs, MBTiles/PMTiles layers, and JSON/NDJSON routing graphs into queryable tables with GeoJSON geometry columns |
| **Tile Layers** | Open imported MBTiles or PMTiles as local TileJSON-backed raster or vector layers; MBTiles TMS rows are normalized to XYZ at the API boundary |
| **Routing Analysis** | Calculate directed or undirected shortest paths and reachable areas from imported RG graph nodes and edges, returning GeoJSON for maps or downstream use |
| **Import Quality Reports** | Persist source name, format, SHA-256, source size, geometry validity, geometry types, and bounds for every local file/API import |
| **Export** | Download whole tables/views or SQL query results as CSV, Excel-safe CSV, TSV, XLSX, JSON, NDJSON, XML, HTML, SQLite, GeoJSON, GeoJSON summaries, KML, GPX, or Shapefile ZIP |
| **Drop Table** | Delete any table with a one-click confirmation |
| **SQL Editor** | Monaco-enhanced SQL editor with SQL syntax highlighting and textarea fallback; selected text can be executed with Run, export, Ctrl+Enter, or F5 |
| **Example Queries & Prompts** | One-click sample SQL queries and natural-language prompts against the demo dataset, so the editor and LLM assistant can be tried immediately with zero setup; picking an example query auto-imports the demo dataset first if it isn't loaded yet |
| **Local Query History** | Browser-local history for recently executed queries, with JSON/CSV export from the History tab |
| **Shareable Queries** | Copy a browser URL that restores the editor SQL from a compact hash |
| **Versioned SQL Pipelines** | Admin-managed, immutable multi-step read-only SQL workflows with opt-in bounded parallelism, portable definition bundles, and result-free run lineage |
| **Quick Charts** | D3-powered first chart preview for numeric query results |
| **Geo Views** | Map query results from GeoJSON geometry columns or latitude/longitude columns |
| **Connection Manager** | Register managed database connections and switch the active connection from the GUI |
| **Session-scoped Active Connection** | Each browser session can use a different active connection |
| **Table Migration** | Copy a table from one registered connection into another, with optional target table creation |
| **Matching** | Compare two tables — from the same or different connections, or an uploaded CSV/XLSX file — and find rows that likely describe the same entity (customers, articles, or anything else) despite differing spelling, casing, legal-form suffixes, or address abbreviations; save confirmed matches as a queryable crosswalk table or export as CSV |
| **LLM Assistant** | Optional OpenAI-compatible assistant for SQL generation, schema context preview, and natural-language explanations |
| **Runtime Admin Settings** | Dialect, timeouts, LLM provider, page size, and default theme/density can be changed from the Admin UI or API without YAML/JSON config files |
| **Admin Catalog View** | Authenticated Admin sessions can see DataDock/system tables such as `__datadock_settings`; normal sessions keep them hidden |
| **Maintenance Mode** | Admin toggle that blocks writes (record edits, DDL, imports, migrations, DML) server-wide while keeping read-only SQL available |
| **Demo Dataset** | One-click demo data (departments/people/projects, sales funnel, metrics time series, GeoJSON/Map locations, JSON/XML payloads) with a sample scheduled job, and a one-click removal |
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

## Import And Export

Imports currently target the local embedded tinySQL database. That keeps file
normalization predictable and lets uploaded data immediately participate in SQL
queries, matching, maps, exports, and migration into another registered
connection.

Supported import families:

- **Tabular and structured:** CSV, TSV, JSON, NDJSON, YAML, XML, multi-sheet
  XLSX, HTML tables, SQLite tables/views, MessagePack, CBOR, BSON, iCalendar,
  and vCard.
- **Columnar manifests:** Parquet, Arrow, Feather, and DuckDB files are recorded
  as file-level manifest rows. Full row-group/page decoding is intentionally not
  bundled yet.
- **Map data:** GeoJSON, GeoPackage, GPX, KML, OSM XML, OSM PBF, Shapefile ZIP,
  MBTiles, PMTiles v3, and JSON/NDJSON routing graphs.

Exports are available from table/view pages, the Manage Tables export tab, and
`POST /api/export`. Standard table exports include CSV, Excel-safe CSV, TSV,
XLSX, JSON, NDJSON, XML, HTML, and SQLite. Map-aware exports derive geometries
from a GeoJSON geometry column or latitude/longitude columns and can output
GeoJSON, GeoJSON summary JSON, KML, GPX, or Shapefile ZIP.

GeoJSON-derived exports support native, mapshaper-like options for practical
workflows: `explode`, `simplify`, `bbox`, `fields`, and `drop`. Topology-heavy
operations such as robust clip/erase/dissolve still belong behind a dedicated
geometry engine or optional mapshaper CLI integration. Shapefile exports are
limited by the format to one geometry type per ZIP; DataDock writes the first
compatible geometry family it finds.

MBTiles and PMTiles are retained as local, queryable tile tables and exposed as
TileJSON at `GET /api/map/tiles/{table}/tilejson`, with individual XYZ tiles at
`GET /api/map/tiles/{table}/{z}/{x}/{y}`. The table's **Map Layer** action opens
a raster or vector map view. Vector layer metadata is read from archive metadata
or inferred from the first MVT payload when necessary.

Routing graph tables expose **Route**. `POST /api/routing/{table}/route`
calculates a shortest path by `cost`, `distance`, or a numeric
`properties.<field>` cost profile. `POST /api/routing/{table}/reachable` returns
reachable nodes and a clearly labelled convex-hull area approximation as GeoJSON.
Both endpoints accept node IDs or nearest-node longitude/latitude input.

Every file/API import also stores an **Import Report**. It records the source
name, original-upload SHA-256, source size and format, normalized table row
count, geometry columns/types, valid/missing/invalid geometry counts, and WGS84
bounds. `GET /api/spatial-reports/{table}` returns the same report as JSON.

With tinySQL v0.19.1, map tables can also be queried directly with native geo
functions. `GEO_POINT`/`ST_MAKEPOINT` creates GeoJSON points, `ST_X`/`ST_Y`
extract longitude/latitude, `GEO_DISTANCE`/`ST_DISTANCE` computes haversine
distance in meters, and `GEO_WITHIN_BBOX` / `GEO_DWITHIN` cover common spatial
filters.

tinySQL v0.19.1 also persists `CREATE INDEX` metadata. DataDock can execute
`CREATE INDEX` / `DROP INDEX` in the SQL editor and inspect the catalog through
`sys.indexes`; tinySQL still treats indexes as metadata rather than a
planner-backed access path.

tinySQL v0.19.1 also adds a bounded 30-second result cache for local
`VEC_SEARCH` queries. DataDock enables 128 entries and keeps a bounded,
vector-free query-shape history at `GET /api/tinysql/vector-cache`. The cache
is most useful for repeated ad-hoc k-NN queries; filtered logic search uses an
exact cosine-ranking fallback to preserve connection/model scoping.
Logic retrieval uses a widened `VEC_SEARCH` candidate window and reapplies
connection/model metadata filters before returning results; if that window is
too noisy, DataDock falls back to exact scoped ranking. `flat` is the default.
HNSW must be explicitly enabled in Admin Settings and is warmed only after a
bulk logic reindex, never after ordinary ingestion.

### Storage and Encryption

The default `memory` mode retains DataDock's compatible GOB snapshot behavior.
For encrypted table files, set `-storage-mode` (or `DATADOCK_STORAGE_MODE`) to
`disk`, `json`, `hybrid`, or `index`, choose a directory with `-db`, and pass a
32-byte AES-256 key as hexadecimal or base64 in `DATADOCK_ENCRYPTION_KEY`.
The key is read only from the environment and is never stored in DataDock
settings. WAL modes are intentionally rejected with encryption because tinySQL
does not encrypt WAL files. Storage metadata remains unencrypted.

tinySQL v0.20.0 also adds `paged_index` for read-mostly artifacts. Select it
with `-storage-mode paged_index`, bound page-cache residency with
`-storage-cache-bytes`/`DATADOCK_STORAGE_CACHE_BYTES`, and use
`-storage-read-only`/`DATADOCK_STORAGE_READ_ONLY` when serving an existing
artifact. It is intentionally not offered with encryption because its pager
backend does not use the disk-table encryptor.

The embedded `database/sql` bridge uses tinySQL's `OpenWithDB` path because
v0.20 intentionally isolates named `mem://` DSNs. Consequently the embedded
bridge operates in the `default` tenant; use an external managed connection
when separate tenant namespaces are required.

CSV and TSV inputs accept UTF-8 (including BOM) and UTF-16 BOM exports. Binary
payloads remain encoded or typed as BLOB values rather than coerced through
text decoding. GeoJSON, KML, and OSM imports are directly supported. Shapefile
and MBTiles import support depends on tinySQL builds tagged `shapefile` and
`sqliteimport` respectively; deployments without these optional capabilities
receive the importer error instead of a partial decode.

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

# Or let DataDock pick/store a free port in 8000-8100
go run . -port 0

# Build or test the standalone datadock command
make build
make test
```

Open your browser at the address printed by the server, commonly
**http://localhost:8080** when using `-addr :8080`.

## Command-line flags

| Flag | Default | Description |
|---|---|---|
| `-addr` | empty | HTTP listen address; when set, takes precedence over `-port` and any stored port setting |
| `-port` | `0` | HTTP listen port. `0` uses the stored setting or auto-detects a free port between `8000` and `8100`. |
| `-find-free-port` | `false` | Print a free TCP port between `8000` and `8100` and exit. |
| `-db` | `datadock.db` | Path to a `.gob` database file. Use `:memory:` or an empty value for in-memory mode. |
| `-tenant` | `default` | Tenant namespace within the database |
| `-dialect` | `$DATADOCK_SQL_DIALECT` or `tinysql` | SQL dialect profile for LLM guidance: `tinysql`, `sqlite`, `postgres`, `mysql`, `mariadb`, `mssql`. |
| `-llm-base-url` | `$DATADOCK_LLM_BASE_URL` | OpenAI-compatible API base URL. |
| `-llm-api-key` | `$DATADOCK_LLM_API_KEY` or `$OPENAI_API_KEY` | API key. Optional for local providers. |
| `-llm-model` | `$DATADOCK_LLM_MODEL` | Model name sent to the provider. |
| `-connect-timeout` | `$DATADOCK_CONNECT_TIMEOUT` or `10s` | Timeout for adding and pinging managed database connections. |
| `-query-timeout` | `$DATADOCK_QUERY_TIMEOUT` or `60s` | Default timeout for interactive SQL queries, browsing, and exports. |
| `-llm-timeout` | `$DATADOCK_LLM_TIMEOUT` or `45s` | Timeout for OpenAI-compatible LLM requests. |
| `-verbose` | `$DATADOCK_VERBOSE` or `false` | Write redacted communication logs for LLM HTTP calls, LLM discovery, database opens/pings, SQL queries, mutations, imports, jobs, and migrations to stdout. |
| `-watch-dir` | `$DATADOCK_WATCH_DIR` | Optional directory to auto-import/update supported files into local tinySQL tables. |
| `-watch-interval` | `$DATADOCK_WATCH_INTERVAL` or `3s` | Polling interval for `-watch-dir`. |
| `-audit-log` | `$DATADOCK_AUDIT_LOG` | Optional path for a tamper-evident tinySQL audit log. |

Flags and environment variables are bootstrap defaults only. After startup, the
Admin settings page can change the active dialect, connection timeout, query
timeout, LLM timeout, LLM base URL, model, and API key for the running server.
Leaving the LLM base URL or model empty disables LLM support cleanly.

Verbose mode is intended to be safe for production diagnostics: it is opt-in,
logs to stdout as structured JSON lines, masks credentials and secret-looking
fields, records SQL parameter counts instead of parameter values, and truncates
payload previews. It still logs SQL text and request/response metadata, so only
enable it where operational stdout access is appropriately restricted.

The SQL editor supports multiple result views per tab: table, live logs, cards,
JSON/XML trees, pivot summaries, column profiles, schema graph, and notebook.
Query share links include the active SQL, tab title, view mode, live settings,
and log filter. `-watch-dir` imports supported structured and map files on
startup and whenever their size or modification time changes; the table name is
derived from the filename and refreshed on update.

## Admin Settings

Open **Admin** to edit runtime settings without touching any config file. On the
first visit, DataDock redirects to `/admin/setup`: operators can keep a local,
loopback-only single-user instance without login, or create the first Admin
account for a team instance. Local users are stored in `__datadock_users` with
bcrypt password hashes and explicit roles: **Admin**, **User**, and
**Read-only**. Existing deployments with the legacy single Admin password are
migrated into an Admin user automatically at startup.

Later visits use `/admin/login` and a session cookie. Admin settings, shared
connection persistence/default changes, job management, demo-data admin actions,
LLM discovery/health, user management, and Admin APIs require an authenticated
Admin session. User accounts can read and write data but cannot manage Admin
settings; read-only accounts can browse data and run result-producing SQL, while
write routes and non-SELECT SQL are blocked.

The same settings are available for automation through:

| Endpoint | Method | Description |
|---|---|---|
| `/admin/settings` | `POST` | HTML form endpoint used by the Admin UI |
| `/api/admin/settings` | `GET` | Return the current runtime settings as JSON, with secrets masked |
| `/api/admin/settings` | `POST` | Apply runtime settings from JSON |

Automation clients use the same session flow as the browser. Before the first
Admin account is set, Admin APIs return `428 Precondition Required`; after an
account exists but the request has no authenticated session, they return `401
Unauthorized`. Use a cookie jar with `curl` or your HTTP client:

```bash
# First run only: create the first Admin user and keep the session cookie.
curl -c datadock.cookies -X POST http://localhost:8080/admin/setup \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode 'username=admin' \
  --data-urlencode 'password=change-this-password' \
  --data-urlencode 'password_confirm=change-this-password' \
  --data-urlencode 'next=/admin'

# Later runs: log in and refresh the cookie jar.
curl -c datadock.cookies -X POST http://localhost:8080/admin/login \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode 'password=change-this-password' \
  --data-urlencode 'next=/admin'
```

In addition to dialect/timeouts/LLM provider settings, Admin also controls:

| Setting | Description |
|---|---|
| `page_size` | Rows per page in the datasheet view (default `50`, max `1000`) |
| `match_max_rows` | Rows per side, per Matching run, the interactive matcher will load into memory (default `2,000,000`, max `50,000,000`) — raise this for large master-data tables |
| `default_theme` | Fallback UI theme for browsers without a saved preference (`workbench`, `midnight`, `forest`, `contrast`, `solaris`, `xp`, `classic2000`, `kde`) |
| `default_density` | Fallback table density for browsers without a saved preference (`comfortable`, `compact`) |
| `read_only_mode` | Maintenance mode: blocks record edits, table/DDL changes, imports, migrations, and non-result SQL editor statements for every user until turned off |

The Admin page also has a **Demo Data** section to load or remove the built-in
demo dataset (`POST /demo-data`, `POST /demo-data/remove`) without needing an
empty database. The dataset includes demo tables for maps (`datadock_demo_locations`),
JSON/XML tree views and Excel-safe exports (`datadock_demo_payloads`), charts,
pivots, profiles, and scheduled jobs.

Example API update:

```bash
curl -X POST http://localhost:8080/api/admin/settings \
  -b datadock.cookies \
  -H 'Content-Type: application/json' \
  -d '{
    "dialect": "mssql",
    "connect_timeout": "5s",
    "query_timeout": "90s",
    "llm_base_url": "",
    "llm_model": "",
    "llm_timeout": "45s",
    "page_size": 50,
    "match_max_rows": 2000000,
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
- Excel-safe CSV is an explicit export mode that rewrites ambiguous text and ISO date/time values for Excel import while leaving standard CSV unchanged.
- XLSX exports use Office Open XML spreadsheet packages with typed numeric, boolean, date, time, and datetime cells.
- XLSX imports merge multiple worksheets into one table with a `sheet` column; single-sheet imports keep the traditional first-sheet shape.
- HTML, SQLite, MessagePack, CBOR, BSON, iCalendar, and vCard imports normalize common non-geospatial files into queryable tables.
- Parquet, Arrow, Feather, and DuckDB imports currently create file-level manifest rows rather than fully decoding row groups/pages.
- GeoJSON exports use RFC 7946 FeatureCollection output where geometry or lat/lon columns are present.
- Map data imports normalize GeoJSON/GeoPackage/GPX/KML/OSM/Shapefile geometries into GeoJSON text columns; MBTiles and PMTiles v3 persist tile metadata plus payloads for local TileJSON serving; RG imports JSON/NDJSON routing graph nodes and edges.
- Imported tile tables expose TileJSON at `/api/map/tiles/{table}/tilejson` and XYZ tile payloads at `/api/map/tiles/{table}/{z}/{x}/{y}`; MBTiles TMS rows are translated at the API boundary.
- Routing requests use `/api/routing/{table}/route` for shortest paths and `/api/routing/{table}/reachable` for cost-bounded reachability GeoJSON. Reachable-area polygons use a convex-hull approximation and declare that method in their feature properties.
- Each file/API import persists a source SHA-256 and spatial quality report, available in the table's **Import Report** action and from `/api/spatial-reports/{table}`.
- DataDock tracks tinySQL v0.19.1 and exposes its read-only PRAGMA support,
  result-producing stored procedure calls in the SQL editor, and native agent
  context generation for the local tinySQL connection.
- tinySQL geo functions such as `ST_MAKEPOINT`, `ST_X`, `ST_Y`,
  `ST_DISTANCE`, `GEO_DWITHIN`, and `GEO_WITHIN_BBOX` work in the SQL editor
  against imported map tables.
- tinySQL index catalog metadata from `CREATE INDEX` is visible through
  `sys.indexes`, including index name, table, columns, uniqueness, and creation
  time.
- tinySQL v0.19.1's bounded `VEC_SEARCH` cache and vector-free cache analytics
  are enabled for the embedded local engine.
- tinySQL-facing error classes can use ISO/IEC 9075 SQLSTATE helpers exposed by
  the public tinySQL API.

## Connections

Open **Connections** in the top navigation to add and activate external
databases. Connections added there are session-private by default. After logging
in as Admin, a connection can be saved for everyone, forgotten, or made the
server-wide default. Supported connection kinds:

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

## Pipelines

Open **Pipelines** from the Operations menu to save a reusable sequence of
read-only tinySQL statements. Saving a definition with an existing name creates
a new immutable version rather than overwriting the old one. A run is tied to a
specific version and persists only structural lineage: source tables inferred
from `FROM`/`JOIN`, a SHA-256 digest of the statement, result columns/row count,
elapsed time, and status. Raw query result values are never copied into pipeline
metadata.

Pipelines are currently deliberately restricted to one result-producing SQL
statement per step (`SELECT`, read-only `WITH`, `SHOW`, `EXPLAIN`, `DESCRIBE`,
or `PRAGMA`). DDL, DML, procedures, scripts, and write CTEs are rejected. This
keeps the first pipeline implementation safe to run and audit; materializing,
external, and scheduled pipeline steps can be added later with explicit
operator controls.

`max_parallelism` defaults to `1`, preserving step order. It can be raised to
`2` or `4` only when the steps are independent: they cannot consume each
other's result artifacts because this release does not materialize outputs.
DataDock uses a context-cancellable, bounded worker pool and returns lineage in
definition order even when independent reads finish in another order.

Pipeline bundles are portable JSON definition exports. They include immutable
pipeline versions but exclude runs, query result values, and connection
credentials. Imports are bounded to 16 MiB, validate every statement, append
new local versions, and skip definitions already present by canonical hash.

The admin-protected API is:

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/pipelines` | List current pipeline versions and recorded runs; pass `?name=name` for all versions of one pipeline |
| `POST` | `/api/pipelines` | Save a new immutable version from `{name, description, max_parallelism, steps:[{name, sql}]}` |
| `POST` | `/api/pipelines/run` | Run `{name, version}`; omit `version` to run the latest definition |
| `POST` | `/api/pipelines/delete` | Delete all saved definition versions for `{name}` while retaining run lineage |
| `GET` | `/api/pipelines/export` | Download a portable definition-only JSON bundle |
| `POST` | `/api/pipelines/import` | Append validated definitions from a portable JSON bundle; identical definitions are skipped |

## Matching

Open **Matching** to compare two tables — from the same or different
registered connections, e.g. two customer master-data exports from different
ERP systems — and find rows that likely describe the same real-world entity.
The feature is intentionally domain-agnostic: nothing about it is specific to
customers, so the same wizard works for articles, contacts, or any other
entity type.

The wizard walks through:

1. choosing a source and target connection (any two registered connections,
   including two different database engines) and a table on each — or
   picking the special **File Upload** entry instead of a connection, which
   swaps that side's table dropdown for a file picker: choosing a
   supported structured or map file imports it into the local tinySQL database
   (via the same import path as **Manage Tables → Import**) and uses it
   immediately as that side's table, no separate import step and no loss of
   whatever table is already selected on the other side,
2. picking a key column on each side (used to identify a row in the results),
3. mapping the columns to compare, each with its own comparison method and
   weight,
4. auto-match / review score thresholds and an optional "compare every row"
   mode for small tables,
5. running a preview, exporting the candidates as CSV, or saving them into a
   new/append-only crosswalk table (`source_key`, `source_label`,
   `target_key`, `target_label`, `score`, `status`, `matched_at`) on any
   registered connection — an ordinary table, immediately browsable,
   queryable, and exportable like any other.

Comparison methods, applied per field pair:

| Method | Description |
|---|---|
| `exact` | Byte-for-byte identical values |
| `exact_ci` | Case-insensitive exact match |
| `normalized` | Case, diacritics, punctuation, and common company legal-form suffixes (GmbH, AG, Inc., Ltd., ...) folded away, then compared exactly |
| `similarity` | Typo-tolerant Jaro-Winkler similarity on the normalized value |
| `token_set` | Word-set comparison, ignoring word order — "Elektro Müller" matches "Müller Elektro" |
| `address` | Splits a free-text "street + house number" column and normalizes the German Str./Straße abbreviation before scoring |
| `numeric` | Compares two numbers within a relative tolerance |

To stay fast without loading a full cross join, non-exact string fields are
used to build a token-based blocking index (candidates share at least one
normalized word), which can be disabled for small tables to guarantee full
recall at the cost of comparing every row against every row.

Matching runs synchronously within one request and loads both sides fully
into memory, bounded by the admin-configurable `match_max_rows` setting
(default 2,000,000 rows per side — see [Admin Settings](#admin-settings)).
Raise it if a table legitimately has more rows than that; there is no
background-job variant of Matching yet.

### Composite keys (field groups)

A single field is often not a reliable identifier on its own: the same street
name exists in many cities, and the same manufacturer part number can exist
at a different manufacturer. Giving two or more fields the exact same
**Group** name combines them into one composite key — e.g. `street` +
`postal_code`, or `manufacturer` + `part_number` — scored as the *minimum* of
their individual scores rather than a weighted average. A strong match on one
member can never make up for a mismatch on another: "Hauptstraße 3" matching
perfectly in the wrong city, or a part number matching for the wrong
manufacturer, correctly scores the whole group as no match. Fields without a
Group continue to count individually, exactly as before this existed.

## LLM Assistant

The SQL editor can call an OpenAI-compatible `/v1/chat/completions` endpoint to:

- turn a natural-language prompt into SQL,
- safely generate and run read-only result queries,
- explain query results,
- explain SQL/database errors.

Generated SQL is placed in the editor for review. It is not executed
automatically unless the user chooses **Ask & Run**. Automatic execution is
limited to result-producing SQL (`SELECT`, `WITH`, `SHOW`, `EXPLAIN`,
`DESCRIBE`, `PRAGMA`) and blocks common DDL/DML operations.

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

For the local tinySQL connection, `GET /api/tinysql/agent-context` exposes
tinySQL's native prompt-ready database profile. The SQL editor also registers a
read-only helper procedure, `CALL datadock_agent_context(max_tables, max_chars)`,
which returns a compact table/column context from inside SQL.

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
├── matching.go      # Cross-table/cross-connection record matching orchestration
├── rag.go           # Schema retrieval and result profiling for LLM context
├── handlers.go      # HTTP route handlers
├── main_test.go     # HTTP integration tests
├── internal/
│   └── match/       # Domain-agnostic normalization, similarity, and matching engine
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
    ├── match.html         # Record matching wizard
    └── create_table.html # Table design wizard
```

### HTTP Routes

| Method | Path | Description |
|---|---|---|
| `GET` | `/` | Redirect to first table, or empty-state |
| `GET` | `/t/{table}` | Datasheet view (query params: `page`, `sort`, `dir`) |
| `GET` | `/t/{table}/map` | Render a local MBTiles/PMTiles layer |
| `GET` | `/t/{table}/route` | Route and reachability workspace for an RG table |
| `GET` | `/t/{table}/quality` | Persisted import provenance and spatial quality report |
| `GET` | `/t/{table}/export?format=csv\|csv-excel\|tsv\|xlsx\|json\|xml\|html\|sqlite\|geojson\|geojson-summary\|kml\|gpx\|shp` | Download a full table/view export |
| `GET` | `/t/{table}/new` | New record form |
| `POST` | `/t/{table}/new` | Create record |
| `GET` | `/t/{table}/{id}/edit` | Edit record form |
| `POST` | `/t/{table}/{id}/edit` | Update record |
| `POST` | `/t/{table}/{id}/delete` | Delete record |
| `POST` | `/drop-table/{table}` | Drop table |
| `GET` | `/query` | SQL editor page |
| `POST` | `/api/query` | Execute SQL (JSON API) |
| `POST` | `/api/export` | Download SQL query results as CSV, Excel-safe CSV, TSV, XLSX, JSON, NDJSON, XML, HTML, SQLite, GeoJSON, GeoJSON summary, KML, GPX, or Shapefile ZIP |
| `GET` | `/api/schema` | Return the compact active-connection schema snapshot used for LLM context |
| `GET` | `/api/tinysql/agent-context?max_tables=12&max_chars=6000` | Return tinySQL's native agent-context profile for the local tinySQL connection |
| `GET` | `/api/catalog` | Return async catalog roots for non-tinySQL connections |
| `GET` | `/api/catalog/expand` | Expand an async catalog node |
| `GET` | `/api/map/tiles/{table}/tilejson` | Local TileJSON metadata for an imported MBTiles/PMTiles layer |
| `GET` | `/api/map/tiles/{table}/{z}/{x}/{y}` | XYZ tile payload; supports MBTiles TMS conversion transparently |
| `POST` | `/api/routing/{table}/route` | Shortest path as GeoJSON (`from_id`/`to_id` or coordinates, optional cost profile) |
| `POST` | `/api/routing/{table}/reachable` | Cost-bounded reachable nodes and convex-hull GeoJSON area |
| `GET` | `/api/spatial-reports/{table}` | Provenance and normalized spatial data-quality report |
| `GET` | `/api/llm/health` | Test server-side connectivity to the configured LLM provider (Admin session) |
| `GET` | `/connections` | Connection manager |
| `POST` | `/connections` | Add a session-private managed connection |
| `POST` | `/connections/active` | Switch the active connection for the current session |
| `GET` | `/admin/setup` | First-run auth-mode chooser or Admin account setup |
| `POST` | `/admin/setup/mode` | Choose local no-login mode for loopback-only solo use |
| `POST` | `/admin/setup` | Create the initial Admin account |
| `GET` | `/admin/login` | Admin login form |
| `POST` | `/admin/login` | Start an authenticated Admin session |
| `POST` | `/admin/logout` | End the current Admin session |
| `GET` | `/admin` | Admin status and runtime settings (Admin session) |
| `POST` | `/admin/settings` | Apply runtime settings from the Admin UI (Admin session) |
| `POST` | `/admin/maintenance/toggle` | Toggle server-wide maintenance mode (Admin session) |
| `POST` | `/admin/change-password` | Change the current Admin account password (Admin session) |
| `POST` | `/admin/connections/persist` | Save/share a connection for all sessions (Admin session) |
| `POST` | `/admin/connections/forget` | Remove a saved connection (Admin session) |
| `POST` | `/admin/connections/default` | Change the server-wide default connection (Admin session) |
| `GET` | `/admin/users` | Manage local users and roles (Admin session) |
| `POST` | `/admin/users` | Create a local user (Admin session) |
| `POST` | `/admin/users/role` | Change a user's role (Admin session) |
| `POST` | `/admin/users/disable` | Enable or disable a local user (Admin session) |
| `POST` | `/admin/users/reset-password` | Reset a user's password (Admin session) |
| `POST` | `/admin/users/delete` | Delete a local user (Admin session) |
| `GET` | `/api/admin/status` | Admin status JSON (Admin session) |
| `GET/POST` | `/api/admin/settings` | Read or apply runtime settings as JSON (Admin session) |
| `GET` | `/jobs` | Job overview (Admin session) |
| `GET` | `/pipelines` | Versioned SQL pipeline manager (Admin session) |
| `GET/POST` | `/api/jobs` | List or create jobs (Admin session) |
| `POST` | `/api/jobs/run` | Run a registered job manually (Admin session) |
| `GET/POST` | `/api/pipelines` | List pipelines/runs or save a new immutable pipeline version (Admin session; POST blocked in maintenance mode) |
| `POST` | `/api/pipelines/run` | Run a pipeline version and record structural lineage (Admin session; blocked in maintenance mode) |
| `POST` | `/api/pipelines/delete` | Delete pipeline definitions while retaining run lineage (Admin session; blocked in maintenance mode) |
| `GET` | `/api/pipelines/export` | Download portable pipeline definitions without runs or results (Admin session) |
| `POST` | `/api/pipelines/import` | Import a bounded, validated definition bundle (Admin session; blocked in maintenance mode) |
| `GET` | `/migrate` | Table migration wizard |
| `POST` | `/migrate` | Run table migration |
| `GET` | `/match` | Record matching wizard |
| `POST` | `/match` | Run a match (`mode=preview\|export\|save`) |
| `POST` | `/match/tables` | Submit target for the Tables step: resolves each side's connection/table, importing an uploaded file first if that side is set to "File Upload" |
| `GET` | `/create-table` | Table designer |
| `POST` | `/create-table` | Create table |
| `GET` | `/export` | Export query form |
| `GET` | `/history` | Local query history page |
| `GET` | `/about` | Runtime and local browser storage information |
| `POST` | `/demo-data` | Load (or reset) the built-in demo dataset plus a sample scheduled job (Admin session) |
| `POST` | `/demo-data/remove` | Drop every demo table and the sample demo job (Admin session) |
| `GET/POST` | `/static/*` | Static assets |

Every data-mutating route above is blocked while **maintenance mode** is enabled
in Admin, returning `503 Service Unavailable`. Session-local connection changes,
Admin settings, the maintenance toggle itself, login/logout, and read-only query
execution stay available so an administrator can turn maintenance mode off again
and users can continue inspecting data with `SELECT`, `WITH`, `SHOW`,
`EXPLAIN`, `DESCRIBE`, or `PRAGMA`. `POST /match` is a partial exception: previewing candidates and
exporting them as CSV are read-only and stay available, like the SQL editor's
read-only queries; only `mode=save` (which creates/inserts into a real table)
is blocked. `POST /match/tables` follows the same pattern: a plain table
dropdown resubmit is read-only and stays available, but resolving a "File
Upload" side (which creates a table) is blocked, like other imports.

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

The endpoint accepts result-producing SQL (`SELECT`, `WITH`, `SHOW`,
`EXPLAIN`, `DESCRIBE`, `PRAGMA`) and returns an attachment in `csv`, `csv-excel`, `tsv`, `xlsx`,
`json`, `ndjson`, `xml`, `html`, `sqlite`, `geojson`, `geojson-summary`, `kml`,
`gpx`, or `shp` format.

GeoJSON-derived exports support a small native, mapshaper-like option set:
table exports accept query params such as `explode=1`, `simplify=0.001`,
`bbox=minx,miny,maxx,maxy`, `fields=name,type`, and `drop=internal_id`.
`POST /api/export` accepts matching JSON fields: `explode`,
`simplify_tolerance`, `bbox`, `fields`, and `drop_fields`. The native
implementation covers practical export transforms; topology-heavy operations
such as robust clip/erase/dissolve remain better suited to a dedicated
geometry engine or the mapshaper CLI.

**GET /api/schema**

Returns the active connection's compact dialect/schema/context snapshot as
JSON. The query editor uses this for the schema preview, and the LLM assistant
uses the same shape for prompt grounding.

**GET /api/tinysql/agent-context**

Returns tinySQL's native `BuildAgentContext` profile for the local tinySQL
connection. Optional query parameters `max_tables` and `max_chars` control the
profile size. For SQL-only workflows, `CALL datadock_agent_context(12, 6000)`
returns a compact non-reentrant context summary in the query result grid.

**GET /api/tinysql/vector-cache**

Returns bounded cache counts, approximate memory use, and recent local
`VEC_SEARCH` query shapes. Raw embedding vectors and answer texts are never
included. This diagnostic endpoint requires an Admin session.

**GET /api/admin/snapshot**

Downloads a portable tinySQL snapshot via `SaveToWriter`. It requires an Admin
session. DataDock intentionally has no restore endpoint; safe restore requires
an explicit operator action with size limits, validation, and an atomic swap.

## Development Workflows

```bash
make fmt-check
make test
make vet
make build-check
make vulncheck
```

`make ci` runs the local CI subset (`fmt-check`, tests, vet, and build) without
requiring optional external linters. `make check` runs the full local quality
gate: formatting checks, tests, vet, build, `staticcheck`, and `govulncheck`.
Install the optional tools with:

```bash
go install honnef.co/go/tools/cmd/staticcheck@latest
go install golang.org/x/vuln/cmd/govulncheck@latest
```

GitHub Actions mirrors these checks:

- **CI** runs on pushes to `main`, pull requests, and manual dispatch. It checks
  Go and asset formatting, runs `go test ./...`, `go vet ./...`,
  `go build ./...`, and `staticcheck ./...`.
- **Security** runs on pushes to `main`, pull requests, a weekly schedule, and
  manual dispatch. It runs `govulncheck` for every Go package.

The workflows intentionally use read-only repository permissions and cancel
superseded runs on the same ref.

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
