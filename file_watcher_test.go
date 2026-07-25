package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAutoImportFileRecognizesEveryDocumentedConfigAndFixedWidthExtension(t *testing.T) {
	tests := []struct {
		name   string
		format string
	}{
		{name: "inventory.toml", format: "toml"},
		{name: "service.ini", format: "ini"},
		{name: "service.cfg", format: "cfg"},
		{name: "service.conf", format: "conf"},
		{name: "ledger.fwf", format: "fwf"},
		{name: "legacy.txt", format: "fwf"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !isAutoImportFile(tt.name) {
				t.Fatalf("%s is not eligible for auto import", tt.name)
			}
			if got := importFormatFromName(tt.name, ""); got != tt.format {
				t.Fatalf("import format = %q, want %q", got, tt.format)
			}
		})
	}

	if isAutoImportFile("report.pdf") {
		t.Fatal("unsupported files must not be watched")
	}
}

func TestAutoImportFileSupportsDocumentedConfigAndFixedWidthFormats(t *testing.T) {
	app := newTestApp(t)
	dir := t.TempDir()
	tests := []struct {
		name    string
		content string
		query   string
		want    []string
	}{
		{
			name:    "watched_toml.toml",
			content: "name = \"server-1\"\nport = 8080\n",
			query:   "SELECT name, port FROM watched_toml",
			want:    []string{"server-1", "8080"},
		},
		{
			name:    "watched_ini.ini",
			content: "[database]\nhost = localhost\nport = 5432\n",
			query:   "SELECT section, host, port FROM watched_ini",
			want:    []string{"database", "localhost", "5432"},
		},
		{
			name:    "watched_fixed.fwf",
			content: "id   name       score\n1    Ada        98.5 \n",
			query:   "SELECT id, name, score FROM watched_fixed",
			want:    []string{"1", "Ada", "98.5"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, tt.name)
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("write %s: %v", tt.name, err)
			}
			if err := autoImportFile(context.Background(), app, path); err != nil {
				t.Fatalf("auto import %s: %v", tt.name, err)
			}
			result := app.executeTinySQL(context.Background(), tt.query)
			if result.Err != "" {
				t.Fatalf("query imported %s: %s", tt.name, result.Err)
			}
			if len(result.Rows) != 1 || strings.Join(result.Rows[0], "|") != strings.Join(tt.want, "|") {
				t.Fatalf("imported rows = %#v, want %#v", result.Rows, tt.want)
			}
		})
	}
}

func TestAutoImportFileRefreshesExistingTable(t *testing.T) {
	app := newTestApp(t)
	path := filepath.Join(t.TempDir(), "watched_people.csv")

	if err := os.WriteFile(path, []byte("id,name\n1,Ada\n"), 0o600); err != nil {
		t.Fatalf("write initial file: %v", err)
	}
	if err := autoImportFile(context.Background(), app, path); err != nil {
		t.Fatalf("import initial file: %v", err)
	}

	if err := os.WriteFile(path, []byte("id,name\n2,Grace\n"), 0o600); err != nil {
		t.Fatalf("write replacement file: %v", err)
	}
	if err := autoImportFile(context.Background(), app, path); err != nil {
		t.Fatalf("refresh imported file: %v", err)
	}

	result := app.executeTinySQL(context.Background(), "SELECT id, name FROM watched_people")
	if result.Err != "" {
		t.Fatalf("query refreshed table: %s", result.Err)
	}
	if len(result.Rows) != 1 || strings.Join(result.Rows[0], "|") != "2|Grace" {
		t.Fatalf("refreshed rows = %#v, want only Grace", result.Rows)
	}
}

func TestScanAutoImportDirRetriesUnchangedFailedState(t *testing.T) {
	app := newTestApp(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "watched_retry.toml")
	if err := os.WriteFile(path, []byte("name = ?\n"), 0o600); err != nil {
		t.Fatalf("write invalid watched file: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat invalid watched file: %v", err)
	}

	seen := make(map[string]watchedFileState)
	failures := make(map[string]watchedImportFailure)
	scanAutoImportDir(context.Background(), app, dir, seen, failures)
	if len(seen) != 0 || len(failures) != 1 {
		t.Fatalf("failed import must remain retryable, seen=%#v failures=%#v", seen, failures)
	}

	if err := os.WriteFile(path, []byte("name = 1\n"), 0o600); err != nil {
		t.Fatalf("write corrected watched file: %v", err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatalf("restore watched file timestamp: %v", err)
	}
	scanAutoImportDir(context.Background(), app, dir, seen, failures)
	if len(seen) != 1 || len(failures) != 0 {
		t.Fatalf("successful retry must record the file, seen=%#v failures=%#v", seen, failures)
	}
	result := app.executeTinySQL(context.Background(), "SELECT name FROM watched_retry")
	if result.Err != "" || len(result.Rows) != 1 || result.Rows[0][0] != "1" {
		t.Fatalf("retry result = %#v, err=%q", result.Rows, result.Err)
	}
}

func TestReadAutoImportContentRejectsOversizedFiles(t *testing.T) {
	_, err := readAutoImportContent(strings.NewReader("12345"), 4)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected explicit size-limit error, got %v", err)
	}

	content, err := readAutoImportContent(strings.NewReader("1234"), 4)
	if err != nil || string(content) != "1234" {
		t.Fatalf("expected exact limit to succeed, got %q, %v", content, err)
	}
}
