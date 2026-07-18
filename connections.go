package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	tinysql "github.com/SimonWaldherr/tinySQL"

	mysqldriver "github.com/go-sql-driver/mysql"
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
	Verbose *VerboseLogger

	// Owner is the ID of the session that created this connection. An empty
	// Owner means the connection is shared/global (the built-in tinySQL
	// connection, anything loaded from server settings or env vars at
	// startup, and anything an admin has persisted). A non-empty Owner
	// restricts visibility to that one session, so concurrent users on the
	// same running server don't see or use each other's ad hoc connections
	// until an admin explicitly shares one (see MarkPersisted).
	Owner string

	// crossDB caches secondary *sql.DB handles to OTHER databases on the same
	// server (same host/user/credentials, different database name), used to
	// browse the full server catalog for dialects that can't cross databases
	// within a single connection (PostgreSQL). Lazily populated.
	crossDBMu sync.Mutex
	crossDB   map[string]*sql.DB
}

// closeCrossDB closes any secondary cross-database connections opened for
// server-wide catalog browsing.
func (c *DBConnection) closeCrossDB() {
	c.crossDBMu.Lock()
	defer c.crossDBMu.Unlock()
	for _, db := range c.crossDB {
		_ = db.Close()
	}
	c.crossDB = nil
}

type ConnectionInfo struct {
	ID         string
	Name       string
	Kind       string
	Dialect    string
	DSNDisplay string
	Active     bool
	Default    bool
	// Persisted is true when this connection's credentials are written to
	// the server's shared settings (survives restarts, visible to every
	// session) rather than only held in memory for the current process.
	Persisted bool
	// Private is true when this connection is only visible to the session
	// that created it (Owner != ""). Only shared (non-private) connections
	// can be made the server-wide default; see SetDefault.
	Private bool
}

func (c *DBConnection) verboseTarget() string {
	if c == nil {
		return "database://unknown"
	}
	if c.ID == defaultConnectionID || c.IsTinySQL() {
		return "tinysql://" + c.ID
	}
	if display := maskedDSN(c.DSN); display != "" {
		return c.Kind + "://" + c.ID + " " + display
	}
	return c.Kind + "://" + c.ID
}

func (c *DBConnection) queryContext(ctx context.Context, operation, query string, args ...any) (*sql.Rows, error) {
	start := time.Now()
	if c.Verbose.Enabled() {
		c.Verbose.Log(VerboseEvent{
			System:    "database",
			Direction: "outbound",
			Operation: operation,
			Target:    c.verboseTarget(),
			SQL:       query,
			ArgsCount: len(args),
		})
	}
	rows, err := c.DB.QueryContext(ctx, query, args...)
	if c.Verbose.Enabled() {
		event := VerboseEvent{
			System:    "database",
			Direction: "inbound",
			Operation: operation,
			Target:    c.verboseTarget(),
			Duration:  time.Since(start),
			Status:    "ok",
		}
		if err != nil {
			event.Status = "error"
			event.Error = err.Error()
		}
		c.Verbose.Log(event)
	}
	return rows, err
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
	// persisted marks which connection IDs are written to the server's
	// shared settings; everything else only lives in memory for this
	// process and disappears on restart. Only an admin-gated action sets
	// this (see adminPersistConnectionHandler in admin_auth.go).
	persisted map[string]bool
}

func NewConnectionManager(defaultConn *DBConnection) *ConnectionManager {
	m := &ConnectionManager{
		active:          defaultConn.ID,
		defaultID:       defaultConn.ID,
		activeBySession: make(map[string]string),
		conns:           map[string]*DBConnection{defaultConn.ID: defaultConn},
		persisted:       make(map[string]bool),
	}
	return m
}

// MarkPersisted flags an existing connection as one whose credentials should
// be included in StoredConfigs() (and thus written to the server's shared
// settings the next time saveManagedConnections runs). This is also the
// point where a session-private connection becomes shared/global: it's only
// reachable via adminPersistConnectionHandler, which is admin-gated, so
// "persist" doubles as "share with everyone" by admin decision.
func (m *ConnectionManager) MarkPersisted(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if conn := m.conns[id]; conn != nil {
		m.persisted[id] = true
		conn.Owner = ""
	}
}

// UnmarkPersisted stops a connection from being included in StoredConfigs();
// it does not by itself remove the connection from the running process (see
// Remove for that).
func (m *ConnectionManager) UnmarkPersisted(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.persisted, id)
}

func (m *ConnectionManager) IsPersisted(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.persisted[id]
}

// Remove closes and fully drops a managed connection from the running
// process (used by "Forget", which is a real delete, not just "stop
// persisting"). The built-in tinySQL connection can't be removed.
func (m *ConnectionManager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == defaultConnectionID {
		return fmt.Errorf("cannot remove the built-in tinySQL connection")
	}
	conn := m.conns[id]
	if conn == nil {
		return fmt.Errorf("connection %q not found", id)
	}
	if conn.DB != nil {
		_ = conn.DB.Close()
	}
	conn.closeCrossDB()
	delete(m.conns, id)
	delete(m.persisted, id)
	if m.active == id {
		m.active = defaultConnectionID
	}
	if m.defaultID == id {
		m.defaultID = defaultConnectionID
	}
	for sessionID, activeID := range m.activeBySession {
		if activeID == id {
			delete(m.activeBySession, sessionID)
		}
	}
	return nil
}

func (m *ConnectionManager) Active() *DBConnection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.conns[m.active]
}

// RebindSession migrates per-session state (the active-connection pointer,
// and ownership of any private connections) from oldID to newID. Used when
// rotating a session ID at the login/setup authentication boundary (see
// rotateSessionOnAuth in session.go) so that doesn't silently drop a
// pre-login active-connection choice or any connections privately added
// before logging in.
func (m *ConnectionManager) RebindSession(oldID, newID string) {
	oldID = strings.TrimSpace(oldID)
	newID = strings.TrimSpace(newID)
	if oldID == "" || newID == "" || oldID == newID {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if activeID, ok := m.activeBySession[oldID]; ok {
		m.activeBySession[newID] = activeID
		delete(m.activeBySession, oldID)
	}
	for _, conn := range m.conns {
		if conn.Owner == oldID {
			conn.Owner = newID
		}
	}
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

// visibleTo reports whether conn is visible to sessionID: shared (no Owner)
// or privately owned by this exact session.
func (c *DBConnection) visibleTo(sessionID string) bool {
	return c != nil && (c.Owner == "" || c.Owner == sessionID)
}

// GetFor looks up a connection the same way Get does, but returns nil if the
// connection is privately owned by a different session — used anywhere a
// session-supplied ID (form field, query param) selects a connection, so one
// session can't reach another session's in-memory-only connection by
// guessing or replaying its ID.
func (m *ConnectionManager) GetFor(sessionID, id string) *DBConnection {
	m.mu.RLock()
	defer m.mu.RUnlock()
	conn := m.conns[strings.TrimSpace(id)]
	if !conn.visibleTo(sessionID) {
		return nil
	}
	return conn
}

// Add inserts conn into the store, keyed by conn.ID. If conn is privately
// owned (conn.Owner != "") and that ID collides with a connection owned by
// someone else, conn.ID is transparently disambiguated first — this and the
// collision check run under the same lock as the insert, so two concurrent
// Add calls for the same ID from two different sessions can't race past the
// check and have one silently close/replace the other's live *sql.DB. A
// second Add for a connection this session already owns (or a shared one)
// still overwrites/replaces it as before — that's an intentional "update"
// path, not a bug.
func (m *ConnectionManager) Add(conn *DBConnection) error {
	if strings.TrimSpace(conn.ID) == "" {
		return fmt.Errorf("connection id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if conn.Owner != "" {
		conn.ID = m.disambiguateIDLocked(conn.Owner, conn.ID)
	}
	if old := m.conns[conn.ID]; old != nil && old.DB != nil && old.ID != defaultConnectionID {
		_ = old.DB.Close()
		old.closeCrossDB()
	}
	m.conns[conn.ID] = conn
	return nil
}

// disambiguateIDLocked returns id unchanged unless it's already taken by a
// connection privately owned by a different session, in which case it
// returns a deterministic, disambiguated variant (so resubmitting the same
// form from the same session reuses the same suffixed ID instead of piling
// up duplicates every time). Two sessions independently naming a connection
// "prod-mssql" must not collide and silently close/replace each other's
// live *sql.DB, since each is only supposed to be visible to its own
// session until an admin shares it. Must be called with m.mu held.
func (m *ConnectionManager) disambiguateIDLocked(sessionID, id string) string {
	if existing := m.conns[id]; existing == nil || existing.visibleTo(sessionID) {
		return id
	}
	suffix := shortSessionSuffix(sessionID)
	candidate := id + "-" + suffix
	for i := 2; ; i++ {
		if existing := m.conns[candidate]; existing == nil || existing.visibleTo(sessionID) {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%s-%d", id, suffix, i)
	}
}

// shortSessionSuffix derives a short, non-reversible tag from a session ID
// for disambiguating connection IDs, without exposing any part of the
// actual session cookie value in URLs/HTML.
func shortSessionSuffix(sessionID string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(sessionID))
	return fmt.Sprintf("%06x", h.Sum32()&0xffffff)
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

// SetDefault changes the server-wide fallback connection used for any
// session that hasn't picked its own active connection. This is
// deliberately restricted to shared connections (Owner == ""): letting a
// session-private connection become the global default would make every
// other session silently start using it — the exact same leak the Owner
// model exists to prevent — without ever going through the explicit,
// admin-only "share" step (MarkPersisted). Callers must be admin-gated
// (see adminSetDefaultConnectionHandler); this method only enforces the
// shared-connection precondition.
func (m *ConnectionManager) SetDefault(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	conn := m.conns[id]
	if conn == nil {
		return fmt.Errorf("connection %q not found", id)
	}
	if conn.Owner != "" {
		return fmt.Errorf("connection %q is private to one session; share it first before making it the default for everyone", id)
	}
	m.defaultID = id
	m.active = id
	return nil
}

func (m *ConnectionManager) SetActiveFor(sessionID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	conn := m.conns[id]
	if !conn.visibleTo(sessionID) {
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
		if !c.visibleTo(sessionID) {
			continue
		}
		out = append(out, ConnectionInfo{
			ID:         c.ID,
			Name:       c.Name,
			Kind:       c.Kind,
			Dialect:    c.Dialect.Name,
			DSNDisplay: maskedDSN(c.DSN),
			Active:     c.ID == activeID,
			Default:    c.ID == defaultID,
			Persisted:  m.persisted[c.ID],
			Private:    c.Owner != "",
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
		if !m.persisted[c.ID] {
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

func (m *ConnectionManager) SetVerbose(verbose *VerboseLogger) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, conn := range m.conns {
		if conn != nil {
			conn.Verbose = verbose
		}
	}
}

func OpenManagedConnection(ctx context.Context, id, name, kind, dsn string) (*DBConnection, error) {
	return OpenManagedConnectionVerbose(ctx, id, name, kind, dsn, nil)
}

func OpenManagedConnectionVerbose(ctx context.Context, id, name, kind, dsn string, verbose *VerboseLogger) (*DBConnection, error) {
	id = sanitizeConnectionID(id)
	if id == "" {
		return nil, fmt.Errorf("connection id is required")
	}
	kind, driverName, connStr, dialect := parseConnectionDSN(kind, dsn)
	start := time.Now()
	if verbose.Enabled() {
		verbose.Log(VerboseEvent{
			System:    "database",
			Direction: "outbound",
			Operation: "open",
			Target:    kind + "://" + id + " " + maskedDSN(dsn),
			Preview:   "driver=" + driverName,
		})
	}
	db, err := sql.Open(driverName, connStr)
	if err != nil {
		if verbose.Enabled() {
			verbose.Log(VerboseEvent{
				System:    "database",
				Direction: "inbound",
				Operation: "open",
				Target:    kind + "://" + id + " " + maskedDSN(dsn),
				Duration:  time.Since(start),
				Status:    "error",
				Error:     err.Error(),
			})
		}
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		if verbose.Enabled() {
			verbose.Log(VerboseEvent{
				System:    "database",
				Direction: "inbound",
				Operation: "ping",
				Target:    kind + "://" + id + " " + maskedDSN(dsn),
				Duration:  time.Since(start),
				Status:    "error",
				Error:     err.Error(),
			})
		}
		return nil, err
	}
	if verbose.Enabled() {
		verbose.Log(VerboseEvent{
			System:    "database",
			Direction: "inbound",
			Operation: "ping",
			Target:    kind + "://" + id + " " + maskedDSN(dsn),
			Duration:  time.Since(start),
			Status:    "ok",
		})
	}
	if strings.TrimSpace(name) == "" {
		name = id
	}
	return &DBConnection{ID: id, Name: name, Kind: kind, DSN: dsn, Dialect: dialect, DB: db, Verbose: verbose}, nil
}

// QuickConnectFields carries the discrete host/port/user/password fields
// from the "Quick Connect" form so callers don't need to hand-craft a DSN
// string (and get its escaping/quoting rules right) themselves. This is
// especially valuable for SQL Server, whose native DSN syntax (URL form or
// ADO key=value form, instance names, encryption flags) trips up most users
// who just have a host, port, database, and credentials.
type QuickConnectFields struct {
	Kind      string
	Host      string
	Port      string
	Database  string
	User      string
	Password  string
	Instance  string // SQL Server named instance, e.g. "SQLEXPRESS"
	SSLMode   string // PostgreSQL sslmode, e.g. "disable", "require"
	Encrypt   string // SQL Server encrypt setting, e.g. "disable", "true"
	TrustCert bool   // SQL Server TrustServerCertificate
	Params    string // extra driver-specific query parameters (MySQL)
	AuthMode  string // SQL Server auth mode: "sql" (default), "windows-current", "windows-account"
}

// mssqlAuthMode normalizes the requested SQL Server authentication mode,
// defaulting to plain SQL Server authentication (username/password).
func mssqlAuthMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "windows-current":
		return "windows-current"
	case "windows-account":
		return "windows-account"
	default:
		return "sql"
	}
}

// buildDSNFromFields turns QuickConnectFields into a driver-ready DSN,
// applying sane per-database defaults (standard port, useful TLS/encoding
// defaults for local and self-signed setups) and properly escaping
// usernames/passwords so special characters don't break the connection
// string.
func buildDSNFromFields(f QuickConnectFields) (string, error) {
	host := strings.TrimSpace(f.Host)
	if host == "" {
		return "", fmt.Errorf("host is required")
	}
	user := strings.TrimSpace(f.User)
	database := strings.TrimSpace(f.Database)

	switch strings.ToLower(strings.TrimSpace(f.Kind)) {
	case "mssql", "sqlserver":
		port := strings.TrimSpace(f.Port)
		if port == "" {
			port = "1433"
		}
		u := &url.URL{Scheme: "sqlserver", Host: host + ":" + port}
		switch mssqlAuthMode(f.AuthMode) {
		case "windows-current":
			// Leave u.User unset: go-mssqldb's winsspi/ntlm integrated-auth
			// provider then authenticates as the OS user running DataDock.
			if user != "" || f.Password != "" {
				return "", fmt.Errorf("windows authentication (current user) does not take a username or password; clear those fields or switch authentication mode")
			}
		case "windows-account":
			if !strings.Contains(user, `\`) {
				return "", fmt.Errorf(`windows authentication needs a "DOMAIN\username" user name`)
			}
			u.User = url.UserPassword(user, f.Password)
		default: // "sql": plain SQL Server authentication
			if user == "" {
				return "", fmt.Errorf("user is required for SQL Server authentication")
			}
			u.User = url.UserPassword(user, f.Password)
		}
		if instance := strings.TrimSpace(f.Instance); instance != "" {
			u.Path = "/" + instance
		}
		q := url.Values{}
		if database != "" {
			q.Set("database", database)
		}
		encrypt := strings.TrimSpace(f.Encrypt)
		if encrypt == "" {
			encrypt = "disable"
		}
		q.Set("encrypt", encrypt)
		if f.TrustCert {
			q.Set("TrustServerCertificate", "true")
		}
		u.RawQuery = q.Encode()
		return u.String(), nil

	case "postgres", "postgresql":
		port := strings.TrimSpace(f.Port)
		if port == "" {
			port = "5432"
		}
		u := &url.URL{Scheme: "postgres", Host: host + ":" + port, Path: "/" + database}
		if user != "" {
			u.User = url.UserPassword(user, f.Password)
		}
		mode := strings.TrimSpace(f.SSLMode)
		if mode == "" {
			mode = "disable"
		}
		q := url.Values{}
		q.Set("sslmode", mode)
		u.RawQuery = q.Encode()
		return u.String(), nil

	case "mysql", "mariadb":
		port := strings.TrimSpace(f.Port)
		if port == "" {
			port = "3306"
		}
		cfg := mysqldriver.NewConfig()
		cfg.User = user
		cfg.Passwd = f.Password
		cfg.Net = "tcp"
		cfg.Addr = host + ":" + port
		cfg.DBName = database
		cfg.ParseTime = true
		dsn := cfg.FormatDSN()
		if params := strings.TrimSpace(f.Params); params != "" {
			sep := "&"
			if !strings.Contains(dsn, "?") {
				sep = "?"
			}
			dsn += sep + params
		}
		return dsn, nil

	case "sqlite":
		if database == "" {
			return "", fmt.Errorf("database (file path) is required for SQLite")
		}
		return database, nil

	default:
		return "", fmt.Errorf("quick connect does not support %q; use the raw DSN field instead", f.Kind)
	}
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
	// ADO/ODBC-style connection strings (e.g. pasted from SQL Server
	// Management Studio or the Azure Portal) don't carry a URL prefix, so
	// they need to be recognized by their characteristic keywords instead.
	case looksLikeADOConnectionString(lower):
		return "mssql"
	// libpq keyword/value connection strings ("host=... dbname=... user=...").
	case strings.Contains(lower, "dbname="):
		return "postgres"
	default:
		return "sqlite"
	}
}

// looksLikeADOConnectionString reports whether dsn (already lower-cased)
// resembles a SQL Server ADO/ODBC connection string such as
// "Server=host;Database=db;User Id=user;Password=pass;" or
// "Data Source=host;Initial Catalog=db;Integrated Security=true;".
func looksLikeADOConnectionString(lowerDSN string) bool {
	hasServer := strings.Contains(lowerDSN, "server=") || strings.Contains(lowerDSN, "data source=") || strings.Contains(lowerDSN, "addr=")
	if !hasServer {
		return false
	}
	return strings.Contains(lowerDSN, "user id=") ||
		strings.Contains(lowerDSN, "uid=") ||
		strings.Contains(lowerDSN, "initial catalog=") ||
		strings.Contains(lowerDSN, "integrated security=") ||
		strings.Contains(lowerDSN, "trustservercertificate=")
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
	// Not a scheme://user:pass@host URL — cover the other DSN shapes
	// DataDock explicitly supports: ADO/ODBC ("Server=...;Uid=sa;Pwd=...;")
	// and libpq keyword ("host=... password=..."). Both carry the same
	// plaintext-credential risk the URL branch above already masks.
	return redactInlineSecrets(dsn)
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
	rows, err := c.queryContext(ctx, "metadata.column_values", sampleColumnValuesQuery(c, table, column, limit))
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
