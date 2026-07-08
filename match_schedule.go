package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// matchSchedulesTable stores at most one recurring schedule per saved
// MatchConfig — see MatchSchedule.
const matchSchedulesTable = "__datadock_match_schedules"

// errMatchScheduleSkippedMaintenance marks a scheduled run that didn't
// execute because maintenance mode was active, so recordMatchScheduleRun
// can record it as "skipped" rather than "error".
var errMatchScheduleSkippedMaintenance = errors.New("skipped: maintenance mode is active")

// MatchSchedule attaches a standard 5-field cron expression to a saved
// MatchConfig, so it re-runs (and appends to its configured crosswalk
// table) on its own without anyone opening the Matching wizard. tinySQL's
// built-in job scheduler only executes raw SQL text, so a match run —
// which reads from arbitrary connections and scores rows in Go — needs its
// own scheduler; see App.matchCron.
type MatchSchedule struct {
	ConfigName string
	CronExpr   string
	Enabled    bool
	LastRunAt  string // RFC3339; empty if it has never run
	LastStatus string // "ok", "skipped: ...", or "error: ..."
	LastRows   int
}

// startMatchScheduler creates the cron runner (once) and registers every
// enabled schedule found in the database. Called once at startup; safe to
// call again (e.g. in tests) since it only (re)creates the Cron instance
// the first time.
func (a *App) startMatchScheduler(ctx context.Context) error {
	a.matchCronMu.Lock()
	if a.matchCron == nil {
		a.matchCron = cron.New()
		a.matchCronEntries = make(map[string]cron.EntryID)
		a.matchCron.Start()
	}
	a.matchCronMu.Unlock()

	schedules, err := a.listMatchSchedules(ctx)
	if err != nil {
		return fmt.Errorf("load match schedules: %w", err)
	}
	for _, sched := range schedules {
		if !sched.Enabled {
			continue
		}
		if err := a.registerMatchScheduleEntry(sched); err != nil {
			log.Printf("match schedule %q: %v", sched.ConfigName, err)
		}
	}
	return nil
}

// registerMatchScheduleEntry (re)registers sched with the running cron
// instance, replacing any existing entry for the same configuration first
// so saving a changed cron expression or disabling a schedule takes effect
// immediately, without a server restart.
func (a *App) registerMatchScheduleEntry(sched MatchSchedule) error {
	a.matchCronMu.Lock()
	defer a.matchCronMu.Unlock()
	if a.matchCron == nil {
		return fmt.Errorf("match scheduler not started")
	}
	if id, ok := a.matchCronEntries[sched.ConfigName]; ok {
		a.matchCron.Remove(id)
		delete(a.matchCronEntries, sched.ConfigName)
	}
	if !sched.Enabled {
		return nil
	}
	configName := sched.ConfigName
	id, err := a.matchCron.AddFunc(sched.CronExpr, func() { a.runScheduledMatch(configName) })
	if err != nil {
		return fmt.Errorf("invalid cron expression %q: %w", sched.CronExpr, err)
	}
	a.matchCronEntries[configName] = id
	return nil
}

// unregisterMatchScheduleEntry removes configName's cron entry, if any,
// without touching the database — used when deleting a schedule/config.
func (a *App) unregisterMatchScheduleEntry(configName string) {
	a.matchCronMu.Lock()
	defer a.matchCronMu.Unlock()
	if a.matchCron == nil {
		return
	}
	if id, ok := a.matchCronEntries[configName]; ok {
		a.matchCron.Remove(id)
		delete(a.matchCronEntries, configName)
	}
}

// runScheduledMatch is the cron callback: it loads the named configuration,
// runs it exactly as a manual "Run & Save as Table" click would (using the
// configuration's own save connection/table/scope), and records the
// outcome back onto the schedule row so the wizard can show "last ran at
// ... (123 rows)" or the last error. It runs with no user session, so — the
// same as any other background/scheduled operation — only connections
// shared with every session are reachable, not one private to whoever last
// used the wizard.
func (a *App) runScheduledMatch(configName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// A scheduled run ultimately writes to the save table, so it must
	// respect maintenance mode exactly like a manual "Run & Save as Table"
	// click would — otherwise turning maintenance mode on wouldn't
	// actually stop every write server-wide, just the ones a user could
	// see. Skipping (rather than erroring) keeps the schedule enabled so
	// it resumes on its own once maintenance mode is turned off again.
	if a.currentReadOnlyMode() {
		a.recordMatchScheduleRun(ctx, configName, 0, errMatchScheduleSkippedMaintenance)
		return
	}

	cfg, ok, err := a.loadMatchConfig(ctx, configName)
	if err != nil {
		a.recordMatchScheduleRun(ctx, configName, 0, fmt.Errorf("load configuration: %w", err))
		return
	}
	if !ok {
		a.recordMatchScheduleRun(ctx, configName, 0, fmt.Errorf("configuration %q no longer exists", configName))
		return
	}

	req := MatchRequest{
		SourceConnID:    cfg.SourceConnID,
		TargetConnID:    cfg.TargetConnID,
		SourceTable:     cfg.SourceTable,
		TargetTable:     cfg.TargetTable,
		SourceKeyColumn: cfg.SourceKeyColumn,
		TargetKeyColumn: cfg.TargetKeyColumn,
		Fields:          cfg.Fields,
		AutoThreshold:   cfg.AutoThreshold,
		ReviewThreshold: cfg.ReviewThreshold,
		NoBlocking:      cfg.NoBlocking,
	}
	summary, err := a.runMatch(ctx, "", req)
	if err != nil {
		a.recordMatchScheduleRun(ctx, configName, 0, fmt.Errorf("run match: %w", err))
		return
	}
	saveConn := a.conns.GetFor("", cfg.SaveConnID)
	if saveConn == nil {
		a.recordMatchScheduleRun(ctx, configName, 0, fmt.Errorf("save connection %q not found or not shared", cfg.SaveConnID))
		return
	}
	filtered := filterMatchResultsByScope(summary.Results, cfg.SaveScope)
	written, err := a.saveMatchResults(ctx, saveConn, cfg.SaveTable, filtered, time.Now())
	a.recordMatchScheduleRun(ctx, configName, written, err)
}

// recordMatchScheduleRun updates the schedule row's last-run status. It's a
// plain UPDATE, not an upsert, deliberately: the only caller of
// runScheduledMatch is the cron closure registered in
// registerMatchScheduleEntry, which is only ever created from a schedule
// saveMatchSchedule already persisted, so a matching row is always present
// — except in the harmless race where the schedule was deleted while a run
// was in flight, where there being nothing left to update is correct.
func (a *App) recordMatchScheduleRun(ctx context.Context, configName string, rows int, runErr error) {
	status := "ok"
	switch {
	case errors.Is(runErr, errMatchScheduleSkippedMaintenance):
		status = runErr.Error()
	case runErr != nil:
		status = "error: " + runErr.Error()
		log.Printf("scheduled match %q failed: %v", configName, runErr)
	}
	if err := a.ensureMatchSchedulesTable(ctx); err != nil {
		return
	}
	_, _ = a.execConn(ctx, a.localTinySQLConn(), "match_schedule.record_run", matchSchedulesRecordRunSQL,
		time.Now().UTC().Format(time.RFC3339), status, rows, configName)
}

// saveMatchSchedule validates sched's cron expression, persists it, and
// (re)registers it with the running scheduler so the change is live
// immediately.
func (a *App) saveMatchSchedule(ctx context.Context, sched MatchSchedule) error {
	name := strings.TrimSpace(sched.ConfigName)
	if name == "" {
		return fmt.Errorf("configuration name is required")
	}
	sched.ConfigName = name
	sched.CronExpr = strings.TrimSpace(sched.CronExpr)
	if sched.CronExpr == "" {
		return fmt.Errorf("cron expression is required")
	}
	if _, err := cron.ParseStandard(sched.CronExpr); err != nil {
		return fmt.Errorf("invalid cron expression %q: %w", sched.CronExpr, err)
	}
	if err := a.ensureMatchSchedulesTable(ctx); err != nil {
		return err
	}

	existing, hadExisting, err := a.loadMatchSchedule(ctx, name)
	if err != nil {
		return err
	}
	if hadExisting {
		sched.LastRunAt = existing.LastRunAt
		sched.LastStatus = existing.LastStatus
		sched.LastRows = existing.LastRows
	}

	conn := a.localTinySQLConn()
	if _, err := a.execConn(ctx, conn, "match_schedule.delete", matchSchedulesDeleteSQL, name); err != nil {
		return err
	}
	enabledInt := 0
	if sched.Enabled {
		enabledInt = 1
	}
	if _, err := a.execConn(ctx, conn, "match_schedule.insert", matchSchedulesInsertSQL,
		name, sched.CronExpr, enabledInt, sched.LastRunAt, sched.LastStatus, sched.LastRows); err != nil {
		return err
	}
	return a.registerMatchScheduleEntry(sched)
}

// loadMatchSchedule looks up configName's schedule. ok is false (nil error)
// when it has no schedule.
func (a *App) loadMatchSchedule(ctx context.Context, configName string) (MatchSchedule, bool, error) {
	configName = strings.TrimSpace(configName)
	if configName == "" {
		return MatchSchedule{}, false, nil
	}
	if err := a.ensureMatchSchedulesTable(ctx); err != nil {
		return MatchSchedule{}, false, err
	}
	rows, err := a.queryConn(ctx, a.localTinySQLConn(), "match_schedule.load", matchSchedulesSelectOneSQL, configName)
	if err != nil {
		return MatchSchedule{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return MatchSchedule{}, false, nil
	}
	sched, err := scanMatchSchedule(rows)
	if err != nil {
		return MatchSchedule{}, false, err
	}
	return sched, true, nil
}

// listMatchSchedules returns every schedule, sorted by configuration name.
func (a *App) listMatchSchedules(ctx context.Context) ([]MatchSchedule, error) {
	if err := a.ensureMatchSchedulesTable(ctx); err != nil {
		return nil, err
	}
	rows, err := a.queryConn(ctx, a.localTinySQLConn(), "match_schedule.list", matchSchedulesSelectAllSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var schedules []MatchSchedule
	for rows.Next() {
		sched, err := scanMatchSchedule(rows)
		if err != nil {
			return nil, err
		}
		schedules = append(schedules, sched)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(schedules, func(i, j int) bool {
		return strings.ToLower(schedules[i].ConfigName) < strings.ToLower(schedules[j].ConfigName)
	})
	return schedules, nil
}

// deleteMatchSchedule removes configName's schedule, if any, from both the
// database and the running scheduler. It is not an error for no schedule to
// exist — callers (e.g. deleting a MatchConfig that was never scheduled)
// call this unconditionally.
func (a *App) deleteMatchSchedule(ctx context.Context, configName string) error {
	configName = strings.TrimSpace(configName)
	if configName == "" {
		return nil
	}
	a.unregisterMatchScheduleEntry(configName)
	if err := a.ensureMatchSchedulesTable(ctx); err != nil {
		return err
	}
	_, err := a.execConn(ctx, a.localTinySQLConn(), "match_schedule.delete", matchSchedulesDeleteSQL, configName)
	return err
}

func (a *App) ensureMatchSchedulesTable(ctx context.Context) error {
	_, err := a.execConn(ctx, a.localTinySQLConn(), "match_schedule.ensure_table", matchSchedulesEnsureTableSQL)
	if err == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return nil
	}
	return fmt.Errorf("ensure match schedules table: %w", err)
}

// rowScanner is the subset of *sql.Rows scanMatchSchedule needs, so it works
// identically for a single-row lookup and a multi-row list query.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanMatchSchedule(row rowScanner) (MatchSchedule, error) {
	var sched MatchSchedule
	var enabledInt, lastRows int
	var lastRunAt, lastStatus string
	if err := row.Scan(&sched.ConfigName, &sched.CronExpr, &enabledInt, &lastRunAt, &lastStatus, &lastRows); err != nil {
		return MatchSchedule{}, err
	}
	sched.Enabled = enabledInt != 0
	sched.LastRunAt = lastRunAt
	sched.LastStatus = lastStatus
	sched.LastRows = lastRows
	return sched, nil
}
