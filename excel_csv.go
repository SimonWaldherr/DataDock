package main

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/SimonWaldherr/datadock/internal/standards"
	"github.com/SimonWaldherr/datadock/internal/typed"
)

var (
	excelLeadingZeroRE  = regexp.MustCompile(`^[+-]?0[0-9]+$`)
	excelLongDigitsRE   = regexp.MustCompile(`^[0-9]{15,}$`)
	excelLocalDateLike  = regexp.MustCompile(`^[0-9]{1,2}[./-][0-9]{1,2}([./-][0-9]{2,4})?$`)
	excelFormulaStartRE = regexp.MustCompile(`^[=+\-@]`)
)

func writeExcelCSV(w http.ResponseWriter, columns []string, rows [][]string, kinds []typed.Kind, filenameBase string) error {
	w.Header().Set("Content-Type", standards.MediaTypeCSV)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.excel.csv"`, filenameBase))
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})
	if _, err := fmt.Fprintln(w, "sep=,"); err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(columns); err != nil {
		return err
	}
	for _, row := range rows {
		out := make([]string, len(columns))
		for i := range columns {
			value := ""
			if i < len(row) {
				value = row[i]
			}
			kind := typed.KindText
			if i < len(kinds) {
				kind = kinds[i]
			}
			out[i] = excelCSVCell(value, kind)
		}
		if err := cw.Write(out); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func excelCSVCell(value string, kind typed.Kind) string {
	v := typed.Classify(value)
	if v.Kind == typed.KindBlank {
		return ""
	}
	trimmed := strings.TrimSpace(value)
	switch kind {
	case typed.KindInt:
		if v.Kind == typed.KindInt {
			return trimmed
		}
	case typed.KindFloat:
		if v.Kind == typed.KindInt || v.Kind == typed.KindFloat {
			return trimmed
		}
	case typed.KindBool:
		if v.Kind == typed.KindBool {
			return strings.ToUpper(trimmed)
		}
	case typed.KindDate:
		if v.Kind == typed.KindDate {
			return v.Time.Format("2006-01-02")
		}
	case typed.KindTime:
		if v.Kind == typed.KindTime {
			return v.Time.Format("15:04:05")
		}
	case typed.KindDateTime:
		if v.Kind == typed.KindDateTime {
			return v.Time.Format("2006-01-02 15:04:05")
		}
	}
	if excelTextNeedsGuard(trimmed) {
		return `="` + strings.ReplaceAll(value, `"`, `""`) + `"`
	}
	return value
}

func excelTextNeedsGuard(value string) bool {
	if value == "" {
		return false
	}
	if excelFormulaStartRE.MatchString(value) ||
		excelLeadingZeroRE.MatchString(value) ||
		excelLongDigitsRE.MatchString(value) ||
		excelLocalDateLike.MatchString(value) {
		return true
	}
	switch typed.Classify(value).Kind {
	case typed.KindInt, typed.KindFloat, typed.KindDate, typed.KindTime, typed.KindDateTime:
		return true
	default:
		return false
	}
}
