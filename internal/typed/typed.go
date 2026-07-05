package typed

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Kind string

const (
	KindBlank    Kind = "blank"
	KindText     Kind = "text"
	KindInt      Kind = "int"
	KindFloat    Kind = "float"
	KindBool     Kind = "bool"
	KindDate     Kind = "date"
	KindTime     Kind = "time"
	KindDateTime Kind = "datetime"
)

type Value struct {
	Raw   string
	Kind  Kind
	Time  time.Time
	Int   int64
	Float float64
	Bool  bool
}

var (
	intRE      = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
	floatRE    = regexp.MustCompile(`^-?((0|[1-9][0-9]*)\.[0-9]+|[1-9][0-9]*[eE][+-]?[0-9]+|[1-9][0-9]*\.[0-9]+[eE][+-]?[0-9]+|0\.[0-9]+[eE][+-]?[0-9]+)$`)
	dateRE     = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	timeRE     = regexp.MustCompile(`^[0-9]{2}:[0-9]{2}(:[0-9]{2}(\.[0-9]+)?)?$`)
	dateTimeRE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}`)
)

// Classify uses intentionally conservative rules: only unambiguous ISO time
// formats and canonical dot-decimal numbers become typed values.
func Classify(raw string) Value {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Value{Raw: raw, Kind: KindBlank}
	}

	switch strings.ToLower(trimmed) {
	case "true":
		return Value{Raw: raw, Kind: KindBool, Bool: true}
	case "false":
		return Value{Raw: raw, Kind: KindBool}
	}

	if dateTimeRE.MatchString(trimmed) {
		for _, layout := range []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02 15:04:05.999999999",
			"2006-01-02 15:04:05",
			"2006-01-02T15:04:05.999999999",
			"2006-01-02T15:04:05",
		} {
			if t, err := time.Parse(layout, trimmed); err == nil {
				return Value{Raw: raw, Kind: KindDateTime, Time: t}
			}
		}
	}

	if dateRE.MatchString(trimmed) {
		if t, err := time.Parse("2006-01-02", trimmed); err == nil {
			return Value{Raw: raw, Kind: KindDate, Time: t}
		}
	}

	if timeRE.MatchString(trimmed) {
		for _, layout := range []string{"15:04:05.999999999", "15:04:05", "15:04"} {
			if t, err := time.Parse(layout, trimmed); err == nil {
				return Value{Raw: raw, Kind: KindTime, Time: t}
			}
		}
	}

	if intRE.MatchString(trimmed) {
		if i, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return Value{Raw: raw, Kind: KindInt, Int: i, Float: float64(i)}
		}
	}

	if floatRE.MatchString(trimmed) {
		if f, err := strconv.ParseFloat(trimmed, 64); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
			return Value{Raw: raw, Kind: KindFloat, Float: f}
		}
	}

	return Value{Raw: raw, Kind: KindText}
}

func InferColumn(values []string) Kind {
	kind := KindBlank
	for _, raw := range values {
		v := Classify(raw)
		if v.Kind == KindBlank {
			continue
		}
		if kind == KindBlank {
			kind = v.Kind
			continue
		}
		if kind == v.Kind {
			continue
		}
		if (kind == KindInt && v.Kind == KindFloat) || (kind == KindFloat && v.Kind == KindInt) {
			kind = KindFloat
			continue
		}
		return KindText
	}
	if kind == KindBlank {
		return KindText
	}
	return kind
}

func InferColumns(rows [][]string, columnCount int) []Kind {
	kinds := make([]Kind, columnCount)
	for col := 0; col < columnCount; col++ {
		values := make([]string, 0, len(rows))
		for _, row := range rows {
			if col < len(row) {
				values = append(values, row[col])
			} else {
				values = append(values, "")
			}
		}
		kinds[col] = InferColumn(values)
	}
	return kinds
}

func JSONValue(raw string, kind Kind) any {
	v := Classify(raw)
	if v.Kind == KindBlank {
		return nil
	}
	if kind != KindFloat && kind != KindInt && kind != KindBool {
		return raw
	}
	if kind == KindInt && v.Kind == KindInt {
		return v.Int
	}
	if kind == KindFloat && (v.Kind == KindFloat || v.Kind == KindInt) {
		return v.Float
	}
	if kind == KindBool && v.Kind == KindBool {
		return v.Bool
	}
	return raw
}

func ExcelSerial(v Value, kind Kind) (float64, bool) {
	if v.Kind == KindBlank {
		return 0, false
	}
	base := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	switch kind {
	case KindDate, KindDateTime:
		t := v.Time
		if t.Location() != time.UTC {
			t = t.UTC()
		}
		return t.Sub(base).Hours() / 24, true
	case KindTime:
		h, m, s := v.Time.Clock()
		seconds := h*3600 + m*60 + s
		return (float64(seconds) + float64(v.Time.Nanosecond())/1e9) / 86400, true
	default:
		return 0, false
	}
}
