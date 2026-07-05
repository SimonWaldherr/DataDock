package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	tinysql "github.com/SimonWaldherr/tinySQL"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/microsoft/go-mssqldb"
	_ "modernc.org/sqlite"
)

const defaultConnectionID = "default"

type DBConnection struct {
	ID      string
	Name    string
	Kind    string
	DSN     string
	Dialect DialectProfile
	DB      *sql.DB
	Native  *tinysql.DB
}

type ConnectionInfo struct {
	ID         string
	Name       string
	Kind       string
	Dialect    string
	DSNDisplay string
	Active     bool
	Default    bool
}

type ManagedConnectionConfig struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	DSN     string `json:"dsn"`
	Default bool   `json:"default,omitempty"`
}

type ConnectionManager struct {
	mu              sync.RWMutex
	active          string
	defaultID       string
	activeBySession map[string]string
	conns           map[string]*DBConnection
}

func NewConnectionManager(defaultConn *DBConnection) *ConnectionManager {
	m := &ConnectionManager{
		active:          defaultConn.ID,
		defaultID:       defaultConn.ID,
		activeBySession: make(map[string]string),
		conns:           map[string]*DBConnection{defaultConn.ID: defaultConn},
	}
	return m
}

func (m *ConnectionManager) Active() *DBConnection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.conns[m.active]
}

func (m *ConnectionManager) ActiveFor(sessionID string) *DBConnection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if id := strings.TrimSpace(sessionID); id != "" {
		if conn := m.conns[m.activeBySession[id]]; conn != nil {
			return conn
		}
	}
	return m.conns[m.active]
}

func (m *ConnectionManager) Get(id string) *DBConnection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.conns[strings.TrimSpace(id)]
}

func (m *ConnectionManager) Add(conn *DBConnection) error {
	if strings.TrimSpace(conn.ID) == "" {
		return fmt.Errorf("connection id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old := m.conns[conn.ID]; old != nil && old.DB != nil && old.ID != defaultConnectionID {
		_ = old.DB.Close()
	}
	m.conns[conn.ID] = conn
	return nil
}

func (m *ConnectionManager) SetActive(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conns[id] == nil {
		return fmt.Errorf("connection %q not found", id)
	}
	m.active = id
	return nil
}

func (m *ConnectionManager) SetDefault(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conns[id] == nil {
		return fmt.Errorf("connection %q not found", id)
	}
	m.defaultID = id
	m.active = id
	return nil
}

func (m *ConnectionManager) SetActiveFor(sessionID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conns[id] == nil {
		return fmt.Errorf("connection %q not found", id)
	}
	if strings.TrimSpace(sessionID) == "" {
		m.active = id
		return nil
	}
	m.activeBySession[sessionID] = id
	return nil
}

func (m *ConnectionManager) List() []ConnectionInfo {
	return m.ListFor("")
}

func (m *ConnectionManager) ListFor(sessionID string) []ConnectionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	activeID := m.active
	defaultID := m.defaultID
	if id := strings.TrimSpace(sessionID); id != "" {
		if conn := m.conns[m.activeBySession[id]]; conn != nil {
			activeID = conn.ID
		}
	}
	out := make([]ConnectionInfo, 0, len(m.conns))
	for _, c := range m.conns {
		out = append(out, ConnectionInfo{
			ID:         c.ID,
			Name:       c.Name,
			Kind:       c.Kind,
			Dialect:    c.Dialect.Name,
			DSNDisplay: maskedDSN(c.DSN),
			Active:     c.ID == activeID,
			Default:    c.ID == defaultID,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID == defaultConnectionID {
			return true
		}
		if out[j].ID == defaultConnectionID {
			return false
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func (m *ConnectionManager) StoredConfigs() []ManagedConnectionConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ManagedConnectionConfig, 0, len(m.conns))
	for _, c := range m.conns {
		if c == nil || c.ID == defaultConnectionID || strings.TrimSpace(c.DSN) == "" {
			continue
		}
		out = append(out, ManagedConnectionConfig{
			ID:      c.ID,
			Name:    c.Name,
			Kind:    c.Kind,
			DSN:     c.DSN,
			Default: c.ID == m.defaultID,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (m *ConnectionManager) DefaultID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.defaultID
}

func OpenManagedConnection(ctx context.Context, id, name, kind, dsn string) (*DBConnection, error) {
	id = sanitizeConnectionID(id)
	if id == "" {
		return nil, fmt.Errorf("connection id is required")
	}
	kind, driverName, connStr, dialect := parseConnectionDSN(kind, dsn)
	db, err := sql.Open(driverName, connStr)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if strings.TrimSpace(name) == "" {
		name = id
	}
	return &DBConnection{ID: id, Name: name, Kind: kind, DSN: dsn, Dialect: dialect, DB: db}, nil
}

func parseConnectionDSN(kind, dsn string) (normalizedKind, driverName, connStr string, dialect DialectProfile) {
	lowerKind := strings.ToLower(strings.TrimSpace(kind))
	lowerDSN := strings.ToLower(strings.TrimSpace(dsn))
	switch {
	case lowerKind == "", lowerKind == "auto":
		return parseConnectionDSN(detectConnectionKind(dsn), dsn)
	case lowerKind == "postgres" || lowerKind == "postgresql" || strings.HasPrefix(lowerDSN, "postgres://") || strings.HasPrefix(lowerDSN, "postgresql://"):
		return "postgres", "postgres", dsn, DialectProfileForName("postgres")
	case lowerKind == "mysql" || lowerKind == "mariadb" || strings.HasPrefix(lowerDSN, "mysql://"):
		if strings.HasPrefix(lowerDSN, "mysql://") {
			dsn = dsn[len("mysql://"):]
		}
		return "mysql", "mysql", dsn, DialectProfileForName("mysql")
	case lowerKind == "mssql" || lowerKind == "sqlserver" || strings.HasPrefix(lowerDSN, "mssql://") || strings.HasPrefix(lowerDSN, "sqlserver://"):
		if strings.HasPrefix(lowerDSN, "mssql://") {
			dsn = "sqlserver://" + dsn[len("mssql://"):]
		}
		return "mssql", "sqlserver", dsn, DialectProfileForName("mssql")
	case lowerKind == "sqlite" || strings.HasPrefix(lowerDSN, "sqlite://") || strings.HasSuffix(lowerDSN, ".db") || strings.HasSuffix(lowerDSN, ".sqlite") || strings.HasSuffix(lowerDSN, ".sqlite3"):
		if strings.HasPrefix(lowerDSN, "sqlite://") {
			dsn = dsn[len("sqlite://"):]
		}
		return "sqlite", "sqlite", dsn, DialectProfileForName("sqlite")
	default:
		return "sqlite", "sqlite", dsn, DialectProfileForName("sqlite")
	}
}

func detectConnectionKind(dsn string) string {
	lower := strings.ToLower(strings.TrimSpace(dsn))
	switch {
	case strings.HasPrefix(lower, "postgres://"), strings.HasPrefix(lower, "postgresql://"):
		return "postgres"
	case strings.HasPrefix(lower, "mysql://"), strings.Contains(lower, "@tcp("):
		return "mysql"
	case strings.HasPrefix(lower, "mssql://"), strings.HasPrefix(lower, "sqlserver://"):
		return "mssql"
	case strings.HasPrefix(lower, "sqlite://"), strings.HasSuffix(lower, ".db"), strings.HasSuffix(lower, ".sqlite"), strings.HasSuffix(lower, ".sqlite3"), lower == ":memory:":
		return "sqlite"
	default:
		return "sqlite"
	}
}

func sanitizeConnectionID(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

func maskedDSN(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return ""
	}
	if i := strings.Index(dsn, "://"); i >= 0 {
		prefix := dsn[:i+3]
		rest := dsn[i+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			return prefix + "******@" + rest[at+1:]
		}
	}
	return dsn
}

func encodeManagedConnectionConfigs(configs []ManagedConnectionConfig) (string, error) {
	data, err := json.Marshal(configs)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func decodeManagedConnectionConfigs(value string) ([]ManagedConnectionConfig, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	var configs []ManagedConnectionConfig
	if err := json.Unmarshal([]byte(value), &configs); err != nil {
		return nil, err
	}
	return configs, nil
}

func discoverEnvironmentConnections() []ManagedConnectionConfig {
	var configs []ManagedConnectionConfig
	add := func(id, name, kind, dsn string) {
		dsn = strings.TrimSpace(dsn)
		if dsn == "" {
			return
		}
		id = sanitizeConnectionID(id)
		if id == "" {
			id = sanitizeConnectionID(name)
		}
		configs = append(configs, ManagedConnectionConfig{
			ID:   id,
			Name: name,
			Kind: kind,
			DSN:  dsn,
		})
	}
	add("database-url", "DATABASE_URL", "auto", os.Getenv("DATABASE_URL"))
	add("postgres-url", "PostgreSQL", "postgres", firstEnv("POSTGRES_URL", "POSTGRES_DSN", "DATABASE_POSTGRES_URL"))
	add("mysql-url", "MySQL", "mysql", firstEnv("MYSQL_URL", "MYSQL_DSN", "MARIADB_URL", "MARIADB_DSN"))
	add("mssql-url", "Microsoft SQL Server", "mssql", firstEnv("MSSQL_URL", "MSSQL_DSN", "SQLSERVER_URL", "SQLSERVER_DSN"))
	add("sqlite-path", "SQLite", "sqlite", firstEnv("SQLITE_PATH", "SQLITE_DSN", "DATADOCK_SQLITE_PATH"))
	if raw := strings.TrimSpace(os.Getenv("DATADOCK_CONNECTIONS")); raw != "" {
		if decoded, err := decodeManagedConnectionConfigs(raw); err == nil {
			configs = append(configs, decoded...)
		}
	}
	return dedupeConnectionConfigs(configs)
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func dedupeConnectionConfigs(configs []ManagedConnectionConfig) []ManagedConnectionConfig {
	seen := map[string]bool{}
	var out []ManagedConnectionConfig
	for _, cfg := range configs {
		cfg.ID = sanitizeConnectionID(cfg.ID)
		if cfg.ID == "" || strings.TrimSpace(cfg.DSN) == "" || seen[cfg.ID] {
			continue
		}
		if strings.TrimSpace(cfg.Kind) == "" {
			cfg.Kind = "auto"
		}
		if strings.TrimSpace(cfg.Name) == "" {
			cfg.Name = cfg.ID
		}
		seen[cfg.ID] = true
		out = append(out, cfg)
	}
	return out
}

func (c *DBConnection) QuoteIdent(name string) string {
	parts := strings.Split(name, ".")
	if len(parts) > 1 {
		quoted := make([]string, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			quoted = append(quoted, c.quoteIdentPart(part))
		}
		if len(quoted) > 0 {
			return strings.Join(quoted, ".")
		}
	}
	return c.quoteIdentPart(name)
}

func (c *DBConnection) quoteIdentPart(name string) string {
	switch c.Dialect.Name {
	case "MariaDB/MySQL":
		return "`" + strings.ReplaceAll(name, "`", "``") + "`"
	case "Microsoft SQL Server":
		return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
	default:
		return quoteName(name)
	}
}

func (c *DBConnection) Placeholder(pos int) string {
	switch c.Dialect.Name {
	case "PostgreSQL":
		return fmt.Sprintf("$%d", pos)
	case "Microsoft SQL Server":
		return fmt.Sprintf("@p%d", pos)
	default:
		return "?"
	}
}

func (c *DBConnection) IsTinySQL() bool {
	return c.Native != nil
}

func (c *DBConnection) sampleColumnValues(ctx context.Context, table, column string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	query := fmt.Sprintf("SELECT %s FROM %s LIMIT %d", c.QuoteIdent(column), c.QuoteIdent(table), limit)
	if c.Dialect.Name == "Microsoft SQL Server" {
		query = fmt.Sprintf("SELECT TOP %d %s FROM %s", limit, c.QuoteIdent(column), c.QuoteIdent(table))
	}
	rows, err := c.DB.QueryContext(ctx, query)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var samples []string
	for rows.Next() {
		var v any
		if err := rows.Scan(&v); err != nil {
			return samples
		}
		if v != nil {
			samples = append(samples, anyToString(v))
		}
	}
	return samples
}
