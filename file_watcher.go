package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	dbimporter "github.com/SimonWaldherr/datadock/internal/importer"
)

type watchedFileState struct {
	size    int64
	modTime time.Time
}

type watchedImportFailure struct {
	state watchedFileState
	err   string
}

const maxAutoImportBytes int64 = 64 << 20

func startAutoImportWatcher(ctx context.Context, app *App, dir string, interval time.Duration) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return
	}
	if interval <= 0 {
		interval = 3 * time.Second
	}
	go func() {
		log.Printf("auto-import watcher started: dir=%s interval=%s", dir, interval)
		seen := make(map[string]watchedFileState)
		failures := make(map[string]watchedImportFailure)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			scanAutoImportDir(ctx, app, dir, seen, failures)
			select {
			case <-ctx.Done():
				log.Printf("auto-import watcher stopped: dir=%s", dir)
				return
			case <-ticker.C:
			}
		}
	}()
}

func scanAutoImportDir(ctx context.Context, app *App, dir string, seen map[string]watchedFileState, failures map[string]watchedImportFailure) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("auto-import watcher: read %s: %v", dir, err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if !isAutoImportFile(path) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			log.Printf("auto-import watcher: stat %s: %v", path, err)
			continue
		}
		state := watchedFileState{size: info.Size(), modTime: info.ModTime()}
		if prev, ok := seen[path]; ok && prev == state {
			continue
		}
		if err := autoImportFile(ctx, app, path); err != nil {
			failure := watchedImportFailure{state: state, err: err.Error()}
			if previous, ok := failures[path]; !ok || previous != failure {
				log.Printf("auto-import watcher: import %s: %v", path, err)
			}
			failures[path] = failure
			continue
		}
		seen[path] = state
		delete(failures, path)
	}
}

func isAutoImportFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".csv", ".tsv", ".json", ".ndjson", ".xml", ".yaml", ".yml", ".toml", ".ini", ".cfg", ".conf", ".fwf", ".txt", ".xlsx", ".geojson", ".gpkg", ".gpx", ".kml", ".osm", ".pbf", ".mbtiles", ".pmtiles", ".rg", ".zip", ".shp", ".sqlite", ".sqlite3", ".db", ".duckdb", ".parquet", ".arrow", ".feather", ".html", ".htm", ".msgpack", ".mpack", ".msg", ".cbor", ".bson", ".ics", ".ical", ".vcf", ".vcard":
		return true
	default:
		return false
	}
}

func readAutoImportContent(src io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("auto-import size limit must be positive")
	}
	content, err := io.ReadAll(io.LimitReader(src, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > maxBytes {
		return nil, fmt.Errorf("auto-import file exceeds the %d MiB limit", maxBytes>>20)
	}
	return content, nil
}

func autoImportFile(ctx context.Context, app *App, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	content, err := readAutoImportContent(file, maxAutoImportBytes)
	if err != nil {
		return err
	}
	table := sanitizeIdentifier(tableNameFromFilename(filepath.Base(path)))
	if table == "" || !isValidIdentifier(table) {
		return fmt.Errorf("invalid table name derived from %s", filepath.Base(path))
	}
	format := importFormatFromName(path, "")
	if format == "xlsx" {
		content, err = xlsxToCSV(content)
		if err != nil {
			return fmt.Errorf("xlsx: %w", err)
		}
		format = "csv"
	}
	opts := &dbimporter.ImportOptions{
		CreateTable:   true,
		Truncate:      true,
		HeaderMode:    "auto",
		TypeInference: true,
	}
	if format == "tsv" {
		opts.DelimiterCandidates = []rune{'\t'}
		format = "csv"
	}
	importCtx, cancel := app.withQueryTimeout(ctx)
	defer cancel()
	res, err := importContent(importCtx, app.nativeDB, app.tenant, table, format, bytes.NewReader(content), opts)
	if err != nil {
		return err
	}
	log.Printf("auto-import watcher: imported %d rows into %s from %s", res.RowsInserted, table, path)
	return nil
}
