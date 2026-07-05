package jobs

import (
	"errors"
	"strings"
	"time"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

type Config struct {
	Name         string
	SQL          string
	ScheduleType string
	CronExpr     string
	IntervalMs   int64
	RunAt        *time.Time
	Timezone     string
	Enabled      *bool
	CatchUp      bool
	NoOverlap    bool
	MaxRuntimeMs int64
}

func Build(cfg Config) (*tinysql.CatalogJob, error) {
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		return nil, errors.New("name is required")
	}
	sqlText := strings.TrimSpace(cfg.SQL)
	if sqlText == "" {
		return nil, errors.New("sql is required")
	}
	enabled := true
	if cfg.Enabled != nil {
		enabled = *cfg.Enabled
	}
	job := &tinysql.CatalogJob{
		Name:         name,
		SQLText:      sqlText,
		ScheduleType: strings.ToUpper(strings.TrimSpace(cfg.ScheduleType)),
		CronExpr:     strings.TrimSpace(cfg.CronExpr),
		Enabled:      enabled,
		CatchUp:      cfg.CatchUp,
		NoOverlap:    cfg.NoOverlap,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	if job.ScheduleType == "" {
		job.ScheduleType = "MANUAL"
	}
	if cfg.IntervalMs > 0 {
		job.IntervalMs = cfg.IntervalMs
	}
	if cfg.RunAt != nil {
		job.RunAt = cfg.RunAt
	}
	if cfg.MaxRuntimeMs > 0 {
		job.MaxRuntimeMs = cfg.MaxRuntimeMs
	}
	if cfg.Timezone != "" {
		job.Timezone = strings.TrimSpace(cfg.Timezone)
	}
	return job, nil
}
