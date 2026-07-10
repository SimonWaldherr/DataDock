package importer

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	tinysql "github.com/SimonWaldherr/tinySQL"
	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

const pmtilesHeaderSize = 127

// ImportPMTiles reads a PMTiles v3 archive into the same portable tile table
// shape used for MBTiles. Keeping tiles in tinySQL makes the layer available
// after restart and lets the HTTP layer serve both source formats uniformly.
func ImportPMTiles(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	records, err := pmtilesRecords(data)
	if err != nil {
		return nil, err
	}
	return importSpatialRecords(ctx, db, tenant, tableName, records, opts)
}

type pmtilesHeader struct {
	RootOffset, RootLength         uint64
	MetadataOffset, MetadataLength uint64
	LeafOffset, LeafLength         uint64
	TileOffset, TileLength         uint64
	TileCompression                byte
	InternalCompression            byte
	TileType                       byte
	MinZoom, MaxZoom               byte
	MinLonE7, MinLatE7             int32
	MaxLonE7, MaxLatE7             int32
}

type pmtilesDirEntry struct {
	TileID    uint64
	RunLength uint64
	Length    uint64
	Offset    uint64
}

func pmtilesRecords(data []byte) ([][]string, error) {
	header, err := parsePMTilesHeader(data)
	if err != nil {
		return nil, err
	}
	root, err := pmtilesRange(data, header.RootOffset, header.RootLength)
	if err != nil {
		return nil, fmt.Errorf("read PMTiles root directory: %w", err)
	}
	entries, err := decodePMTilesDirectory(root, header.InternalCompression)
	if err != nil {
		return nil, fmt.Errorf("decode PMTiles root directory: %w", err)
	}

	records := [][]string{{"record_type", "name", "value", "zoom_level", "tile_column", "tile_row", "tile_size", "tile_sha256", "tile_data_base64"}}
	metadata, err := pmtilesMetadata(data, header)
	if err != nil {
		return nil, err
	}
	metadata["datadock:source_format"] = "pmtiles"
	metadata["datadock:tile_scheme"] = "xyz"
	metadata["datadock:tile_compression"] = pmtilesCompressionName(header.TileCompression)
	if _, ok := metadata["format"]; !ok {
		metadata["format"] = pmtilesTileTypeName(header.TileType)
	}
	if _, ok := metadata["minzoom"]; !ok {
		metadata["minzoom"] = strconv.Itoa(int(header.MinZoom))
	}
	if _, ok := metadata["maxzoom"]; !ok {
		metadata["maxzoom"] = strconv.Itoa(int(header.MaxZoom))
	}
	if _, ok := metadata["bounds"]; !ok && header.MinLonE7 <= header.MaxLonE7 && header.MinLatE7 <= header.MaxLatE7 {
		metadata["bounds"] = strings.Join([]string{
			strconv.FormatFloat(float64(header.MinLonE7)/1e7, 'f', -1, 64),
			strconv.FormatFloat(float64(header.MinLatE7)/1e7, 'f', -1, 64),
			strconv.FormatFloat(float64(header.MaxLonE7)/1e7, 'f', -1, 64),
			strconv.FormatFloat(float64(header.MaxLatE7)/1e7, 'f', -1, 64),
		}, ",")
	}
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		records = append(records, []string{"metadata", key, metadata[key], "", "", "", "", "", ""})
	}

	seenDirectories := map[uint64]bool{}
	var appendEntries func([]pmtilesDirEntry) error
	appendEntries = func(dir []pmtilesDirEntry) error {
		for _, entry := range dir {
			if entry.RunLength == 0 {
				if seenDirectories[entry.Offset] {
					continue
				}
				seenDirectories[entry.Offset] = true
				leaf, err := pmtilesRange(data, header.LeafOffset+entry.Offset, entry.Length)
				if err != nil {
					return fmt.Errorf("read PMTiles leaf directory: %w", err)
				}
				children, err := decodePMTilesDirectory(leaf, header.InternalCompression)
				if err != nil {
					return fmt.Errorf("decode PMTiles leaf directory: %w", err)
				}
				if err := appendEntries(children); err != nil {
					return err
				}
				continue
			}
			tile, err := pmtilesRange(data, header.TileOffset+entry.Offset, entry.Length)
			if err != nil {
				return fmt.Errorf("read PMTiles tile data: %w", err)
			}
			hash := sha256.Sum256(tile)
			encoded := base64.StdEncoding.EncodeToString(tile)
			for offset := uint64(0); offset < entry.RunLength; offset++ {
				z, x, y, ok := pmtilesIDToZXY(entry.TileID + offset)
				if !ok {
					return fmt.Errorf("invalid PMTiles tile id %d", entry.TileID+offset)
				}
				records = append(records, []string{
					"tile", "", "", strconv.Itoa(z), strconv.FormatUint(uint64(x), 10), strconv.FormatUint(uint64(y), 10),
					strconv.Itoa(len(tile)), fmt.Sprintf("%x", hash[:]), encoded,
				})
			}
		}
		return nil
	}
	if err := appendEntries(entries); err != nil {
		return nil, err
	}
	if len(records) == 1+len(metadata) {
		return nil, fmt.Errorf("PMTiles contains no tiles")
	}
	return records, nil
}

func parsePMTilesHeader(data []byte) (pmtilesHeader, error) {
	if len(data) < pmtilesHeaderSize || string(data[:7]) != "PMTiles" {
		return pmtilesHeader{}, fmt.Errorf("not a PMTiles archive")
	}
	if data[7] != 3 {
		return pmtilesHeader{}, fmt.Errorf("unsupported PMTiles version %d (only v3 is supported)", data[7])
	}
	read64 := func(offset int) uint64 { return binary.LittleEndian.Uint64(data[offset : offset+8]) }
	read32 := func(offset int) int32 { return int32(binary.LittleEndian.Uint32(data[offset : offset+4])) }
	return pmtilesHeader{
		RootOffset:          read64(8),
		RootLength:          read64(16),
		MetadataOffset:      read64(24),
		MetadataLength:      read64(32),
		LeafOffset:          read64(40),
		LeafLength:          read64(48),
		TileOffset:          read64(56),
		TileLength:          read64(64),
		InternalCompression: data[97],
		TileCompression:     data[98],
		TileType:            data[99],
		MinZoom:             data[100],
		MaxZoom:             data[101],
		MinLonE7:            read32(102),
		MinLatE7:            read32(106),
		MaxLonE7:            read32(110),
		MaxLatE7:            read32(114),
	}, nil
}

func pmtilesMetadata(data []byte, header pmtilesHeader) (map[string]string, error) {
	raw, err := pmtilesRange(data, header.MetadataOffset, header.MetadataLength)
	if err != nil {
		return nil, fmt.Errorf("read PMTiles metadata: %w", err)
	}
	if len(raw) == 0 {
		return map[string]string{}, nil
	}
	decoded, err := pmtilesDecompress(raw, header.InternalCompression)
	if err != nil {
		return nil, fmt.Errorf("decode PMTiles metadata: %w", err)
	}
	var values map[string]any
	if err := json.Unmarshal(decoded, &values); err != nil {
		return nil, fmt.Errorf("decode PMTiles metadata JSON: %w", err)
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		if text, ok := value.(string); ok {
			out[key] = text
			continue
		}
		body, err := json.Marshal(value)
		if err != nil {
			out[key] = fmt.Sprint(value)
			continue
		}
		out[key] = string(body)
	}
	return out, nil
}

func decodePMTilesDirectory(raw []byte, compression byte) ([]pmtilesDirEntry, error) {
	data, err := pmtilesDecompress(raw, compression)
	if err != nil {
		return nil, err
	}
	pos := 0
	count, err := pmtilesVarint(data, &pos)
	if err != nil {
		return nil, err
	}
	if count > uint64(len(data)) {
		return nil, fmt.Errorf("invalid directory entry count %d", count)
	}
	entries := make([]pmtilesDirEntry, count)
	var lastID uint64
	for i := range entries {
		delta, err := pmtilesVarint(data, &pos)
		if err != nil {
			return nil, err
		}
		lastID += delta
		entries[i].TileID = lastID
	}
	for i := range entries {
		v, err := pmtilesVarint(data, &pos)
		if err != nil {
			return nil, err
		}
		entries[i].RunLength = v
	}
	for i := range entries {
		v, err := pmtilesVarint(data, &pos)
		if err != nil {
			return nil, err
		}
		entries[i].Length = v
	}
	var previousOffset, previousLength uint64
	for i := range entries {
		v, err := pmtilesVarint(data, &pos)
		if err != nil {
			return nil, err
		}
		if v == 0 && i > 0 {
			entries[i].Offset = previousOffset + previousLength
		} else if v > 0 {
			entries[i].Offset = v - 1
		} else {
			return nil, fmt.Errorf("first PMTiles directory offset is zero")
		}
		previousOffset = entries[i].Offset
		previousLength = entries[i].Length
	}
	return entries, nil
}

func pmtilesRange(data []byte, offset, length uint64) ([]byte, error) {
	if offset > uint64(len(data)) || length > uint64(len(data))-offset {
		return nil, io.ErrUnexpectedEOF
	}
	return data[offset : offset+length], nil
}

func pmtilesVarint(data []byte, pos *int) (uint64, error) {
	var value uint64
	for shift := uint(0); ; shift += 7 {
		if *pos >= len(data) || shift >= 64 {
			return 0, fmt.Errorf("invalid PMTiles varint")
		}
		b := data[*pos]
		*pos++
		value |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return value, nil
		}
	}
}

func pmtilesDecompress(data []byte, compression byte) ([]byte, error) {
	switch compression {
	case 1: // none
		return data, nil
	case 2: // gzip
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		decompressed, err := io.ReadAll(reader)
		closeErr := reader.Close()
		if err != nil {
			return nil, err
		}
		return decompressed, closeErr
	case 3: // brotli
		return io.ReadAll(brotli.NewReader(bytes.NewReader(data)))
	case 4: // zstd
		reader, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		decompressed, err := io.ReadAll(reader)
		reader.Close()
		return decompressed, err
	default:
		return nil, fmt.Errorf("unsupported PMTiles compression %d", compression)
	}
}

func pmtilesCompressionName(compression byte) string {
	switch compression {
	case 1:
		return "none"
	case 2:
		return "gzip"
	case 3:
		return "br"
	case 4:
		return "zstd"
	default:
		return "unknown"
	}
}

func pmtilesTileTypeName(tileType byte) string {
	switch tileType {
	case 1:
		return "pbf"
	case 2:
		return "png"
	case 3:
		return "jpg"
	case 4:
		return "webp"
	case 5:
		return "avif"
	default:
		return "unknown"
	}
}

func pmtilesIDToZXY(id uint64) (int, uint32, uint32, bool) {
	zoom := 0
	for zoom < 31 {
		nextBase := (uint64(1)<<uint(2*(zoom+1)) - 1) / 3
		if id < nextBase {
			break
		}
		zoom++
	}
	if zoom >= 31 {
		return 0, 0, 0, false
	}
	base := (uint64(1)<<uint(2*zoom) - 1) / 3
	if id < base {
		return 0, 0, 0, false
	}
	d := id - base
	limit := uint64(1) << uint(2*zoom)
	if d >= limit {
		return 0, 0, 0, false
	}
	var x, y uint32
	for scale, t := uint32(1), d; scale < uint32(1)<<uint(zoom); scale, t = scale<<1, t>>2 {
		rx := uint32((t >> 1) & 1)
		ry := uint32((t ^ uint64(rx)) & 1)
		if ry == 0 {
			if rx == 1 {
				x = scale - 1 - x
				y = scale - 1 - y
			}
			x, y = y, x
		}
		x += scale * rx
		y += scale * ry
	}
	return zoom, x, y, true
}
