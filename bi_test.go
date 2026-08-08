package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSavedQuerySaveLoadListDelete(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	if _, ok, err := app.loadSavedQuery(ctx, "does-not-exist"); err != nil || ok {
		t.Fatalf("loadSavedQuery for a missing id: ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	saved, err := app.saveSavedQuery(ctx, SavedQuery{Name: "Top Customers", SQLText: "SELECT * FROM customers", Owner: "alice"})
	if err != nil {
		t.Fatalf("saveSavedQuery: %v", err)
	}
	if saved.ID == "" {
		t.Fatal("expected a generated id")
	}
	if saved.CreatedAt.IsZero() || saved.UpdatedAt.IsZero() {
		t.Error("expected CreatedAt/UpdatedAt to be stamped on save")
	}

	loaded, ok, err := app.loadSavedQuery(ctx, saved.ID)
	if err != nil || !ok {
		t.Fatalf("loadSavedQuery: ok=%v err=%v", ok, err)
	}
	if loaded.Name != "Top Customers" || loaded.SQLText != "SELECT * FROM customers" || loaded.Owner != "alice" {
		t.Errorf("loaded saved query = %+v, want fields to round-trip", loaded)
	}

	// Saving again under the same id overwrites, not duplicates, and keeps
	// the original CreatedAt.
	saved.SQLText = "SELECT * FROM customers WHERE active = 1"
	updated, err := app.saveSavedQuery(ctx, saved)
	if err != nil {
		t.Fatalf("saveSavedQuery (overwrite): %v", err)
	}
	if !updated.CreatedAt.Equal(saved.CreatedAt) {
		t.Errorf("expected CreatedAt to be preserved across overwrite, got %v want %v", updated.CreatedAt, saved.CreatedAt)
	}

	list, err := app.listSavedQueries(ctx)
	if err != nil {
		t.Fatalf("listSavedQueries: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 saved query after overwriting, got %d", len(list))
	}
	if list[0].SQLText != "SELECT * FROM customers WHERE active = 1" {
		t.Errorf("expected the overwrite to take effect, got SQLText=%q", list[0].SQLText)
	}

	if err := app.deleteSavedQuery(ctx, saved.ID); err != nil {
		t.Fatalf("deleteSavedQuery: %v", err)
	}
	if _, ok, err := app.loadSavedQuery(ctx, saved.ID); err != nil || ok {
		t.Errorf("expected the saved query to be gone after delete, got ok=%v err=%v", ok, err)
	}
}

func TestSavedQueryValidation(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	if _, err := app.saveSavedQuery(ctx, SavedQuery{SQLText: "SELECT 1"}); err == nil {
		t.Error("expected an error saving a saved query with no name")
	}
	if _, err := app.saveSavedQuery(ctx, SavedQuery{Name: "x"}); err == nil {
		t.Error("expected an error saving a saved query with no sql_text")
	}
}

func TestChartSaveLoadListDelete(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	spec := json.RawMessage(`{"mark":"bar","encoding":{"x":{"field":"region"},"y":{"field":"total","aggregate":"sum"}}}`)
	chart, err := app.saveChart(ctx, Chart{Name: "Sales by Region", SQLText: "SELECT region, total FROM sales", VegaLiteSpec: spec, Owner: "bob"})
	if err != nil {
		t.Fatalf("saveChart: %v", err)
	}
	if chart.ID == "" {
		t.Fatal("expected a generated id")
	}

	loaded, ok, err := app.loadChart(ctx, chart.ID)
	if err != nil || !ok {
		t.Fatalf("loadChart: ok=%v err=%v", ok, err)
	}
	if loaded.Name != "Sales by Region" || string(loaded.VegaLiteSpec) != string(spec) {
		t.Errorf("loaded chart did not round-trip: name=%q spec=%s", loaded.Name, loaded.VegaLiteSpec)
	}

	list, err := app.listCharts(ctx)
	if err != nil {
		t.Fatalf("listCharts: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 chart, got %d", len(list))
	}

	if err := app.deleteChart(ctx, chart.ID); err != nil {
		t.Fatalf("deleteChart: %v", err)
	}
	if _, ok, err := app.loadChart(ctx, chart.ID); err != nil || ok {
		t.Errorf("expected the chart to be gone after delete, got ok=%v err=%v", ok, err)
	}
}

func TestChartRequiresValidVegaLiteSpecJSON(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	if _, err := app.saveChart(ctx, Chart{Name: "x", SQLText: "SELECT 1", VegaLiteSpec: json.RawMessage(`not json`)}); err == nil {
		t.Error("expected an error saving a chart with an invalid vega_lite_spec")
	}
	if _, err := app.saveChart(ctx, Chart{Name: "x", SQLText: "SELECT 1"}); err == nil {
		t.Error("expected an error saving a chart with no vega_lite_spec")
	}
}

func TestDashboardSaveLoadListDelete(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	layout := []DashboardTile{{ChartID: "chart_abc", X: 0, Y: 0, W: 6, H: 3}}
	dash, err := app.saveDashboard(ctx, Dashboard{Name: "Overview", Description: "Main KPIs", Layout: layout, Owner: "carol"})
	if err != nil {
		t.Fatalf("saveDashboard: %v", err)
	}
	if dash.ID == "" {
		t.Fatal("expected a generated id")
	}

	loaded, ok, err := app.loadDashboard(ctx, dash.ID)
	if err != nil || !ok {
		t.Fatalf("loadDashboard: ok=%v err=%v", ok, err)
	}
	if len(loaded.Layout) != 1 || loaded.Layout[0].ChartID != "chart_abc" || loaded.Layout[0].W != 6 {
		t.Errorf("loaded dashboard layout did not round-trip: %+v", loaded.Layout)
	}

	// Updating the layout (e.g. after a drag/resize) overwrites in place.
	loaded.Layout = append(loaded.Layout, DashboardTile{ChartID: "chart_def", X: 6, Y: 0, W: 6, H: 3})
	updated, err := app.saveDashboard(ctx, loaded)
	if err != nil {
		t.Fatalf("saveDashboard (update layout): %v", err)
	}
	if !updated.CreatedAt.Equal(dash.CreatedAt) {
		t.Errorf("expected CreatedAt to be preserved across a layout update, got %v want %v", updated.CreatedAt, dash.CreatedAt)
	}

	list, err := app.listDashboards(ctx)
	if err != nil {
		t.Fatalf("listDashboards: %v", err)
	}
	if len(list) != 1 || len(list[0].Layout) != 2 {
		t.Fatalf("expected exactly 1 dashboard with 2 tiles after the update, got %d dashboards, layout=%+v", len(list), list)
	}

	if err := app.deleteDashboard(ctx, dash.ID); err != nil {
		t.Fatalf("deleteDashboard: %v", err)
	}
	if _, ok, err := app.loadDashboard(ctx, dash.ID); err != nil || ok {
		t.Errorf("expected the dashboard to be gone after delete, got ok=%v err=%v", ok, err)
	}
}

// postJSON calls handler directly (mirroring existing handler-level tests
// like TestMatchHandlerLoadsSavedConfig) with ctx attached and body encoded
// as JSON, returning the recorder for status/body assertions.
func postJSON(t *testing.T, ctx context.Context, handler http.HandlerFunc, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(string(encoded))).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func TestBIOwnershipBlocksNonOwnerUpdatesAndDeletes(t *testing.T) {
	app := newTestApp(t)
	aliceCtx := sessionCtx(app, "sess-alice-0000000000000", "alice", RoleUser)
	bobCtx := sessionCtx(app, "sess-bob-00000000000000", "bob", RoleUser)
	adminCtx := sessionCtx(app, "sess-admin-0000000000000", "root", RoleAdmin)

	rec := postJSON(t, aliceCtx, app.apiSavedQueriesHandler, http.MethodPost, "/api/bi/queries",
		SavedQuery{Name: "Alice's Query", SQLText: "SELECT 1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("create as alice: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var created SavedQuery
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created saved query: %v", err)
	}
	if created.Owner != "alice" {
		t.Fatalf("expected owner=alice, got %q", created.Owner)
	}

	// Bob cannot update alice's saved query, including trying to steal it by
	// resubmitting with his own name as the owner.
	rec = postJSON(t, bobCtx, app.apiSavedQueriesHandler, http.MethodPost, "/api/bi/queries",
		SavedQuery{ID: created.ID, Name: "Hijacked", SQLText: "SELECT 2", Owner: "bob"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bob updating alice's saved query: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}
	if reloaded, _, _ := app.loadSavedQuery(context.Background(), created.ID); reloaded.Name != "Alice's Query" || reloaded.Owner != "alice" {
		t.Fatalf("expected alice's saved query to be unmodified by bob's attempt, got %+v", reloaded)
	}

	// Bob cannot delete it either.
	rec = postJSON(t, bobCtx, app.apiDeleteSavedQueryHandler, http.MethodPost, "/api/bi/queries/delete",
		map[string]string{"id": created.ID})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bob deleting alice's saved query: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	// Alice herself can update it.
	rec = postJSON(t, aliceCtx, app.apiSavedQueriesHandler, http.MethodPost, "/api/bi/queries",
		SavedQuery{ID: created.ID, Name: "Alice's Query v2", SQLText: "SELECT 3"})
	if rec.Code != http.StatusOK {
		t.Fatalf("alice updating her own saved query: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Admin can delete anyone's saved query.
	rec = postJSON(t, adminCtx, app.apiDeleteSavedQueryHandler, http.MethodPost, "/api/bi/queries/delete",
		map[string]string{"id": created.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("admin deleting alice's saved query: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}
