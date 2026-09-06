package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestTinySQLStreamWindowMatchesFullResults(t *testing.T) {
	app := newWindowTestApp(t)
	ctx := context.Background()
	for _, query := range []string{
		"SELECT * FROM people",
		"SELECT name, id FROM people ORDER BY id DESC",
		"SELECT COUNT(*) AS total FROM people",
		"SELECT NULL AS missing, '00123' AS code, true AS active FROM people",
		"SELECT * FROM people WHERE id < 0",
	} {
		cols, rows, err := app.queryTinySQLRows(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		for _, window := range [][2]int{{0, 1}, {1, 1}, {0, len(rows)}, {len(rows), 1}, {100, 2}, {-1, 0}} {
			wantCols, wantRows, wantMore := sliceRowsWindow(cols, rows, window[0], window[1])
			gotCols, gotRows, more, err := app.queryRowsWindow(ctx, query, window[0], window[1])
			if err != nil || !reflect.DeepEqual(gotCols, wantCols) || !reflect.DeepEqual(gotRows, wantRows) || more != wantMore {
				t.Fatalf("%s window %v: cols=%v rows=%v more=%v err=%v; want cols=%v rows=%v more=%v", query, window, gotCols, gotRows, more, err, wantCols, wantRows, wantMore)
			}
		}
	}
	// A page abandoned before EOF must release the producer before a write.
	if _, _, _, err := app.queryRowsWindow(ctx, "SELECT * FROM people", 0, 1); err != nil {
		t.Fatal(err)
	}
	writeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if _, err := app.sqlDB.ExecContext(writeCtx, "INSERT INTO people (id, name) VALUES (99, 'After stream')"); err != nil {
		t.Fatalf("write after early stream close: %v", err)
	}
}

func TestTinySQLStreamWindowErrors(t *testing.T) {
	app := newWindowTestApp(t)
	if _, _, _, err := app.queryRowsWindow(context.Background(), "SELECT * FROM missing_table", 0, 1); err == nil {
		t.Fatal("expected query error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, _, err := app.queryRowsWindow(ctx, "SELECT * FROM people", 0, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func newWindowTestApp(t *testing.T) *App {
	t.Helper()
	app := newTestApp(t)
	for _, query := range []string{
		"CREATE TABLE people (id INT, name TEXT)",
		"INSERT INTO people VALUES (1, 'Alice'), (2, 'Bob'), (3, 'Carol')",
	} {
		if _, err := app.sqlDB.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	return app
}
