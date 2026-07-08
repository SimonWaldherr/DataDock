package importer

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	tinysql "github.com/SimonWaldherr/tinySQL"
	"github.com/jonas-p/go-shp"
	_ "modernc.org/sqlite"
)

func ImportKML(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	placemarks, err := readKMLPlacemarks(src)
	if err != nil {
		return nil, err
	}
	if len(placemarks) == 0 {
		return nil, fmt.Errorf("kml contains no importable placemarks")
	}
	extraKeys := orderedStringKeysFromMaps(func(yield func(map[string]string)) {
		for _, p := range placemarks {
			yield(p.Extended)
		}
	})
	header := append([]string{"geometry", "geometry_type", "name", "description", "style_url"}, extraKeys...)
	records := [][]string{header}
	for _, p := range placemarks {
		geometryText := ""
		geometryType := ""
		if p.Geometry != nil {
			if body, err := json.Marshal(p.Geometry); err == nil {
				geometryText = string(body)
				geometryType = geoJSONString(p.Geometry["type"])
			}
		}
		row := []string{geometryText, geometryType, p.Name, p.Description, p.StyleURL}
		for _, key := range extraKeys {
			row = append(row, p.Extended[key])
		}
		records = append(records, row)
	}
	return importSpatialRecords(ctx, db, tenant, tableName, records, opts)
}

type kmlPlacemark struct {
	Name        string
	Description string
	StyleURL    string
	Extended    map[string]string
	Geometry    map[string]any
}

type kmlPlacemarkXML struct {
	Name        string            `xml:"name"`
	Description string            `xml:"description"`
	StyleURL    string            `xml:"styleUrl"`
	Point       *kmlPoint         `xml:"Point"`
	LineString  *kmlLineString    `xml:"LineString"`
	Polygon     *kmlPolygon       `xml:"Polygon"`
	Multi       *kmlMultiGeometry `xml:"MultiGeometry"`
	Extended    kmlExtendedData   `xml:"ExtendedData"`
}

type kmlPoint struct {
	Coordinates string `xml:"coordinates"`
}

type kmlLineString struct {
	Coordinates string `xml:"coordinates"`
}

type kmlPolygon struct {
	Outer kmlLinearRing   `xml:"outerBoundaryIs>LinearRing"`
	Inner []kmlLinearRing `xml:"innerBoundaryIs>LinearRing"`
}

type kmlLinearRing struct {
	Coordinates string `xml:"coordinates"`
}

type kmlMultiGeometry struct {
	Points      []kmlPoint      `xml:"Point"`
	LineStrings []kmlLineString `xml:"LineString"`
	Polygons    []kmlPolygon    `xml:"Polygon"`
}

type kmlExtendedData struct {
	Data       []kmlData       `xml:"Data"`
	SchemaData []kmlSchemaData `xml:"SchemaData"`
}

type kmlData struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value"`
}

type kmlSchemaData struct {
	SimpleData []kmlData `xml:"SimpleData"`
}

func readKMLPlacemarks(src io.Reader) ([]kmlPlacemark, error) {
	dec := xml.NewDecoder(src)
	var out []kmlPlacemark
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				return out, nil
			}
			return nil, err
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "Placemark" {
			continue
		}
		var raw kmlPlacemarkXML
		if err := dec.DecodeElement(&raw, &start); err != nil {
			return nil, err
		}
		geometry := raw.Geometry()
		if geometry == nil {
			continue
		}
		out = append(out, kmlPlacemark{
			Name:        strings.TrimSpace(raw.Name),
			Description: strings.TrimSpace(raw.Description),
			StyleURL:    strings.TrimSpace(raw.StyleURL),
			Extended:    raw.Extended.Properties(),
			Geometry:    geometry,
		})
	}
}

func (p kmlPlacemarkXML) Geometry() map[string]any {
	if p.Point != nil {
		return kmlPointGeometry(p.Point.Coordinates)
	}
	if p.LineString != nil {
		return kmlLineStringGeometry(p.LineString.Coordinates)
	}
	if p.Polygon != nil {
		return kmlPolygonGeometry(*p.Polygon)
	}
	if p.Multi != nil {
		var geometries []any
		for _, point := range p.Multi.Points {
			if geom := kmlPointGeometry(point.Coordinates); geom != nil {
				geometries = append(geometries, geom)
			}
		}
		for _, line := range p.Multi.LineStrings {
			if geom := kmlLineStringGeometry(line.Coordinates); geom != nil {
				geometries = append(geometries, geom)
			}
		}
		for _, polygon := range p.Multi.Polygons {
			if geom := kmlPolygonGeometry(polygon); geom != nil {
				geometries = append(geometries, geom)
			}
		}
		if len(geometries) > 0 {
			return map[string]any{"type": "GeometryCollection", "geometries": geometries}
		}
	}
	return nil
}

func (e kmlExtendedData) Properties() map[string]string {
	out := map[string]string{}
	for _, d := range e.Data {
		if key := strings.TrimSpace(d.Name); key != "" {
			out[key] = strings.TrimSpace(d.Value)
		}
	}
	for _, schema := range e.SchemaData {
		for _, d := range schema.SimpleData {
			if key := strings.TrimSpace(d.Name); key != "" {
				out[key] = strings.TrimSpace(d.Value)
			}
		}
	}
	return out
}

func kmlPointGeometry(raw string) map[string]any {
	coords := parseKMLCoordinates(raw)
	if len(coords) == 0 {
		return nil
	}
	return map[string]any{"type": "Point", "coordinates": coords[0]}
}

func kmlLineStringGeometry(raw string) map[string]any {
	coords := parseKMLCoordinates(raw)
	if len(coords) < 2 {
		return nil
	}
	return map[string]any{"type": "LineString", "coordinates": coords}
}

func kmlPolygonGeometry(poly kmlPolygon) map[string]any {
	outer := parseKMLCoordinates(poly.Outer.Coordinates)
	if len(outer) < 4 {
		return nil
	}
	rings := []any{outer}
	for _, inner := range poly.Inner {
		if ring := parseKMLCoordinates(inner.Coordinates); len(ring) >= 4 {
			rings = append(rings, ring)
		}
	}
	return map[string]any{"type": "Polygon", "coordinates": rings}
}

func parseKMLCoordinates(raw string) []any {
	var coords []any
	for _, item := range strings.Fields(raw) {
		parts := strings.Split(item, ",")
		if len(parts) < 2 {
			continue
		}
		lon, errLon := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		lat, errLat := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if errLon != nil || errLat != nil {
			continue
		}
		coord := []float64{lon, lat}
		if len(parts) >= 3 {
			if alt, err := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64); err == nil {
				coord = append(coord, alt)
			}
		}
		coords = append(coords, coord)
	}
	return coords
}

func ImportGPX(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	var doc gpxDoc
	dec := xml.NewDecoder(src)
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	records := [][]string{{"record_type", "name", "description", "time", "geometry", "geometry_type", "properties"}}
	for _, wpt := range doc.Waypoints {
		geom := pointGeometry(wpt.Lon, wpt.Lat)
		records = append(records, []string{"waypoint", strings.TrimSpace(wpt.Name), strings.TrimSpace(wpt.Description), strings.TrimSpace(wpt.Time), mustJSON(geom), "Point", mustJSON(wpt.Properties())})
	}
	for _, rte := range doc.Routes {
		coords := gpxCoords(rte.Points)
		if len(coords) >= 2 {
			geom := map[string]any{"type": "LineString", "coordinates": coords}
			records = append(records, []string{"route", strings.TrimSpace(rte.Name), strings.TrimSpace(rte.Description), "", mustJSON(geom), "LineString", mustJSON(rte.Properties())})
		}
	}
	for _, trk := range doc.Tracks {
		var lines []any
		for _, seg := range trk.Segments {
			if coords := gpxCoords(seg.Points); len(coords) >= 2 {
				lines = append(lines, coords)
			}
		}
		if len(lines) == 0 {
			continue
		}
		geomType := "LineString"
		geom := map[string]any{"type": geomType, "coordinates": lines[0]}
		if len(lines) > 1 {
			geomType = "MultiLineString"
			geom = map[string]any{"type": geomType, "coordinates": lines}
		}
		records = append(records, []string{"track", strings.TrimSpace(trk.Name), strings.TrimSpace(trk.Description), "", mustJSON(geom), geomType, mustJSON(trk.Properties())})
	}
	if len(records) == 1 {
		return nil, fmt.Errorf("gpx contains no importable waypoints, routes, or tracks")
	}
	return importSpatialRecords(ctx, db, tenant, tableName, records, opts)
}

type gpxDoc struct {
	Waypoints []gpxPoint `xml:"wpt"`
	Routes    []gpxRoute `xml:"rte"`
	Tracks    []gpxTrack `xml:"trk"`
}

type gpxPoint struct {
	Lat         float64 `xml:"lat,attr"`
	Lon         float64 `xml:"lon,attr"`
	Name        string  `xml:"name"`
	Description string  `xml:"desc"`
	Time        string  `xml:"time"`
	Elevation   string  `xml:"ele"`
}

type gpxRoute struct {
	Name        string     `xml:"name"`
	Description string     `xml:"desc"`
	Points      []gpxPoint `xml:"rtept"`
}

type gpxTrack struct {
	Name        string       `xml:"name"`
	Description string       `xml:"desc"`
	Segments    []gpxSegment `xml:"trkseg"`
}

type gpxSegment struct {
	Points []gpxPoint `xml:"trkpt"`
}

func (p gpxPoint) Properties() map[string]string {
	props := map[string]string{}
	if value := strings.TrimSpace(p.Elevation); value != "" {
		props["ele"] = value
	}
	return props
}

func (r gpxRoute) Properties() map[string]any {
	return map[string]any{"point_count": len(r.Points)}
}

func (t gpxTrack) Properties() map[string]any {
	n := 0
	for _, seg := range t.Segments {
		n += len(seg.Points)
	}
	return map[string]any{"segment_count": len(t.Segments), "point_count": n}
}

func gpxCoords(points []gpxPoint) []any {
	coords := make([]any, 0, len(points))
	for _, point := range points {
		coord := []float64{point.Lon, point.Lat}
		if ele, err := strconv.ParseFloat(strings.TrimSpace(point.Elevation), 64); err == nil {
			coord = append(coord, ele)
		}
		coords = append(coords, coord)
	}
	return coords
}

func ImportOSM(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	doc, err := readOSM(src)
	if err != nil {
		return nil, err
	}
	return importOSMDoc(ctx, db, tenant, tableName, doc, opts)
}

func ImportOSMPBF(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	doc, err := readOSMPBF(src)
	if err != nil {
		return nil, err
	}
	return importOSMDoc(ctx, db, tenant, tableName, doc, opts)
}

func importOSMDoc(ctx context.Context, db *tinysql.DB, tenant, tableName string, doc *osmDoc, opts *ImportOptions) (*ImportResult, error) {
	records := [][]string{{"record_type", "osm_id", "geometry", "geometry_type", "name", "highway", "amenity", "natural", "building", "tags"}}
	for _, node := range doc.Nodes {
		if len(node.Tags) == 0 {
			continue
		}
		records = append(records, osmRecord("node", node.ID, pointGeometry(node.Lon, node.Lat), node.Tags))
	}
	for _, way := range doc.Ways {
		if len(way.Tags) == 0 && len(way.Refs) < 2 {
			continue
		}
		coords := make([]any, 0, len(way.Refs))
		for _, ref := range way.Refs {
			if node, ok := doc.NodeIndex[ref]; ok {
				coords = append(coords, []float64{node.Lon, node.Lat})
			}
		}
		var geom map[string]any
		if len(coords) >= 4 && way.Refs[0] == way.Refs[len(way.Refs)-1] {
			geom = map[string]any{"type": "Polygon", "coordinates": []any{coords}}
		} else if len(coords) >= 2 {
			geom = map[string]any{"type": "LineString", "coordinates": coords}
		}
		records = append(records, osmRecord("way", way.ID, geom, way.Tags))
	}
	for _, rel := range doc.Relations {
		records = append(records, osmRecord("relation", rel.ID, nil, rel.Tags))
	}
	if len(records) == 1 {
		return nil, fmt.Errorf("osm contains no importable tagged nodes, ways, or relations")
	}
	return importSpatialRecords(ctx, db, tenant, tableName, records, opts)
}

type osmDoc struct {
	Nodes     []osmNode
	Ways      []osmWay
	Relations []osmRelation
	NodeIndex map[string]osmNode
}

type osmNode struct {
	ID   string
	Lat  float64
	Lon  float64
	Tags map[string]string
}

type osmWay struct {
	ID   string
	Refs []string
	Tags map[string]string
}

type osmRelation struct {
	ID   string
	Tags map[string]string
}

func readOSM(src io.Reader) (*osmDoc, error) {
	dec := xml.NewDecoder(src)
	doc := &osmDoc{NodeIndex: map[string]osmNode{}}
	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				return doc, nil
			}
			return nil, err
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "node":
			node, err := decodeOSMNode(dec, start)
			if err != nil {
				return nil, err
			}
			doc.Nodes = append(doc.Nodes, node)
			doc.NodeIndex[node.ID] = node
		case "way":
			way, err := decodeOSMWay(dec, start)
			if err != nil {
				return nil, err
			}
			doc.Ways = append(doc.Ways, way)
		case "relation":
			rel, err := decodeOSMRelation(dec, start)
			if err != nil {
				return nil, err
			}
			doc.Relations = append(doc.Relations, rel)
		}
	}
}

func decodeOSMNode(dec *xml.Decoder, start xml.StartElement) (osmNode, error) {
	node := osmNode{Tags: map[string]string{}}
	for _, attr := range start.Attr {
		switch attr.Name.Local {
		case "id":
			node.ID = attr.Value
		case "lat":
			node.Lat, _ = strconv.ParseFloat(attr.Value, 64)
		case "lon":
			node.Lon, _ = strconv.ParseFloat(attr.Value, 64)
		}
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return node, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "tag" {
				key, value := osmTagAttrs(t)
				if key != "" {
					node.Tags[key] = value
				}
			}
		case xml.EndElement:
			if t.Name.Local == start.Name.Local {
				return node, nil
			}
		}
	}
}

func decodeOSMWay(dec *xml.Decoder, start xml.StartElement) (osmWay, error) {
	way := osmWay{Tags: map[string]string{}}
	for _, attr := range start.Attr {
		if attr.Name.Local == "id" {
			way.ID = attr.Value
		}
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return way, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "nd":
				for _, attr := range t.Attr {
					if attr.Name.Local == "ref" {
						way.Refs = append(way.Refs, attr.Value)
					}
				}
			case "tag":
				key, value := osmTagAttrs(t)
				if key != "" {
					way.Tags[key] = value
				}
			}
		case xml.EndElement:
			if t.Name.Local == start.Name.Local {
				return way, nil
			}
		}
	}
}

func decodeOSMRelation(dec *xml.Decoder, start xml.StartElement) (osmRelation, error) {
	rel := osmRelation{Tags: map[string]string{}}
	for _, attr := range start.Attr {
		if attr.Name.Local == "id" {
			rel.ID = attr.Value
		}
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return rel, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "tag" {
				key, value := osmTagAttrs(t)
				if key != "" {
					rel.Tags[key] = value
				}
			}
		case xml.EndElement:
			if t.Name.Local == start.Name.Local {
				return rel, nil
			}
		}
	}
}

func osmTagAttrs(start xml.StartElement) (string, string) {
	var key, value string
	for _, attr := range start.Attr {
		switch attr.Name.Local {
		case "k":
			key = attr.Value
		case "v":
			value = attr.Value
		}
	}
	return key, value
}

func osmRecord(recordType, id string, geom map[string]any, tags map[string]string) []string {
	geometryText := ""
	geometryType := ""
	if geom != nil {
		if body, err := json.Marshal(geom); err == nil {
			geometryText = string(body)
			geometryType = geoJSONString(geom["type"])
		}
	}
	body, _ := json.Marshal(tags)
	return []string{recordType, id, geometryText, geometryType, tags["name"], tags["highway"], tags["amenity"], tags["natural"], tags["building"], string(body)}
}

func readOSMPBF(src io.Reader) (*osmDoc, error) {
	doc := &osmDoc{NodeIndex: map[string]osmNode{}}
	for {
		var sizeBytes [4]byte
		if _, err := io.ReadFull(src, sizeBytes[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return doc, nil
			}
			return nil, err
		}
		headerSize := binary.BigEndian.Uint32(sizeBytes[:])
		if headerSize == 0 || headerSize > 64<<20 {
			return nil, fmt.Errorf("invalid osm pbf blob header size %d", headerSize)
		}
		headerBody := make([]byte, headerSize)
		if _, err := io.ReadFull(src, headerBody); err != nil {
			return nil, err
		}
		header := parseProtoFields(headerBody)
		blobType := string(firstBytesField(header, 1))
		blobSize := int(firstVarintField(header, 3))
		if blobSize <= 0 || blobSize > 512<<20 {
			return nil, fmt.Errorf("invalid osm pbf blob size %d", blobSize)
		}
		blobBody := make([]byte, blobSize)
		if _, err := io.ReadFull(src, blobBody); err != nil {
			return nil, err
		}
		if blobType != "OSMData" {
			continue
		}
		data, err := pbfBlobData(blobBody)
		if err != nil {
			return nil, err
		}
		if err := readPBFPrimitiveBlock(data, doc); err != nil {
			return nil, err
		}
	}
}

func pbfBlobData(blobBody []byte) ([]byte, error) {
	fields := parseProtoFields(blobBody)
	if raw := firstBytesField(fields, 1); len(raw) > 0 {
		return raw, nil
	}
	compressed := firstBytesField(fields, 3)
	if len(compressed) == 0 {
		return nil, fmt.Errorf("osm pbf blob has no raw or zlib payload")
	}
	zr, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

func readPBFPrimitiveBlock(data []byte, doc *osmDoc) error {
	fields := parseProtoFields(data)
	stringsTable := pbfStringTable(firstBytesField(fields, 1))
	granularity := int64(firstVarintFieldDefault(fields, 17, 100))
	latOffset := int64(firstVarintField(fields, 19))
	lonOffset := int64(firstVarintField(fields, 20))
	for _, groupBody := range bytesFields(fields, 2) {
		group := parseProtoFields(groupBody)
		for _, nodeBody := range bytesFields(group, 1) {
			node := pbfNode(nodeBody, stringsTable, granularity, latOffset, lonOffset)
			doc.Nodes = append(doc.Nodes, node)
			doc.NodeIndex[node.ID] = node
		}
		if denseBody := firstBytesField(group, 2); len(denseBody) > 0 {
			nodes := pbfDenseNodes(denseBody, stringsTable, granularity, latOffset, lonOffset)
			for _, node := range nodes {
				doc.Nodes = append(doc.Nodes, node)
				doc.NodeIndex[node.ID] = node
			}
		}
		for _, wayBody := range bytesFields(group, 3) {
			doc.Ways = append(doc.Ways, pbfWay(wayBody, stringsTable))
		}
	}
	return nil
}

func pbfStringTable(data []byte) []string {
	var out []string
	for _, field := range parseProtoFields(data) {
		if field.num == 1 && field.wire == 2 {
			out = append(out, string(field.bytes))
		}
	}
	return out
}

func pbfNode(data []byte, stringsTable []string, granularity, latOffset, lonOffset int64) osmNode {
	fields := parseProtoFields(data)
	id := zigZag(firstVarintField(fields, 1))
	keys := packedVarints(bytes.Join(bytesFields(fields, 2), nil))
	vals := packedVarints(bytes.Join(bytesFields(fields, 3), nil))
	latRaw := zigZag(firstVarintField(fields, 8))
	lonRaw := zigZag(firstVarintField(fields, 9))
	return osmNode{
		ID:   strconv.FormatInt(id, 10),
		Lat:  pbfCoord(latRaw, granularity, latOffset),
		Lon:  pbfCoord(lonRaw, granularity, lonOffset),
		Tags: pbfTags(stringsTable, keys, vals),
	}
}

func pbfDenseNodes(data []byte, stringsTable []string, granularity, latOffset, lonOffset int64) []osmNode {
	fields := parseProtoFields(data)
	ids := packedSVarints(bytes.Join(bytesFields(fields, 1), nil))
	lats := packedSVarints(bytes.Join(bytesFields(fields, 8), nil))
	lons := packedSVarints(bytes.Join(bytesFields(fields, 9), nil))
	keysVals := packedVarints(bytes.Join(bytesFields(fields, 10), nil))
	var nodes []osmNode
	var id, latRaw, lonRaw int64
	tagPos := 0
	for i := range ids {
		id += ids[i]
		if i < len(lats) {
			latRaw += lats[i]
		}
		if i < len(lons) {
			lonRaw += lons[i]
		}
		tags := map[string]string{}
		for tagPos < len(keysVals) {
			keyID := keysVals[tagPos]
			tagPos++
			if keyID == 0 {
				break
			}
			if tagPos >= len(keysVals) {
				break
			}
			valID := keysVals[tagPos]
			tagPos++
			key := pbfString(stringsTable, int(keyID))
			if key != "" {
				tags[key] = pbfString(stringsTable, int(valID))
			}
		}
		node := osmNode{
			ID:   strconv.FormatInt(id, 10),
			Lat:  pbfCoord(latRaw, granularity, latOffset),
			Lon:  pbfCoord(lonRaw, granularity, lonOffset),
			Tags: tags,
		}
		nodes = append(nodes, node)
	}
	return nodes
}

func pbfWay(data []byte, stringsTable []string) osmWay {
	fields := parseProtoFields(data)
	id := int64(firstVarintField(fields, 1))
	keys := packedVarints(bytes.Join(bytesFields(fields, 2), nil))
	vals := packedVarints(bytes.Join(bytesFields(fields, 3), nil))
	refDeltas := packedSVarints(bytes.Join(bytesFields(fields, 8), nil))
	refs := make([]string, 0, len(refDeltas))
	var ref int64
	for _, delta := range refDeltas {
		ref += delta
		refs = append(refs, strconv.FormatInt(ref, 10))
	}
	return osmWay{ID: strconv.FormatInt(id, 10), Refs: refs, Tags: pbfTags(stringsTable, keys, vals)}
}

func pbfTags(stringsTable []string, keys, vals []uint64) map[string]string {
	tags := map[string]string{}
	for i, keyID := range keys {
		if i >= len(vals) {
			break
		}
		key := pbfString(stringsTable, int(keyID))
		if key != "" {
			tags[key] = pbfString(stringsTable, int(vals[i]))
		}
	}
	return tags
}

func pbfString(stringsTable []string, idx int) string {
	if idx < 0 || idx >= len(stringsTable) {
		return ""
	}
	return stringsTable[idx]
}

func pbfCoord(raw, granularity, offset int64) float64 {
	return float64(offset+granularity*raw) / 1e9
}

func ImportShapefileZip(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp("", "datadock-shapefile-*.zip")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	names, err := shp.ShapesInZip(tmpName)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("shapefile zip contains no .shp file")
	}
	reader, err := shp.OpenShapeFromZip(tmpName, names[0])
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	fields := reader.Fields()
	attrNames := make([]string, 0, len(fields))
	for _, field := range fields {
		name := strings.TrimSpace(field.String())
		if name != "" {
			attrNames = append(attrNames, name)
		}
	}
	header := append([]string{"geometry", "geometry_type", "shape_id", "shape_type"}, attrNames...)
	records := [][]string{header}
	for reader.Next() {
		idx, shape := reader.Shape()
		geom := geoJSONFromShape(shape)
		geometryText := ""
		geometryType := ""
		if geom != nil {
			if body, err := json.Marshal(geom); err == nil {
				geometryText = string(body)
				geometryType = geoJSONString(geom["type"])
			}
		}
		row := []string{geometryText, geometryType, strconv.Itoa(idx), shapeTypeName(shape)}
		for i := range attrNames {
			row = append(row, strings.TrimRight(reader.Attribute(i), "\x00 "))
		}
		records = append(records, row)
	}
	if err := reader.Err(); err != nil {
		return nil, err
	}
	if len(records) == 1 {
		return nil, fmt.Errorf("shapefile contains no records")
	}
	return importSpatialRecords(ctx, db, tenant, tableName, records, opts)
}

func geoJSONFromShape(shape shp.Shape) map[string]any {
	switch s := shape.(type) {
	case *shp.Point:
		return pointGeometry(s.X, s.Y)
	case *shp.PointZ:
		return pointGeometry(s.X, s.Y)
	case *shp.PointM:
		return pointGeometry(s.X, s.Y)
	case *shp.MultiPoint:
		return multiPointGeometry(s.Points)
	case *shp.MultiPointZ:
		return multiPointGeometry(s.Points)
	case *shp.MultiPointM:
		return multiPointGeometry(s.Points)
	case *shp.PolyLine:
		return lineGeometry(s.Parts, s.Points)
	case *shp.PolyLineZ:
		return lineGeometry(s.Parts, s.Points)
	case *shp.PolyLineM:
		return lineGeometry(s.Parts, s.Points)
	case *shp.Polygon:
		poly := shp.PolyLine(*s)
		return polygonGeometry(poly.Parts, poly.Points)
	case *shp.PolygonZ:
		poly := shp.PolyLineZ(*s)
		return polygonGeometry(poly.Parts, poly.Points)
	case *shp.PolygonM:
		poly := shp.PolyLineZ(*s)
		return polygonGeometry(poly.Parts, poly.Points)
	default:
		return nil
	}
}

func pointGeometry(lon, lat float64) map[string]any {
	return map[string]any{"type": "Point", "coordinates": []float64{lon, lat}}
}

func multiPointGeometry(points []shp.Point) map[string]any {
	coords := make([]any, 0, len(points))
	for _, point := range points {
		coords = append(coords, []float64{point.X, point.Y})
	}
	return map[string]any{"type": "MultiPoint", "coordinates": coords}
}

func lineGeometry(parts []int32, points []shp.Point) map[string]any {
	lines := pointsByParts(parts, points)
	if len(lines) == 0 {
		return nil
	}
	if len(lines) == 1 {
		return map[string]any{"type": "LineString", "coordinates": lines[0]}
	}
	return map[string]any{"type": "MultiLineString", "coordinates": lines}
}

func polygonGeometry(parts []int32, points []shp.Point) map[string]any {
	rings := pointsByParts(parts, points)
	if len(rings) == 0 {
		return nil
	}
	return map[string]any{"type": "Polygon", "coordinates": rings}
}

func pointsByParts(parts []int32, points []shp.Point) []any {
	if len(points) == 0 {
		return nil
	}
	if len(parts) == 0 {
		parts = []int32{0}
	}
	out := make([]any, 0, len(parts))
	for i, start := range parts {
		end := int32(len(points))
		if i+1 < len(parts) {
			end = parts[i+1]
		}
		if start < 0 || end > int32(len(points)) || start >= end {
			continue
		}
		line := make([]any, 0, end-start)
		for _, point := range points[start:end] {
			line = append(line, []float64{point.X, point.Y})
		}
		out = append(out, line)
	}
	return out
}

func shapeTypeName(shape shp.Shape) string {
	switch shape.(type) {
	case *shp.Point, *shp.PointZ, *shp.PointM:
		return "Point"
	case *shp.MultiPoint, *shp.MultiPointZ, *shp.MultiPointM:
		return "MultiPoint"
	case *shp.PolyLine, *shp.PolyLineZ, *shp.PolyLineM:
		return "PolyLine"
	case *shp.Polygon, *shp.PolygonZ, *shp.PolygonM:
		return "Polygon"
	default:
		return fmt.Sprintf("%T", shape)
	}
}

func ImportMBTiles(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp("", "datadock-mbtiles-*.mbtiles")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	sqlDB, err := sql.Open("sqlite", tmpName)
	if err != nil {
		return nil, err
	}
	defer sqlDB.Close()
	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, err
	}

	records := [][]string{{"record_type", "name", "value", "zoom_level", "tile_column", "tile_row", "tile_size", "tile_sha256", "tile_data_base64"}}
	if rows, err := sqlDB.QueryContext(ctx, "SELECT name, value FROM metadata ORDER BY name"); err == nil {
		for rows.Next() {
			var name, value string
			if err := rows.Scan(&name, &value); err == nil {
				records = append(records, []string{"metadata", name, value, "", "", "", "", "", ""})
			}
		}
		rows.Close()
	}
	rows, err := sqlDB.QueryContext(ctx, "SELECT zoom_level, tile_column, tile_row, tile_data FROM tiles ORDER BY zoom_level, tile_column, tile_row")
	if err != nil {
		return nil, fmt.Errorf("read mbtiles tiles: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var z, x, y int
		var tile []byte
		if err := rows.Scan(&z, &x, &y, &tile); err != nil {
			return nil, err
		}
		hash := sha256.Sum256(tile)
		tileData := ""
		if len(tile) <= 4096 {
			tileData = base64.StdEncoding.EncodeToString(tile)
		}
		records = append(records, []string{
			"tile", "", "", strconv.Itoa(z), strconv.Itoa(x), strconv.Itoa(y),
			strconv.Itoa(len(tile)), fmt.Sprintf("%x", hash[:]), tileData,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(records) == 1 {
		return nil, fmt.Errorf("mbtiles contains no metadata or tiles")
	}
	return importSpatialRecords(ctx, db, tenant, tableName, records, opts)
}

func ImportGeoPackage(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp("", "datadock-geopackage-*.gpkg")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	sqlDB, err := sql.Open("sqlite", tmpName)
	if err != nil {
		return nil, err
	}
	defer sqlDB.Close()
	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, err
	}
	layers, err := geopackageLayers(ctx, sqlDB)
	if err != nil {
		return nil, err
	}
	if len(layers) == 0 {
		return nil, fmt.Errorf("geopackage contains no feature layers")
	}
	records := [][]string{{"layer", "feature_id", "geometry", "geometry_type", "properties"}}
	for _, layer := range layers {
		rows, err := sqlDB.QueryContext(ctx, "SELECT * FROM "+quoteIdent(layer.Table))
		if err != nil {
			return nil, fmt.Errorf("read geopackage layer %q: %w", layer.Table, err)
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return nil, err
		}
		for rows.Next() {
			values := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return nil, err
			}
			props := map[string]any{}
			featureID := ""
			geometryText := ""
			geometryType := ""
			for i, col := range cols {
				value := values[i]
				if strings.EqualFold(col, layer.GeometryColumn) {
					if body, ok := value.([]byte); ok {
						if geom, err := geoPackageGeometry(body); err == nil && geom != nil {
							geometryText = mustJSON(geom)
							geometryType = geoJSONString(geom["type"])
						}
					}
					continue
				}
				if strings.EqualFold(col, "fid") || strings.EqualFold(col, "id") {
					featureID = fmt.Sprint(value)
				}
				props[col] = sqliteValue(value)
			}
			records = append(records, []string{layer.Table, featureID, geometryText, geometryType, mustJSON(props)})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	if len(records) == 1 {
		return nil, fmt.Errorf("geopackage contains no features")
	}
	return importSpatialRecords(ctx, db, tenant, tableName, records, opts)
}

type geopackageLayer struct {
	Table          string
	GeometryColumn string
}

func geopackageLayers(ctx context.Context, db *sql.DB) ([]geopackageLayer, error) {
	rows, err := db.QueryContext(ctx, `SELECT table_name, column_name FROM gpkg_geometry_columns ORDER BY table_name, column_name`)
	if err == nil {
		defer rows.Close()
		var layers []geopackageLayer
		for rows.Next() {
			var layer geopackageLayer
			if err := rows.Scan(&layer.Table, &layer.GeometryColumn); err != nil {
				return nil, err
			}
			if layer.Table != "" && layer.GeometryColumn != "" {
				layers = append(layers, layer)
			}
		}
		return layers, rows.Err()
	}
	rows, err = db.QueryContext(ctx, `SELECT table_name FROM gpkg_contents WHERE data_type = 'features' ORDER BY table_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var layers []geopackageLayer
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, err
		}
		layers = append(layers, geopackageLayer{Table: table, GeometryColumn: "geom"})
	}
	return layers, rows.Err()
}

func sqliteValue(v any) any {
	switch x := v.(type) {
	case []byte:
		return string(x)
	default:
		return x
	}
}

func ImportRoutingGraph(ctx context.Context, db *tinysql.DB, tenant, tableName string, src io.Reader, opts *ImportOptions) (*ImportResult, error) {
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, err
	}
	records, err := routingGraphRecords(data)
	if err != nil {
		return nil, err
	}
	return importSpatialRecords(ctx, db, tenant, tableName, records, opts)
}

func routingGraphRecords(data []byte) ([][]string, error) {
	header := []string{"record_type", "id", "from_id", "to_id", "lat", "lon", "cost", "distance", "geometry", "geometry_type", "properties"}
	records := [][]string{header}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("routing graph is empty")
	}
	if trimmed[0] == '[' {
		var items []map[string]any
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.UseNumber()
		if err := dec.Decode(&items); err != nil {
			return nil, err
		}
		for _, obj := range items {
			obj = normalizeImportJSONNumbers(obj).(map[string]any)
			if _, ok := obj["from"]; ok {
				records = append(records, routingGraphEdgeRecord(obj))
			} else if _, ok := obj["to"]; ok {
				records = append(records, routingGraphEdgeRecord(obj))
			} else {
				records = append(records, routingGraphNodeRecord(obj))
			}
		}
	} else if trimmed[0] == '{' {
		var root map[string]any
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.UseNumber()
		if err := dec.Decode(&root); err != nil {
			return nil, err
		}
		root = normalizeImportJSONNumbers(root).(map[string]any)
		for _, node := range graphObjects(root["nodes"]) {
			records = append(records, routingGraphNodeRecord(node))
		}
		for _, edge := range graphObjects(root["edges"]) {
			records = append(records, routingGraphEdgeRecord(edge))
		}
	} else {
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.UseNumber()
		for {
			var obj map[string]any
			if err := dec.Decode(&obj); err != nil {
				if err == io.EOF {
					break
				}
				return nil, err
			}
			obj = normalizeImportJSONNumbers(obj).(map[string]any)
			switch strings.ToLower(geoJSONString(obj["type"])) {
			case "node":
				records = append(records, routingGraphNodeRecord(obj))
			case "edge", "link":
				records = append(records, routingGraphEdgeRecord(obj))
			default:
				if _, ok := obj["from"]; ok {
					records = append(records, routingGraphEdgeRecord(obj))
				} else {
					records = append(records, routingGraphNodeRecord(obj))
				}
			}
		}
	}
	if len(records) == 1 {
		return nil, fmt.Errorf("routing graph contains no nodes or edges")
	}
	return records, nil
}

func graphObjects(v any) []map[string]any {
	items, _ := v.([]any)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if obj, ok := item.(map[string]any); ok {
			out = append(out, obj)
		}
	}
	return out
}

func routingGraphNodeRecord(obj map[string]any) []string {
	id := graphString(obj, "id", "node_id")
	lat, lon := graphLatLon(obj)
	geom := pointGeometry(lon, lat)
	geometryText := mustJSON(geom)
	return []string{"node", id, "", "", graphFloatString(lat), graphFloatString(lon), "", "", geometryText, "Point", mustJSON(obj)}
}

func routingGraphEdgeRecord(obj map[string]any) []string {
	fromID := graphString(obj, "from", "from_id", "source", "source_id", "u")
	toID := graphString(obj, "to", "to_id", "target", "target_id", "v")
	id := graphString(obj, "id", "edge_id")
	cost := graphString(obj, "cost", "weight", "duration")
	distance := graphString(obj, "distance", "length", "meters")
	geom := graphGeometry(obj)
	geometryText := ""
	geometryType := ""
	if geom != nil {
		geometryText = mustJSON(geom)
		geometryType = geoJSONString(geom["type"])
	}
	return []string{"edge", id, fromID, toID, "", "", cost, distance, geometryText, geometryType, mustJSON(obj)}
}

func graphLatLon(obj map[string]any) (float64, float64) {
	lat := graphFloat(obj, "lat", "latitude", "y")
	lon := graphFloat(obj, "lon", "lng", "longitude", "x")
	if lon == 0 && lat == 0 {
		if coords, ok := obj["coordinates"].([]any); ok && len(coords) >= 2 {
			lon, _ = numericAny(coords[0])
			lat, _ = numericAny(coords[1])
		}
	}
	return lat, lon
}

func graphGeometry(obj map[string]any) map[string]any {
	if geom, ok := obj["geometry"].(map[string]any); ok && isImportGeometryOrCollection(geom) {
		return geom
	}
	coords, ok := obj["coordinates"].([]any)
	if !ok || len(coords) < 2 {
		return nil
	}
	return map[string]any{"type": "LineString", "coordinates": coords}
}

func graphString(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := obj[key]; ok && v != nil {
			return fmt.Sprint(v)
		}
	}
	return ""
}

func graphFloat(obj map[string]any, keys ...string) float64 {
	for _, key := range keys {
		if v, ok := obj[key]; ok {
			if f, ok := numericAny(v); ok {
				return f
			}
		}
	}
	return 0
}

func graphFloatString(v float64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func numericAny(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func isImportGeometryOrCollection(obj map[string]any) bool {
	if strings.EqualFold(geoJSONString(obj["type"]), "GeometryCollection") {
		return true
	}
	return isImportGeometry(obj)
}

func orderedStringKeysFromMaps(iter func(func(map[string]string))) []string {
	seen := map[string]bool{}
	var keys []string
	iter(func(values map[string]string) {
		for key := range values {
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	})
	sort.Strings(keys)
	return keys
}

func mustJSON(v any) string {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(body)
}

func geoPackageGeometry(data []byte) (map[string]any, error) {
	if len(data) < 8 || string(data[:2]) != "GP" {
		return wkbGeometry(data)
	}
	flags := data[3]
	envelopeCode := (flags >> 1) & 0x07
	offset := 8 + geoPackageEnvelopeSize(envelopeCode)
	if offset > len(data) {
		return nil, fmt.Errorf("invalid geopackage geometry header")
	}
	return wkbGeometry(data[offset:])
}

func geoPackageEnvelopeSize(code byte) int {
	switch code {
	case 1:
		return 32
	case 2, 3:
		return 48
	case 4:
		return 64
	default:
		return 0
	}
}

func wkbGeometry(data []byte) (map[string]any, error) {
	r := &wkbReader{data: data}
	return r.geometry()
}

type wkbReader struct {
	data  []byte
	pos   int
	order binary.ByteOrder
}

func (r *wkbReader) geometry() (map[string]any, error) {
	orderByte, err := r.byte()
	if err != nil {
		return nil, err
	}
	switch orderByte {
	case 0:
		r.order = binary.BigEndian
	case 1:
		r.order = binary.LittleEndian
	default:
		return nil, fmt.Errorf("invalid wkb byte order %d", orderByte)
	}
	typRaw, err := r.uint32()
	if err != nil {
		return nil, err
	}
	typ := typRaw % 1000
	switch typ {
	case 1:
		x, y, err := r.xy()
		if err != nil {
			return nil, err
		}
		return pointGeometry(x, y), nil
	case 2:
		coords, err := r.coordSeq()
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "LineString", "coordinates": coords}, nil
	case 3:
		rings, err := r.rings()
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "Polygon", "coordinates": rings}, nil
	case 4:
		points, err := r.childGeometries()
		if err != nil {
			return nil, err
		}
		coords := make([]any, 0, len(points))
		for _, point := range points {
			if c, ok := point["coordinates"]; ok {
				coords = append(coords, c)
			}
		}
		return map[string]any{"type": "MultiPoint", "coordinates": coords}, nil
	case 5:
		lines, err := r.childGeometries()
		if err != nil {
			return nil, err
		}
		coords := make([]any, 0, len(lines))
		for _, line := range lines {
			if c, ok := line["coordinates"]; ok {
				coords = append(coords, c)
			}
		}
		return map[string]any{"type": "MultiLineString", "coordinates": coords}, nil
	case 6:
		polys, err := r.childGeometries()
		if err != nil {
			return nil, err
		}
		coords := make([]any, 0, len(polys))
		for _, poly := range polys {
			if c, ok := poly["coordinates"]; ok {
				coords = append(coords, c)
			}
		}
		return map[string]any{"type": "MultiPolygon", "coordinates": coords}, nil
	case 7:
		children, err := r.childGeometries()
		if err != nil {
			return nil, err
		}
		geoms := make([]any, len(children))
		for i := range children {
			geoms[i] = children[i]
		}
		return map[string]any{"type": "GeometryCollection", "geometries": geoms}, nil
	default:
		return nil, fmt.Errorf("unsupported wkb geometry type %d", typRaw)
	}
}

func (r *wkbReader) childGeometries() ([]map[string]any, error) {
	n, err := r.uint32()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, n)
	for i := uint32(0); i < n; i++ {
		geom, err := r.geometry()
		if err != nil {
			return nil, err
		}
		out = append(out, geom)
	}
	return out, nil
}

func (r *wkbReader) rings() ([]any, error) {
	n, err := r.uint32()
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, n)
	for i := uint32(0); i < n; i++ {
		coords, err := r.coordSeq()
		if err != nil {
			return nil, err
		}
		out = append(out, coords)
	}
	return out, nil
}

func (r *wkbReader) coordSeq() ([]any, error) {
	n, err := r.uint32()
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, n)
	for i := uint32(0); i < n; i++ {
		x, y, err := r.xy()
		if err != nil {
			return nil, err
		}
		out = append(out, []float64{x, y})
	}
	return out, nil
}

func (r *wkbReader) xy() (float64, float64, error) {
	x, err := r.float64()
	if err != nil {
		return 0, 0, err
	}
	y, err := r.float64()
	if err != nil {
		return 0, 0, err
	}
	return x, y, nil
}

func (r *wkbReader) byte() (byte, error) {
	if r.pos >= len(r.data) {
		return 0, io.ErrUnexpectedEOF
	}
	b := r.data[r.pos]
	r.pos++
	return b, nil
}

func (r *wkbReader) uint32() (uint32, error) {
	if r.pos+4 > len(r.data) {
		return 0, io.ErrUnexpectedEOF
	}
	v := r.order.Uint32(r.data[r.pos : r.pos+4])
	r.pos += 4
	return v, nil
}

func (r *wkbReader) float64() (float64, error) {
	if r.pos+8 > len(r.data) {
		return 0, io.ErrUnexpectedEOF
	}
	v := math.Float64frombits(r.order.Uint64(r.data[r.pos : r.pos+8]))
	r.pos += 8
	return v, nil
}

type protoField struct {
	num   int
	wire  int
	value uint64
	bytes []byte
}

func parseProtoFields(data []byte) []protoField {
	var fields []protoField
	for pos := 0; pos < len(data); {
		key, n := readUvarint(data[pos:])
		if n <= 0 {
			break
		}
		pos += n
		field := protoField{num: int(key >> 3), wire: int(key & 0x7)}
		switch field.wire {
		case 0:
			value, n := readUvarint(data[pos:])
			if n <= 0 {
				return fields
			}
			field.value = value
			pos += n
		case 1:
			if pos+8 > len(data) {
				return fields
			}
			field.bytes = data[pos : pos+8]
			pos += 8
		case 2:
			length, n := readUvarint(data[pos:])
			if n <= 0 {
				return fields
			}
			pos += n
			if length > uint64(len(data)-pos) {
				return fields
			}
			field.bytes = data[pos : pos+int(length)]
			pos += int(length)
		case 5:
			if pos+4 > len(data) {
				return fields
			}
			field.bytes = data[pos : pos+4]
			pos += 4
		default:
			return fields
		}
		fields = append(fields, field)
	}
	return fields
}

func firstBytesField(fields []protoField, num int) []byte {
	for _, field := range fields {
		if field.num == num && field.wire == 2 {
			return field.bytes
		}
	}
	return nil
}

func bytesFields(fields []protoField, num int) [][]byte {
	var out [][]byte
	for _, field := range fields {
		if field.num == num && field.wire == 2 {
			out = append(out, field.bytes)
		}
	}
	return out
}

func firstVarintField(fields []protoField, num int) uint64 {
	return firstVarintFieldDefault(fields, num, 0)
}

func firstVarintFieldDefault(fields []protoField, num int, fallback uint64) uint64 {
	for _, field := range fields {
		if field.num == num && field.wire == 0 {
			return field.value
		}
	}
	return fallback
}

func packedVarints(data []byte) []uint64 {
	var out []uint64
	for pos := 0; pos < len(data); {
		value, n := readUvarint(data[pos:])
		if n <= 0 {
			break
		}
		out = append(out, value)
		pos += n
	}
	return out
}

func packedSVarints(data []byte) []int64 {
	raw := packedVarints(data)
	out := make([]int64, len(raw))
	for i, value := range raw {
		out[i] = zigZag(value)
	}
	return out
}

func readUvarint(data []byte) (uint64, int) {
	var x uint64
	var s uint
	for i, b := range data {
		if b < 0x80 {
			if i > 9 || i == 9 && b > 1 {
				return 0, -1
			}
			return x | uint64(b)<<s, i + 1
		}
		x |= uint64(b&0x7f) << s
		s += 7
	}
	return 0, 0
}

func zigZag(v uint64) int64 {
	return int64(v>>1) ^ -int64(v&1)
}

func importSpatialRecords(ctx context.Context, db *tinysql.DB, tenant, tableName string, records [][]string, opts *ImportOptions) (*ImportResult, error) {
	importOpts := normalizeOptions(opts)
	localOpts := *importOpts
	localOpts.HeaderMode = "present"
	return importRecords(ctx, db, tenant, tableName, records, ',', &localOpts)
}
