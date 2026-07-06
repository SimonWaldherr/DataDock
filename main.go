package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tinysql "github.com/SimonWaldherr/tinySQL"
	tsqldriver "github.com/SimonWaldherr/tinySQL/driver"
)

//go:embed templates static
var webFS embed.FS

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dbFile := flag.String("db", "datadock.db", "Database file path (empty or :memory: for in-memory)")
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
	auditPath := flag.String("audit-log", envDefault("DATADOCK_AUDIT_LOG", ""), "Optional path for a tamper-evident tinySQL audit log")
	adminUser := flag.String("admin-user", envDefault("DATADOCK_ADMIN_USER", "admin"), "Username for HTTP Basic authentication on Admin pages and Admin APIs")
	adminPassword := flag.String("admin-password", envDefault("DATADOCK_ADMIN_PASSWORD", ""), "Password for HTTP Basic authentication on Admin pages and Admin APIs; generated at startup when empty")
	flag.Parse()
	verbose := NewVerboseLogger(*verboseMode)
	if verbose.Enabled() {
		verbose.Log(VerboseEvent{System: "datadock", Operation: "verbose_enabled", Target: "stdout", Preview: "redacted communication logging enabled"})
	}

	// Open or create the tinySQL database.
	nativeDB, err := openNativeDB(*dbFile)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}

	// Autosave on clean shutdown when using a file.
	if *dbFile != "" && *dbFile != ":memory:" {
		defer func() {
			if saveErr := tinysql.SaveToFile(nativeDB, *dbFile); saveErr != nil {
				log.Printf("autosave: %v", saveErr)
			}
		}()
	}

	// Register the native DB instance with the database/sql driver so that
	// sql.Open("tinysql", ...) shares the same underlying storage.
	tsqldriver.SetDefaultDB(nativeDB)

	sqlDB, err := sql.Open("tinysql", "mem://?tenant="+*tenant)
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
	adminUserName := strings.TrimSpace(*adminUser)
	if adminUserName == "" {
		adminUserName = "admin"
	}
	adminPass := *adminPassword
	if adminPass == "" {
		generated, err := newGeneratedAdminPassword()
		if err != nil {
			log.Fatalf("generate admin password: %v", err)
		}
		adminPass = generated
		log.Printf("Admin Basic Auth enabled with generated credentials: user=%q password=%q", adminUserName, adminPass)
	} else {
		log.Printf("Admin Basic Auth enabled for user %q", adminUserName)
	}
	app.setAdminAuth(AdminAuthConfig{Username: adminUserName, Password: adminPass})
	settings := RuntimeSettings{
		Dialect:        *sqlDialect,
		ConnectTimeout: *connectTimeout,
		QueryTimeout:   *queryTimeout,
		LLMBaseURL:     *llmBaseURL,
		LLMAPIKey:      *llmAPIKey,
		LLMModel:       *llmModel,
		LLMTimeout:     *llmTimeout,
	}
	if stored, ok, err := app.loadRuntimeSettings(context.Background()); err != nil {
		log.Fatalf("load settings: %v", err)
	} else if ok {
		settings = mergeRuntimeSettingsWithExplicitFlags(stored, settings)
	}
	if err := app.applyRuntimeSettings(settings); err != nil {
		log.Fatalf("settings: %v", err)
	}
	if err := app.saveRuntimeSettings(context.Background()); err != nil {
		log.Fatalf("save settings: %v", err)
	}
	connCtx, connCancel := context.WithTimeout(context.Background(), app.currentConnectTimeout())
	if err := app.loadManagedConnections(connCtx); err != nil {
		log.Printf("load managed connections: %v", err)
	}
	if err := app.autoDetectManagedConnections(connCtx); err != nil {
		log.Printf("auto-detect managed connections: %v", err)
	}
	connCancel()
	if strings.TrimSpace(*auditPath) != "" {
		log.Printf("audit log flag ignored: github.com/SimonWaldherr/tinySQL v0.12.0 does not expose audit logging")
	}
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	if strings.TrimSpace(*watchDir) != "" {
		startAutoImportWatcher(watchCtx, app, *watchDir, *watchInterval)
	}

	mux := http.NewServeMux()
	app.registerRoutes(mux)
	mux.Handle("GET /static/", http.FileServer(http.FS(webFS)))

	handler := securityHeaders(mux)
	log.Printf("DataDock listening on %s  (db: %s, tenant: %s, dialect: %s)", *addr, dbLabel(*dbFile), *tenant, app.currentDialect().Name)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.ListenAndServe()
	}()

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case <-signalCtx.Done():
		watchCancel()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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

// openNativeDB loads a file-backed DB or creates a new in-memory one.
func openNativeDB(filePath string) (*tinysql.DB, error) {
	if filePath == "" || filePath == ":memory:" {
		return tinysql.NewDB(), nil
	}
	db, err := tinysql.LoadFromFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return tinysql.NewDB(), nil
		}
		return nil, err
	}
	return db, nil
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
	}).ParseFS(webFS, "templates/*.html")
}

// securityHeaders adds baseline browser security headers.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net https://cdnjs.cloudflare.com; "+
				"script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net https://cdnjs.cloudflare.com; "+
				"font-src 'self' https://cdn.jsdelivr.net https://cdnjs.cloudflare.com; "+
				"img-src 'self' data:; worker-src 'self' blob: data:; form-action 'self'; frame-ancestors 'none'; base-uri 'self'")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}
