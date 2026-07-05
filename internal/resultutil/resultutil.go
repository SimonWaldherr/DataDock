package resultutil

import (
	"fmt"
	"math/big"
	"sort"
	"strconv"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

type ValueCount struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

type ColumnProfile struct {
	Name      string       `json:"name"`
	Examples  []string     `json:"examples,omitempty"`
	TopValues []ValueCount `json:"top_values,omitempty"`
	Nulls     int          `json:"nulls"`
	Distinct  int          `json:"distinct"`
	Numeric   bool         `json:"numeric"`
	Sum       string       `json:"sum,omitempty"`
	Avg       string       `json:"avg,omitempty"`
}

type Summary struct {
	Columns    []string        `json:"columns"`
	Rows       [][]string      `json:"rows,omitempty"`
	RowCount   int             `json:"row_count"`
	Profiles   []ColumnProfile `json:"profiles,omitempty"`
	TotalRows  int             `json:"total_rows"`
	Summarized bool            `json:"summarized"`
	Profile    []ColumnProfile `json:"profile,omitempty"`
}

type SummaryOptions struct {
	MaxRows      int
	MaxExamples  int
	MaxTopValues int
}

func ResultSetToStringMatrix(rs *tinysql.ResultSet) ([]string, [][]string) {
	if rs == nil {
		return nil, nil
	}
	columns := append([]string(nil), rs.Cols...)
	rows := make([][]string, 0, len(rs.Rows))
	for _, row := range rs.Rows {
		out := make([]string, len(columns))
		for i, col := range columns {
			out[i] = fmt.Sprint(row[col])
		}
		rows = append(rows, out)
	}
	return columns, rows
}

func SummarizeMatrix(columns []string, rows [][]string, opts SummaryOptions) *Summary {
	if opts.MaxRows <= 0 || opts.MaxRows > len(rows) {
		opts.MaxRows = len(rows)
	}
	s := &Summary{
		Columns:    append([]string(nil), columns...),
		Rows:       append([][]string(nil), rows[:opts.MaxRows]...),
		RowCount:   len(rows),
		TotalRows:  len(rows),
		Summarized: len(rows) > opts.MaxRows,
		Profiles:   make([]ColumnProfile, 0, len(columns)),
	}
	for i, col := range columns {
		counts := map[string]int{}
		profile := ColumnProfile{Name: col}
		sum := big.NewRat(0, 1)
		numericCount := 0
		nonEmptyCount := 0
		for _, row := range rows {
			if i >= len(row) || row[i] == "" {
				profile.Nulls++
				continue
			}
			v := row[i]
			nonEmptyCount++
			counts[v]++
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				sum.Add(sum, new(big.Rat).SetFloat64(f))
				numericCount++
			}
			if opts.MaxExamples <= 0 || len(profile.Examples) < opts.MaxExamples {
				profile.Examples = append(profile.Examples, v)
			}
		}
		profile.Distinct = len(counts)
		if nonEmptyCount > 0 && numericCount == nonEmptyCount {
			profile.Numeric = true
			profile.Sum = ratString(sum)
			profile.Avg = ratString(new(big.Rat).Quo(sum, big.NewRat(int64(numericCount), 1)))
		}
		values := make([]ValueCount, 0, len(counts))
		for value, count := range counts {
			values = append(values, ValueCount{Value: value, Count: count})
		}
		sort.Slice(values, func(i, j int) bool {
			if values[i].Count == values[j].Count {
				return values[i].Value < values[j].Value
			}
			return values[i].Count > values[j].Count
		})
		if opts.MaxTopValues > 0 && len(values) > opts.MaxTopValues {
			values = values[:opts.MaxTopValues]
		}
		profile.TopValues = values
		s.Profiles = append(s.Profiles, profile)
	}
	s.Profile = s.Profiles
	return s
}

func ratString(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	f, _ := r.Float64()
	return strconv.FormatFloat(f, 'f', -1, 64)
}
