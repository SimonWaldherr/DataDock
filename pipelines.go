package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SimonWaldherr/datadock/internal/sqlutil"
)

const (
	pipelinesTable    = "__datadock_pipelines"
	pipelineRunsTable = "__datadock_pipeline_runs"

	maxPipelineNameLength        = 128
	maxPipelineDescriptionLength = 4_000
	maxPipelineSteps             = 32
	maxPipelineStepNameLength    = 128
	maxPipelineSQLLength         = 64 << 10
	maxPipelineParallelism       = 4
	maxPipelineBundleBytes       = 16 << 20

	pipelineBundleFormat        = "datadock.pipeline.bundle"
	pipelineBundleSchemaVersion = 1
)

// Pipeline is an immutable, versioned sequence of read-only SQL steps. The
// first release deliberately has no materializing or external side-effect
// step: every saved definition is safe to execute only against local tinySQL
// and produces a structural lineage record rather than persistent output data.
type Pipeline struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Version     int    `json:"version"`
	// MaxParallelism is opt-in. A value of one preserves declared step order;
	// higher values are appropriate only when steps do not depend on each
	// other's result artifacts.
	MaxParallelism int            `json:"max_parallelism,omitempty"`
	Steps          []PipelineStep `json:"steps"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// PipelineStep is one named, result-producing SQL statement in a Pipeline.
type PipelineStep struct {
	Name string `json:"name"`
	SQL  string `json:"sql"`
}

// PipelineRun records an execution without copying result values into
// metadata. That lets operators answer what ran and what it read while keeping
// potentially sensitive query output in the database/query editor.
type PipelineRun struct {
	RunID           string            `json:"run_id"`
	PipelineName    string            `json:"pipeline_name"`
	PipelineVersion int               `json:"pipeline_version"`
	StartedAt       time.Time         `json:"started_at"`
	FinishedAt      time.Time         `json:"finished_at"`
	Status          string            `json:"status"`
	Lineage         []PipelineLineage `json:"lineage"`
	Error           string            `json:"error,omitempty"`
}

// PipelineLineage describes a step's declared table inputs and its ephemeral
// result artifact. SQL text is represented only by a SHA-256 digest here; the
// complete statement remains in the immutable pipeline version definition.
type PipelineLineage struct {
	StepName  string   `json:"step_name"`
	SQLSHA256 string   `json:"sql_sha256"`
	Sources   []string `json:"sources"`
	Artifact  string   `json:"artifact"`
	Columns   []string `json:"columns,omitempty"`
	RowCount  int      `json:"row_count"`
	ElapsedMS int64    `json:"elapsed_ms"`
	Status    string   `json:"status"`
	Error     string   `json:"error,omitempty"`
}

// PipelineBundle is the portable JSON representation of immutable pipeline
// definitions. It deliberately excludes run history and query output so it is
// safe to transfer workflow logic without copying potentially sensitive data.
type PipelineBundle struct {
	Format        string     `json:"format"`
	SchemaVersion int        `json:"schema_version"`
	ExportedAt    time.Time  `json:"exported_at"`
	Pipelines     []Pipeline `json:"pipelines"`
}

// PipelineImportResult reports append-only imports. Existing identical
// definitions are skipped, making repeated import of the same bundle safe.
type PipelineImportResult struct {
	Imported []Pipeline `json:"imported"`
	Skipped  int        `json:"skipped"`
}

type pipelineWorkItem struct {
	Index int
	Step  PipelineStep
}

type pipelineWorkResult struct {
	Index   int
	Lineage PipelineLineage
	Err     string
}

var (
	pipelineSourcePattern = regexp.MustCompile(`(?i)\b(?:from|join)\s+((?:"(?:[^"]|"")+")|(?:\[[^\]]+\])|(?:[A-Za-z_][A-Za-z0-9_.$]*))`)
	pipelineCTEPattern    = regexp.MustCompile(`(?i)(?:\bwith\s+(?:recursive\s+)?|,)\s*([A-Za-z_][A-Za-z0-9_]*)\s+as\s*\(`)
)

// validatePipelineDefinition normalizes an API/UI definition before it ever
// reaches the metadata store. Read-only SQL prevents a pipeline definition
// from becoming an unattended write primitive as pipeline scheduling evolves.
func validatePipelineDefinition(pipeline Pipeline) (Pipeline, error) {
	pipeline.Name = strings.TrimSpace(pipeline.Name)
	pipeline.Description = strings.TrimSpace(pipeline.Description)
	if pipeline.Name == "" {
		return Pipeline{}, fmt.Errorf("pipeline name is required")
	}
	if len(pipeline.Name) > maxPipelineNameLength {
		return Pipeline{}, fmt.Errorf("pipeline name must be at most %d characters", maxPipelineNameLength)
	}
	if len(pipeline.Description) > maxPipelineDescriptionLength {
		return Pipeline{}, fmt.Errorf("pipeline description must be at most %d characters", maxPipelineDescriptionLength)
	}
	if pipeline.MaxParallelism <= 0 {
		pipeline.MaxParallelism = 1
	}
	if pipeline.MaxParallelism > maxPipelineParallelism {
		return Pipeline{}, fmt.Errorf("pipeline max_parallelism must be between 1 and %d", maxPipelineParallelism)
	}
	if len(pipeline.Steps) == 0 {
		return Pipeline{}, fmt.Errorf("pipeline requires at least one step")
	}
	if len(pipeline.Steps) > maxPipelineSteps {
		return Pipeline{}, fmt.Errorf("pipeline supports at most %d steps", maxPipelineSteps)
	}

	seenNames := make(map[string]struct{}, len(pipeline.Steps))
	for index := range pipeline.Steps {
		step := &pipeline.Steps[index]
		step.Name = strings.TrimSpace(step.Name)
		step.SQL = strings.TrimSpace(step.SQL)
		if step.Name == "" {
			return Pipeline{}, fmt.Errorf("step %d name is required", index+1)
		}
		if len(step.Name) > maxPipelineStepNameLength {
			return Pipeline{}, fmt.Errorf("step %q name must be at most %d characters", step.Name, maxPipelineStepNameLength)
		}
		key := strings.ToLower(step.Name)
		if _, exists := seenNames[key]; exists {
			return Pipeline{}, fmt.Errorf("step name %q is duplicated", step.Name)
		}
		seenNames[key] = struct{}{}
		if step.SQL == "" {
			return Pipeline{}, fmt.Errorf("step %q SQL is required", step.Name)
		}
		if len(step.SQL) > maxPipelineSQLLength {
			return Pipeline{}, fmt.Errorf("step %q SQL must be at most %d bytes", step.Name, maxPipelineSQLLength)
		}
		if !sqlutil.IsResultProducing(step.SQL) {
			return Pipeline{}, fmt.Errorf("step %q must be a single result-producing SELECT/WITH/SHOW/EXPLAIN/DESCRIBE/PRAGMA statement", step.Name)
		}
	}
	return pipeline, nil
}

// savePipeline appends a new immutable version. It never mutates old records,
// which makes a PipelineRun's version reference reproducible after edits.
func (a *App) savePipeline(ctx context.Context, pipeline Pipeline) (Pipeline, error) {
	pipeline, err := validatePipelineDefinition(pipeline)
	if err != nil {
		return Pipeline{}, err
	}
	if err := a.ensurePipelineTables(ctx); err != nil {
		return Pipeline{}, err
	}

	history, err := a.listPipelineVersions(ctx, pipeline.Name)
	if err != nil {
		return Pipeline{}, err
	}
	now := time.Now().UTC()
	pipeline.Version = 1
	pipeline.CreatedAt = now
	if len(history) > 0 {
		latest := history[len(history)-1]
		pipeline.Version = latest.Version + 1
		pipeline.CreatedAt = latest.CreatedAt
	}
	pipeline.UpdatedAt = now

	encoded, err := json.Marshal(pipeline)
	if err != nil {
		return Pipeline{}, fmt.Errorf("encode pipeline: %w", err)
	}
	_, err = a.execConn(ctx, a.localTinySQLConn(), "pipeline.insert", pipelinesInsertSQL,
		pipeline.Name, pipeline.Version, string(encoded),
		pipeline.CreatedAt.Format(time.RFC3339Nano), pipeline.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return Pipeline{}, fmt.Errorf("save pipeline: %w", err)
	}
	return pipeline, nil
}

func (a *App) loadPipeline(ctx context.Context, name string, version int) (Pipeline, error) {
	history, err := a.listPipelineVersions(ctx, name)
	if err != nil {
		return Pipeline{}, err
	}
	if len(history) == 0 {
		return Pipeline{}, fmt.Errorf("pipeline %q was not found", strings.TrimSpace(name))
	}
	if version <= 0 {
		return history[len(history)-1], nil
	}
	for _, pipeline := range history {
		if pipeline.Version == version {
			return pipeline, nil
		}
	}
	return Pipeline{}, fmt.Errorf("pipeline %q version %d was not found", strings.TrimSpace(name), version)
}

// listPipelines returns only each pipeline's latest version for the main UI.
func (a *App) listPipelines(ctx context.Context) ([]Pipeline, error) {
	if err := a.ensurePipelineTables(ctx); err != nil {
		return nil, err
	}
	pipelines, err := a.listPipelineRows(ctx, pipelinesSelectAllSQL)
	if err != nil {
		return nil, err
	}
	latest := make(map[string]Pipeline, len(pipelines))
	for _, pipeline := range pipelines {
		current, exists := latest[pipeline.Name]
		if !exists || pipeline.Version > current.Version {
			latest[pipeline.Name] = pipeline
		}
	}
	result := make([]Pipeline, 0, len(latest))
	for _, pipeline := range latest {
		result = append(result, pipeline)
	}
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	return result, nil
}

func (a *App) listPipelineVersions(ctx context.Context, name string) ([]Pipeline, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil
	}
	if err := a.ensurePipelineTables(ctx); err != nil {
		return nil, err
	}
	pipelines, err := a.listPipelineRows(ctx, pipelinesSelectByNameSQL, name)
	if err != nil {
		return nil, err
	}
	sort.Slice(pipelines, func(i, j int) bool { return pipelines[i].Version < pipelines[j].Version })
	return pipelines, nil
}

// writePipelineBundle writes every immutable pipeline definition to an
// arbitrary destination. HTTP handlers use it with an in-memory buffer before
// sending a response, while callers can use a file, archive, or network writer
// without duplicating serialization logic.
func (a *App) writePipelineBundle(ctx context.Context, dst io.Writer) error {
	if dst == nil {
		return fmt.Errorf("pipeline bundle writer is required")
	}
	if err := a.ensurePipelineTables(ctx); err != nil {
		return err
	}
	pipelines, err := a.listPipelineRows(ctx, pipelinesSelectAllSQL)
	if err != nil {
		return err
	}
	sort.Slice(pipelines, func(i, j int) bool {
		left, right := strings.ToLower(pipelines[i].Name), strings.ToLower(pipelines[j].Name)
		if left == right {
			return pipelines[i].Version < pipelines[j].Version
		}
		return left < right
	})
	return json.NewEncoder(dst).Encode(PipelineBundle{
		Format:        pipelineBundleFormat,
		SchemaVersion: pipelineBundleSchemaVersion,
		ExportedAt:    time.Now().UTC(),
		Pipelines:     pipelines,
	})
}

// readPipelineBundle imports a portable bundle from any io.Reader. Definitions
// are validated just like HTTP-created pipelines, appended as local versions,
// and deduplicated by canonical definition hash for idempotent imports.
func (a *App) readPipelineBundle(ctx context.Context, src io.Reader) (PipelineImportResult, error) {
	if src == nil {
		return PipelineImportResult{}, fmt.Errorf("pipeline bundle reader is required")
	}
	data, err := io.ReadAll(io.LimitReader(src, maxPipelineBundleBytes+1))
	if err != nil {
		return PipelineImportResult{}, fmt.Errorf("read pipeline bundle: %w", err)
	}
	if len(data) > maxPipelineBundleBytes {
		return PipelineImportResult{}, fmt.Errorf("pipeline bundle exceeds the %d byte limit", maxPipelineBundleBytes)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	var bundle PipelineBundle
	if err := decoder.Decode(&bundle); err != nil {
		return PipelineImportResult{}, fmt.Errorf("decode pipeline bundle: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return PipelineImportResult{}, fmt.Errorf("pipeline bundle contains more than one JSON value")
		}
		return PipelineImportResult{}, fmt.Errorf("decode trailing pipeline bundle data: %w", err)
	}
	if bundle.Format != pipelineBundleFormat {
		return PipelineImportResult{}, fmt.Errorf("unsupported pipeline bundle format %q", bundle.Format)
	}
	if bundle.SchemaVersion != pipelineBundleSchemaVersion {
		return PipelineImportResult{}, fmt.Errorf("unsupported pipeline bundle schema version %d", bundle.SchemaVersion)
	}
	if len(bundle.Pipelines) > 512 {
		return PipelineImportResult{}, fmt.Errorf("pipeline bundle contains more than 512 definitions")
	}

	sort.SliceStable(bundle.Pipelines, func(i, j int) bool {
		left, right := strings.ToLower(strings.TrimSpace(bundle.Pipelines[i].Name)), strings.ToLower(strings.TrimSpace(bundle.Pipelines[j].Name))
		if left == right {
			return bundle.Pipelines[i].Version < bundle.Pipelines[j].Version
		}
		return left < right
	})
	known := make(map[string]map[string]struct{})
	result := PipelineImportResult{Imported: make([]Pipeline, 0)}
	for _, candidate := range bundle.Pipelines {
		pipeline, err := validatePipelineDefinition(candidate)
		if err != nil {
			return PipelineImportResult{}, fmt.Errorf("validate imported pipeline %q: %w", candidate.Name, err)
		}
		digest, err := pipelineDefinitionDigest(pipeline)
		if err != nil {
			return PipelineImportResult{}, err
		}
		if _, loaded := known[pipeline.Name]; !loaded {
			history, err := a.listPipelineVersions(ctx, pipeline.Name)
			if err != nil {
				return PipelineImportResult{}, err
			}
			known[pipeline.Name] = make(map[string]struct{}, len(history))
			for _, existing := range history {
				existingDigest, err := pipelineDefinitionDigest(existing)
				if err != nil {
					return PipelineImportResult{}, err
				}
				known[pipeline.Name][existingDigest] = struct{}{}
			}
		}
		if _, exists := known[pipeline.Name][digest]; exists {
			result.Skipped++
			continue
		}
		saved, err := a.savePipeline(ctx, pipeline)
		if err != nil {
			return PipelineImportResult{}, err
		}
		known[pipeline.Name][digest] = struct{}{}
		result.Imported = append(result.Imported, saved)
	}
	return result, nil
}

func pipelineDefinitionDigest(pipeline Pipeline) (string, error) {
	pipeline.Version = 0
	pipeline.CreatedAt = time.Time{}
	pipeline.UpdatedAt = time.Time{}
	encoded, err := json.Marshal(pipeline)
	if err != nil {
		return "", fmt.Errorf("encode pipeline definition digest: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func (a *App) listPipelineRows(ctx context.Context, query string, args ...any) ([]Pipeline, error) {
	rows, err := a.queryConn(ctx, a.localTinySQLConn(), "pipeline.list", query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pipelines []Pipeline
	for rows.Next() {
		var name, definition string
		var version int
		if err := rows.Scan(&name, &version, &definition); err != nil {
			return nil, err
		}
		var pipeline Pipeline
		if err := json.Unmarshal([]byte(definition), &pipeline); err != nil {
			return nil, fmt.Errorf("decode pipeline %q version %d: %w", name, version, err)
		}
		pipeline.Name = name
		pipeline.Version = version
		// Bundles and metadata saved before MaxParallelism existed retain the
		// original sequential behavior when they are listed or re-exported.
		if pipeline.MaxParallelism <= 0 {
			pipeline.MaxParallelism = 1
		}
		pipelines = append(pipelines, pipeline)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return pipelines, nil
}

// deletePipeline removes every saved definition version. Run history is
// intentionally retained as an audit/lineage record even after removal.
func (a *App) deletePipeline(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("pipeline name is required")
	}
	if err := a.ensurePipelineTables(ctx); err != nil {
		return err
	}
	_, err := a.execConn(ctx, a.localTinySQLConn(), "pipeline.delete", pipelinesDeleteByNameSQL, name)
	return err
}

// runPipeline executes a single stored definition version. It only permits
// read queries, and it stores table-source lineage plus response shape rather
// than raw vectors, result text, or result values.
func (a *App) runPipeline(ctx context.Context, name string, version int) (PipelineRun, error) {
	pipeline, err := a.loadPipeline(ctx, name, version)
	if err != nil {
		return PipelineRun{}, err
	}
	// Revalidate at execution time as well as save time. Metadata may have
	// been restored from an older artifact or changed outside the HTTP API,
	// and a stored definition must never turn into an unattended write.
	pipeline, err = validatePipelineDefinition(pipeline)
	if err != nil {
		return PipelineRun{}, fmt.Errorf("validate stored pipeline %q: %w", name, err)
	}
	runID, err := newPipelineRunID()
	if err != nil {
		return PipelineRun{}, err
	}
	run := PipelineRun{
		RunID:           runID,
		PipelineName:    pipeline.Name,
		PipelineVersion: pipeline.Version,
		StartedAt:       time.Now().UTC(),
		Status:          "succeeded",
		Lineage:         make([]PipelineLineage, 0, len(pipeline.Steps)),
	}

	// Keep a short, independent context for the run record. The execution
	// context can legitimately time out or be cancelled; losing its status
	// record at exactly that moment would defeat the point of run lineage.
	recordCtx, recordCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer recordCancel()
	executionCtx, cancel := a.withQueryTimeout(ctx)
	defer cancel()
	run.Lineage, err = a.executePipelineSteps(executionCtx, pipeline)
	if err != nil {
		run.Status = "failed"
		run.Error = err.Error()
	}
	run.FinishedAt = time.Now().UTC()
	if err := a.recordPipelineRun(recordCtx, run); err != nil {
		return PipelineRun{}, err
	}
	return run, nil
}

// executePipelineSteps runs independent read-only steps through a bounded
// worker pool. The default worker count is one, preserving the pipeline's
// declared order. A caller must explicitly opt in to parallelism, and result
// lineage is always returned in definition order even when work completes in a
// different order.
func (a *App) executePipelineSteps(ctx context.Context, pipeline Pipeline) ([]PipelineLineage, error) {
	workerCount := pipeline.MaxParallelism
	if workerCount > len(pipeline.Steps) {
		workerCount = len(pipeline.Steps)
	}
	if workerCount < 1 {
		workerCount = 1
	}

	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan pipelineWorkItem)
	results := make(chan pipelineWorkResult, len(pipeline.Steps))

	var workers sync.WaitGroup
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-workCtx.Done():
					return
				case item, ok := <-jobs:
					if !ok {
						return
					}
					lineage, errText := a.executePipelineStep(workCtx, pipeline, item.Step)
					// The channel is fully buffered to the number of submitted
					// steps, so a worker can always report its terminal outcome
					// even when another worker cancelled the shared context.
					results <- pipelineWorkResult{Index: item.Index, Lineage: lineage, Err: errText}
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for index, step := range pipeline.Steps {
			select {
			case jobs <- pipelineWorkItem{Index: index, Step: step}:
			case <-workCtx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()

	lineageByIndex := make([]PipelineLineage, len(pipeline.Steps))
	completed := make([]bool, len(pipeline.Steps))
	var firstErr error
	for result := range results {
		completed[result.Index] = true
		lineageByIndex[result.Index] = result.Lineage
		if result.Err != "" && firstErr == nil {
			firstErr = fmt.Errorf("step %q: %s", result.Lineage.StepName, result.Err)
			cancel()
		}
	}
	if firstErr == nil && ctx.Err() != nil {
		firstErr = ctx.Err()
	}

	lineage := make([]PipelineLineage, 0, len(pipeline.Steps))
	for index, step := range pipeline.Steps {
		if completed[index] {
			lineage = append(lineage, lineageByIndex[index])
			continue
		}
		if firstErr != nil {
			lineage = append(lineage, skippedPipelineLineage(pipeline, step, firstErr))
		}
	}
	return lineage, firstErr
}

func (a *App) executePipelineStep(ctx context.Context, pipeline Pipeline, step PipelineStep) (PipelineLineage, string) {
	digest := sha256.Sum256([]byte(step.SQL))
	lineage := PipelineLineage{
		StepName:  step.Name,
		SQLSHA256: hex.EncodeToString(digest[:]),
		Sources:   extractPipelineSources(step.SQL),
		Artifact:  pipelineResultArtifact(pipeline, step),
		Status:    "succeeded",
	}
	result := a.executeTinySQL(ctx, step.SQL)
	lineage.Columns = result.Columns
	lineage.RowCount = len(result.Rows)
	lineage.ElapsedMS = result.Elapsed.Milliseconds()
	if result.Err != "" {
		lineage.Status = "failed"
		lineage.Error = result.Err
		return lineage, result.Err
	}
	return lineage, ""
}

func skippedPipelineLineage(pipeline Pipeline, step PipelineStep, runErr error) PipelineLineage {
	digest := sha256.Sum256([]byte(step.SQL))
	return PipelineLineage{
		StepName:  step.Name,
		SQLSHA256: hex.EncodeToString(digest[:]),
		Sources:   extractPipelineSources(step.SQL),
		Artifact:  pipelineResultArtifact(pipeline, step),
		Status:    "skipped",
		Error:     "not started after pipeline failure: " + runErr.Error(),
	}
}

func (a *App) recordPipelineRun(ctx context.Context, run PipelineRun) error {
	if err := a.ensurePipelineTables(ctx); err != nil {
		return err
	}
	encoded, err := json.Marshal(run.Lineage)
	if err != nil {
		return fmt.Errorf("encode pipeline lineage: %w", err)
	}
	_, err = a.execConn(ctx, a.localTinySQLConn(), "pipeline_run.insert", pipelineRunsInsertSQL,
		run.RunID, run.PipelineName, run.PipelineVersion,
		run.StartedAt.Format(time.RFC3339Nano), run.FinishedAt.Format(time.RFC3339Nano),
		run.Status, string(encoded), run.Error)
	if err != nil {
		return fmt.Errorf("record pipeline run: %w", err)
	}
	return nil
}

func (a *App) listPipelineRuns(ctx context.Context, name string) ([]PipelineRun, error) {
	if err := a.ensurePipelineTables(ctx); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	query := pipelineRunsSelectAllSQL
	var args []any
	if name != "" {
		query = pipelineRunsSelectByNameSQL
		args = append(args, name)
	}
	rows, err := a.queryConn(ctx, a.localTinySQLConn(), "pipeline_run.list", query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var runs []PipelineRun
	for rows.Next() {
		var run PipelineRun
		var startedAt, finishedAt, lineageJSON string
		if err := rows.Scan(&run.RunID, &run.PipelineName, &run.PipelineVersion, &startedAt, &finishedAt, &run.Status, &lineageJSON, &run.Error); err != nil {
			return nil, err
		}
		var err error
		run.StartedAt, err = time.Parse(time.RFC3339Nano, startedAt)
		if err != nil {
			return nil, fmt.Errorf("parse pipeline run %q start time: %w", run.RunID, err)
		}
		run.FinishedAt, err = time.Parse(time.RFC3339Nano, finishedAt)
		if err != nil {
			return nil, fmt.Errorf("parse pipeline run %q finish time: %w", run.RunID, err)
		}
		if err := json.Unmarshal([]byte(lineageJSON), &run.Lineage); err != nil {
			return nil, fmt.Errorf("decode pipeline run %q lineage: %w", run.RunID, err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].StartedAt.After(runs[j].StartedAt) })
	return runs, nil
}

func (a *App) ensurePipelineTables(ctx context.Context) error {
	for operation, query := range map[string]string{
		"pipeline.ensure_table":     pipelinesEnsureTableSQL,
		"pipeline_run.ensure_table": pipelineRunsEnsureTableSQL,
	} {
		_, err := a.execConn(ctx, a.localTinySQLConn(), operation, query)
		if err == nil || strings.Contains(strings.ToLower(err.Error()), "already exists") {
			continue
		}
		return fmt.Errorf("ensure pipeline metadata tables: %w", err)
	}
	return nil
}

func newPipelineRunID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate pipeline run ID: %w", err)
	}
	return "pipeline_" + hex.EncodeToString(bytes[:]), nil
}

func extractPipelineSources(sqlText string) []string {
	matches := pipelineSourcePattern.FindAllStringSubmatch(sqlText, -1)
	cteMatches := pipelineCTEPattern.FindAllStringSubmatch(sqlText, -1)
	cteNames := make(map[string]struct{}, len(cteMatches))
	for _, match := range cteMatches {
		if len(match) > 1 {
			cteNames[strings.ToLower(match[1])] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		name := strings.TrimSpace(match[1])
		name = strings.Trim(name, `"[]`)
		if name != "" {
			if _, isCTE := cteNames[strings.ToLower(name)]; isCTE {
				continue
			}
			seen[name] = struct{}{}
		}
	}
	sources := make([]string, 0, len(seen))
	for source := range seen {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	return sources
}

func pipelineResultArtifact(pipeline Pipeline, step PipelineStep) string {
	return fmt.Sprintf("result://pipeline/%s/v%d/%s", pipeline.Name, pipeline.Version, step.Name)
}
