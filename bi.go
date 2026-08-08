// Package main: bi.go implements the BI foundation — saved SQL queries,
// charts (a query plus a Vega-Lite spec), and dashboards (a named layout of
// charts). Persistence follows the same delete-then-insert-by-id upsert
// idiom as match_config.go, against the local tinySQL connection exactly
// like every other piece of DataDock's own metadata (settings, pipelines,
// match configs, users). See queries.go's "BI foundation" section for the
// SQL text.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	savedQueriesTable = "__datadock_saved_queries"
	chartsTable       = "__datadock_charts"
	dashboardsTable   = "__datadock_dashboards"
)

// SavedQuery is a named, reusable SQL query — the simplest BI building
// block, and an optional source a Chart can point back to (via
// Chart.SavedQueryID) instead of carrying its own copy of the SQL.
type SavedQuery struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	SQLText      string    `json:"sql_text"`
	ConnectionID string    `json:"connection_id"`
	Owner        string    `json:"owner"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Chart is a named visualization: its own SQL text (so "save this chart" is
// always a single action, never blocked on saving a query first) plus a
// Vega-Lite spec describing how to render whatever that SQL returns.
// SavedQueryID is an optional link back to a SavedQuery for reuse/traceability
// — nothing reads through it to re-fetch SQLText, so an edited/deleted saved
// query never breaks an existing chart.
type Chart struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	SavedQueryID string          `json:"saved_query_id,omitempty"`
	SQLText      string          `json:"sql_text"`
	VegaLiteSpec json.RawMessage `json:"vega_lite_spec"`
	Owner        string          `json:"owner"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// DashboardTile positions one chart on a dashboard's CSS-grid canvas.
type DashboardTile struct {
	ChartID string `json:"chart_id"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	W       int    `json:"w"`
	H       int    `json:"h"`
}

// Dashboard is a named arrangement of charts. Layout is stored as opaque
// JSON (a []DashboardTile) rather than normalized rows, matching pipelines.go's
// definition_json rationale: adding a tile field later never needs an ALTER
// TABLE.
type Dashboard struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Layout      []DashboardTile `json:"layout"`
	Owner       string          `json:"owner"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// newBIObjectID generates a random, unguessable ID for a BI entity, mirroring
// pipelines.go's newPipelineRunID (same rand-8-bytes-as-hex shape, different
// prefix per entity kind so IDs are self-describing in logs/URLs).
func newBIObjectID(prefix string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate %s id: %w", prefix, err)
	}
	return prefix + "_" + hex.EncodeToString(b[:]), nil
}

func (a *App) ensureBITables(ctx context.Context) error {
	for name, query := range map[string]string{
		"saved_query.ensure_table": savedQueriesEnsureTableSQL,
		"chart.ensure_table":       chartsEnsureTableSQL,
		"dashboard.ensure_table":   dashboardsEnsureTableSQL,
	} {
		if _, err := a.execConn(ctx, a.localTinySQLConn(), name, query); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "already exists") {
				continue
			}
			return fmt.Errorf("ensure BI metadata tables: %w", err)
		}
	}
	return nil
}

// ─── Saved Queries ──────────────────────────────────────────────────────────

func (a *App) saveSavedQuery(ctx context.Context, q SavedQuery) (SavedQuery, error) {
	if strings.TrimSpace(q.Name) == "" {
		return SavedQuery{}, fmt.Errorf("name is required")
	}
	if strings.TrimSpace(q.SQLText) == "" {
		return SavedQuery{}, fmt.Errorf("sql_text is required")
	}
	if err := a.ensureBITables(ctx); err != nil {
		return SavedQuery{}, err
	}
	now := time.Now().UTC()
	if strings.TrimSpace(q.ID) == "" {
		id, err := newBIObjectID("sq")
		if err != nil {
			return SavedQuery{}, err
		}
		q.ID = id
		q.CreatedAt = now
	} else if existing, found, err := a.loadSavedQuery(ctx, q.ID); err != nil {
		return SavedQuery{}, err
	} else if found {
		q.CreatedAt = existing.CreatedAt
	} else {
		q.CreatedAt = now
	}
	q.UpdatedAt = now

	conn := a.localTinySQLConn()
	if _, err := a.execConn(ctx, conn, "saved_query.delete", savedQueriesDeleteSQL, q.ID); err != nil {
		return SavedQuery{}, err
	}
	_, err := a.execConn(ctx, conn, "saved_query.insert", savedQueriesInsertSQL,
		q.ID, q.Name, q.Description, q.SQLText, q.ConnectionID, q.Owner,
		q.CreatedAt.Format(time.RFC3339Nano), q.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return SavedQuery{}, err
	}
	return q, nil
}

func (a *App) loadSavedQuery(ctx context.Context, id string) (SavedQuery, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return SavedQuery{}, false, nil
	}
	if err := a.ensureBITables(ctx); err != nil {
		return SavedQuery{}, false, err
	}
	rows, err := a.queryConn(ctx, a.localTinySQLConn(), "saved_query.load", savedQueriesSelectByIDSQL, id)
	if err != nil {
		return SavedQuery{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return SavedQuery{}, false, nil
	}
	q, err := scanSavedQuery(rows.Scan)
	if err != nil {
		return SavedQuery{}, false, err
	}
	return q, true, nil
}

func (a *App) listSavedQueries(ctx context.Context) ([]SavedQuery, error) {
	if err := a.ensureBITables(ctx); err != nil {
		return nil, err
	}
	rows, err := a.queryConn(ctx, a.localTinySQLConn(), "saved_query.list", savedQueriesSelectAllSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SavedQuery
	for rows.Next() {
		q, err := scanSavedQuery(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

func (a *App) deleteSavedQuery(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("id is required")
	}
	if err := a.ensureBITables(ctx); err != nil {
		return err
	}
	_, err := a.execConn(ctx, a.localTinySQLConn(), "saved_query.delete", savedQueriesDeleteSQL, id)
	return err
}

func scanSavedQuery(scan func(dest ...any) error) (SavedQuery, error) {
	var q SavedQuery
	var createdAt, updatedAt string
	if err := scan(&q.ID, &q.Name, &q.Description, &q.SQLText, &q.ConnectionID, &q.Owner, &createdAt, &updatedAt); err != nil {
		return SavedQuery{}, err
	}
	q.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	q.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return q, nil
}

// ─── Charts ─────────────────────────────────────────────────────────────────

func (a *App) saveChart(ctx context.Context, c Chart) (Chart, error) {
	if strings.TrimSpace(c.Name) == "" {
		return Chart{}, fmt.Errorf("name is required")
	}
	if strings.TrimSpace(c.SQLText) == "" {
		return Chart{}, fmt.Errorf("sql_text is required")
	}
	if len(c.VegaLiteSpec) == 0 {
		return Chart{}, fmt.Errorf("vega_lite_spec is required")
	}
	if !json.Valid(c.VegaLiteSpec) {
		return Chart{}, fmt.Errorf("vega_lite_spec must be valid JSON")
	}
	if err := a.ensureBITables(ctx); err != nil {
		return Chart{}, err
	}
	now := time.Now().UTC()
	if strings.TrimSpace(c.ID) == "" {
		id, err := newBIObjectID("chart")
		if err != nil {
			return Chart{}, err
		}
		c.ID = id
		c.CreatedAt = now
	} else if existing, found, err := a.loadChart(ctx, c.ID); err != nil {
		return Chart{}, err
	} else if found {
		c.CreatedAt = existing.CreatedAt
	} else {
		c.CreatedAt = now
	}
	c.UpdatedAt = now

	conn := a.localTinySQLConn()
	if _, err := a.execConn(ctx, conn, "chart.delete", chartsDeleteSQL, c.ID); err != nil {
		return Chart{}, err
	}
	_, err := a.execConn(ctx, conn, "chart.insert", chartsInsertSQL,
		c.ID, c.Name, c.SavedQueryID, c.SQLText, string(c.VegaLiteSpec), c.Owner,
		c.CreatedAt.Format(time.RFC3339Nano), c.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return Chart{}, err
	}
	return c, nil
}

func (a *App) loadChart(ctx context.Context, id string) (Chart, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Chart{}, false, nil
	}
	if err := a.ensureBITables(ctx); err != nil {
		return Chart{}, false, err
	}
	rows, err := a.queryConn(ctx, a.localTinySQLConn(), "chart.load", chartsSelectByIDSQL, id)
	if err != nil {
		return Chart{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Chart{}, false, nil
	}
	c, err := scanChart(rows.Scan)
	if err != nil {
		return Chart{}, false, err
	}
	return c, true, nil
}

func (a *App) listCharts(ctx context.Context) ([]Chart, error) {
	if err := a.ensureBITables(ctx); err != nil {
		return nil, err
	}
	rows, err := a.queryConn(ctx, a.localTinySQLConn(), "chart.list", chartsSelectAllSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chart
	for rows.Next() {
		c, err := scanChart(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

func (a *App) deleteChart(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("id is required")
	}
	if err := a.ensureBITables(ctx); err != nil {
		return err
	}
	_, err := a.execConn(ctx, a.localTinySQLConn(), "chart.delete", chartsDeleteSQL, id)
	return err
}

func scanChart(scan func(dest ...any) error) (Chart, error) {
	var c Chart
	var spec, createdAt, updatedAt string
	if err := scan(&c.ID, &c.Name, &c.SavedQueryID, &c.SQLText, &spec, &c.Owner, &createdAt, &updatedAt); err != nil {
		return Chart{}, err
	}
	c.VegaLiteSpec = json.RawMessage(spec)
	c.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	c.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return c, nil
}

// ─── Dashboards ─────────────────────────────────────────────────────────────

func (a *App) saveDashboard(ctx context.Context, d Dashboard) (Dashboard, error) {
	if strings.TrimSpace(d.Name) == "" {
		return Dashboard{}, fmt.Errorf("name is required")
	}
	if err := a.ensureBITables(ctx); err != nil {
		return Dashboard{}, err
	}
	if d.Layout == nil {
		d.Layout = []DashboardTile{}
	}
	layoutJSON, err := json.Marshal(d.Layout)
	if err != nil {
		return Dashboard{}, fmt.Errorf("encode layout: %w", err)
	}
	now := time.Now().UTC()
	if strings.TrimSpace(d.ID) == "" {
		id, err := newBIObjectID("dash")
		if err != nil {
			return Dashboard{}, err
		}
		d.ID = id
		d.CreatedAt = now
	} else if existing, found, err := a.loadDashboard(ctx, d.ID); err != nil {
		return Dashboard{}, err
	} else if found {
		d.CreatedAt = existing.CreatedAt
	} else {
		d.CreatedAt = now
	}
	d.UpdatedAt = now

	conn := a.localTinySQLConn()
	if _, err := a.execConn(ctx, conn, "dashboard.delete", dashboardsDeleteSQL, d.ID); err != nil {
		return Dashboard{}, err
	}
	_, err = a.execConn(ctx, conn, "dashboard.insert", dashboardsInsertSQL,
		d.ID, d.Name, d.Description, string(layoutJSON), d.Owner,
		d.CreatedAt.Format(time.RFC3339Nano), d.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return Dashboard{}, err
	}
	return d, nil
}

func (a *App) loadDashboard(ctx context.Context, id string) (Dashboard, bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Dashboard{}, false, nil
	}
	if err := a.ensureBITables(ctx); err != nil {
		return Dashboard{}, false, err
	}
	rows, err := a.queryConn(ctx, a.localTinySQLConn(), "dashboard.load", dashboardsSelectByIDSQL, id)
	if err != nil {
		return Dashboard{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Dashboard{}, false, nil
	}
	d, err := scanDashboard(rows.Scan)
	if err != nil {
		return Dashboard{}, false, err
	}
	return d, true, nil
}

func (a *App) listDashboards(ctx context.Context) ([]Dashboard, error) {
	if err := a.ensureBITables(ctx); err != nil {
		return nil, err
	}
	rows, err := a.queryConn(ctx, a.localTinySQLConn(), "dashboard.list", dashboardsSelectAllSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Dashboard
	for rows.Next() {
		d, err := scanDashboard(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

func (a *App) deleteDashboard(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("id is required")
	}
	if err := a.ensureBITables(ctx); err != nil {
		return err
	}
	_, err := a.execConn(ctx, a.localTinySQLConn(), "dashboard.delete", dashboardsDeleteSQL, id)
	return err
}

func scanDashboard(scan func(dest ...any) error) (Dashboard, error) {
	var d Dashboard
	var layoutJSON, createdAt, updatedAt string
	if err := scan(&d.ID, &d.Name, &d.Description, &layoutJSON, &d.Owner, &createdAt, &updatedAt); err != nil {
		return Dashboard{}, err
	}
	if strings.TrimSpace(layoutJSON) != "" {
		if err := json.Unmarshal([]byte(layoutJSON), &d.Layout); err != nil {
			return Dashboard{}, fmt.Errorf("decode layout for dashboard %q: %w", d.ID, err)
		}
	}
	d.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	d.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return d, nil
}
