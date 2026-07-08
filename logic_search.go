package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// objectEmbeddingsTable stores one embedding vector per (connection, fully
// qualified view/procedure/function name) — see LogicIndexReport and the
// functions below for how it's populated and searched. SQL text lives in
// queries.go, following this codebase's usual split.
const objectEmbeddingsTable = "__datadock_object_embeddings"

// logicEmbeddingBatchSize caps how many definitions are sent to the
// embedding provider per HTTP call during a reindex, matching the batch
// size a reference bulk-ingest project (external/R3) uses for its own
// inserts — large enough to avoid one round trip per object on a connection
// with hundreds of routines, small enough to keep any one request modest.
const logicEmbeddingBatchSize = 16

// LogicIndexReport summarizes one reindexConnectionLogic run, shown on the
// connections page after it completes.
type LogicIndexReport struct {
	Indexed   int       `json:"indexed"`
	Skipped   int       `json:"skipped"`
	Removed   int       `json:"removed"`
	Errors    []string  `json:"errors,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// LogicSearchHit is one ranked result from searchObjectLogic. It
// deliberately excludes the definition text itself — callers link to the
// existing /t/{name} (view) or /r/{name}?kind=... (routine) pages for that.
type LogicSearchHit struct {
	ObjectName string  `json:"objectName"`
	ObjectKind string  `json:"objectKind"`
	Score      float64 `json:"score"`
}

func (a *App) ensureObjectEmbeddingsTable(ctx context.Context) error {
	_, err := a.execConn(ctx, a.localTinySQLConn(), "logic.ensure_table", objectEmbeddingsEnsureTableSQL)
	if err == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return nil
	}
	return fmt.Errorf("ensure object embeddings table: %w", err)
}

// logicSearchSupported reports whether conn's dialect exposes retrievable
// view/routine definitions at all (see fetchViewDefinition/
// fetchRoutineDefinition) — the same set of dialects Routines/Dependencies
// already support elsewhere in the catalog browser.
func logicSearchSupported(conn *DBConnection) bool {
	switch conn.Dialect.Name {
	case "PostgreSQL", "MariaDB/MySQL", "Microsoft SQL Server":
		return true
	default:
		return false
	}
}

// startReindexConnectionLogic validates connID (resolved via sessionID,
// exactly like any other session-supplied connection ID elsewhere in this
// codebase — see ConnectionManager.GetFor — so one session can't trigger a
// reindex of a connection it can't even see) and the embedding config
// synchronously, then runs the actual reindex in a detached goroutine
// (mirroring runScheduledMatch in match_schedule.go) since embedding
// hundreds of routines can take a while and must outlive the triggering
// HTTP request. Pass sessionID="" for a shared/admin-triggered reindex
// (matching runScheduledMatch's own background-context convention); a
// private connection can only ever be reindexed with its owner's actual
// sessionID. Returns an error immediately if a reindex for this connection
// is already running rather than racing two runs against each other.
func (a *App) startReindexConnectionLogic(sessionID, connID string) error {
	connID = strings.TrimSpace(connID)
	if connID == "" {
		return fmt.Errorf("connection id is required")
	}
	conn := a.conns.GetFor(sessionID, connID)
	if conn == nil {
		return fmt.Errorf("connection %q not found", connID)
	}
	if !logicSearchSupported(conn) {
		return fmt.Errorf("logic search isn't supported for %s", conn.Dialect.Name)
	}
	if a.embeddingClientFn() == nil {
		return fmt.Errorf("an embedding model must be configured in Admin Settings before reindexing")
	}

	a.logicIndexMu.Lock()
	if a.logicIndexing == nil {
		a.logicIndexing = make(map[string]bool)
	}
	if a.logicIndexing[connID] {
		a.logicIndexMu.Unlock()
		return fmt.Errorf("a reindex for this connection is already running")
	}
	a.logicIndexing[connID] = true
	a.logicIndexMu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		report, err := a.reindexConnectionLogic(ctx, sessionID, connID)
		if err != nil {
			report.Errors = append(report.Errors, err.Error())
		}
		report.UpdatedAt = time.Now()
		a.logicIndexMu.Lock()
		delete(a.logicIndexing, connID)
		if a.logicIndexStatus == nil {
			a.logicIndexStatus = make(map[string]LogicIndexReport)
		}
		a.logicIndexStatus[connID] = report
		a.logicIndexMu.Unlock()
	}()
	return nil
}

// logicIndexStatusFor returns the last completed reindex report for connID
// (zero value if none has ever run) and whether a run is currently in
// progress, for the connections page to display.
func (a *App) logicIndexStatusFor(connID string) (LogicIndexReport, bool) {
	a.logicIndexMu.Lock()
	defer a.logicIndexMu.Unlock()
	return a.logicIndexStatus[connID], a.logicIndexing[connID]
}

// logicObjectTarget is one view/procedure/function enumerated for reindexing.
type logicObjectTarget struct {
	name string // fully qualified, matching fetchViewDefinition/fetchRoutineDefinition's expectations
	kind string // "view" | "procedure" | "function"
}

// logicObjectDefinition is one enumerated object with its definition text
// already fetched, ready for applyLogicIndex's hash-check/embed/store
// pipeline. Splitting this out from the dialect-specific enumeration below
// is what makes that pipeline directly testable without a live Postgres/
// MySQL/MSSQL server (see logic_search_test.go).
type logicObjectDefinition struct {
	name, kind, definition string
}

// reindexConnectionLogic re-embeds every view/procedure/function definition
// in connID's CURRENT database that has changed since the last run, and
// removes rows for objects no longer present. It deliberately does NOT walk
// the full multi-database catalog tree (see ExpandCatalogDatabase) — that's
// the same expensive, timeout-prone operation listCatalogMSSQL's own
// comments warn about, so this is a stated v1 limitation, not an oversight.
// connID is resolved via sessionID exactly like startReindexConnectionLogic.
func (a *App) reindexConnectionLogic(ctx context.Context, sessionID, connID string) (LogicIndexReport, error) {
	report := LogicIndexReport{}
	conn := a.conns.GetFor(sessionID, connID)
	if conn == nil {
		return report, fmt.Errorf("connection %q not found", connID)
	}
	if !logicSearchSupported(conn) {
		return report, fmt.Errorf("logic search isn't supported for %s", conn.Dialect.Name)
	}
	embedder := a.embeddingClientFn()
	if embedder == nil {
		return report, fmt.Errorf("an embedding model must be configured in Admin Settings before reindexing")
	}

	tree, err := conn.ListCatalog(ctx)
	if err != nil {
		return report, fmt.Errorf("list catalog: %w", err)
	}

	var targets []logicObjectTarget
	for _, db := range tree {
		if !db.Current {
			continue
		}
		for _, schema := range db.Schemas {
			for _, v := range schema.Views {
				targets = append(targets, logicObjectTarget{name: qualifyLogicObjectName(schema.Name, v.Name), kind: "view"})
			}
			for _, p := range schema.Procedures {
				targets = append(targets, logicObjectTarget{name: qualifyLogicObjectName(schema.Name, p.Name), kind: p.Kind})
			}
		}
	}

	var defs []logicObjectDefinition
	var fetchErrors []string
	for _, t := range targets {
		var definition string
		var err error
		if t.kind == "view" {
			definition, err = conn.fetchViewDefinition(ctx, t.name)
		} else {
			definition, err = conn.fetchRoutineDefinition(ctx, t.name, t.kind)
		}
		if err != nil {
			fetchErrors = append(fetchErrors, fmt.Sprintf("%s: %v", t.name, err))
			continue
		}
		defs = append(defs, logicObjectDefinition{name: t.name, kind: t.kind, definition: definition})
	}

	report, err = a.applyLogicIndex(ctx, connID, a.currentEmbeddingModel(), embedder, defs)
	report.Errors = append(fetchErrors, report.Errors...)
	return report, err
}

// applyLogicIndex is the dialect-independent half of a reindex: hash-skip
// unchanged definitions (scoped to embedModel — see objectEmbeddingsSelectHashSQL's
// comment in queries.go for why changing the configured embedding model
// must NOT count as "unchanged"), embed+upsert new/changed ones in batches,
// and delete rows for objects no longer present in defs. Separated from
// reindexConnectionLogic's catalog enumeration so it's directly testable.
func (a *App) applyLogicIndex(ctx context.Context, connID, embedModel string, embedder EmbeddingClient, defs []logicObjectDefinition) (LogicIndexReport, error) {
	report := LogicIndexReport{}
	if err := a.ensureObjectEmbeddingsTable(ctx); err != nil {
		return report, err
	}

	local := a.localTinySQLConn()
	seen := make(map[string]bool, len(defs))

	type pendingObject struct {
		name, kind, hash, definition string
	}
	var toEmbed []pendingObject
	for _, d := range defs {
		seen[d.name] = true
		hash := contentHashHex(d.definition)
		if existingHash, ok := a.objectEmbeddingHash(ctx, local, connID, d.name, embedModel); ok && existingHash == hash {
			report.Skipped++
			continue
		}
		toEmbed = append(toEmbed, pendingObject{name: d.name, kind: d.kind, hash: hash, definition: d.definition})
	}

	for i := 0; i < len(toEmbed); i += logicEmbeddingBatchSize {
		end := i + logicEmbeddingBatchSize
		if end > len(toEmbed) {
			end = len(toEmbed)
		}
		batch := toEmbed[i:end]
		texts := make([]string, len(batch))
		for j, p := range batch {
			texts[j] = p.definition
		}
		vectors, err := embedder.Embed(ctx, texts)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("embed batch: %v", err))
			continue
		}
		if len(vectors) != len(batch) {
			report.Errors = append(report.Errors, fmt.Sprintf("embed batch: expected %d vectors, got %d", len(batch), len(vectors)))
			continue
		}
		for j, p := range batch {
			vecJSON, err := json.Marshal(vectors[j])
			if err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: encode vector: %v", p.name, err))
				continue
			}
			// Delete-then-insert, keyed WITHOUT embed_model (see
			// objectEmbeddingsDeleteSQL's comment in queries.go): this is
			// what prunes a stale vector from a previously configured
			// embedding model for free, on the very next reindex.
			if _, err := a.execConn(ctx, local, "logic.delete", objectEmbeddingsDeleteSQL, connID, p.name); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", p.name, err))
				continue
			}
			if _, err := a.execConn(ctx, local, "logic.insert", objectEmbeddingsInsertSQL,
				connID, p.name, p.kind, p.hash, embedModel, string(vecJSON), time.Now().UTC().Format(time.RFC3339)); err != nil {
				report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", p.name, err))
				continue
			}
			report.Indexed++
		}
	}

	if existingNames, err := a.objectEmbeddingNames(ctx, local, connID); err == nil {
		for _, name := range existingNames {
			if seen[name] {
				continue
			}
			if _, err := a.execConn(ctx, local, "logic.delete_stale", objectEmbeddingsDeleteSQL, connID, name); err == nil {
				report.Removed++
			}
		}
	}

	return report, nil
}

// searchObjectLogic embeds query and ranks every indexed view/procedure/
// function on connID by cosine similarity, returning at most topK hits.
// connID is resolved via sessionID (like every other session-supplied
// connection ID — see ConnectionManager.GetFor) so one session can never
// pull search results for a connection it can't see, even by guessing or
// replaying another session's private connection ID.
func (a *App) searchObjectLogic(ctx context.Context, sessionID, connID, query string, topK int) ([]LogicSearchHit, error) {
	connID = strings.TrimSpace(connID)
	query = strings.TrimSpace(query)
	if connID == "" {
		return nil, fmt.Errorf("connection id is required")
	}
	if query == "" {
		return nil, fmt.Errorf("search query is required")
	}
	if a.conns.GetFor(sessionID, connID) == nil {
		return nil, fmt.Errorf("connection %q not found", connID)
	}
	embedder := a.embeddingClientFn()
	if embedder == nil {
		return nil, fmt.Errorf("an embedding model must be configured in Admin Settings before searching")
	}
	embedModel := a.currentEmbeddingModel()
	if topK <= 0 {
		topK = 5
	}

	vectors, err := embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("embed query: expected 1 vector, got %d", len(vectors))
	}
	vecJSON, err := json.Marshal(vectors[0])
	if err != nil {
		return nil, err
	}

	rows, err := a.queryConn(ctx, a.localTinySQLConn(), "logic.search", objectEmbeddingsSearchQuery(topK),
		string(vecJSON), connID, embedModel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hits []LogicSearchHit
	for rows.Next() {
		var h LogicSearchHit
		if err := rows.Scan(&h.ObjectName, &h.ObjectKind, &h.Score); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// deleteObjectEmbeddingsForConnection removes every embedding row for
// connID, called when that connection itself is deleted (see
// admin_auth.go) so a later connection ID reuse can't resurface stale
// vectors against an unrelated database.
func (a *App) deleteObjectEmbeddingsForConnection(ctx context.Context, connID string) error {
	connID = strings.TrimSpace(connID)
	if connID == "" {
		return nil
	}
	if err := a.ensureObjectEmbeddingsTable(ctx); err != nil {
		return err
	}
	_, err := a.execConn(ctx, a.localTinySQLConn(), "logic.delete_connection", objectEmbeddingsDeleteForConnectionSQL, connID)
	return err
}

// objectEmbeddingHash looks up the stored definition hash for one object
// under embedModel specifically, returning ok=false if no such row exists
// (including when the object was previously embedded under a different
// model — that's treated as "not yet embedded under the current model").
func (a *App) objectEmbeddingHash(ctx context.Context, local *DBConnection, connID, objectName, embedModel string) (hash string, ok bool) {
	rows, err := a.queryConn(ctx, local, "logic.select_hash", objectEmbeddingsSelectHashSQL, connID, objectName, embedModel)
	if err != nil {
		return "", false
	}
	defer rows.Close()
	if !rows.Next() {
		return "", false
	}
	if err := rows.Scan(&hash); err != nil {
		return "", false
	}
	return hash, true
}

// objectEmbeddingNames lists every currently-indexed object name for connID,
// regardless of embed_model, so reindexConnectionLogic can diff it against
// the freshly-enumerated catalog and delete rows for objects that no longer
// exist (renamed/dropped views or routines).
func (a *App) objectEmbeddingNames(ctx context.Context, local *DBConnection, connID string) ([]string, error) {
	rows, err := a.queryConn(ctx, local, "logic.select_names", objectEmbeddingsSelectNamesSQL, connID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// qualifyLogicObjectName joins a schema and object name the same way every
// fetchViewDefinition/fetchRoutineDefinition caller elsewhere in this
// codebase already does (see splitQualifiedName in catalog_browser.go),
// producing a 2-part "schema.name" (schema being MySQL's database, for that
// dialect) scoped to the connection's current database — never a 3-part
// cross-database name, matching reindexConnectionLogic's current-database-only scope.
func qualifyLogicObjectName(schema, name string) string {
	if schema == "" {
		return name
	}
	return schema + "." + name
}

// contentHashHex returns a stable hex-encoded hash of text, used to detect
// that a definition hasn't changed since the last reindex so re-embedding
// can be skipped (same idea as a reference project's contentHash helper,
// external/R3/vectorstore_tinysql.go — reference-only code, not imported).
func contentHashHex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
