package main

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	tinysql "github.com/SimonWaldherr/tinySQL"
	tsqldriver "github.com/SimonWaldherr/tinySQL/driver"
)

//go:embed templates static
var webFS embed.FS

// autoPortRangeLow and autoPortRangeHigh bound the automatic free-port scan
// used when neither -addr, -port, nor a stored setting picks a port.
const (
	autoPortRangeLow  = 8000
	autoPortRangeHigh = 8100
)

func main() {
	addr := flag.String("addr", "", "HTTP listen address (host:port); when set, takes precedence over -port and any stored port setting")
	port := flag.Int("port", 0, fmt.Sprintf("HTTP listen port; 0 = use the stored setting or auto-detect a free port between %d and %d", autoPortRangeLow, autoPortRangeHigh))
	findFreePortFlag := flag.Bool("find-free-port", false, fmt.Sprintf("print a free TCP port between %d and %d and exit (tooling helper)", autoPortRangeLow, autoPortRangeHigh))
	dbFile := flag.String("db", "datadock.db", "Database file path (empty or :memory: for in-memory)")
	storageMode := flag.String("storage-mode", envDefault("DATADOCK_STORAGE_MODE", "memory"), "tinySQL storage mode: memory, disk, json, hybrid, index, or paged_index (encryption is supported only by disk/json/hybrid/index)")
	storageReadOnly := flag.Bool("storage-read-only", envBoolDefault("DATADOCK_STORAGE_READ_ONLY", false), "Open the configured tinySQL storage artifact read-only for serving")
	storageCacheBytes := flag.Int64("storage-cache-bytes", envInt64Default("DATADOCK_STORAGE_CACHE_BYTES", 0), "Maximum resident storage cache bytes for index, hybrid, and paged_index modes (0 uses tinySQL defaults)")
	tenant := flag.String("tenant", "default", "Tenant / schema name")
	sqlDialect := flag.String("dialect", envDefault("DATADOCK_SQL_DIALECT", "tinysql"), "SQL dialect profile for LLM guidance (tinysql, sqlite, postgres, mysql, mariadb, mssql)")
	llmBaseURL := flag.String("llm-base-url", envDefault("DATADOCK_LLM_BASE_URL", ""), "OpenAI-compatible base URL, e.g. https://api.openai.com/v1 or http://127.0.0.1:1234/v1")
	llmAPIKey := flag.String("llm-api-key", envDefault("DATADOCK_LLM_API_KEY", envDefault("OPENAI_API_KEY", "")), "OpenAI-compatible API key (optional for local providers)")
	llmModel := flag.String("llm-model", envDefault("DATADOCK_LLM_MODEL", ""), "LLM model name")
	connectTimeout := flag.Duration("connect-timeout", envDurationDefault("DATADOCK_CONNECT_TIMEOUT", 10*time.Second), "Timeout for adding/pinging managed database connections")
	queryTimeout := flag.Duration("query-timeout", envDurationDefault("DATADOCK_QUERY_TIMEOUT", 60*time.Second), "Default timeout for interactive SQL queries and exports")
	llmTimeout := flag.Duration("llm-timeout", envDurationDefault("DATADOCK_LLM_TIMEOUT", 45*time.Second), "Timeout for OpenAI-compatible LLM requests")
	verboseMode := flag.Bool("verbose", envBoolDefault("DATADOCK_VERBOSE", false), "Write redacted outbound/inbound system communication logs to stdout")
	watchDir := flag.String("watch-dir", envDefault("DATADOCK_WATCH_DIR", ""), "Optional directory to auto-import supported files into tinySQL tables")
	watchInterval := flag.Duration("watch-interval", envDurationDefault("DATADOCK_WATCH_INTERVAL", 3*time.Second), "Polling interval for -watch-dir auto-import")
	auditPath := flag.String("audit-log", envDefault("DATADOCK_AUDIT_LOG", ""), "Optional file path for a JSON-lines audit log of write operations (record edits, DDL, imports, migrations, admin changes, and write/DDL SQL)")
	authModeFlag := flag.String("auth-mode", envDefault("DATADOCK_AUTH_MODE", ""), "How DataDock gates access to itself: \"local\" (default: a single Admin password) or \"none\" (no login at all, for single-user/local use only; defaults to binding 127.0.0.1 unless -addr says otherwise, see -allow-insecure-remote)")
	allowInsecureRemote := flag.Bool("allow-insecure-remote", envBoolDefault("DATADOCK_ALLOW_INSECURE_REMOTE", false), "Allow -auth-mode=none to bind to a non-loopback address. Only set this on a network you already trust (e.g. a private VPN/Tailscale), since anyone who can reach that address gets full access with no login.")
	behindTLSProxy := flag.Bool("behind-tls-proxy", envBoolDefault("DATADOCK_BEHIND_TLS_PROXY", false), "Set when a TLS-terminating reverse proxy sits in front of DataDock, so the session cookie is marked Secure even though DataDock's own listener only ever sees plain HTTP.")
	flag.Parse()

	if *findFreePortFlag {
		p, err := findFreePort(autoPortRangeLow, autoPortRangeHigh)
		if err != nil {
			log.Fatalf("find free port: %v", err)
		}
		fmt.Println(p)
		return
	}

	verbose := NewVerboseLogger(*verboseMode)
	if verbose.Enabled() {
		verbose.Log(VerboseEvent{System: "datadock", Operation: "verbose_enabled", Target: "stdout", Preview: "redacted communication logging enabled"})
	}

	// Open or create the tinySQL database.
	encryptionKey, err := storageEncryptionKeyFromEnv()
	if err != nil {
		log.Fatalf("storage encryption: %v", err)
	}
	nativeDB, err := openNativeDBWithStorageOptions(*dbFile, *storageMode, encryptionKey, *storageReadOnly, *storageCacheBytes)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	if len(encryptionKey) == 0 {
		if mode, err := tinysql.ParseStorageMode(*storageMode); err == nil {
			switch mode {
			case tinysql.ModeDisk, tinysql.ModeJSON, tinysql.ModeHybrid, tinysql.ModeIndex:
				// This mode CAN encrypt at rest but isn't; settings and
				// managed-connection DSNs (which can carry credentials and
				// LLM/embedding API keys) are being written to disk in
				// plaintext, indistinguishable in the logs from an
				// encrypted setup unless this is called out explicitly.
				log.Printf("WARNING: storage mode %q writes to disk without DATADOCK_ENCRYPTION_KEY set; settings and stored connection credentials are stored in plaintext", *storageMode)
			}
		}
	}

	// Persist and close cleanly on shutdown. File-backed DataDock uses
	// tinySQL's memory mode with a save path (see openNativeDB), so Close
	// writes the final GOB snapshot without attaching a WAL.
	defer func() {
		if closeErr := nativeDB.Close(); closeErr != nil {
			log.Printf("database close: %v", closeErr)
		}
	}()

	// v0.20 deliberately isolates named mem:// DSNs. OpenWithDB is the
	// supported embedding path for sharing this native instance with
	// database/sql callers.
	if *tenant != "default" {
		log.Fatalf("tenant %q is not supported with the embedded database/sql bridge; use the default tenant", *tenant)
	}
	sqlDB, err := tsqldriver.OpenWithDB(nativeDB)
	if err != nil {
		log.Fatalf("sql.Open: %v", err)
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(4)

	if verbose.Enabled() {
		verbose.Log(VerboseEvent{System: "database", Direction: "outbound", Operation: "open", Target: "tinysql://default", Preview: "tenant=" + *tenant})
	}
	pingStart := time.Now()
	if err := sqlDB.PingContext(context.Background()); err != nil {
		if verbose.Enabled() {
			verbose.Log(VerboseEvent{System: "database", Direction: "inbound", Operation: "ping", Target: "tinysql://default", Duration: time.Since(pingStart), Error: err.Error()})
		}
		log.Fatalf("ping: %v", err)
	}
	if verbose.Enabled() {
		verbose.Log(VerboseEvent{System: "database", Direction: "inbound", Operation: "ping", Target: "tinysql://default", Duration: time.Since(pingStart), Status: "ok"})
	}

	if err := nativeDB.StartJobScheduler(verboseSQLJobExecutor{db: nativeDB, tenant: *tenant, verbose: verbose}); err != nil {
		log.Printf("job scheduler disabled: %v", err)
	} else {
		defer tinysql.StopJobScheduler(nativeDB)
	}

	tpl, err := parseTemplates()
	if err != nil {
		log.Fatalf("templates: %v", err)
	}

	app := newApp(nativeDB, sqlDB, *tenant, tpl)
	app.setVerboseLogger(verbose)
	settings := RuntimeSettings{
		Dialect:        *sqlDialect,
		ConnectTimeout: *connectTimeout,
		QueryTimeout:   *queryTimeout,
		LLMBaseURL:     *llmBaseURL,
		LLMAPIKey:      *llmAPIKey,
		LLMModel:       *llmModel,
		LLMTimeout:     *llmTimeout,
		Port:           *port,
		AuthMode:       *authModeFlag,
	}
	if stored, ok, err := app.loadRuntimeSettings(context.Background()); err != nil {
		log.Fatalf("load settings: %v", err)
	} else if ok {
		settings = mergeRuntimeSettingsWithExplicitFlags(stored, settings)
	}

	// Resolved once here (not just inside applyRuntimeSettings) because the
	// listen-address default below depends on it: auth-mode=none should
	// default to a loopback-only bind, not "all interfaces", so the common
	// case (a developer running `go run .` locally with -auth-mode=none)
	// is safe by construction without needing -allow-insecure-remote.
	effectiveAuthMode, err := normalizeAuthMode(settings.AuthMode)
	if err != nil {
		log.Fatalf("auth-mode: %v", err)
	}

	// Resolve the port to listen on, unless -addr gives an explicit host:port
	// that takes full precedence. Precedence otherwise: -port flag > stored
	// setting > auto-detected free port in the configured range.
	listenAddr := strings.TrimSpace(*addr)
	if listenAddr == "" {
		if !flagWasSet("port") && (settings.Port <= 0 || !isPortFree(settings.Port)) {
			p, err := findFreePort(autoPortRangeLow, autoPortRangeHigh)
			if err != nil {
				log.Fatalf("find free port: %v", err)
			}
			settings.Port = p
		}
		if effectiveAuthMode == AuthModeNone {
			listenAddr = fmt.Sprintf("127.0.0.1:%d", settings.Port)
		} else {
			listenAddr = fmt.Sprintf(":%d", settings.Port)
		}
	}

	// Set before applyRuntimeSettings: its auth-mode validation refuses to
	// enable AuthModeNone on a non-loopback bind unless allowInsecureRemote
	// is set, so it needs to see both up front (see settings.go).
	app.listenAddr = listenAddr
	app.allowInsecureRemote = *allowInsecureRemote
	app.behindTLSProxy = *behindTLSProxy
	app.authModeExplicit = flagWasSet("auth-mode") || strings.TrimSpace(os.Getenv("DATADOCK_AUTH_MODE")) != ""

	if err := app.applyRuntimeSettings(settings); err != nil {
		log.Fatalf("settings: %v", err)
	}
	if effectiveAuthMode == AuthModeNone && !isLoopbackAddr(listenAddr) {
		log.Printf("WARNING: -auth-mode=none with -allow-insecure-remote on %s — DataDock is reachable there with no login at all.", listenAddr)
	}
	if err := app.saveRuntimeSettings(context.Background()); err != nil {
		log.Fatalf("save settings: %v", err)
	}
	if err := app.migrateLegacyAdminPassword(context.Background()); err != nil {
		log.Fatalf("migrate legacy admin password: %v", err)
	}
	connCtx, connCancel := context.WithTimeout(context.Background(), app.currentConnectTimeout())
	if err := app.loadManagedConnections(connCtx); err != nil {
		log.Printf("load managed connections: %v", err)
	}
	if err := app.autoDetectManagedConnections(connCtx); err != nil {
		log.Printf("auto-detect managed connections: %v", err)
	}
	connCancel()
	if err := app.startMatchScheduler(context.Background()); err != nil {
		log.Printf("start match scheduler: %v", err)
	}
	if strings.TrimSpace(*auditPath) != "" {
		auditLogger, err := NewAuditLogger(*auditPath)
		if err != nil {
			log.Fatalf("audit log: could not open %q: %v", *auditPath, err)
		}
		app.setAuditLogger(auditLogger)
		defer auditLogger.Close()
		// Keep tinySQL's tamper-evident stream separate from DataDock's
		// request audit JSONL. The engine receives WithAuditText contexts at
		// every native single-statement execution, preserving exact SQL.
		nativeAudit, err := tinysql.OpenAuditLog(*auditPath + ".tinysql")
		if err != nil {
			log.Fatalf("tinySQL audit log: could not open %q: %v", *auditPath+".tinysql", err)
		}
		nativeDB.AttachAuditLog(nativeAudit)
		defer nativeAudit.Close()
		log.Printf("audit log: recording write operations to %s", *auditPath)
	}
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	if strings.TrimSpace(*watchDir) != "" {
		startAutoImportWatcher(watchCtx, app, *watchDir, *watchInterval)
	}

	mux := http.NewServeMux()
	// Unauthenticated and deliberately outside app.registerRoutes (no
	// session cookie, no DB/template rendering): a liveness/readiness probe
	// for process supervisors (systemd, Docker, Kubernetes) must stay cheap
	// and must not depend on anything that could itself be degraded.
	mux.HandleFunc("GET /healthz", healthzHandler)
	app.registerRoutes(mux)
	mux.Handle("GET /static/", cacheableStaticHandler(http.FileServer(http.FS(webFS))))

	// Middleware order (outermost first): every request is logged exactly
	// once with its final status, including ones recovered from a panic;
	// recoverMiddleware sits between logging and the actual routes so a
	// panic becomes a clean 500 the access log can still see, instead of
	// net/http's default behavior of just aborting the connection.
	handler := loggingMiddleware(recoverMiddleware(securityHeaders(app.csrfProtectedHandler(mux))))
	log.Printf("DataDock listening on %s  (db: %s, tenant: %s, dialect: %s, auth-mode: %s)", listenAddr, dbLabel(*dbFile), *tenant, app.currentDialect().Name, effectiveAuthMode)
	srv := &http.Server{
		Addr:    listenAddr,
		Handler: handler,
		// ReadHeaderTimeout guards against Slowloris-style attacks that
		// trickle in request headers to hold a connection open; it's the
		// one blanket deadline that's always safe regardless of how long a
		// particular request body/response takes.
		ReadHeaderTimeout: 10 * time.Second,
		// ReadTimeout bounds how long reading the full request (headers +
		// body) may take, so a slow/malicious client can't tie up a
		// connection indefinitely while uploading. 2 minutes comfortably
		// covers the largest supported import upload (16 MiB, see
		// importFileHandler) even on a slow link.
		ReadTimeout: 2 * time.Minute,
		// IdleTimeout closes keep-alive connections that sit idle between
		// requests, bounding the number of sockets a client can pin open
		// for free.
		IdleTimeout: 2 * time.Minute,
		// WriteTimeout is deliberately left unset. Query/LLM/connect
		// timeouts are already enforced per-request via context deadlines
		// derived from the admin-configurable settings (see
		// withQueryTimeout/withConnectTimeout and llmConfig.Timeout), and
		// full-table CSV/XLSX exports stream an unbounded number of rows —
		// a fixed server-wide write deadline would silently truncate a
		// legitimately large export instead of only catching the
		// stuck-connection case it's meant to guard against.
	}
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.ListenAndServe()
	}()

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case <-signalCtx.Done():
		log.Printf("shutdown signal received, draining in-flight requests")
		watchCancel()
		// Generous relative to ReadTimeout/IdleTimeout above: this is the
		// grace period for requests already in flight (e.g. a large export
		// or a slow LLM call) to finish before the process exits, not a
		// per-request limit.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("http shutdown: %v", err)
		}
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http server: %v", err)
		}
	}
}

// healthzHandler is a minimal liveness probe: if the process can accept a
// connection and return 200, it's up. It deliberately does not touch the
// database or render templates, so it stays meaningful even if those are
// the thing that's actually broken.
func healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// statusRecorder wraps a ResponseWriter to capture the status code that was
// actually sent, so logging middleware can report it after the handler has
// already written the response. It passes through Flush so handlers that
// stream a response (e.g. a large export) still work if wrapped.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(status int) {
	if !s.wroteHeader {
		s.status = status
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// loggingMiddleware writes one access-log line per request (method, path,
// status, duration, client address) after the handler completes. It skips
// /healthz to avoid drowning the log in probe traffic from process
// supervisors that poll every few seconds.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond), r.RemoteAddr)
	})
}

// recoverMiddleware turns a panic anywhere in the handler chain into a
// logged stack trace and a clean 500 response, instead of net/http's
// default of logging to stderr and abruptly closing the connection. This is
// a safety net, not a substitute for fixing the underlying bug.
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("panic serving %s %s: %v\n%s", r.Method, r.URL.Path, err, debug.Stack())
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// openNativeDB loads a file-backed DB or creates a new in-memory one.
func openNativeDB(filePath string) (*tinysql.DB, error) {
	return openNativeDBWithStorage(filePath, "memory", nil)
}

// openNativeDBWithStorage retains the historic memory-plus-snapshot default
// while allowing disk-backed tinySQL modes to use AES-256-GCM. WAL is never
// passed an encryption key because tinySQL deliberately does not encrypt WAL.
func openNativeDBWithStorage(filePath, modeName string, encryptionKey []byte) (*tinysql.DB, error) {
	return openNativeDBWithStorageOptions(filePath, modeName, encryptionKey, false, 0)
}

func openNativeDBWithStorageOptions(filePath, modeName string, encryptionKey []byte, readOnly bool, maxMemoryBytes int64) (*tinysql.DB, error) {
	if filePath == "" || filePath == ":memory:" {
		if len(encryptionKey) > 0 {
			return nil, fmt.Errorf("DATADOCK_ENCRYPTION_KEY requires a disk-backed storage mode (disk, json, hybrid, or index)")
		}
		if readOnly {
			return nil, fmt.Errorf("read-only serving requires an existing file or storage directory")
		}
		return tinysql.NewDB(), nil
	}
	mode, err := tinysql.ParseStorageMode(modeName)
	if err != nil {
		return nil, err
	}
	if maxMemoryBytes < 0 {
		return nil, fmt.Errorf("storage cache bytes must not be negative")
	}
	if len(encryptionKey) > 0 && mode != tinysql.ModeDisk && mode != tinysql.ModeJSON && mode != tinysql.ModeHybrid && mode != tinysql.ModeIndex {
		return nil, fmt.Errorf("storage encryption is supported only by disk, json, hybrid, and index modes; WAL and paged_index are intentionally not encrypted")
	}
	db, err := tinysql.OpenDB(tinysql.StorageConfig{
		Mode:           mode,
		Path:           filePath,
		EncryptionKey:  encryptionKey,
		ReadOnly:       readOnly,
		MaxMemoryBytes: maxMemoryBytes,
	})
	if err != nil {
		if os.IsNotExist(err) {
			return tinysql.NewDB(), nil
		}
		return nil, err
	}
	return db, nil
}

// storageEncryptionKeyFromEnv accepts a 32-byte AES-256 key encoded as hex
// or standard base64. It never reads from nor writes to runtime settings.
func storageEncryptionKeyFromEnv() ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv("DATADOCK_ENCRYPTION_KEY"))
	if raw == "" {
		return nil, nil
	}
	if key, err := hex.DecodeString(raw); err == nil && len(key) == tinysql.EncryptionKeySize {
		return key, nil
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(key) != tinysql.EncryptionKeySize {
		return nil, fmt.Errorf("DATADOCK_ENCRYPTION_KEY must be %d bytes encoded as hex or base64", tinysql.EncryptionKeySize)
	}
	return key, nil
}

// isPortFree reports whether a TCP port is currently available to bind on
// all interfaces.
func isPortFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// findFreePort scans [lo, hi] and returns the first port that can be bound.
func findFreePort(lo, hi int) (int, error) {
	for p := lo; p <= hi; p++ {
		if isPortFree(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port available in range %d-%d", lo, hi)
}

// dbLabel returns a human-readable label for the database location.
func dbLabel(filePath string) string {
	if filePath == "" || filePath == ":memory:" {
		return "in-memory"
	}
	return filePath
}

func envDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envDurationDefault(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("invalid duration in %s=%q, using %s", key, v, fallback)
		return fallback
	}
	return d
}

func envBoolDefault(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	switch strings.ToLower(v) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	case "0", "false", "f", "no", "n", "off":
		return false
	default:
		log.Printf("invalid boolean in %s=%q, using %t", key, v, fallback)
		return fallback
	}
}

func envInt64Default(key string, fallback int64) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		log.Printf("invalid integer in %s=%q, using %d", key, v, fallback)
		return fallback
	}
	return n
}

func mergeRuntimeSettingsWithExplicitFlags(stored, flags RuntimeSettings) RuntimeSettings {
	merged := stored
	if flagWasSet("dialect") {
		merged.Dialect = flags.Dialect
	}
	if flagWasSet("connect-timeout") {
		merged.ConnectTimeout = flags.ConnectTimeout
	}
	if flagWasSet("query-timeout") {
		merged.QueryTimeout = flags.QueryTimeout
	}
	if flagWasSet("llm-base-url") {
		merged.LLMBaseURL = flags.LLMBaseURL
	}
	if flagWasSet("llm-api-key") {
		merged.LLMAPIKey = flags.LLMAPIKey
	}
	if flagWasSet("llm-model") {
		merged.LLMModel = flags.LLMModel
	}
	if flagWasSet("llm-timeout") {
		merged.LLMTimeout = flags.LLMTimeout
	}
	if flagWasSet("port") {
		merged.Port = flags.Port
	}
	if flagWasSet("auth-mode") {
		merged.AuthMode = flags.AuthMode
	}
	return merged
}

func flagWasSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// parseTemplates parses all embedded HTML templates.
func parseTemplates() (*template.Template, error) {
	return template.New("").Funcs(template.FuncMap{
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
		"seq": func(n int) []int {
			s := make([]int, n)
			for i := range s {
				s[i] = i + 1
			}
			return s
		},
		// dict builds a map[string]interface{} for use inside template calls.
		"dict": func(pairs ...interface{}) (map[string]interface{}, error) {
			if len(pairs)%2 != 0 {
				return nil, fmt.Errorf("dict: odd number of arguments")
			}
			m := make(map[string]interface{}, len(pairs)/2)
			for i := 0; i < len(pairs); i += 2 {
				key, ok := pairs[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: key must be string, got %T", pairs[i])
				}
				m[key] = pairs[i+1]
			}
			return m, nil
		},
		"not": func(b bool) bool { return !b },
		// tablesOnly/viewsOnly split a mixed []TableObject (as injected into
		// every render() call) so the sidebar template can render the two
		// kind groups from one shared "sidebar_kind_group" partial instead
		// of duplicating the markup per kind.
		"tablesOnly": func(objects []TableObject) []TableObject {
			out := make([]TableObject, 0, len(objects))
			for _, o := range objects {
				if !strings.EqualFold(o.Kind, "view") {
					out = append(out, o)
				}
			}
			return out
		},
		"viewsOnly": func(objects []TableObject) []TableObject {
			out := make([]TableObject, 0, len(objects))
			for _, o := range objects {
				if strings.EqualFold(o.Kind, "view") {
					out = append(out, o)
				}
			}
			return out
		},
		// inputType maps a column's database type name to an HTML5 input
		// type, purely for client-side keyboard/validation affordance (the
		// form still posts a plain string either way, so this can't change
		// what gets written). Anything not recognized falls back to "text".
		"inputType": func(typeName string) string {
			switch strings.ToUpper(strings.SplitN(strings.TrimSpace(typeName), "(", 2)[0]) {
			case "INT", "INTEGER", "SMALLINT", "BIGINT", "TINYINT", "MEDIUMINT",
				"FLOAT", "DOUBLE", "REAL", "DECIMAL", "NUMERIC", "MONEY":
				return "number"
			default:
				return "text"
			}
		},
	}).ParseFS(webFS, "templates/*.html")
}

// securityHeaders adds baseline browser security headers.
// cacheableStaticHandler adds a short-lived Cache-Control to everything
// under /static/ (app.js, style.css, and any other embedded static asset),
// which previously had no cache headers or validators at all and was
// re-downloaded in full on every single page navigation. Short enough
// (5 minutes) that a DataDock upgrade's frontend changes show up well
// within a typical browsing session without needing a build-time cache-
// busting/content-hash scheme.
func cacheableStaticHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net https://cdnjs.cloudflare.com; "+
				"script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net https://cdnjs.cloudflare.com; "+
				"font-src 'self' https://cdn.jsdelivr.net https://cdnjs.cloudflare.com; "+
				"img-src 'self' data:; worker-src 'self' blob: data:; form-action 'self'; frame-ancestors 'none'; base-uri 'self'")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Belt-and-suspenders alongside the CSP's frame-ancestors 'none' for
		// browsers that only honor the older header.
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		next.ServeHTTP(w, r)
	})
}
