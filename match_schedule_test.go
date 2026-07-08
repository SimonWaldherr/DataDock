package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMatchScheduleSaveLoadDeleteAndCronRegistration(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	if err := app.startMatchScheduler(ctx); err != nil {
		t.Fatalf("startMatchScheduler: %v", err)
	}

	if _, ok, err := app.loadMatchSchedule(ctx, "erp1-vs-erp2"); err != nil || ok {
		t.Fatalf("loadMatchSchedule for a config with no schedule: ok=%v err=%v", ok, err)
	}

	sched := MatchSchedule{ConfigName: "erp1-vs-erp2", CronExpr: "*/15 * * * *", Enabled: true}
	if err := app.saveMatchSchedule(ctx, sched); err != nil {
		t.Fatalf("saveMatchSchedule: %v", err)
	}

	app.matchCronMu.Lock()
	_, registered := app.matchCronEntries["erp1-vs-erp2"]
	app.matchCronMu.Unlock()
	if !registered {
		t.Error("expected an enabled schedule to be registered with the cron runner")
	}

	loaded, ok, err := app.loadMatchSchedule(ctx, "erp1-vs-erp2")
	if err != nil || !ok {
		t.Fatalf("loadMatchSchedule: ok=%v err=%v", ok, err)
	}
	if loaded.CronExpr != "*/15 * * * *" || !loaded.Enabled {
		t.Errorf("loaded schedule = %+v, want CronExpr=*/15 * * * * Enabled=true", loaded)
	}

	// An invalid cron expression must be rejected before touching the DB or
	// the cron runner.
	bad := MatchSchedule{ConfigName: "erp1-vs-erp2", CronExpr: "not a cron expression", Enabled: true}
	if err := app.saveMatchSchedule(ctx, bad); err == nil {
		t.Error("expected an error for an invalid cron expression")
	}
	// ...and must not have clobbered the previously-valid schedule.
	if loaded, ok, err := app.loadMatchSchedule(ctx, "erp1-vs-erp2"); err != nil || !ok || loaded.CronExpr != "*/15 * * * *" {
		t.Errorf("a rejected save must not modify the stored schedule, got %+v ok=%v err=%v", loaded, ok, err)
	}

	// Disabling must unregister the cron entry immediately, without a
	// restart.
	sched.Enabled = false
	if err := app.saveMatchSchedule(ctx, sched); err != nil {
		t.Fatalf("saveMatchSchedule (disable): %v", err)
	}
	app.matchCronMu.Lock()
	_, stillRegistered := app.matchCronEntries["erp1-vs-erp2"]
	app.matchCronMu.Unlock()
	if stillRegistered {
		t.Error("expected disabling a schedule to remove its cron entry")
	}

	if err := app.deleteMatchSchedule(ctx, "erp1-vs-erp2"); err != nil {
		t.Fatalf("deleteMatchSchedule: %v", err)
	}
	if _, ok, err := app.loadMatchSchedule(ctx, "erp1-vs-erp2"); err != nil || ok {
		t.Errorf("expected the schedule to be gone after delete, got ok=%v err=%v", ok, err)
	}
	// Deleting a schedule that doesn't exist must not error — the
	// unconditional call from deleteMatchConfigHandler relies on this.
	if err := app.deleteMatchSchedule(ctx, "never-existed"); err != nil {
		t.Errorf("deleteMatchSchedule on a nonexistent schedule should be a no-op, got: %v", err)
	}
}

// TestRunScheduledMatchAppendsResultsAndRecordsStatus exercises the actual
// cron callback end-to-end: it must run the saved configuration exactly
// like a manual "Run & Save as Table" click and record the outcome.
func TestRunScheduledMatchAppendsResultsAndRecordsStatus(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	if err := app.startMatchScheduler(ctx); err != nil {
		t.Fatalf("startMatchScheduler: %v", err)
	}
	exec := func(sql string) {
		t.Helper()
		if _, err := app.execConn(ctx, app.localTinySQLConn(), "test.setup", sql); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`CREATE TABLE erp1_customers (id INT, name TEXT, address TEXT)`)
	exec(`INSERT INTO erp1_customers (id, name, address) VALUES (1, 'ACME Trading GmbH', 'Hauptstrasse 12')`)
	exec(`CREATE TABLE erp2_kunden (kdnr INT, firma TEXT, anschrift TEXT)`)
	exec(`INSERT INTO erp2_kunden (kdnr, firma, anschrift) VALUES (101, 'acme trading', 'Hauptstr. 12')`)

	cfg := testMatchConfig("erp1-vs-erp2")
	cfg.Fields = []MatchFieldSpec{
		{SourceColumn: "name", TargetColumn: "firma", Method: "token_set", Weight: 2},
		{SourceColumn: "address", TargetColumn: "anschrift", Method: "address", Weight: 1},
	}
	cfg.ReviewThreshold = 0.5
	if err := app.saveMatchConfig(ctx, cfg); err != nil {
		t.Fatalf("saveMatchConfig: %v", err)
	}
	// In production runScheduledMatch is only ever invoked as a cron
	// callback for a schedule saveMatchSchedule already persisted — so its
	// status-recording UPDATE always has a row to match. Mirror that here
	// with Enabled=false so this test controls exactly when it runs,
	// instead of racing the real cron ticker.
	if err := app.saveMatchSchedule(ctx, MatchSchedule{ConfigName: "erp1-vs-erp2", CronExpr: "0 * * * *", Enabled: false}); err != nil {
		t.Fatalf("saveMatchSchedule: %v", err)
	}

	app.runScheduledMatch("erp1-vs-erp2")

	rows, err := app.execConn(ctx, app.localTinySQLConn(), "test.verify", `SELECT * FROM erp_matches LIMIT 0`)
	if err != nil {
		t.Fatalf("expected runScheduledMatch to have created the crosswalk table: %v", err)
	}
	_ = rows

	sched, ok, err := app.loadMatchSchedule(ctx, "erp1-vs-erp2")
	if err != nil || !ok {
		t.Fatalf("expected a schedule status row to exist after a run (created on first record), ok=%v err=%v", ok, err)
	}
	if sched.LastStatus != "ok" {
		t.Errorf("LastStatus = %q, want %q", sched.LastStatus, "ok")
	}
	if sched.LastRows != 1 {
		t.Errorf("LastRows = %d, want 1", sched.LastRows)
	}
	if sched.LastRunAt == "" {
		t.Error("expected LastRunAt to be stamped")
	}
	if _, err := time.Parse(time.RFC3339, sched.LastRunAt); err != nil {
		t.Errorf("LastRunAt %q is not RFC3339: %v", sched.LastRunAt, err)
	}
}

// TestRunScheduledMatchRecordsFailureForMissingConfig guards against a
// schedule outliving its configuration (e.g. the config was deleted but
// the schedule delete failed) silently doing nothing forever.
func TestRunScheduledMatchRecordsFailureForMissingConfig(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	// Seed a schedule row directly (bypassing saveMatchSchedule, which
	// would refuse this since there's no matching config) to simulate the
	// orphaned-schedule scenario.
	if err := app.ensureMatchSchedulesTable(ctx); err != nil {
		t.Fatalf("ensureMatchSchedulesTable: %v", err)
	}
	if _, err := app.execConn(ctx, app.localTinySQLConn(), "test.setup",
		"INSERT INTO "+matchSchedulesTable+" (config_name, cron_expr, enabled, last_run_at, last_status, last_rows) VALUES (?, ?, ?, ?, ?, ?)",
		"ghost-config", "0 * * * *", 1, "", "", 0); err != nil {
		t.Fatalf("seed orphaned schedule: %v", err)
	}

	app.runScheduledMatch("ghost-config")

	sched, ok, err := app.loadMatchSchedule(ctx, "ghost-config")
	if err != nil || !ok {
		t.Fatalf("loadMatchSchedule: ok=%v err=%v", ok, err)
	}
	if !strings.HasPrefix(sched.LastStatus, "error:") {
		t.Errorf("LastStatus = %q, want it to start with \"error:\"", sched.LastStatus)
	}
}

// TestRunScheduledMatchSkipsDuringMaintenanceMode guards a real gap:
// maintenance mode is supposed to block every write server-wide, but a
// scheduled match's save step was never checking it, so a recurring match
// would keep writing during a maintenance window.
func TestRunScheduledMatchSkipsDuringMaintenanceMode(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	if err := app.startMatchScheduler(ctx); err != nil {
		t.Fatalf("startMatchScheduler: %v", err)
	}
	exec := func(sql string) {
		t.Helper()
		if _, err := app.execConn(ctx, app.localTinySQLConn(), "test.setup", sql); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`CREATE TABLE erp1_customers (id INT, name TEXT, address TEXT)`)
	exec(`INSERT INTO erp1_customers (id, name, address) VALUES (1, 'ACME Trading GmbH', 'Hauptstrasse 12')`)
	exec(`CREATE TABLE erp2_kunden (kdnr INT, firma TEXT, anschrift TEXT)`)
	exec(`INSERT INTO erp2_kunden (kdnr, firma, anschrift) VALUES (101, 'acme trading', 'Hauptstr. 12')`)

	cfg := testMatchConfig("erp1-vs-erp2")
	cfg.Fields = []MatchFieldSpec{{SourceColumn: "name", TargetColumn: "firma", Method: "token_set", Weight: 1}}
	if err := app.saveMatchConfig(ctx, cfg); err != nil {
		t.Fatalf("saveMatchConfig: %v", err)
	}
	if err := app.saveMatchSchedule(ctx, MatchSchedule{ConfigName: "erp1-vs-erp2", CronExpr: "0 * * * *", Enabled: false}); err != nil {
		t.Fatalf("saveMatchSchedule: %v", err)
	}

	settings := app.currentRuntimeSettings()
	settings.ReadOnlyMode = true
	if err := app.applyRuntimeSettings(settings); err != nil {
		t.Fatalf("applyRuntimeSettings: %v", err)
	}

	app.runScheduledMatch("erp1-vs-erp2")

	if _, err := app.execConn(ctx, app.localTinySQLConn(), "test.verify", `SELECT * FROM erp_matches LIMIT 0`); err == nil {
		t.Error("expected the crosswalk table NOT to be created while maintenance mode is active")
	}
	sched, ok, err := app.loadMatchSchedule(ctx, "erp1-vs-erp2")
	if err != nil || !ok {
		t.Fatalf("loadMatchSchedule: ok=%v err=%v", ok, err)
	}
	if !strings.HasPrefix(sched.LastStatus, "skipped:") {
		t.Errorf("LastStatus = %q, want it to start with \"skipped:\"", sched.LastStatus)
	}
}
