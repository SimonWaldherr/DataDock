package main

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tinysql "github.com/SimonWaldherr/tinySQL"
	tsqldriver "github.com/SimonWaldherr/tinySQL/driver"
)

// newFileBackedTestApp builds an isolated App backed by a real on-disk
// snapshot file in the given tinySQL storage mode, unlike newTestApp (pure
// in-memory, no Path) — needed to exercise apiSnapshotRestoreHandler, which
// requires a real file to replace.
//
// Uses its own MkdirTemp/RemoveAll rather than t.TempDir(): on Windows, a
// storage backend's WAL/lock sidecar can still hold its file handle open for
// a moment after Close() returns, which would otherwise fail t.TempDir()'s
// own strict (non-retrying) cleanup and fail the test even though every
// actual assertion passed. The retry loop below absorbs that race.
func newFileBackedTestApp(t *testing.T, storageMode string) (*App, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "datadock-restore-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() {
		for i := 0; i < 10; i++ {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	path := filepath.Join(dir, "datadock.db")

	nativeDB, err := openNativeDBWithStorageOptions(path, storageMode, nil, false, 0)
	if err != nil {
		t.Fatalf("open native db (%s): %v", storageMode, err)
	}
	ensureNativeRBACSafetyNet(nativeDB)
	t.Cleanup(func() { _ = nativeDB.Close() })

	sqlDB, err := tsqldriver.OpenWithDB(nativeDB)
	if err != nil {
		t.Fatalf("open sql db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(0)
	t.Cleanup(func() { sqlDB.Close() })

	tpl, err := parseTemplates()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	app := newApp(nativeDB, sqlDB, "default", tpl)

	// Priming a real GOB snapshot at path only makes sense for memory/wal
	// (the only modes apiSnapshotRestoreHandler treats as restorable — see
	// restorableStorageModes in snapshot_restore.go); other modes manage
	// their own on-disk layout from OpenDB itself; forcing a GOB write over
	// that would conflict with (not represent) what those modes actually do.
	if storageMode == "memory" || storageMode == "wal" {
		if err := tinysql.SaveToFile(nativeDB, path); err != nil {
			t.Fatalf("prime snapshot file: %v", err)
		}
	}
	return app, path
}

// setMaintenanceMode flips ReadOnlyMode the same way adminToggleMaintenanceHandler
// does, without going through HTTP — restore requires it to already be on.
func setMaintenanceMode(t *testing.T, app *App, enabled bool) {
	t.Helper()
	settings := app.currentRuntimeSettings()
	settings.ReadOnlyMode = enabled
	if err := app.applyRuntimeSettings(settings); err != nil {
		t.Fatalf("applyRuntimeSettings: %v", err)
	}
	if err := app.saveRuntimeSettings(context.Background()); err != nil {
		t.Fatalf("saveRuntimeSettings: %v", err)
	}
}

// buildRestoreRequest constructs a multipart POST /api/admin/snapshot/restore
// request carrying the given file bytes and confirm value.
func buildRestoreRequest(t *testing.T, fileContents []byte, confirm string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if confirm != "" {
		if err := w.WriteField("confirm", confirm); err != nil {
			t.Fatalf("write confirm field: %v", err)
		}
	}
	part, err := w.CreateFormFile("snapshot", "datadock-snapshot.gob")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(fileContents); err != nil {
		t.Fatalf("write file contents: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/snapshot/restore", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req
}

func validSnapshotBytes(t *testing.T) []byte {
	t.Helper()
	db := tinysql.NewDB()
	var buf bytes.Buffer
	if err := tinysql.SaveToWriter(db, &buf); err != nil {
		t.Fatalf("SaveToWriter: %v", err)
	}
	return buf.Bytes()
}

func TestSnapshotRestoreRejectsWrongConfirmValue(t *testing.T) {
	app, _ := newFileBackedTestApp(t, "memory")
	setMaintenanceMode(t, app, true)

	req := buildRestoreRequest(t, validSnapshotBytes(t), "not-restore")
	rec := httptest.NewRecorder()
	app.apiSnapshotRestoreHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a wrong confirm value, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSnapshotRestoreRequiresMaintenanceMode(t *testing.T) {
	app, _ := newFileBackedTestApp(t, "memory")
	// Maintenance mode is off by default; do not enable it.

	req := buildRestoreRequest(t, validSnapshotBytes(t), "RESTORE")
	rec := httptest.NewRecorder()
	app.apiSnapshotRestoreHandler(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 when maintenance mode is off, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSnapshotRestoreRejectsCorruptUpload(t *testing.T) {
	app, path := newFileBackedTestApp(t, "memory")
	setMaintenanceMode(t, app, true)

	req := buildRestoreRequest(t, []byte("this is not a gob snapshot"), "RESTORE")
	rec := httptest.NewRecorder()
	app.apiSnapshotRestoreHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a corrupt upload, got %d: %s", rec.Code, rec.Body.String())
	}
	// The live file must be untouched: validation happens before anything
	// live is replaced.
	if _, err := tinysql.LoadFromFile(path); err != nil {
		t.Errorf("expected the original file to remain valid after a rejected restore: %v", err)
	}
}

func TestSnapshotRestoreRejectsUnsupportedStorageMode(t *testing.T) {
	app, _ := newFileBackedTestApp(t, "disk")
	setMaintenanceMode(t, app, true)

	req := buildRestoreRequest(t, validSnapshotBytes(t), "RESTORE")
	rec := httptest.NewRecorder()
	app.apiSnapshotRestoreHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unsupported storage mode, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSnapshotRestoreRoundTrips(t *testing.T) {
	app, path := newFileBackedTestApp(t, "memory")
	setMaintenanceMode(t, app, true)

	// Build a distinguishable snapshot (a table the destination app doesn't
	// have) so a successful restore is verifiable by more than just "the
	// handler returned 200".
	srcDB := tinysql.NewDB()
	srcSQLDB, err := tsqldriver.OpenWithDB(srcDB)
	if err != nil {
		t.Fatalf("open source sql db: %v", err)
	}
	defer srcSQLDB.Close()
	if _, err := srcSQLDB.ExecContext(context.Background(), "CREATE TABLE restored_marker (id INT)"); err != nil {
		t.Fatalf("create marker table: %v", err)
	}
	var snapshot bytes.Buffer
	if err := tinysql.SaveToWriter(srcDB, &snapshot); err != nil {
		t.Fatalf("SaveToWriter: %v", err)
	}

	req := buildRestoreRequest(t, snapshot.Bytes(), "RESTORE")
	rec := httptest.NewRecorder()
	app.apiSnapshotRestoreHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for a valid restore, got %d: %s", rec.Code, rec.Body.String())
	}

	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if !bytes.Equal(onDisk, snapshot.Bytes()) {
		t.Error("expected the on-disk file to exactly match the uploaded snapshot bytes")
	}

	restoredDB, err := tinysql.LoadFromFile(path)
	if err != nil {
		t.Fatalf("load restored file: %v", err)
	}
	defer restoredDB.Close()
	restoredSQLDB, err := tsqldriver.OpenWithDB(restoredDB)
	if err != nil {
		t.Fatalf("open restored sql db: %v", err)
	}
	defer restoredSQLDB.Close()
	var count int
	if err := restoredSQLDB.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM restored_marker").Scan(&count); err != nil {
		t.Fatalf("query restored marker table (restore did not take effect): %v", err)
	}

	// A backup of the pre-restore file must exist alongside it.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("read db directory: %v", err)
	}
	foundBackup := false
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".pre-restore-") {
			foundBackup = true
			break
		}
	}
	if !foundBackup {
		t.Errorf("expected a .pre-restore-* backup file in %s, found: %v", filepath.Dir(path), entries)
	}
}
