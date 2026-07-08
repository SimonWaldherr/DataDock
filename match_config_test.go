package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testMatchConfig(name string) MatchConfig {
	return MatchConfig{
		Name:            name,
		SourceConnID:    "default",
		TargetConnID:    "default",
		SourceTable:     "erp1_customers",
		TargetTable:     "erp2_kunden",
		SourceKeyColumn: "id",
		TargetKeyColumn: "kdnr",
		Fields: []MatchFieldSpec{
			{SourceColumn: "name", TargetColumn: "firma", Method: "token_set", Weight: 2},
			{SourceColumn: "address", TargetColumn: "anschrift", Method: "address", Weight: 1, Group: "addr"},
		},
		AutoThreshold:   0.9,
		ReviewThreshold: 0.6,
		SaveConnID:      "default",
		SaveTable:       "erp_matches",
		SaveScope:       "auto_and_review",
	}
}

func TestMatchConfigSaveLoadListDelete(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	if _, ok, err := app.loadMatchConfig(ctx, "does-not-exist"); err != nil || ok {
		t.Fatalf("loadMatchConfig for a missing name: ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	cfg := testMatchConfig("erp1-vs-erp2")
	if err := app.saveMatchConfig(ctx, cfg); err != nil {
		t.Fatalf("saveMatchConfig: %v", err)
	}

	loaded, ok, err := app.loadMatchConfig(ctx, "erp1-vs-erp2")
	if err != nil || !ok {
		t.Fatalf("loadMatchConfig: ok=%v err=%v", ok, err)
	}
	if loaded.SourceTable != cfg.SourceTable || loaded.TargetTable != cfg.TargetTable {
		t.Errorf("loaded config tables = %s/%s, want %s/%s", loaded.SourceTable, loaded.TargetTable, cfg.SourceTable, cfg.TargetTable)
	}
	if len(loaded.Fields) != 2 || loaded.Fields[1].Group != "addr" {
		t.Errorf("loaded config did not round-trip field rules correctly: %+v", loaded.Fields)
	}
	if loaded.UpdatedAt.IsZero() {
		t.Error("expected UpdatedAt to be stamped on save")
	}

	// Saving again under the same name overwrites, not duplicates.
	cfg.TargetTable = "erp2_kunden_v2"
	if err := app.saveMatchConfig(ctx, cfg); err != nil {
		t.Fatalf("saveMatchConfig (overwrite): %v", err)
	}
	configs, err := app.listMatchConfigs(ctx)
	if err != nil {
		t.Fatalf("listMatchConfigs: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected exactly 1 configuration after overwriting, got %d", len(configs))
	}
	if configs[0].TargetTable != "erp2_kunden_v2" {
		t.Errorf("expected the overwrite to take effect, got TargetTable=%q", configs[0].TargetTable)
	}

	if err := app.deleteMatchConfig(ctx, "erp1-vs-erp2"); err != nil {
		t.Fatalf("deleteMatchConfig: %v", err)
	}
	if _, ok, err := app.loadMatchConfig(ctx, "erp1-vs-erp2"); err != nil || ok {
		t.Errorf("expected the configuration to be gone after delete, got ok=%v err=%v", ok, err)
	}
}

// TestMatchHandlerLoadsSavedConfig covers the actual "make it simple" path:
// save a configuration once via mode=save_config, then GET /match?config=X
// must repopulate the whole wizard from it in one request.
func TestMatchHandlerLoadsSavedConfig(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	if _, err := app.execConn(ctx, app.localTinySQLConn(), "test.setup", `CREATE TABLE erp1_customers (id INT, name TEXT, address TEXT)`); err != nil {
		t.Fatalf("create source table: %v", err)
	}
	if _, err := app.execConn(ctx, app.localTinySQLConn(), "test.setup", `CREATE TABLE erp2_kunden (kdnr INT, firma TEXT, anschrift TEXT)`); err != nil {
		t.Fatalf("create target table: %v", err)
	}

	cfg := testMatchConfig("erp1-vs-erp2")
	if err := app.saveMatchConfig(ctx, cfg); err != nil {
		t.Fatalf("saveMatchConfig: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/match?config=erp1-vs-erp2", nil)
	rec := httptest.NewRecorder()
	app.matchHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`value="erp1_customers" selected`, // source table select, rendered server-side
		`value="erp2_kunden" selected`,    // target table select, rendered server-side
		`source:"name"`,                   // field row data embedded for the client-side row builder
		`group:"addr"`,                    // the saved Group name round-tripped into that field row
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected loaded page to contain %q; body:\n%s", want, body)
		}
	}
}
