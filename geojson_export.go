package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/SimonWaldherr/datadock/internal/standards"
	"github.com/SimonWaldherr/datadock/internal/typed"
)

type geoJSONFeatureCollection struct {
	Type     string           `json:"type"`
	Features []geoJSONFeature `json:"features"`
}

type geoJSONFeature struct {
	Type       string         `json:"type"`
	Geometry   map[string]any `json:"geometry"`
	Properties map[string]any `json:"properties,omitempty"`
}

var (
	geoJSONGeometryNames = map[string]bool{
		"point":              true,
		"multipoint":         true,
		"linestring":         true,
		"multilinestring":    true,
		"polygon":            true,
		"multipolygon":       true,
		"geometrycollection": true,
	}
	lonColumnRE      = regexp.MustCompile(`(?i)^(lon|lng|long|longitude|x)$`)
	latColumnRE      = regexp.MustCompile(`(?i)^(lat|latitude|y)$`)
	geometryColumnRE = regexp.MustCompile(`(?i)(geojson|geometry|geom|shape)`)
)

func writeGeoJSONExport(w http.ResponseWriter, columns []string, rows [][]string, kinds []typed.Kind, filenameBase string) error {
	w.Header().Set("Content-Type", standards.MediaTypeGeoJSON)
	w.Header().Set("Content-Disposition", exportContentDisposition(filenameBase, "geojson"))
	return json.NewEncoder(w).Encode(buildGeoJSONFeatureCollection(columns, rows, kinds))
}

func buildGeoJSONFeatureCollection(columns []string, rows [][]string, kinds []typed.Kind) geoJSONFeatureCollection {
	fc := geoJSONFeatureCollection{Type: "FeatureCollection"}
	geometryIdx := detectGeometryColumn(columns, rows)
	lonIdx, latIdx := detectLonLatColumns(columns)

	for _, row := range rows {
		props := rowProperties(columns, row, kinds, geometryIdx, lonIdx, latIdx)
		if geometryIdx >= 0 && geometryIdx < len(row) {
			fc.Features = append(fc.Features, featuresFromGeoJSONCell(row[geometryIdx], props)...)
			continue
		}
		if lonIdx >= 0 && latIdx >= 0 && lonIdx < len(row) && latIdx < len(row) {
			lon, lonOK := parseGeoFloat(row[lonIdx])
			lat, latOK := parseGeoFloat(row[latIdx])
			if lonOK && latOK && lon >= -180 && lon <= 180 && lat >= -90 && lat <= 90 {
				fc.Features = append(fc.Features, geoJSONFeature{
					Type: "Feature",
					Geometry: map[string]any{
						"type":        "Point",
						"coordinates": []float64{lon, lat},
					},
					Properties: props,
				})
			}
		}
	}
	return fc
}

func detectGeometryColumn(columns []string, rows [][]string) int {
	bestIdx := -1
	bestScore := 0
	for idx, col := range columns {
		score := 0
		if geometryColumnRE.MatchString(col) {
			score += 2
		}
		for _, row := range rows {
			if idx >= len(row) || strings.TrimSpace(row[idx]) == "" {
				continue
			}
			if len(featuresFromGeoJSONCell(row[idx], nil)) > 0 {
				score += 3
			}
		}
		if score > bestScore {
			bestScore = score
			bestIdx = idx
		}
	}
	if bestScore < 3 {
		return -1
	}
	return bestIdx
}

func detectLonLatColumns(columns []string) (int, int) {
	lonIdx, latIdx := -1, -1
	for idx, col := range columns {
		name := strings.TrimSpace(col)
		if lonIdx < 0 && lonColumnRE.MatchString(name) {
			lonIdx = idx
		}
		if latIdx < 0 && latColumnRE.MatchString(name) {
			latIdx = idx
		}
	}
	return lonIdx, latIdx
}

func rowProperties(columns []string, row []string, kinds []typed.Kind, skip ...int) map[string]any {
	skipSet := map[int]bool{}
	for _, idx := range skip {
		if idx >= 0 {
			skipSet[idx] = true
		}
	}
	props := map[string]any{}
	for idx, col := range columns {
		if skipSet[idx] {
			continue
		}
		value := ""
		if idx < len(row) {
			value = row[idx]
		}
		kind := typed.KindText
		if idx < len(kinds) {
			kind = kinds[idx]
		}
		props[col] = typed.JSONValue(value, kind)
	}
	return props
}

func featuresFromGeoJSONCell(raw string, rowProps map[string]any) []geoJSONFeature {
	obj, ok := parseGeoJSONObject(raw)
	if !ok {
		return nil
	}
	return featuresFromGeoJSONObject(obj, rowProps)
}

func featuresFromGeoJSONObject(obj map[string]any, rowProps map[string]any) []geoJSONFeature {
	typ := strings.ToLower(stringValue(obj["type"]))
	switch typ {
	case "featurecollection":
		var out []geoJSONFeature
		if features, ok := obj["features"].([]any); ok {
			for _, item := range features {
				if featureObj, ok := item.(map[string]any); ok {
					out = append(out, featuresFromGeoJSONObject(featureObj, rowProps)...)
				}
			}
		}
		return out
	case "feature":
		geometry, _ := obj["geometry"].(map[string]any)
		if !isGeoJSONGeometry(geometry) {
			return nil
		}
		props := mergeProperties(rowProps, mapValue(obj["properties"]))
		return []geoJSONFeature{{Type: "Feature", Geometry: geometry, Properties: props}}
	default:
		if !isGeoJSONGeometry(obj) {
			return nil
		}
		return []geoJSONFeature{{Type: "Feature", Geometry: obj, Properties: rowProps}}
	}
}

func parseGeoJSONObject(raw string) (map[string]any, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.HasPrefix(raw, "{") {
		return nil, false
	}
	var obj map[string]any
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return nil, false
	}
	return normalizeJSONNumbers(obj).(map[string]any), true
}

func isGeoJSONGeometry(obj map[string]any) bool {
	if obj == nil {
		return false
	}
	return geoJSONGeometryNames[strings.ToLower(stringValue(obj["type"]))]
}

func mergeProperties(first, second map[string]any) map[string]any {
	if len(first) == 0 && len(second) == 0 {
		return nil
	}
	out := map[string]any{}
	for k, v := range first {
		out[k] = v
	}
	for k, v := range second {
		out[k] = v
	}
	return out
}

func mapValue(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func parseGeoFloat(raw string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	return f, err == nil
}

func normalizeJSONNumbers(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			x[k] = normalizeJSONNumbers(child)
		}
		return x
	case []any:
		for i, child := range x {
			x[i] = normalizeJSONNumbers(child)
		}
		return x
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	default:
		return v
	}
}
