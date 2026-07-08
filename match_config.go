package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// matchConfigsTable stores named, reusable Matching wizard setups so a
// working configuration only has to be built once — see MatchConfig.
const matchConfigsTable = "__datadock_match_configs"

// MatchConfig is a complete, named Matching wizard setup: which two
// tables/connections to compare, how to compare them, and where to save
// results. Saving one under a name turns "configure everything again" into
// "pick it from a dropdown", and is also what a Match Schedule (see
// match_schedule.go) runs on a timer.
type MatchConfig struct {
	Name            string
	SourceConnID    string
	TargetConnID    string
	SourceTable     string
	TargetTable     string
	SourceKeyColumn string
	TargetKeyColumn string
	Fields          []MatchFieldSpec
	AutoThreshold   float64
	ReviewThreshold float64
	NoBlocking      bool
	SaveConnID      string
	SaveTable       string
	SaveScope       string
	UpdatedAt       time.Time
}

// saveMatchConfig creates or overwrites (by name) a saved configuration.
// Configurations live in the local tinySQL database regardless of which
// connections they reference, exactly like runtime settings and jobs.
func (a *App) saveMatchConfig(ctx context.Context, cfg MatchConfig) error {
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		return fmt.Errorf("configuration name is required")
	}
	if err := a.ensureMatchConfigsTable(ctx); err != nil {
		return err
	}
	cfg.Name = name
	cfg.UpdatedAt = time.Now()
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}
	conn := a.localTinySQLConn()
	if _, err := a.execConn(ctx, conn, "match_config.delete", matchConfigsDeleteSQL, name); err != nil {
		return err
	}
	_, err = a.execConn(ctx, conn, "match_config.insert", matchConfigsInsertSQL,
		name, string(encoded), cfg.UpdatedAt.UTC().Format(time.RFC3339))
	return err
}

// loadMatchConfig looks up a saved configuration by name. ok is false (with
// a nil error) when no configuration exists under that name.
func (a *App) loadMatchConfig(ctx context.Context, name string) (MatchConfig, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return MatchConfig{}, false, nil
	}
	if err := a.ensureMatchConfigsTable(ctx); err != nil {
		return MatchConfig{}, false, err
	}
	rows, err := a.queryConn(ctx, a.localTinySQLConn(), "match_config.load", matchConfigsSelectOneSQL, name)
	if err != nil {
		return MatchConfig{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return MatchConfig{}, false, nil
	}
	var raw string
	if err := rows.Scan(&raw); err != nil {
		return MatchConfig{}, false, err
	}
	var cfg MatchConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return MatchConfig{}, false, fmt.Errorf("decode configuration %q: %w", name, err)
	}
	return cfg, true, nil
}

// listMatchConfigs returns every saved configuration, sorted by name, for
// the Matching wizard's "Saved configurations" picker.
func (a *App) listMatchConfigs(ctx context.Context) ([]MatchConfig, error) {
	if err := a.ensureMatchConfigsTable(ctx); err != nil {
		return nil, err
	}
	rows, err := a.queryConn(ctx, a.localTinySQLConn(), "match_config.list", matchConfigsSelectAllSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var configs []MatchConfig
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var cfg MatchConfig
		if err := json.Unmarshal([]byte(raw), &cfg); err == nil {
			configs = append(configs, cfg)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(configs, func(i, j int) bool { return strings.ToLower(configs[i].Name) < strings.ToLower(configs[j].Name) })
	return configs, nil
}

// deleteMatchConfig removes a saved configuration by name. Deleting a
// configuration that has a Match Schedule attached also unregisters that
// schedule (see match_schedule.go).
func (a *App) deleteMatchConfig(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("configuration name is required")
	}
	if err := a.ensureMatchConfigsTable(ctx); err != nil {
		return err
	}
	_, err := a.execConn(ctx, a.localTinySQLConn(), "match_config.delete", matchConfigsDeleteSQL, name)
	return err
}

func (a *App) ensureMatchConfigsTable(ctx context.Context) error {
	_, err := a.execConn(ctx, a.localTinySQLConn(), "match_config.ensure_table", matchConfigsEnsureTableSQL)
	if err == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return nil
	}
	return fmt.Errorf("ensure match configs table: %w", err)
}
