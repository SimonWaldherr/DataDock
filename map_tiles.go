package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// mapTileLayer is the durable, table-backed description of an imported tile
// archive. The tile rows are normalized to XYZ at the HTTP boundary, so
// callers never need to know whether the original archive used TMS (MBTiles)
// or XYZ (PMTiles) row addressing.
type mapTileLayer struct {
	Table           string
	SourceFormat    string
	TileScheme      string
	Format          string
	TileCompression string
	Metadata        map[string]string
	MinZoom         int
	MaxZoom         int
	Bounds          []float64
	VectorLayers    []map[string]any
}

func (a *App) isMapTileTable(ctx context.Context, table string) bool {
	if conn := a.activeConn(ctx); conn == nil || !conn.IsTinySQL() {
		return false
	}
	_, err := a.loadMapTileLayer(ctx, table)
	return err == nil
}

func (a *App) loadMapTileLayer(ctx context.Context, table string) (mapTileLayer, error) {
	if !isValidIdentifier(table) || isDataDockSystemObject(table) {
		return mapTileLayer{}, fmt.Errorf("invalid tile table")
	}
	rows, err := a.sqlDB.QueryContext(ctx, "SELECT name, value FROM "+quoteName(table)+" WHERE record_type = ?", "metadata")
	if err != nil {
		return mapTileLayer{}, err
	}
	defer rows.Close()
	layer := mapTileLayer{Table: table, Metadata: map[string]string{}}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return mapTileLayer{}, err
		}
		layer.Metadata[strings.ToLower(strings.TrimSpace(name))] = value
	}
	if err := rows.Err(); err != nil {
		return mapTileLayer{}, err
	}
	layer.SourceFormat = strings.ToLower(strings.TrimSpace(layer.Metadata["datadock:source_format"]))
	if layer.SourceFormat != "mbtiles" && layer.SourceFormat != "pmtiles" {
		return mapTileLayer{}, fmt.Errorf("table %q is not an imported MBTiles or PMTiles layer", table)
	}
	layer.TileScheme = strings.ToLower(strings.TrimSpace(layer.Metadata["datadock:tile_scheme"]))
	if layer.TileScheme == "" {
		layer.TileScheme = "tms"
	}
	layer.Format = strings.ToLower(strings.TrimSpace(layer.Metadata["format"]))
	layer.TileCompression = strings.ToLower(strings.TrimSpace(layer.Metadata["datadock:tile_compression"]))
	layer.MinZoom = mapMetadataInt(layer.Metadata, "minzoom", 0)
	layer.MaxZoom = mapMetadataInt(layer.Metadata, "maxzoom", 22)
	if layer.MaxZoom < layer.MinZoom {
		layer.MaxZoom = layer.MinZoom
	}
	layer.Bounds = mapMetadataBounds(layer.Metadata["bounds"])
	layer.VectorLayers = vectorLayersFromTileMetadata(layer.Metadata)
	if layer.Format == "" || layer.Format == "unknown" {
		format, layers := a.inferMapTileFormat(ctx, layer)
		if format != "" {
			layer.Format = format
		}
		if len(layer.VectorLayers) == 0 {
			layer.VectorLayers = layers
		}
	}
	if isMapVectorFormat(layer.Format) && len(layer.VectorLayers) == 0 {
		layer.VectorLayers = a.inferMapVectorLayers(ctx, layer)
	}
	return layer, nil
}

func mapMetadataInt(values map[string]string, key string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(values[key]))
	if err != nil || n < 0 || n > 30 {
		return fallback
	}
	return n
}

func mapMetadataBounds(raw string) []float64 {
	parts := strings.Split(raw, ",")
	if len(parts) != 4 {
		return nil
	}
	bounds := make([]float64, 4)
	for i, part := range parts {
		value, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return nil
		}
		bounds[i] = value
	}
	if bounds[0] > bounds[2] || bounds[1] > bounds[3] || bounds[0] < -180 || bounds[2] > 180 || bounds[1] < -90 || bounds[3] > 90 {
		return nil
	}
	return bounds
}

func vectorLayersFromTileMetadata(metadata map[string]string) []map[string]any {
	var raw any
	for _, key := range []string{"vector_layers", "json"} {
		text := strings.TrimSpace(metadata[key])
		if text == "" {
			continue
		}
		var value any
		if json.Unmarshal([]byte(text), &value) == nil {
			if object, ok := value.(map[string]any); ok && key == "json" {
				raw = object["vector_layers"]
			} else {
				raw = value
			}
		}
		if raw != nil {
			break
		}
	}
	items, _ := raw.([]any)
	layers := make([]map[string]any, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok || strings.TrimSpace(fmt.Sprint(object["id"])) == "" {
			continue
		}
		layers = append(layers, object)
	}
	return layers
}

func isMapVectorFormat(format string) bool {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "pbf", "mvt", "vector", "application/x-protobuf":
		return true
	default:
		return false
	}
}

// inferMapVectorLayers keeps standard MBTiles and PMTiles archives usable
// even when they omit TileJSON's optional vector_layers metadata. MVT layer
// names live in the top-level protobuf Tile message, so this needs no full
// geometry decoder or style-specific assumptions.
func (a *App) inferMapVectorLayers(ctx context.Context, layer mapTileLayer) []map[string]any {
	payload, err := a.sampleMapTilePayload(ctx, layer)
	if err != nil {
		return nil
	}
	names := mvtLayerNames(payload)
	layers := make([]map[string]any, 0, len(names))
	for _, name := range names {
		layers = append(layers, map[string]any{"id": name})
	}
	return layers
}

func (a *App) inferMapTileFormat(ctx context.Context, layer mapTileLayer) (string, []map[string]any) {
	payload, err := a.sampleMapTilePayload(ctx, layer)
	if err != nil {
		return "", nil
	}
	if names := mvtLayerNames(payload); len(names) > 0 {
		layers := make([]map[string]any, 0, len(names))
		for _, name := range names {
			layers = append(layers, map[string]any{"id": name})
		}
		return "pbf", layers
	}
	if len(payload) >= 8 && string(payload[:8]) == "\x89PNG\r\n\x1a\n" {
		return "png", nil
	}
	if len(payload) >= 3 && payload[0] == 0xff && payload[1] == 0xd8 && payload[2] == 0xff {
		return "jpg", nil
	}
	if len(payload) >= 12 && string(payload[:4]) == "RIFF" && string(payload[8:12]) == "WEBP" {
		return "webp", nil
	}
	return "", nil
}

func (a *App) sampleMapTilePayload(ctx context.Context, layer mapTileLayer) ([]byte, error) {
	var encoded string
	if err := a.sqlDB.QueryRowContext(ctx, "SELECT tile_data_base64 FROM "+quoteName(layer.Table)+" WHERE record_type = ? LIMIT 1", "tile").Scan(&encoded); err != nil {
		return nil, err
	}
	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	return decompressMapTilePayload(payload, layer.TileCompression)
}

// maxDecompressedTileBytes bounds a single tile's decompressed size. This
// path runs on every tile request the map viewer makes, not just at import
// time, so a small stored payload that decompresses to gigabytes would be
// a repeatable per-request resource exhaustion, not a one-time import cost.
const maxDecompressedTileBytes = 64 << 20 // 64 MiB

func decompressMapTilePayload(payload []byte, compression string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(compression)) {
	case "", "none":
		return payload, nil
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		decoded, err := readLimitedImport(reader, maxDecompressedTileBytes)
		closeErr := reader.Close()
		if err != nil {
			return nil, err
		}
		return decoded, closeErr
	case "br":
		return readLimitedImport(brotli.NewReader(bytes.NewReader(payload)), maxDecompressedTileBytes)
	case "zstd":
		reader, err := zstd.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		decoded, err := readLimitedImport(reader, maxDecompressedTileBytes)
		reader.Close()
		return decoded, err
	default:
		return nil, fmt.Errorf("unsupported tile compression %q", compression)
	}
}

func mvtLayerNames(data []byte) []string {
	seen := map[string]bool{}
	var names []string
	for pos := 0; pos < len(data); {
		key, ok := readMVTVarint(data, &pos)
		if !ok {
			return names
		}
		field, wire := int(key>>3), int(key&7)
		if field == 3 && wire == 2 {
			layer, ok := readMVTBytes(data, &pos)
			if !ok {
				return names
			}
			if name := mvtLayerName(layer); name != "" && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
			continue
		}
		if !skipMVTField(data, &pos, wire) {
			return names
		}
	}
	return names
}

func mvtLayerName(data []byte) string {
	for pos := 0; pos < len(data); {
		key, ok := readMVTVarint(data, &pos)
		if !ok {
			return ""
		}
		field, wire := int(key>>3), int(key&7)
		if field == 1 && wire == 2 {
			name, ok := readMVTBytes(data, &pos)
			if !ok {
				return ""
			}
			return string(name)
		}
		if !skipMVTField(data, &pos, wire) {
			return ""
		}
	}
	return ""
}

func readMVTVarint(data []byte, pos *int) (uint64, bool) {
	var value uint64
	for shift := uint(0); shift < 64; shift += 7 {
		if *pos >= len(data) {
			return 0, false
		}
		b := data[*pos]
		*pos++
		value |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return value, true
		}
	}
	return 0, false
}

func readMVTBytes(data []byte, pos *int) ([]byte, bool) {
	length, ok := readMVTVarint(data, pos)
	if !ok || length > uint64(len(data)-*pos) {
		return nil, false
	}
	start := *pos
	*pos += int(length)
	return data[start:*pos], true
}

func skipMVTField(data []byte, pos *int, wire int) bool {
	switch wire {
	case 0:
		_, ok := readMVTVarint(data, pos)
		return ok
	case 1:
		*pos += 8
	case 2:
		_, ok := readMVTBytes(data, pos)
		return ok
	case 5:
		*pos += 4
	default:
		return false
	}
	return *pos <= len(data)
}

func (a *App) mapLayerViewHandler(w http.ResponseWriter, r *http.Request) {
	table := r.PathValue("table")
	if !a.canBrowseTableName(r, table) {
		a.renderObjectMissing(w, r, table, fmt.Errorf("table %q not found", table))
		return
	}
	if conn := a.activeConn(r.Context()); conn == nil || !conn.IsTinySQL() {
		a.renderObjectMissing(w, r, table, fmt.Errorf("map tile layers are available only from the local tinySQL database"))
		return
	}
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	layer, err := a.loadMapTileLayer(ctx, table)
	if err != nil {
		a.renderObjectMissing(w, r, table, err)
		return
	}
	a.render(w, r, "map_layer", map[string]any{
		"Table":       table,
		"Layer":       layer,
		"TileJSONURL": "/api/map/tiles/" + table + "/tilejson",
	})
}

func (a *App) apiMapTileJSONHandler(w http.ResponseWriter, r *http.Request) {
	if conn := a.activeConn(r.Context()); conn == nil || !conn.IsTinySQL() {
		a.writeProblem(w, r, http.StatusBadRequest, "Unsupported connection", "map tile layers are available only from the local tinySQL database")
		return
	}
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	layer, err := a.loadMapTileLayer(ctx, r.PathValue("table"))
	if err != nil {
		a.writeProblem(w, r, http.StatusNotFound, "Map layer not found", err.Error())
		return
	}
	response := map[string]any{
		"tilejson": "3.0.0",
		"name":     mapTileName(layer),
		"scheme":   "xyz",
		"tiles":    []string{"/api/map/tiles/" + layer.Table + "/{z}/{x}/{y}"},
		"minzoom":  layer.MinZoom,
		"maxzoom":  layer.MaxZoom,
		"format":   layer.Format,
	}
	if len(layer.Bounds) == 4 {
		response["bounds"] = layer.Bounds
	}
	if len(layer.VectorLayers) > 0 {
		response["vector_layers"] = layer.VectorLayers
	}
	if attribution := strings.TrimSpace(layer.Metadata["attribution"]); attribution != "" {
		response["attribution"] = attribution
	}
	a.writeJSON(w, http.StatusOK, response)
}

func mapTileName(layer mapTileLayer) string {
	if name := strings.TrimSpace(layer.Metadata["name"]); name != "" {
		return name
	}
	return layer.Table
}

func (a *App) apiMapTileHandler(w http.ResponseWriter, r *http.Request) {
	if conn := a.activeConn(r.Context()); conn == nil || !conn.IsTinySQL() {
		a.writeProblem(w, r, http.StatusBadRequest, "Unsupported connection", "map tile layers are available only from the local tinySQL database")
		return
	}
	z, x, y, ok := parseMapTileCoordinates(r)
	if !ok {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid tile coordinates", "z, x, and y must identify a valid XYZ tile")
		return
	}
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	layer, err := a.loadMapTileLayer(ctx, r.PathValue("table"))
	if err != nil {
		a.writeProblem(w, r, http.StatusNotFound, "Map layer not found", err.Error())
		return
	}
	storedY := y
	if layer.TileScheme == "tms" {
		storedY = (1 << uint(z)) - 1 - y
	}
	var encoded string
	err = a.sqlDB.QueryRowContext(ctx,
		"SELECT tile_data_base64 FROM "+quoteName(layer.Table)+" WHERE record_type = ? AND zoom_level = ? AND tile_column = ? AND tile_row = ? LIMIT 1",
		"tile", z, x, storedY,
	).Scan(&encoded)
	if err != nil {
		a.writeProblem(w, r, http.StatusNotFound, "Tile not found", "the requested tile is not present in this layer")
		return
	}
	payload, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		a.writeProblem(w, r, http.StatusInternalServerError, "Invalid tile payload", "the stored tile payload cannot be decoded")
		return
	}
	w.Header().Set("Content-Type", mapTileContentType(layer.Format, payload))
	if encoding := mapTileContentEncoding(layer.TileCompression); encoding != "" {
		w.Header().Set("Content-Encoding", encoding)
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	_, _ = w.Write(payload)
}

func parseMapTileCoordinates(r *http.Request) (int, int, int, bool) {
	z, errZ := strconv.Atoi(r.PathValue("z"))
	x, errX := strconv.Atoi(r.PathValue("x"))
	y, errY := strconv.Atoi(r.PathValue("y"))
	if errZ != nil || errX != nil || errY != nil || z < 0 || z > 30 || x < 0 || y < 0 {
		return 0, 0, 0, false
	}
	limit := int64(1) << uint(z)
	if int64(x) >= limit || int64(y) >= limit {
		return 0, 0, 0, false
	}
	return z, x, y, true
}

func mapTileContentType(format string, payload []byte) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "pbf", "mvt", "vector", "application/x-protobuf":
		return "application/vnd.mapbox-vector-tile"
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	case "avif":
		return "image/avif"
	}
	if len(payload) >= 8 && string(payload[:8]) == "\x89PNG\r\n\x1a\n" {
		return "image/png"
	}
	if len(payload) >= 3 && payload[0] == 0xff && payload[1] == 0xd8 && payload[2] == 0xff {
		return "image/jpeg"
	}
	return "application/octet-stream"
}

func mapTileContentEncoding(compression string) string {
	switch strings.ToLower(strings.TrimSpace(compression)) {
	case "gzip", "br", "zstd":
		return compression
	default:
		return ""
	}
}
