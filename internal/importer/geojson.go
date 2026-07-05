package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

type geoJSONImportFeature struct {
	Geometry   map[string]any
	Properties map[string]any
}

func ImportGeoJSON(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	dec := json.NewDecoder(src)
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	root = normalizeImportJSONNumbers(root).(map[string]any)
	features := importGeoJSONFeatures(root, nil)
	if len(features) == 0 {
		return nil, fmt.Errorf("geojson contains no importable features")
	}

	propKeys := collectGeoJSONPropertyKeys(features)
	columnNames := makeUniqueGeoJSONColumns(propKeys)
	records := make([][]string, 0, len(features)+1)
	header := append([]string{"geometry", "geometry_type"}, columnNames...)
	records = append(records, header)
	for _, feature := range features {
		geometryText := ""
		geometryType := ""
		if feature.Geometry != nil {
			if body, err := json.Marshal(feature.Geometry); err == nil {
				geometryText = string(body)
			}
			geometryType = geoJSONString(feature.Geometry["type"])
		}
		row := []string{geometryText, geometryType}
		for _, key := range propKeys {
			row = append(row, geoJSONPropertyString(feature.Properties[key]))
		}
		records = append(records, row)
	}

	importOpts := normalizeOptions(opts)
	localOpts := *importOpts
	localOpts.HeaderMode = "present"
	return importRecords(ctx, db, tenant, tableName, records, ',', &localOpts)
}

func importGeoJSONFeatures(obj map[string]any, inheritedProps map[string]any) []geoJSONImportFeature {
	switch strings.ToLower(geoJSONString(obj["type"])) {
	case "featurecollection":
		var out []geoJSONImportFeature
		if features, ok := obj["features"].([]any); ok {
			for _, item := range features {
				if child, ok := item.(map[string]any); ok {
					out = append(out, importGeoJSONFeatures(child, inheritedProps)...)
				}
			}
		}
		return out
	case "feature":
		props := mergeImportProperties(inheritedProps, geoJSONMap(obj["properties"]))
		geometry := geoJSONMap(obj["geometry"])
		if strings.EqualFold(geoJSONString(geometry["type"]), "GeometryCollection") {
			return importGeoJSONGeometryCollection(geometry, props)
		}
		if !isImportGeometry(geometry) {
			return nil
		}
		return []geoJSONImportFeature{{Geometry: geometry, Properties: props}}
	case "geometrycollection":
		return importGeoJSONGeometryCollection(obj, inheritedProps)
	default:
		if !isImportGeometry(obj) {
			return nil
		}
		return []geoJSONImportFeature{{Geometry: obj, Properties: inheritedProps}}
	}
}

func importGeoJSONGeometryCollection(obj map[string]any, props map[string]any) []geoJSONImportFeature {
	var out []geoJSONImportFeature
	if geometries, ok := obj["geometries"].([]any); ok {
		for _, item := range geometries {
			if child, ok := item.(map[string]any); ok && isImportGeometry(child) {
				out = append(out, geoJSONImportFeature{Geometry: child, Properties: props})
			}
		}
	}
	return out
}

func isImportGeometry(obj map[string]any) bool {
	switch strings.ToLower(geoJSONString(obj["type"])) {
	case "point", "multipoint", "linestring", "multilinestring", "polygon", "multipolygon":
		return true
	default:
		return false
	}
}

func collectGeoJSONPropertyKeys(features []geoJSONImportFeature) []string {
	seen := map[string]bool{}
	var keys []string
	for _, feature := range features {
		for key := range feature.Properties {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

func makeUniqueGeoJSONColumns(keys []string) []string {
	used := map[string]bool{"geometry": true, "geometry_type": true}
	columns := make([]string, len(keys))
	for i, key := range keys {
		base := sanitizeIdentifier(key, fmt.Sprintf("prop_%d", i+1))
		if used[strings.ToLower(base)] {
			base = "prop_" + base
		}
		name := base
		for suffix := 2; used[strings.ToLower(name)]; suffix++ {
			name = fmt.Sprintf("%s_%d", base, suffix)
		}
		used[strings.ToLower(name)] = true
		columns[i] = name
	}
	return columns
}

func geoJSONPropertyString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int64, int, float64, float32:
		return fmt.Sprint(x)
	default:
		body, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(body)
	}
}

func mergeImportProperties(first, second map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range first {
		out[k] = v
	}
	for k, v := range second {
		out[k] = v
	}
	return out
}

func geoJSONMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func geoJSONString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func normalizeImportJSONNumbers(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			x[k] = normalizeImportJSONNumbers(child)
		}
		return x
	case []any:
		for i, child := range x {
			x[i] = normalizeImportJSONNumbers(child)
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
