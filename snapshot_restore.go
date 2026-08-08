package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

// maxSnapshotRestoreBytes bounds the size of an uploaded restore snapshot —
// the "size limits" half of apiSnapshotExportHandler's original doc comment
// on why restore was never wired up. 2 GiB comfortably covers any DataDock
// instance this single-process, GOB-snapshot storage model is meant for.
const maxSnapshotRestoreBytes = 2 << 30

// restorableStorageModes are the only tinySQL storage modes whose on-disk
// file is the same GOB snapshot format tinysql.SaveToWriter/LoadFromReader
// produce and consume (confirmed against tinySQL's storage_backend.go: both
// modes persist via SaveToFile/LoadFromFile). The other modes (disk, json,
// hybrid, index, advanced_wal, paged_index) use their own on-disk layouts —
// writing a GOB blob into one of those files would corrupt it, so restore
// refuses them rather than guessing.
var restorableStorageModes = map[string]bool{
	"memory": true,
	"wal":    true,
}

// apiSnapshotRestoreHandler is the guarded counterpart to
// apiSnapshotExportHandler. It does not hot-swap the live *tinysql.DB — a.
// nativeDB is read directly by many call sites across the codebase as a
// plain struct field, not through a mutex-guarded accessor, so safely
// swapping it in place would need a larger refactor. Instead this validates
// the upload, backs up, and atomically replaces the on-disk snapshot file;
// the operator must restart the process to load the restored data. That
// restart is the "explicit operator approval outside a request handler"
// the original doc comment called for.
func (a *App) apiSnapshotRestoreHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := sessionIDFromContext(r.Context())
	username, _, _ := a.currentSessionUser(sessionID)
	audit := a.auditLogger()
	logAttempt := func(operation, detail string) {
		if audit.Enabled() {
			audit.Log(AuditEvent{
				Session:   sessionID,
				Username:  username,
				Method:    r.Method,
				Path:      r.URL.Path,
				Operation: operation,
				Detail:    detail,
			})
		}
	}

	if !a.currentReadOnlyMode() {
		a.writeProblem(w, r, http.StatusConflict, "Maintenance mode required",
			"Enable maintenance mode (Admin Settings) before restoring a snapshot, so no other request can write to the database while the file is replaced.")
		return
	}

	health := tinysql.HealthCheck(a.nativeDB)
	if strings.TrimSpace(health.Path) == "" {
		a.writeProblem(w, r, http.StatusBadRequest, "Nothing to restore into",
			"This instance is running with no backing file (pure in-memory storage); there is no on-disk snapshot to replace.")
		return
	}
	if !restorableStorageModes[health.ModeName] {
		a.writeProblem(w, r, http.StatusBadRequest, "Unsupported storage mode",
			fmt.Sprintf("Restore is only supported for the memory/wal storage modes, which use the same snapshot format as the export; this instance is running in %q mode.", health.ModeName))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxSnapshotRestoreBytes)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid upload",
			fmt.Sprintf("Could not read the upload (it may exceed the %d byte limit): %v", maxSnapshotRestoreBytes, err))
		return
	}
	if confirm := strings.TrimSpace(r.FormValue("confirm")); confirm != "RESTORE" {
		a.writeProblem(w, r, http.StatusBadRequest, "Confirmation required",
			`The form field "confirm" must be exactly "RESTORE".`)
		return
	}
	file, _, err := r.FormFile("snapshot")
	if err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Missing file", `The form field "snapshot" must contain the snapshot file.`)
		return
	}
	defer file.Close()

	targetDir := filepath.Dir(health.Path)
	tempFile, err := os.CreateTemp(targetDir, ".datadock-restore-*.tmp")
	if err != nil {
		a.writeProblem(w, r, http.StatusInternalServerError, "Restore failed", fmt.Sprintf("could not create staging file: %v", err))
		return
	}
	tempPath := tempFile.Name()
	cleanupTemp := func() {
		tempFile.Close()
		os.Remove(tempPath)
	}

	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(tempFile, hasher), file)
	if err != nil {
		cleanupTemp()
		a.writeProblem(w, r, http.StatusInternalServerError, "Restore failed", fmt.Sprintf("could not stage upload: %v", err))
		return
	}
	if err := tempFile.Close(); err != nil {
		os.Remove(tempPath)
		a.writeProblem(w, r, http.StatusInternalServerError, "Restore failed", fmt.Sprintf("could not finalize staging file: %v", err))
		return
	}
	checksum := hex.EncodeToString(hasher.Sum(nil))

	// Validate before touching anything live: a corrupt/truncated/wrong-format
	// upload must fail here, not after the real database file is gone.
	validationFile, err := os.Open(tempPath)
	if err != nil {
		os.Remove(tempPath)
		a.writeProblem(w, r, http.StatusInternalServerError, "Restore failed", fmt.Sprintf("could not reopen staged upload: %v", err))
		return
	}
	scratchDB, err := tinysql.LoadFromReader(validationFile)
	validationFile.Close()
	if err != nil {
		os.Remove(tempPath)
		logAttempt("snapshot_restore_failed", fmt.Sprintf("upload failed validation: %v", err))
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid snapshot", fmt.Sprintf("the uploaded file is not a valid DataDock snapshot: %v", err))
		return
	}
	if scratchDB != nil {
		_ = scratchDB.Close()
	}

	var backupPath string
	if _, statErr := os.Stat(health.Path); statErr == nil {
		backupPath = health.Path + ".pre-restore-" + time.Now().UTC().Format("20060102T150405.000000000Z")
		if err := copyFile(health.Path, backupPath); err != nil {
			os.Remove(tempPath)
			a.writeProblem(w, r, http.StatusInternalServerError, "Restore failed", fmt.Sprintf("could not back up the existing database file before replacing it: %v", err))
			return
		}
	}

	logAttempt("snapshot_restore_attempt", fmt.Sprintf("validated upload sha256=%s bytes=%d backup=%s", checksum, written, backupPath))

	if err := os.Rename(tempPath, health.Path); err != nil {
		logAttempt("snapshot_restore_failed", fmt.Sprintf("rename into place failed: %v", err))
		os.Remove(tempPath)
		a.writeProblem(w, r, http.StatusInternalServerError, "Restore failed", fmt.Sprintf("could not replace the live database file: %v", err))
		return
	}

	logAttempt("snapshot_restore", fmt.Sprintf("database file replaced sha256=%s bytes=%d backup=%s; restart required to load it", checksum, written, backupPath))

	a.writeJSON(w, http.StatusOK, map[string]any{
		"status":           "restored",
		"path":             health.Path,
		"backup_path":      backupPath,
		"sha256":           checksum,
		"bytes":            written,
		"restart_required": true,
		"detail":           "The on-disk database file has been validated and replaced. Restart DataDock to load the restored data.",
	})
}

// copyFile copies src to dst, used to back up the live database file before
// it is overwritten by a restore. Not atomic itself — the backup is a safety
// net for the operator, not part of the atomic swap (the rename is).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
