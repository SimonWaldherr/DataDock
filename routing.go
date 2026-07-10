package main

import (
	"container/heap"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const maxRoutingGraphRows = 250000

type routingRequest struct {
	FromID    string   `json:"from_id"`
	ToID      string   `json:"to_id"`
	FromLon   *float64 `json:"from_lon"`
	FromLat   *float64 `json:"from_lat"`
	ToLon     *float64 `json:"to_lon"`
	ToLat     *float64 `json:"to_lat"`
	CostField string   `json:"cost_field"`
	Directed  *bool    `json:"directed"`
	MaxCost   float64  `json:"max_cost"`
}

type routingNode struct {
	ID            string
	Lon           float64
	Lat           float64
	HasCoordinate bool
}

type routingEdge struct {
	ID          string
	From        string
	To          string
	Cost        float64
	HasCost     bool
	Distance    float64
	HasDistance bool
	Geometry    map[string]any
	Properties  map[string]any
}

type routingArc struct {
	From    string
	To      string
	Edge    *routingEdge
	Reverse bool
}

type routingGraph struct {
	Nodes map[string]routingNode
	Arcs  map[string][]routingArc
}

type routingPredecessor struct {
	From string
	Arc  routingArc
}

type routingQueueItem struct {
	Node string
	Cost float64
}

type routingPriorityQueue []routingQueueItem

func (q routingPriorityQueue) Len() int           { return len(q) }
func (q routingPriorityQueue) Less(i, j int) bool { return q[i].Cost < q[j].Cost }
func (q routingPriorityQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *routingPriorityQueue) Push(v any)        { *q = append(*q, v.(routingQueueItem)) }
func (q *routingPriorityQueue) Pop() any {
	old := *q
	last := len(old) - 1
	item := old[last]
	*q = old[:last]
	return item
}

func (a *App) isRoutingGraphTable(ctx context.Context, table string) bool {
	if conn := a.activeConn(ctx); conn == nil || !conn.IsTinySQL() || !isValidIdentifier(table) || isDataDockSystemObject(table) {
		return false
	}
	var recordType string
	err := a.sqlDB.QueryRowContext(ctx, "SELECT record_type FROM "+quoteName(table)+" WHERE record_type = ? LIMIT 1", "edge").Scan(&recordType)
	return err == nil && recordType == "edge"
}

func (a *App) routingViewHandler(w http.ResponseWriter, r *http.Request) {
	table := r.PathValue("table")
	if !a.canBrowseTableName(r, table) {
		a.renderObjectMissing(w, r, table, fmt.Errorf("table %q not found", table))
		return
	}
	if conn := a.activeConn(r.Context()); conn == nil || !conn.IsTinySQL() {
		a.renderObjectMissing(w, r, table, fmt.Errorf("routing graphs are available only from the local tinySQL database"))
		return
	}
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	if !a.isRoutingGraphTable(ctx, table) {
		a.renderObjectMissing(w, r, table, fmt.Errorf("table %q has no imported routing edges", table))
		return
	}
	a.render(w, r, "routing", map[string]any{"Table": table})
}

func (a *App) apiRouteHandler(w http.ResponseWriter, r *http.Request) {
	a.handleRoutingAPI(w, r, false)
}

func (a *App) apiReachableHandler(w http.ResponseWriter, r *http.Request) {
	a.handleRoutingAPI(w, r, true)
}

func (a *App) handleRoutingAPI(w http.ResponseWriter, r *http.Request, reachable bool) {
	if conn := a.activeConn(r.Context()); conn == nil || !conn.IsTinySQL() {
		a.writeProblem(w, r, http.StatusBadRequest, "Unsupported connection", "routing graphs are available only from the local tinySQL database")
		return
	}
	var request routingRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid JSON", "request body must be valid JSON")
		return
	}
	ctx, cancel := a.withQueryTimeout(r.Context())
	defer cancel()
	graph, err := a.loadRoutingGraph(ctx, r.PathValue("table"), request.Directed)
	if err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Routing graph unavailable", err.Error())
		return
	}
	from, err := resolveRoutingNode(graph, request.FromID, request.FromLon, request.FromLat)
	if err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid origin", err.Error())
		return
	}
	if reachable {
		if request.MaxCost <= 0 || math.IsInf(request.MaxCost, 0) || math.IsNaN(request.MaxCost) {
			a.writeProblem(w, r, http.StatusBadRequest, "Invalid maximum cost", "max_cost must be a positive number")
			return
		}
		distances, _, err := graph.shortestPaths(from, request.CostField, request.MaxCost)
		if err != nil {
			a.writeProblem(w, r, http.StatusBadRequest, "Invalid cost field", err.Error())
			return
		}
		geojson := graph.reachableGeoJSON(distances, request.MaxCost, routingCostFieldName(request.CostField))
		a.writeJSON(w, http.StatusOK, map[string]any{
			"source_node_id":  from,
			"cost_field":      routingCostFieldName(request.CostField),
			"max_cost":        request.MaxCost,
			"reachable_nodes": len(distances),
			"geojson":         geojson,
		})
		return
	}
	to, err := resolveRoutingNode(graph, request.ToID, request.ToLon, request.ToLat)
	if err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid destination", err.Error())
		return
	}
	distances, predecessors, err := graph.shortestPaths(from, request.CostField, 0)
	if err != nil {
		a.writeProblem(w, r, http.StatusBadRequest, "Invalid cost field", err.Error())
		return
	}
	cost, found := distances[to]
	if !found {
		a.writeProblem(w, r, http.StatusNotFound, "No route", "no path connects the requested origin and destination")
		return
	}
	nodes, arcs, err := reconstructRoutingPath(from, to, predecessors)
	if err != nil {
		a.writeProblem(w, r, http.StatusInternalServerError, "Route reconstruction failed", err.Error())
		return
	}
	distance := 0.0
	for _, arc := range arcs {
		if arc.Edge.HasDistance {
			distance += arc.Edge.Distance
		} else if value, ok := graph.arcDistance(arc); ok {
			distance += value
		}
	}
	geojson := graph.routeGeoJSON(nodes, arcs, cost, distance, routingCostFieldName(request.CostField))
	a.writeJSON(w, http.StatusOK, map[string]any{
		"source_node_id":      from,
		"destination_node_id": to,
		"node_ids":            nodes,
		"edge_count":          len(arcs),
		"cost":                cost,
		"distance_meters":     distance,
		"cost_field":          routingCostFieldName(request.CostField),
		"geojson":             geojson,
	})
}

func (a *App) loadRoutingGraph(ctx context.Context, table string, directed *bool) (routingGraph, error) {
	if !isValidIdentifier(table) || isDataDockSystemObject(table) {
		return routingGraph{}, fmt.Errorf("invalid routing table")
	}
	rows, err := a.sqlDB.QueryContext(ctx,
		"SELECT record_type, id, from_id, to_id, lat, lon, cost, distance, geometry, properties FROM "+quoteName(table),
	)
	if err != nil {
		return routingGraph{}, err
	}
	defer rows.Close()
	graph := routingGraph{Nodes: map[string]routingNode{}, Arcs: map[string][]routingArc{}}
	edges := make([]*routingEdge, 0)
	rowCount := 0
	for rows.Next() {
		rowCount++
		if rowCount > maxRoutingGraphRows {
			return routingGraph{}, fmt.Errorf("routing graph exceeds the %d-row interactive limit", maxRoutingGraphRows)
		}
		var recordType, id, fromID, toID, latRaw, lonRaw, costRaw, distanceRaw, geometryRaw, propertiesRaw sql.NullString
		if err := rows.Scan(&recordType, &id, &fromID, &toID, &latRaw, &lonRaw, &costRaw, &distanceRaw, &geometryRaw, &propertiesRaw); err != nil {
			return routingGraph{}, err
		}
		switch strings.ToLower(strings.TrimSpace(recordType.String)) {
		case "node":
			nodeID := strings.TrimSpace(id.String)
			if nodeID == "" {
				continue
			}
			lon, lonOK := routingFloat(lonRaw.String)
			lat, latOK := routingFloat(latRaw.String)
			graph.Nodes[nodeID] = routingNode{ID: nodeID, Lon: lon, Lat: lat, HasCoordinate: lonOK && latOK && lon >= -180 && lon <= 180 && lat >= -90 && lat <= 90}
		case "edge", "link":
			from, to := strings.TrimSpace(fromID.String), strings.TrimSpace(toID.String)
			if from == "" || to == "" {
				continue
			}
			edge := &routingEdge{ID: strings.TrimSpace(id.String), From: from, To: to, Geometry: routingGeometry(geometryRaw.String), Properties: routingProperties(propertiesRaw.String)}
			edge.Cost, edge.HasCost = routingFloat(costRaw.String)
			edge.Distance, edge.HasDistance = routingFloat(distanceRaw.String)
			edges = append(edges, edge)
		}
	}
	if err := rows.Err(); err != nil {
		return routingGraph{}, err
	}
	if len(graph.Nodes) == 0 || len(edges) == 0 {
		return routingGraph{}, fmt.Errorf("table %q contains no usable routing nodes and edges", table)
	}
	isDirected := true
	if directed != nil {
		isDirected = *directed
	}
	for _, edge := range edges {
		if _, ok := graph.Nodes[edge.From]; !ok {
			continue
		}
		if _, ok := graph.Nodes[edge.To]; !ok {
			continue
		}
		graph.Arcs[edge.From] = append(graph.Arcs[edge.From], routingArc{From: edge.From, To: edge.To, Edge: edge})
		if !isDirected {
			graph.Arcs[edge.To] = append(graph.Arcs[edge.To], routingArc{From: edge.To, To: edge.From, Edge: edge, Reverse: true})
		}
	}
	if len(graph.Arcs) == 0 {
		return routingGraph{}, fmt.Errorf("routing edges do not reference imported nodes")
	}
	return graph, nil
}

func routingFloat(raw string) (float64, bool) {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	return value, true
}

func routingGeometry(raw string) map[string]any {
	geometry, ok := parseGeoJSONObject(raw)
	if !ok || !isGeoJSONGeometry(geometry) {
		return nil
	}
	return geometry
}

func routingProperties(raw string) map[string]any {
	var properties map[string]any
	if json.Unmarshal([]byte(raw), &properties) != nil {
		return nil
	}
	return properties
}

func resolveRoutingNode(graph routingGraph, id string, lon, lat *float64) (string, error) {
	id = strings.TrimSpace(id)
	if id != "" {
		if _, ok := graph.Nodes[id]; ok {
			return id, nil
		}
		return "", fmt.Errorf("node %q does not exist", id)
	}
	if lon == nil || lat == nil || *lon < -180 || *lon > 180 || *lat < -90 || *lat > 90 {
		return "", fmt.Errorf("provide a node id or valid longitude and latitude")
	}
	var closest string
	best := math.Inf(1)
	for id, node := range graph.Nodes {
		if !node.HasCoordinate {
			continue
		}
		distance := routingHaversine(*lon, *lat, node.Lon, node.Lat)
		if distance < best {
			closest, best = id, distance
		}
	}
	if closest == "" {
		return "", fmt.Errorf("graph has no nodes with coordinates")
	}
	return closest, nil
}

func (graph routingGraph) shortestPaths(from, costField string, maxCost float64) (map[string]float64, map[string]routingPredecessor, error) {
	distances := map[string]float64{from: 0}
	predecessors := map[string]routingPredecessor{}
	queue := &routingPriorityQueue{{Node: from, Cost: 0}}
	heap.Init(queue)
	for queue.Len() > 0 {
		current := heap.Pop(queue).(routingQueueItem)
		if known := distances[current.Node]; current.Cost != known {
			continue
		}
		if maxCost > 0 && current.Cost > maxCost {
			continue
		}
		for _, arc := range graph.Arcs[current.Node] {
			weight, err := graph.arcCost(arc, costField)
			if err != nil {
				return nil, nil, err
			}
			if weight < 0 {
				return nil, nil, fmt.Errorf("negative edge cost is not supported")
			}
			next := current.Cost + weight
			if maxCost > 0 && next > maxCost {
				continue
			}
			if known, ok := distances[arc.To]; !ok || next < known {
				distances[arc.To] = next
				predecessors[arc.To] = routingPredecessor{From: current.Node, Arc: arc}
				heap.Push(queue, routingQueueItem{Node: arc.To, Cost: next})
			}
		}
	}
	return distances, predecessors, nil
}

func (graph routingGraph) arcCost(arc routingArc, field string) (float64, error) {
	field = routingCostFieldName(field)
	switch field {
	case "cost":
		if arc.Edge.HasCost {
			return arc.Edge.Cost, nil
		}
	case "distance":
		if arc.Edge.HasDistance {
			return arc.Edge.Distance, nil
		}
	default:
		key := strings.TrimPrefix(field, "properties.")
		if value, ok := routingNumericProperty(arc.Edge.Properties, key); ok {
			return value, nil
		}
		return 0, fmt.Errorf("edge %q has no numeric property %q", arc.Edge.ID, key)
	}
	if distance, ok := graph.arcDistance(arc); ok {
		return distance, nil
	}
	return 0, fmt.Errorf("edge %q has no %s or geometric distance", arc.Edge.ID, field)
}

func routingCostFieldName(field string) string {
	field = strings.TrimSpace(field)
	if field == "" {
		return "cost"
	}
	if strings.EqualFold(field, "cost") {
		return "cost"
	}
	if strings.EqualFold(field, "distance") {
		return "distance"
	}
	if len(field) > len("properties.") && strings.EqualFold(field[:len("properties.")], "properties.") {
		return "properties." + strings.TrimSpace(field[len("properties."):])
	}
	return "properties." + field
}

func routingNumericProperty(properties map[string]any, key string) (float64, bool) {
	if properties == nil {
		return 0, false
	}
	value, ok := properties[key]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case float64:
		return typed, !math.IsNaN(typed) && !math.IsInf(typed, 0)
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	case string:
		return routingFloat(typed)
	default:
		return routingFloat(fmt.Sprint(value))
	}
}

func (graph routingGraph) arcDistance(arc routingArc) (float64, bool) {
	from, fromOK := graph.Nodes[arc.From]
	to, toOK := graph.Nodes[arc.To]
	if !fromOK || !toOK || !from.HasCoordinate || !to.HasCoordinate {
		return 0, false
	}
	return routingHaversine(from.Lon, from.Lat, to.Lon, to.Lat), true
}

func routingHaversine(lonA, latA, lonB, latB float64) float64 {
	const radius = 6371008.8
	latDelta := (latB - latA) * math.Pi / 180
	lonDelta := (lonB - lonA) * math.Pi / 180
	a := math.Sin(latDelta/2)*math.Sin(latDelta/2) + math.Cos(latA*math.Pi/180)*math.Cos(latB*math.Pi/180)*math.Sin(lonDelta/2)*math.Sin(lonDelta/2)
	return radius * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

func reconstructRoutingPath(from, to string, predecessors map[string]routingPredecessor) ([]string, []routingArc, error) {
	nodes := []string{to}
	arcs := make([]routingArc, 0)
	for current := to; current != from; {
		previous, ok := predecessors[current]
		if !ok {
			return nil, nil, fmt.Errorf("path is incomplete")
		}
		arcs = append(arcs, previous.Arc)
		nodes = append(nodes, previous.From)
		current = previous.From
	}
	for i, j := 0, len(nodes)-1; i < j; i, j = i+1, j-1 {
		nodes[i], nodes[j] = nodes[j], nodes[i]
	}
	for i, j := 0, len(arcs)-1; i < j; i, j = i+1, j-1 {
		arcs[i], arcs[j] = arcs[j], arcs[i]
	}
	return nodes, arcs, nil
}

func (graph routingGraph) routeGeoJSON(nodes []string, arcs []routingArc, cost, distance float64, costField string) map[string]any {
	coordinates := make([][]float64, 0, len(nodes))
	for _, arc := range arcs {
		segment := graph.arcCoordinates(arc)
		if len(segment) == 0 {
			continue
		}
		if len(coordinates) > 0 && sameRoutingCoordinate(coordinates[len(coordinates)-1], segment[0]) {
			segment = segment[1:]
		}
		coordinates = append(coordinates, segment...)
	}
	if len(coordinates) == 0 {
		for _, id := range nodes {
			if node, ok := graph.Nodes[id]; ok && node.HasCoordinate {
				coordinates = append(coordinates, []float64{node.Lon, node.Lat})
			}
		}
	}
	return map[string]any{
		"type":       "Feature",
		"geometry":   map[string]any{"type": "LineString", "coordinates": coordinates},
		"properties": map[string]any{"cost": cost, "distance_meters": distance, "edge_count": len(arcs), "cost_field": costField},
	}
}

func (graph routingGraph) arcCoordinates(arc routingArc) [][]float64 {
	coordinates := routingLineCoordinates(arc.Edge.Geometry)
	from := graph.Nodes[arc.From]
	to := graph.Nodes[arc.To]
	if len(coordinates) < 2 {
		if from.HasCoordinate && to.HasCoordinate {
			return [][]float64{{from.Lon, from.Lat}, {to.Lon, to.Lat}}
		}
		return nil
	}
	if from.HasCoordinate && to.HasCoordinate {
		direct := routingCoordinateDistance(coordinates[0], from) + routingCoordinateDistance(coordinates[len(coordinates)-1], to)
		reversed := routingCoordinateDistance(coordinates[0], to) + routingCoordinateDistance(coordinates[len(coordinates)-1], from)
		if reversed < direct {
			reverseRoutingCoordinates(coordinates)
		}
	}
	return coordinates
}

func routingLineCoordinates(geometry map[string]any) [][]float64 {
	if geometry == nil || !strings.EqualFold(fmt.Sprint(geometry["type"]), "LineString") {
		return nil
	}
	return toPointList(geometry["coordinates"])
}

func routingCoordinateDistance(point []float64, node routingNode) float64 {
	if len(point) < 2 {
		return math.Inf(1)
	}
	return math.Hypot(point[0]-node.Lon, point[1]-node.Lat)
}

func reverseRoutingCoordinates(points [][]float64) {
	for i, j := 0, len(points)-1; i < j; i, j = i+1, j-1 {
		points[i], points[j] = points[j], points[i]
	}
}

func sameRoutingCoordinate(first, second []float64) bool {
	return len(first) >= 2 && len(second) >= 2 && first[0] == second[0] && first[1] == second[1]
}

func (graph routingGraph) reachableGeoJSON(distances map[string]float64, maxCost float64, costField string) map[string]any {
	features := make([]map[string]any, 0, len(distances)+1)
	points := make([][]float64, 0, len(distances))
	ids := make([]string, 0, len(distances))
	for id := range distances {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		node := graph.Nodes[id]
		if !node.HasCoordinate {
			continue
		}
		point := []float64{node.Lon, node.Lat}
		points = append(points, point)
		features = append(features, map[string]any{
			"type": "Feature", "geometry": map[string]any{"type": "Point", "coordinates": point},
			"properties": map[string]any{"node_id": id, "cost": distances[id], "reachable": true},
		})
	}
	if hull := routingConvexHull(points); len(hull) >= 3 {
		hull = append(hull, hull[0])
		features = append([]map[string]any{{
			"type":       "Feature",
			"geometry":   map[string]any{"type": "Polygon", "coordinates": [][][]float64{hull}},
			"properties": map[string]any{"max_cost": maxCost, "cost_field": costField, "method": "convex_hull"},
		}}, features...)
	}
	return map[string]any{"type": "FeatureCollection", "features": features}
}

func routingConvexHull(points [][]float64) [][]float64 {
	if len(points) < 3 {
		return nil
	}
	unique := make([][]float64, 0, len(points))
	seen := map[string]bool{}
	for _, point := range points {
		if len(point) < 2 {
			continue
		}
		key := strconv.FormatFloat(point[0], 'g', -1, 64) + "," + strconv.FormatFloat(point[1], 'g', -1, 64)
		if !seen[key] {
			seen[key] = true
			unique = append(unique, point)
		}
	}
	if len(unique) < 3 {
		return nil
	}
	sort.Slice(unique, func(i, j int) bool {
		if unique[i][0] == unique[j][0] {
			return unique[i][1] < unique[j][1]
		}
		return unique[i][0] < unique[j][0]
	})
	cross := func(origin, first, second []float64) float64 {
		return (first[0]-origin[0])*(second[1]-origin[1]) - (first[1]-origin[1])*(second[0]-origin[0])
	}
	lower := make([][]float64, 0, len(unique))
	for _, point := range unique {
		for len(lower) >= 2 && cross(lower[len(lower)-2], lower[len(lower)-1], point) <= 0 {
			lower = lower[:len(lower)-1]
		}
		lower = append(lower, point)
	}
	upper := make([][]float64, 0, len(unique))
	for i := len(unique) - 1; i >= 0; i-- {
		point := unique[i]
		for len(upper) >= 2 && cross(upper[len(upper)-2], upper[len(upper)-1], point) <= 0 {
			upper = upper[:len(upper)-1]
		}
		upper = append(upper, point)
	}
	return append(lower[:len(lower)-1], upper[:len(upper)-1]...)
}
