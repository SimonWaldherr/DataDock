package main

import (
	"context"
	"testing"
)

func BenchmarkTinySQLQueryWindow(b *testing.B) {
	app := newBenchmarkApp(b, 10000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, rows, more, err := app.queryRowsWindow(context.Background(), "SELECT * FROM people", 0, 50)
		if err != nil || len(rows) != 50 || !more {
			b.Fatalf("rows=%d more=%v err=%v", len(rows), more, err)
		}
	}
}
