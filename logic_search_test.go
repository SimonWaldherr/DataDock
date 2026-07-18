package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// stubEmbedder is a fake EmbeddingClient keyed by exact input text, used to
// test the indexing/search pipeline without a real embedding provider. It
// also counts calls so tests can assert the hash-skip logic actually
// avoided re-embedding unchanged definitions.
type stubEmbedder struct {
	vectors map[string][]float64
	calls   int
}

func (s *stubEmbedder) Embed(_ context.Context, texts []string) ([][]float64, error) {
	s.calls++
	out := make([][]float64, len(texts))
	for i, t := range texts {
		v, ok := s.vectors[t]
		if !ok {
			return nil, fmt.Errorf("stubEmbedder: no vector configured for %q", t)
		}
		out[i] = v
	}
	return out, nil
}

// TestStartReindexConnectionLogicRejectsUnsupportedDialect guards the
// dialect gate: tinySQL/SQLite connections have no retrievable view/routine
// definitions (see fetchViewDefinition/fetchRoutineDefinition), so a
// reindex must fail clearly before ever touching the network.
func TestStartReindexConnectionLogicRejectsUnsupportedDialect(t *testing.T) {
	app := newTestApp(t)
	conn, err := OpenManagedConnectionVerbose(context.Background(), "sqlite-shape-test", "test", "sqlite", ":memory:", nil)
	if err != nil {
		t.Fatalf("open backing connection: %v", err)
	}
	defer conn.DB.Close()
	if err := app.conns.Add(conn); err != nil {
		t.Fatalf("add connection: %v", err)
	}

	err = app.startReindexConnectionLogic("", conn.ID)
	if err == nil {
		t.Fatal("expected an error for a SQLite connection")
	}
	if !strings.Contains(err.Error(), "isn't supported for") {
		t.Errorf("expected a clear \"isn't supported\" reason, got: %v", err)
	}
}

// TestStartReindexConnectionLogicRequiresEmbeddingModel guards the
// embedding-config gate: even a supported dialect must not attempt a
// reindex when no embedding model is configured.
func TestStartReindexConnectionLogicRequiresEmbeddingModel(t *testing.T) {
	app := newTestApp(t)
	conn, err := OpenManagedConnectionVerbose(context.Background(), "mysql-shape-test", "test", "sqlite", ":memory:", nil)
	if err != nil {
		t.Fatalf("open backing connection: %v", err)
	}
	defer conn.DB.Close()
	conn.Dialect = DialectProfileForName("mysql") // force a supported dialect; backing DB only avoids a nil *sql.DB panic
	if err := app.conns.Add(conn); err != nil {
		t.Fatalf("add connection: %v", err)
	}

	err = app.startReindexConnectionLogic("", conn.ID)
	if err == nil {
		t.Fatal("expected an error when no embedding model is configured")
	}
	if !strings.Contains(err.Error(), "embedding model") {
		t.Errorf("expected an \"embedding model\" reason, got: %v", err)
	}
}

// TestApplyLogicIndexSkipsUnchangedDefinitions guards the hash-skip
// behavior: reindexing the same definition twice under the same embedding
// model must not call the embedder a second time.
func TestApplyLogicIndexSkipsUnchangedDefinitions(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	embedder := &stubEmbedder{vectors: map[string][]float64{"SELECT 1": {1, 0}}}
	defs := []logicObjectDefinition{{name: "v1", kind: "view", definition: "SELECT 1"}}

	report, err := app.applyLogicIndex(ctx, "conn1", "model-a", embedder, defs)
	if err != nil {
		t.Fatalf("first index: %v", err)
	}
	if report.Indexed != 1 || report.Skipped != 0 {
		t.Fatalf("first index: expected 1 indexed/0 skipped, got %+v", report)
	}
	if embedder.calls != 1 {
		t.Fatalf("expected 1 embed call after first index, got %d", embedder.calls)
	}

	report, err = app.applyLogicIndex(ctx, "conn1", "model-a", embedder, defs)
	if err != nil {
		t.Fatalf("second index: %v", err)
	}
	if report.Indexed != 0 || report.Skipped != 1 {
		t.Fatalf("second index: expected 0 indexed/1 skipped, got %+v", report)
	}
	if embedder.calls != 1 {
		t.Errorf("expected no additional embed call for an unchanged definition, got %d total calls", embedder.calls)
	}
}

// TestApplyLogicIndexReEmbedsOnModelChange is a regression test for the
// design-review fix: changing the configured embedding model must NOT be
// treated as "unchanged" just because the definition's content hash
// matches a row stored under the old model.
func TestApplyLogicIndexReEmbedsOnModelChange(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	embedder := &stubEmbedder{vectors: map[string][]float64{"SELECT 1": {1, 0}}}
	defs := []logicObjectDefinition{{name: "v1", kind: "view", definition: "SELECT 1"}}

	if _, err := app.applyLogicIndex(ctx, "conn1", "model-a", embedder, defs); err != nil {
		t.Fatalf("index under model-a: %v", err)
	}
	report, err := app.applyLogicIndex(ctx, "conn1", "model-b", embedder, defs)
	if err != nil {
		t.Fatalf("index under model-b: %v", err)
	}
	if report.Indexed != 1 || report.Skipped != 0 {
		t.Fatalf("expected a re-embed on model change, got %+v", report)
	}
	if embedder.calls != 2 {
		t.Errorf("expected 2 total embed calls (one per model), got %d", embedder.calls)
	}

	local := app.localTinySQLConn()
	if _, ok := app.objectEmbeddingHash(ctx, local, "conn1", "v1", "model-a"); ok {
		t.Error("expected the model-a row to have been pruned by the model-b upsert, but it still exists")
	}
	if _, ok := app.objectEmbeddingHash(ctx, local, "conn1", "v1", "model-b"); !ok {
		t.Error("expected a model-b row to exist after reindexing")
	}
}

// TestApplyLogicIndexRemovesStaleObjects guards cleanup of renamed/dropped
// objects: an object present in one reindex run but absent from the next
// must be removed, not left behind as a stale, never-matched row.
func TestApplyLogicIndexRemovesStaleObjects(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	embedder := &stubEmbedder{vectors: map[string][]float64{
		"SELECT 1": {1, 0},
		"SELECT 2": {0, 1},
	}}

	report, err := app.applyLogicIndex(ctx, "conn1", "model-a", embedder,
		[]logicObjectDefinition{
			{name: "v1", kind: "view", definition: "SELECT 1"},
			{name: "v2", kind: "view", definition: "SELECT 2"},
		})
	if err != nil {
		t.Fatalf("first index: %v", err)
	}
	if report.Indexed != 2 {
		t.Fatalf("expected 2 indexed, got %+v", report)
	}

	report, err = app.applyLogicIndex(ctx, "conn1", "model-a", embedder,
		[]logicObjectDefinition{{name: "v1", kind: "view", definition: "SELECT 1"}})
	if err != nil {
		t.Fatalf("second index: %v", err)
	}
	if report.Skipped != 1 || report.Removed != 1 {
		t.Fatalf("expected 1 skipped (v1 unchanged) and 1 removed (v2 gone), got %+v", report)
	}
}

// TestSearchObjectLogicRanksByScore exercises the real tinySQL VECTOR/
// VEC_COSINE_SIMILARITY search path (this part needs no external server —
// it's DataDock's own local engine) against a stub embedder, verifying
// results come back ordered by similarity to the query vector.
func TestSearchObjectLogicRanksByScore(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	conn, err := OpenManagedConnectionVerbose(ctx, "conn1", "test", "sqlite", ":memory:", nil)
	if err != nil {
		t.Fatalf("open backing connection: %v", err)
	}
	defer conn.DB.Close()
	if err := app.conns.Add(conn); err != nil {
		t.Fatalf("add connection: %v", err)
	}

	// "alpha" == query vector exactly (score 1.0); "gamma" is 45 degrees off
	// (score ~0.707); "beta" is orthogonal (score 0.0).
	embedder := &stubEmbedder{vectors: map[string][]float64{
		"alpha": {1, 0},
		"beta":  {0, 1},
		"gamma": {0.7071067811865476, 0.7071067811865476},
		"query": {1, 0},
	}}
	app.embeddingConfig.Model = "test-model"
	app.embeddingClient = embedder

	_, err = app.applyLogicIndex(ctx, conn.ID, "test-model", embedder, []logicObjectDefinition{
		{name: "v_alpha", kind: "view", definition: "alpha"},
		{name: "v_beta", kind: "view", definition: "beta"},
		{name: "p_gamma", kind: "procedure", definition: "gamma"},
	})
	if err != nil {
		t.Fatalf("index: %v", err)
	}

	hits, err := app.searchObjectLogic(ctx, "", conn.ID, "query", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("expected 3 hits, got %d: %+v", len(hits), hits)
	}
	got := []string{hits[0].ObjectName, hits[1].ObjectName, hits[2].ObjectName}
	want := []string{"v_alpha", "p_gamma", "v_beta"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rank %d: got %q, want %q (full order: %v)", i, got[i], want[i], got)
		}
	}
	if hits[0].ObjectKind != "view" {
		t.Errorf("expected v_alpha's kind to be \"view\", got %q", hits[0].ObjectKind)
	}
	if hits[1].ObjectKind != "procedure" {
		t.Errorf("expected p_gamma's kind to be \"procedure\", got %q", hits[1].ObjectKind)
	}
}

// TestDeleteObjectEmbeddingsForConnection guards the connection-lifecycle
// cleanup hook (called from admin_auth.go on connection removal): after
// deletion, nothing for that connection ID should remain searchable, even
// under a reused ID.
func TestDeleteObjectEmbeddingsForConnection(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	embedder := &stubEmbedder{vectors: map[string][]float64{"SELECT 1": {1, 0}}}
	if _, err := app.applyLogicIndex(ctx, "conn1", "model-a", embedder,
		[]logicObjectDefinition{{name: "v1", kind: "view", definition: "SELECT 1"}}); err != nil {
		t.Fatalf("index: %v", err)
	}

	if err := app.deleteObjectEmbeddingsForConnection(ctx, "conn1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	local := app.localTinySQLConn()
	if _, ok := app.objectEmbeddingHash(ctx, local, "conn1", "v1", "model-a"); ok {
		t.Error("expected no rows to remain for conn1 after deleteObjectEmbeddingsForConnection")
	}
}

// TestSearchObjectLogicRejectsConnectionNotVisibleToSession guards the
// per-user isolation this feature needs once each user can bring their own
// private connection: session B must never be able to pull search results
// for session A's private connection just by knowing/guessing its ID, even
// though the embeddings themselves live in the shared local tinySQL table
// with no per-row ACL of their own — the visibility check has to happen in
// Go, via the same ConnectionManager.GetFor every other session-supplied
// connection ID in this codebase already goes through.
func TestSearchObjectLogicRejectsConnectionNotVisibleToSession(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	const alice = "alice-session-000001"

	conn, err := OpenManagedConnectionVerbose(ctx, "alice-private-db", "test", "sqlite", ":memory:", nil)
	if err != nil {
		t.Fatalf("open backing connection: %v", err)
	}
	defer conn.DB.Close()
	conn.Owner = alice
	if err := app.conns.Add(conn); err != nil {
		t.Fatalf("add connection: %v", err)
	}

	embedder := &stubEmbedder{vectors: map[string][]float64{"alpha": {1, 0}, "query": {1, 0}}}
	app.embeddingConfig.Model = "test-model"
	app.embeddingClient = embedder
	if _, err := app.applyLogicIndex(ctx, conn.ID, "test-model", embedder,
		[]logicObjectDefinition{{name: "v1", kind: "view", definition: "alpha"}}); err != nil {
		t.Fatalf("index: %v", err)
	}

	if _, err := app.searchObjectLogic(ctx, alice, conn.ID, "query", 10); err != nil {
		t.Errorf("expected the owning session to search its own private connection, got: %v", err)
	}

	const bob = "bob-session-0000001"
	if _, err := app.searchObjectLogic(ctx, bob, conn.ID, "query", 10); err == nil {
		t.Error("expected a different session to be rejected from searching someone else's private connection")
	}
}

// TestReindexConnectionLogicHandlerAuthorization guards the HTTP-level
// authorization split reindexConnectionLogicHandler enforces: a private
// connection's owner may trigger its own reindex even without being an
// admin (the whole point of letting each user bring their own connection),
// but reindexing a SHARED connection — which affects the index every user's
// prompts draw on — still requires an admin, and a session that can't even
// see a connection (someone else's private one) is rejected outright.
func TestReindexConnectionLogicHandlerAuthorization(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)

	// Admin setup runs first: it internally calls applyRuntimeSettings
	// (to persist the new admin password alongside every other current
	// setting), which would otherwise recompute and clobber the
	// directly-assigned embeddingConfig/embeddingClient set up below.
	adminCookie := setupAdminSession(t, mux)
	app.embeddingConfig.Model = "test-model"
	app.embeddingClient = &stubEmbedder{vectors: map[string][]float64{}}

	const alice = "alice-session-000001"
	const bob = "bob-session-0000001"
	// /connections/reindex-logic now requires being logged in as some role
	// (any authenticated session), on top of the owner-vs-shared check this
	// test exercises below.
	app.markSessionAuthenticated(alice, "alice", RoleUser)
	app.markSessionAuthenticated(bob, "bob", RoleUser)

	privateConn, err := OpenManagedConnectionVerbose(context.Background(), "alice-private-db", "test", "sqlite", ":memory:", nil)
	if err != nil {
		t.Fatalf("open private backing connection: %v", err)
	}
	defer privateConn.DB.Close()
	privateConn.Dialect = DialectProfileForName("mysql")
	privateConn.Owner = alice
	if err := app.conns.Add(privateConn); err != nil {
		t.Fatalf("add private connection: %v", err)
	}

	sharedConn, err := OpenManagedConnectionVerbose(context.Background(), "shared-db", "test", "sqlite", ":memory:", nil)
	if err != nil {
		t.Fatalf("open shared backing connection: %v", err)
	}
	defer sharedConn.DB.Close()
	sharedConn.Dialect = DialectProfileForName("mysql")
	if err := app.conns.Add(sharedConn); err != nil {
		t.Fatalf("add shared connection: %v", err)
	}

	reindex := func(sessionID, connID string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/connections/reindex-logic",
			strings.NewReader(url.Values{"id": {connID}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if sessionID != "" {
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionID})
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	if rec := reindex(alice, privateConn.ID); rec.Code != http.StatusSeeOther {
		t.Errorf("owner reindexing their own private connection: expected 303, got %d: %s", rec.Code, rec.Body.String())
	}

	if rec := reindex(bob, privateConn.ID); rec.Code == http.StatusSeeOther {
		t.Error("expected a non-owning session to be rejected from reindexing someone else's private connection, got a redirect")
	}

	if rec := reindex(bob, sharedConn.ID); rec.Code == http.StatusSeeOther || !strings.Contains(rec.Body.String(), "Only an admin") {
		t.Errorf("expected a non-admin to be rejected from reindexing a shared connection with a clear reason, got %d: %s", rec.Code, rec.Body.String())
	}

	if rec := reindex(adminCookie.Value, sharedConn.ID); rec.Code != http.StatusSeeOther {
		t.Errorf("admin reindexing a shared connection: expected 303, got %d: %s", rec.Code, rec.Body.String())
	}
}
