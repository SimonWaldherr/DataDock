package main

import (
	"io"
	"net/http"
	"strconv"
	"testing"
)

type discardXLSXResponse struct{}

func (discardXLSXResponse) Header() http.Header         { return http.Header{} }
func (discardXLSXResponse) WriteHeader(int)             {}
func (discardXLSXResponse) Write(p []byte) (int, error) { return io.Discard.Write(p) }

func BenchmarkWriteXLSX(b *testing.B) {
	columns := []string{"id", "name", "score", "description"}
	rows := make([][]string, 10000)
	for i := range rows {
		rows[i] = []string{strconv.Itoa(i), "Example customer", "42.5", "Some text with <tags> & special characters"}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := writeXLSX(discardXLSXResponse{}, columns, rows); err != nil {
			b.Fatal(err)
		}
	}
}
