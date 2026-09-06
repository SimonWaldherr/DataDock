package typed

import (
	"reflect"
	"testing"
)

func TestClassifyConservativeTypes(t *testing.T) {
	tests := map[string]Kind{
		"123":                  KindInt,
		"-12":                  KindInt,
		"12.5":                 KindFloat,
		"2026-07-05":           KindDate,
		"2026-07-05T21:25:49Z": KindDateTime,
		"21:25:49":             KindTime,
		"true":                 KindBool,
		"00123":                KindText,
		"05/07/2026":           KindText,
		"1,25":                 KindText,
	}

	for input, want := range tests {
		if got := Classify(input).Kind; got != want {
			t.Fatalf("Classify(%q) = %s, want %s", input, got, want)
		}
	}
}

func TestInferColumnsSparseAndMixedValues(t *testing.T) {
	rows := [][]string{
		{"", "1", "001", "true", "2026-09-05", "1", "", "1.5"},
		nil,
		{"\u2003", "2.5", "2", "FALSE", "2026-09-06", "false", "", "2"},
		{"", "3", "3", "true", "2026-09-07", "2", "", "3.5", "extra"},
	}
	want := []Kind{KindText, KindFloat, KindText, KindBool, KindDate, KindText, KindText, KindFloat}
	if got := InferColumns(rows, len(want)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, kind := range want {
		var values []string
		for _, row := range rows {
			if i < len(row) {
				values = append(values, row[i])
			}
		}
		if got := InferColumn(values); got != kind {
			t.Errorf("column %d: got %v, want %v", i, got, kind)
		}
	}
	if got := InferColumns(nil, 2); !reflect.DeepEqual(got, []Kind{KindText, KindText}) {
		t.Fatalf("empty input: %v", got)
	}
	if got := InferColumns(rows, 0); len(got) != 0 {
		t.Fatalf("zero columns: %v", got)
	}
}

func TestJSONValuePreservesTextAndBlankSemantics(t *testing.T) {
	for _, kind := range []Kind{KindText, KindDate, KindDateTime, KindTime, KindInt, KindFloat, KindBool} {
		for _, blank := range []string{"", " \t\n", "\u2003"} {
			if got := JSONValue(blank, kind); got != nil {
				t.Errorf("blank %q of kind %s: %v", blank, kind, got)
			}
		}
	}
	for _, tc := range []struct {
		raw  string
		kind Kind
		want any
	}{
		{"00123", KindText, "00123"},
		{" 42 ", KindText, " 42 "},
		{"TRUE", KindText, "TRUE"},
		{"2026-09-05", KindDate, "2026-09-05"},
		{" 42 ", KindInt, int64(42)},
		{"42", KindFloat, float64(42)},
		{"TRUE", KindBool, true},
		{"00123", KindInt, "00123"},
		{"n/a", KindFloat, "n/a"},
		{"false", KindInt, "false"},
	} {
		if got := JSONValue(tc.raw, tc.kind); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("JSONValue(%q, %s): %#v, want %#v", tc.raw, tc.kind, got, tc.want)
		}
	}
}

func TestInferColumnPromotesOnlyCompatibleValues(t *testing.T) {
	if got := InferColumn([]string{"1", "2.5", ""}); got != KindFloat {
		t.Fatalf("InferColumn numeric mix = %s, want %s", got, KindFloat)
	}
	if got := InferColumn([]string{"1", "n/a"}); got != KindText {
		t.Fatalf("InferColumn mixed text = %s, want %s", got, KindText)
	}
	if got := InferColumn([]string{"001", "002"}); got != KindText {
		t.Fatalf("InferColumn leading-zero ids = %s, want %s", got, KindText)
	}
}
