package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPipelineVersioningAndLineage(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	if result := app.executeTinySQL(ctx, "CREATE TABLE pipeline_input (id INT, region TEXT)"); result.Err != "" {
		t.Fatalf("create pipeline input: %s", result.Err)
	}
	if result := app.executeTinySQL(ctx, "INSERT INTO pipeline_input VALUES (1, 'north'), (2, 'south')"); result.Err != "" {
		t.Fatalf("seed pipeline input: %s", result.Err)
	}

	first, err := app.savePipeline(ctx, Pipeline{
		Name:        "regional_summary",
		Description: "First reproducible definition",
		Steps: []PipelineStep{{
			Name: "summary",
			SQL:  "SELECT region, COUNT(*) AS total FROM pipeline_input GROUP BY region",
		}},
	})
	if err != nil {
		t.Fatalf("save first pipeline: %v", err)
	}
	if first.Version != 1 {
		t.Fatalf("first version = %d, want 1", first.Version)
	}

	second, err := app.savePipeline(ctx, Pipeline{
		Name:        "regional_summary",
		Description: "Second reproducible definition",
		Steps: []PipelineStep{{
			Name: "summary",
			SQL:  "SELECT region FROM pipeline_input ORDER BY region",
		}},
	})
	if err != nil {
		t.Fatalf("save second pipeline: %v", err)
	}
	if second.Version != 2 {
		t.Fatalf("second version = %d, want 2", second.Version)
	}

	versions, err := app.listPipelineVersions(ctx, "regional_summary")
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(versions) != 2 || versions[0].Version != 1 || versions[1].Version != 2 {
		t.Fatalf("versions = %#v, want v1 and v2", versions)
	}

	run, err := app.runPipeline(ctx, "regional_summary", 1)
	if err != nil {
		t.Fatalf("run first version: %v", err)
	}
	if run.Status != "succeeded" || run.PipelineVersion != 1 || len(run.Lineage) != 1 {
		t.Fatalf("unexpected pipeline run: %#v", run)
	}
	lineage := run.Lineage[0]
	if !containsString(lineage.Sources, "pipeline_input") || lineage.RowCount != 2 || lineage.SQLSHA256 == "" {
		t.Fatalf("unexpected lineage: %#v", lineage)
	}
	if strings.Contains(lineage.SQLSHA256, "SELECT") || strings.Contains(lineage.Artifact, "north") {
		t.Fatalf("lineage leaked SQL or result data: %#v", lineage)
	}

	runs, err := app.listPipelineRuns(ctx, "regional_summary")
	if err != nil {
		t.Fatalf("list pipeline runs: %v", err)
	}
	if len(runs) != 1 || runs[0].RunID != run.RunID || runs[0].Lineage[0].SQLSHA256 != lineage.SQLSHA256 {
		t.Fatalf("persisted runs = %#v", runs)
	}

	if err := app.deletePipeline(ctx, "regional_summary"); err != nil {
		t.Fatalf("delete pipeline: %v", err)
	}
	versions, err = app.listPipelineVersions(ctx, "regional_summary")
	if err != nil || len(versions) != 0 {
		t.Fatalf("versions after delete = %#v, %v", versions, err)
	}
	runs, err = app.listPipelineRuns(ctx, "regional_summary")
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs after delete = %#v, %v", runs, err)
	}
}

func TestPipelineRejectsWriteAndScripts(t *testing.T) {
	app := newTestApp(t)
	for _, sqlText := range []string{
		"INSERT INTO target VALUES (1)",
		"SELECT 1; DELETE FROM target",
		"WITH write_cte AS (DELETE FROM target RETURNING id) SELECT * FROM write_cte",
	} {
		_, err := app.savePipeline(context.Background(), Pipeline{
			Name:  "unsafe_" + strings.ReplaceAll(strings.Split(sqlText, " ")[0], ";", ""),
			Steps: []PipelineStep{{Name: "unsafe", SQL: sqlText}},
		})
		if err == nil {
			t.Fatalf("expected pipeline SQL %q to be rejected", sqlText)
		}
	}
}

func TestPipelineLineageOmitsCTEAliases(t *testing.T) {
	sources := extractPipelineSources("WITH scoped AS (SELECT * FROM pipeline_input) SELECT * FROM scoped JOIN reference_data ON 1 = 1")
	if containsString(sources, "scoped") {
		t.Fatalf("CTE alias should not be reported as a source: %#v", sources)
	}
	if !containsString(sources, "pipeline_input") || !containsString(sources, "reference_data") {
		t.Fatalf("physical sources missing: %#v", sources)
	}
}

func TestPipelineParallelExecutionKeepsDefinitionOrder(t *testing.T) {
	app := newTestApp(t)
	pipeline, err := app.savePipeline(context.Background(), Pipeline{
		Name:           "parallel_read",
		MaxParallelism: 2,
		Steps: []PipelineStep{
			{Name: "first", SQL: "SELECT 1 AS first_value"},
			{Name: "second", SQL: "SELECT 2 AS second_value"},
		},
	})
	if err != nil {
		t.Fatalf("save parallel pipeline: %v", err)
	}
	if pipeline.MaxParallelism != 2 {
		t.Fatalf("parallelism = %d, want 2", pipeline.MaxParallelism)
	}
	run, err := app.runPipeline(context.Background(), pipeline.Name, pipeline.Version)
	if err != nil {
		t.Fatalf("run parallel pipeline: %v", err)
	}
	if run.Status != "succeeded" || len(run.Lineage) != 2 {
		t.Fatalf("parallel run = %#v", run)
	}
	if run.Lineage[0].StepName != "first" || run.Lineage[1].StepName != "second" {
		t.Fatalf("lineage is not definition-ordered: %#v", run.Lineage)
	}
	if run.Lineage[0].RowCount != 1 || run.Lineage[1].RowCount != 1 {
		t.Fatalf("parallel run rows = %#v", run.Lineage)
	}
	_, err = app.savePipeline(context.Background(), Pipeline{
		Name:           "too_parallel",
		MaxParallelism: maxPipelineParallelism + 1,
		Steps:          []PipelineStep{{Name: "read", SQL: "SELECT 1"}},
	})
	if err == nil {
		t.Fatal("expected parallelism above the bound to fail")
	}
}

func TestPipelineBundleRoundTripIsIdempotent(t *testing.T) {
	ctx := context.Background()
	source := newTestApp(t)
	if _, err := source.savePipeline(ctx, Pipeline{
		Name:           "portable_pipeline",
		Description:    "Portable definition",
		MaxParallelism: 2,
		Steps: []PipelineStep{
			{Name: "first", SQL: "SELECT 1 AS value"},
			{Name: "second", SQL: "SELECT 2 AS value"},
		},
	}); err != nil {
		t.Fatalf("save portable pipeline: %v", err)
	}
	var bundle bytes.Buffer
	if err := source.writePipelineBundle(ctx, &bundle); err != nil {
		t.Fatalf("write pipeline bundle: %v", err)
	}
	if strings.Contains(bundle.String(), "run_id") || strings.Contains(bundle.String(), "lineage") {
		t.Fatalf("bundle unexpectedly contains run history: %s", bundle.String())
	}

	destination := newTestApp(t)
	imported, err := destination.readPipelineBundle(ctx, bytes.NewReader(bundle.Bytes()))
	if err != nil {
		t.Fatalf("read pipeline bundle: %v", err)
	}
	if len(imported.Imported) != 1 || imported.Skipped != 0 || imported.Imported[0].MaxParallelism != 2 {
		t.Fatalf("initial bundle import = %#v", imported)
	}
	versions, err := destination.listPipelineVersions(ctx, "portable_pipeline")
	if err != nil || len(versions) != 1 {
		t.Fatalf("imported versions = %#v, %v", versions, err)
	}

	repeated, err := destination.readPipelineBundle(ctx, bytes.NewReader(bundle.Bytes()))
	if err != nil {
		t.Fatalf("repeat bundle import: %v", err)
	}
	if len(repeated.Imported) != 0 || repeated.Skipped != 1 || repeated.Imported == nil {
		t.Fatalf("repeat bundle import = %#v", repeated)
	}
}

func TestPipelineAPIRequiresAdminAndRespectsMaintenanceMode(t *testing.T) {
	app := newTestApp(t)
	mux := http.NewServeMux()
	app.registerRoutes(mux)
	body := []byte(`{"name":"api_pipeline","steps":[{"name":"read","sql":"SELECT 1 AS value"}]}`)

	anonReq := httptest.NewRequest(http.MethodPost, "/api/pipelines", bytes.NewReader(body))
	anonReq.Header.Set("Content-Type", "application/json")
	anonRec := httptest.NewRecorder()
	mux.ServeHTTP(anonRec, anonReq)
	if anonRec.Code == http.StatusCreated {
		t.Fatalf("anonymous pipeline save unexpectedly succeeded: %s", anonRec.Body.String())
	}

	adminCookie := setupAdminSession(t, mux)
	pageReq := httptest.NewRequest(http.MethodGet, "/pipelines", nil)
	pageReq.AddCookie(adminCookie)
	pageRec := httptest.NewRecorder()
	mux.ServeHTTP(pageRec, pageReq)
	if pageRec.Code != http.StatusOK || !strings.Contains(pageRec.Body.String(), "Import a bundle") || !strings.Contains(pageRec.Body.String(), "Max parallelism") {
		t.Fatalf("pipeline page status = %d: %s", pageRec.Code, pageRec.Body.String())
	}
	saveReq := httptest.NewRequest(http.MethodPost, "/api/pipelines", bytes.NewReader(body))
	saveReq.Header.Set("Content-Type", "application/json")
	saveReq.AddCookie(adminCookie)
	saveRec := httptest.NewRecorder()
	mux.ServeHTTP(saveRec, saveReq)
	if saveRec.Code != http.StatusCreated {
		t.Fatalf("pipeline save status = %d: %s", saveRec.Code, saveRec.Body.String())
	}
	var saved struct {
		Pipeline Pipeline `json:"pipeline"`
	}
	if err := json.NewDecoder(saveRec.Body).Decode(&saved); err != nil {
		t.Fatalf("decode saved pipeline: %v", err)
	}
	if saved.Pipeline.Version != 1 {
		t.Fatalf("saved pipeline version = %d, want 1", saved.Pipeline.Version)
	}

	runReq := httptest.NewRequest(http.MethodPost, "/api/pipelines/run", strings.NewReader(`{"name":"api_pipeline"}`))
	runReq.Header.Set("Content-Type", "application/json")
	runReq.AddCookie(adminCookie)
	runRec := httptest.NewRecorder()
	mux.ServeHTTP(runRec, runReq)
	if runRec.Code != http.StatusOK {
		t.Fatalf("pipeline run status = %d: %s", runRec.Code, runRec.Body.String())
	}

	exportReq := httptest.NewRequest(http.MethodGet, "/api/pipelines/export", nil)
	exportReq.AddCookie(adminCookie)
	exportRec := httptest.NewRecorder()
	mux.ServeHTTP(exportRec, exportReq)
	if exportRec.Code != http.StatusOK || !strings.Contains(exportRec.Header().Get("Content-Disposition"), "datadock-pipelines.json") {
		t.Fatalf("pipeline export status = %d headers=%v body=%s", exportRec.Code, exportRec.Header(), exportRec.Body.String())
	}
	importReq := httptest.NewRequest(http.MethodPost, "/api/pipelines/import", bytes.NewReader(exportRec.Body.Bytes()))
	importReq.Header.Set("Content-Type", "application/json")
	importReq.AddCookie(adminCookie)
	importRec := httptest.NewRecorder()
	mux.ServeHTTP(importRec, importReq)
	if importRec.Code != http.StatusOK {
		t.Fatalf("pipeline import status = %d: %s", importRec.Code, importRec.Body.String())
	}
	var imported PipelineImportResult
	if err := json.NewDecoder(importRec.Body).Decode(&imported); err != nil {
		t.Fatalf("decode pipeline import: %v", err)
	}
	if len(imported.Imported) != 0 || imported.Skipped != 1 {
		t.Fatalf("same-instance bundle import = %#v", imported)
	}

	settings := app.currentRuntimeSettings()
	settings.ReadOnlyMode = true
	if err := app.applyRuntimeSettings(settings); err != nil {
		t.Fatalf("enable maintenance mode: %v", err)
	}
	blockedReq := httptest.NewRequest(http.MethodPost, "/api/pipelines", bytes.NewReader([]byte(`{"name":"blocked","steps":[{"name":"read","sql":"SELECT 1"}]}`)))
	blockedReq.Header.Set("Content-Type", "application/json")
	blockedReq.AddCookie(adminCookie)
	blockedRec := httptest.NewRecorder()
	mux.ServeHTTP(blockedRec, blockedReq)
	if blockedRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("maintenance pipeline save status = %d: %s", blockedRec.Code, blockedRec.Body.String())
	}
}
