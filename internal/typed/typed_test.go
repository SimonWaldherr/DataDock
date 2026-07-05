package typed

import "testing"

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
