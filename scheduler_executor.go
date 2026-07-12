package main

import (
	"context"
	"fmt"
	"time"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

type verboseSQLJobExecutor struct {
	db      *tinysql.DB
	tenant  string
	verbose *VerboseLogger
}

func (e verboseSQLJobExecutor) ExecuteSQL(ctx context.Context, sqlText string) (interface{}, error) {
	if e.db == nil {
		return nil, fmt.Errorf("nil SQL job executor")
	}
	tenant := e.tenant
	if tenant == "" {
		tenant = "default"
	}
	start := time.Now()
	if e.verbose.Enabled() {
		e.verbose.Log(VerboseEvent{
			System:    "database",
			Direction: "outbound",
			Operation: "scheduler.execute",
			Target:    "tinysql://" + tenant,
			SQL:       sqlText,
		})
	}
	rs, err := tinysql.ExecSQL(tinysql.WithAuditText(ctx, sqlText), e.db, tenant, sqlText)
	if e.verbose.Enabled() {
		event := VerboseEvent{
			System:    "database",
			Direction: "inbound",
			Operation: "scheduler.execute",
			Target:    "tinysql://" + tenant,
			Duration:  time.Since(start),
			Status:    "ok",
		}
		if err != nil {
			event.Status = "error"
			event.Error = err.Error()
		}
		e.verbose.Log(event)
	}
	if err != nil {
		return nil, err
	}
	return rs, nil
}
