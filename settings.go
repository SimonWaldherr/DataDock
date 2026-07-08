package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const runtimeSettingsTable = "__datadock_settings"
const managedConnectionsSettingKey = "managed_connections"

const (
	defaultPageSize  = 50
	defaultUITheme   = "workbench"
	defaultUIDensity = "comfortable"
	// defaultMatchMaxRows bounds how many rows per side the Matching
	// feature (see matching.go) will load into memory for one synchronous
	// request. 2,000,000 comfortably covers typical master-data tables
	// (customers, articles) with room to spare; admins can raise it further
	// in Admin Settings for larger tables, at the cost of more memory and
	// request time per match run.
	defaultMatchMaxRows = 2_000_000
	// maxMatchMaxRows is a sanity ceiling against fat-finger settings input
	// (an extra zero) causing the server to try to load an unreasonable
	// number of rows into memory at once.
	maxMatchMaxRows = 50_000_000
)

var allowedUIThemes = []string{"workbench", "midnight", "forest", "contrast", "solaris", "xp", "classic2000", "kde"}
var allowedUIDensities = []string{"comfortable", "compact"}

type RuntimeSettings struct {
	Dialect           string
	ConnectTimeout    time.Duration
	QueryTimeout      time.Duration
	LLMBaseURL        string
	LLMAPIKey         string
	LLMModel          string
	LLMTimeout        time.Duration
	ReadOnlyMode      bool
	PageSize          int
	MatchMaxRows      int
	DefaultTheme      string
	DefaultDensity    string
	Port              int
	AdminPasswordHash string
	AuthMode          string
}

type RuntimeSettingsView struct {
	Dialect          string `json:"dialect"`
	ConnectTimeout   string `json:"connect_timeout"`
	QueryTimeout     string `json:"query_timeout"`
	LLMBaseURL       string `json:"llm_base_url"`
	LLMModel         string `json:"llm_model"`
	LLMTimeout       string `json:"llm_timeout"`
	LLMConfigured    bool   `json:"llm_configured"`
	LLMAPIKeySet     bool   `json:"llm_api_key_set"`
	LLMAPIKeyDisplay string `json:"llm_api_key_display"`
	ReadOnlyMode     bool   `json:"read_only_mode"`
	PageSize         int    `json:"page_size"`
	MatchMaxRows     int    `json:"match_max_rows"`
	DefaultTheme     string `json:"default_theme"`
	DefaultDensity   string `json:"default_density"`
	Port             int    `json:"port"`
	AdminPasswordSet bool   `json:"admin_password_set"`
	AuthMode         string `json:"auth_mode"`
}

func isAllowedValue(value string, allowed []string) bool {
	for _, v := range allowed {
		if v == value {
			return true
		}
	}
	return false
}

func (a *App) applyRuntimeSettings(s RuntimeSettings) error {
	dialect := strings.TrimSpace(s.Dialect)
	if dialect == "" {
		dialect = "tinysql"
	}
	if s.ConnectTimeout < 0 {
		return fmt.Errorf("connect timeout must not be negative")
	}
	if s.QueryTimeout < 0 {
		return fmt.Errorf("query timeout must not be negative")
	}
	if s.LLMTimeout < 0 {
		return fmt.Errorf("llm timeout must not be negative")
	}
	if s.ConnectTimeout == 0 {
		s.ConnectTimeout = 10 * time.Second
	}
	if s.QueryTimeout == 0 {
		s.QueryTimeout = 60 * time.Second
	}
	if s.LLMTimeout == 0 {
		s.LLMTimeout = 45 * time.Second
	}
	if s.PageSize < 0 {
		return fmt.Errorf("page size must not be negative")
	}
	if s.PageSize == 0 {
		s.PageSize = defaultPageSize
	}
	if s.PageSize > 1000 {
		return fmt.Errorf("page size must not exceed 1000")
	}
	if s.MatchMaxRows < 0 {
		return fmt.Errorf("match max rows must not be negative")
	}
	if s.MatchMaxRows == 0 {
		s.MatchMaxRows = defaultMatchMaxRows
	}
	if s.MatchMaxRows > maxMatchMaxRows {
		return fmt.Errorf("match max rows must not exceed %d", maxMatchMaxRows)
	}
	if strings.TrimSpace(s.DefaultTheme) == "" {
		s.DefaultTheme = defaultUITheme
	} else if !isAllowedValue(s.DefaultTheme, allowedUIThemes) {
		return fmt.Errorf("unknown default theme %q", s.DefaultTheme)
	}
	if strings.TrimSpace(s.DefaultDensity) == "" {
		s.DefaultDensity = defaultUIDensity
	} else if !isAllowedValue(s.DefaultDensity, allowedUIDensities) {
		return fmt.Errorf("unknown default density %q", s.DefaultDensity)
	}
	if s.Port < 0 || s.Port > 65535 {
		return fmt.Errorf("port must be between 0 and 65535")
	}
	authMode, err := normalizeAuthMode(s.AuthMode)
	if err != nil {
		return err
	}
	// A future mode-switch UI/API will call this same function, so the
	// safety net belongs here rather than only at startup: refuse to drop
	// to no-login while the server is actually reachable beyond localhost,
	// unless the operator has explicitly acknowledged that with
	// -allow-insecure-remote.
	if authMode == AuthModeNone && a.listenAddr != "" && !isLoopbackAddr(a.listenAddr) && !a.allowInsecureRemote {
		return fmt.Errorf("cannot use auth-mode=none while DataDock is listening on a non-loopback address (%s); use auth-mode=local for a network-reachable server, or pass -allow-insecure-remote if this is intentional (e.g. a private VPN/Tailscale network)", a.listenAddr)
	}

	cfg := LLMConfig{
		BaseURL: strings.TrimSpace(s.LLMBaseURL),
		APIKey:  strings.TrimSpace(s.LLMAPIKey),
		Model:   strings.TrimSpace(s.LLMModel),
		Timeout: s.LLMTimeout,
		Verbose: a.verboseLogger(),
	}

	var llm LLMClient
	if cfg.BaseURL != "" && cfg.Model != "" {
		llm = NewOpenAICompatibleClient(cfg)
	}

	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	a.dialect = DialectProfileForName(dialect)
	a.connectTimeout = s.ConnectTimeout
	a.queryTimeout = s.QueryTimeout
	a.llmConfig = cfg
	a.llm = llm
	a.readOnlyMode = s.ReadOnlyMode
	a.pageSize = s.PageSize
	a.matchMaxRows = s.MatchMaxRows
	a.defaultTheme = s.DefaultTheme
	a.defaultDensity = s.DefaultDensity
	a.port = s.Port
	a.adminPasswordHash = s.AdminPasswordHash
	a.authMode = authMode
	return nil
}

func (a *App) saveRuntimeSettings(ctx context.Context) error {
	if err := a.ensureRuntimeSettingsTable(ctx); err != nil {
		return err
	}
	s := a.currentRuntimeSettings()
	values := map[string]string{
		"dialect":             s.Dialect,
		"connect_timeout":     s.ConnectTimeout.String(),
		"query_timeout":       s.QueryTimeout.String(),
		"llm_base_url":        s.LLMBaseURL,
		"llm_api_key":         s.LLMAPIKey,
		"llm_model":           s.LLMModel,
		"llm_timeout":         s.LLMTimeout.String(),
		"read_only_mode":      strconv.FormatBool(s.ReadOnlyMode),
		"page_size":           strconv.Itoa(s.PageSize),
		"match_max_rows":      strconv.Itoa(s.MatchMaxRows),
		"default_theme":       s.DefaultTheme,
		"default_density":     s.DefaultDensity,
		"port":                strconv.Itoa(s.Port),
		"admin_password_hash": s.AdminPasswordHash,
		"auth_mode":           s.AuthMode,
	}
	for key, value := range values {
		if err := a.saveSetting(ctx, key, value); err != nil {
			return fmt.Errorf("save runtime setting %s: %w", key, err)
		}
	}
	return nil
}

func (a *App) saveSetting(ctx context.Context, key, value string) error {
	if err := a.ensureRuntimeSettingsTable(ctx); err != nil {
		return err
	}
	conn := a.localTinySQLConn()
	if _, err := a.execConn(ctx, conn, "settings.delete", settingsDeleteSQL, key); err != nil {
		return err
	}
	_, err := a.execConn(ctx, conn, "settings.insert", settingsInsertSQL, key, value)
	return err
}

func (a *App) loadRuntimeSettings(ctx context.Context) (RuntimeSettings, bool, error) {
	if err := a.ensureRuntimeSettingsTable(ctx); err != nil {
		return RuntimeSettings{}, false, err
	}
	rows, err := a.queryConn(ctx, a.localTinySQLConn(), "settings.load", settingsSelectAllSQL)
	if err != nil {
		return RuntimeSettings{}, false, fmt.Errorf("load runtime settings: %w", err)
	}
	defer rows.Close()

	values := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return RuntimeSettings{}, false, fmt.Errorf("scan runtime setting: %w", err)
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return RuntimeSettings{}, false, fmt.Errorf("iterate runtime settings: %w", err)
	}
	if len(values) == 0 {
		return RuntimeSettings{}, false, nil
	}

	settings, err := runtimeSettingsFromStoredValues(values)
	if err != nil {
		return RuntimeSettings{}, false, err
	}
	return settings, true, nil
}

func (a *App) saveManagedConnections(ctx context.Context) error {
	value, err := encodeManagedConnectionConfigs(a.conns.StoredConfigs())
	if err != nil {
		return err
	}
	return a.saveSetting(ctx, managedConnectionsSettingKey, value)
}

func (a *App) loadManagedConnections(ctx context.Context) error {
	value, ok, err := a.loadSetting(ctx, managedConnectionsSettingKey)
	if err != nil || !ok {
		return err
	}
	configs, err := decodeManagedConnectionConfigs(value)
	if err != nil {
		return err
	}
	return a.openManagedConnectionConfigs(ctx, configs, false)
}

func (a *App) autoDetectManagedConnections(ctx context.Context) error {
	configs := discoverEnvironmentConnections()
	if len(configs) == 0 {
		return nil
	}
	if err := a.openManagedConnectionConfigs(ctx, configs, true); err != nil {
		return err
	}
	return a.saveManagedConnections(ctx)
}

func (a *App) openManagedConnectionConfigs(ctx context.Context, configs []ManagedConnectionConfig, skipExisting bool) error {
	for _, cfg := range configs {
		cfg.ID = sanitizeConnectionID(cfg.ID)
		if cfg.ID == "" || strings.TrimSpace(cfg.DSN) == "" {
			continue
		}
		if skipExisting && a.conns.Get(cfg.ID) != nil {
			continue
		}
		conn, err := OpenManagedConnectionVerbose(ctx, cfg.ID, cfg.Name, cfg.Kind, cfg.DSN, a.verboseLogger())
		if err != nil {
			return fmt.Errorf("%s: %w", cfg.ID, err)
		}
		if err := a.conns.Add(conn); err != nil {
			_ = conn.DB.Close()
			return err
		}
		// These configs came from the settings store or an operator-set env
		// var, not an interactive form submission, so they're already
		// "persisted" in the sense that matters: saving settings again
		// (e.g. via SetDefault below) must keep including them.
		a.conns.MarkPersisted(conn.ID)
		if cfg.Default {
			if err := a.conns.SetDefault(conn.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *App) loadSetting(ctx context.Context, key string) (string, bool, error) {
	if err := a.ensureRuntimeSettingsTable(ctx); err != nil {
		return "", false, err
	}
	var value string
	rows, err := a.queryConn(ctx, a.localTinySQLConn(), "settings.load_one", settingsSelectOneSQL, key)
	if err == nil {
		defer rows.Close()
		if rows.Next() {
			err = rows.Scan(&value)
		} else {
			err = sql.ErrNoRows
		}
		if err == nil {
			err = rows.Err()
		}
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || strings.Contains(strings.ToLower(err.Error()), "no rows") {
			return "", false, nil
		}
		return "", false, err
	}
	return value, true, nil
}

func (a *App) ensureRuntimeSettingsTable(ctx context.Context) error {
	_, err := a.execConn(ctx, a.localTinySQLConn(), "settings.ensure_table", settingsEnsureTableSQL)
	if err == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return nil
	}
	return fmt.Errorf("ensure runtime settings table: %w", err)
}

func (a *App) currentRuntimeSettings() RuntimeSettings {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	return RuntimeSettings{
		Dialect:           a.dialect.Name,
		ConnectTimeout:    a.connectTimeout,
		QueryTimeout:      a.queryTimeout,
		LLMBaseURL:        a.llmConfig.BaseURL,
		LLMAPIKey:         a.llmConfig.APIKey,
		LLMModel:          a.llmConfig.Model,
		LLMTimeout:        a.llmConfig.Timeout,
		ReadOnlyMode:      a.readOnlyMode,
		PageSize:          a.pageSize,
		MatchMaxRows:      a.matchMaxRows,
		DefaultTheme:      a.defaultTheme,
		DefaultDensity:    a.defaultDensity,
		Port:              a.port,
		AdminPasswordHash: a.adminPasswordHash,
		AuthMode:          string(a.authMode),
	}
}

func (a *App) runtimeSettingsView() RuntimeSettingsView {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	return RuntimeSettingsView{
		Dialect:          a.dialect.Name,
		ConnectTimeout:   a.connectTimeout.String(),
		QueryTimeout:     a.queryTimeout.String(),
		LLMBaseURL:       a.llmConfig.BaseURL,
		LLMModel:         a.llmConfig.Model,
		LLMTimeout:       a.llmConfig.Timeout.String(),
		LLMConfigured:    a.llm != nil,
		LLMAPIKeySet:     a.llmConfig.APIKey != "",
		LLMAPIKeyDisplay: maskedSecret(a.llmConfig.APIKey),
		ReadOnlyMode:     a.readOnlyMode,
		PageSize:         a.pageSize,
		MatchMaxRows:     a.matchMaxRows,
		DefaultTheme:     a.defaultTheme,
		DefaultDensity:   a.defaultDensity,
		Port:             a.port,
		AdminPasswordSet: strings.TrimSpace(a.adminPasswordHash) != "",
		AuthMode:         string(a.authMode),
	}
}

func (a *App) currentPort() int {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	return a.port
}

func (a *App) currentAdminPasswordHash() string {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	return a.adminPasswordHash
}

func (a *App) adminPasswordIsSet() bool {
	return strings.TrimSpace(a.currentAdminPasswordHash()) != ""
}

func (a *App) currentReadOnlyMode() bool {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	return a.readOnlyMode
}

func (a *App) currentPageSize() int {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	if a.pageSize <= 0 {
		return defaultPageSize
	}
	return a.pageSize
}

func (a *App) currentMatchMaxRows() int {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	if a.matchMaxRows <= 0 {
		return defaultMatchMaxRows
	}
	return a.matchMaxRows
}

func (a *App) currentDefaultTheme() string {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	if a.defaultTheme == "" {
		return defaultUITheme
	}
	return a.defaultTheme
}

func (a *App) currentDefaultDensity() string {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	if a.defaultDensity == "" {
		return defaultUIDensity
	}
	return a.defaultDensity
}

func (a *App) llmClient() LLMClient {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	return a.llm
}

func (a *App) currentDialect() DialectProfile {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	return a.dialect
}

func (a *App) currentConnectTimeout() time.Duration {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	return a.connectTimeout
}

func (a *App) currentQueryTimeout() time.Duration {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	return a.queryTimeout
}

func (a *App) currentLLMAPIKey() string {
	a.settingsMu.RLock()
	defer a.settingsMu.RUnlock()
	return a.llmConfig.APIKey
}

func runtimeSettingsFromStoredValues(values map[string]string) (RuntimeSettings, error) {
	connectTimeout, err := parseStoredDuration(values["connect_timeout"])
	if err != nil {
		return RuntimeSettings{}, fmt.Errorf("connect_timeout: %w", err)
	}
	queryTimeout, err := parseStoredDuration(values["query_timeout"])
	if err != nil {
		return RuntimeSettings{}, fmt.Errorf("query_timeout: %w", err)
	}
	llmTimeout, err := parseStoredDuration(values["llm_timeout"])
	if err != nil {
		return RuntimeSettings{}, fmt.Errorf("llm_timeout: %w", err)
	}
	pageSize := defaultPageSize
	if raw := strings.TrimSpace(values["page_size"]); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			pageSize = n
		}
	}
	matchMaxRows := defaultMatchMaxRows
	if raw := strings.TrimSpace(values["match_max_rows"]); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			matchMaxRows = n
		}
	}
	readOnlyMode, _ := strconv.ParseBool(values["read_only_mode"])
	defaultTheme := strings.TrimSpace(values["default_theme"])
	if defaultTheme == "" {
		defaultTheme = defaultUITheme
	}
	defaultDensity := strings.TrimSpace(values["default_density"])
	if defaultDensity == "" {
		defaultDensity = defaultUIDensity
	}
	port := 0
	if raw := strings.TrimSpace(values["port"]); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 65535 {
			port = n
		}
	}
	return RuntimeSettings{
		Dialect:           values["dialect"],
		ConnectTimeout:    connectTimeout,
		QueryTimeout:      queryTimeout,
		LLMBaseURL:        values["llm_base_url"],
		LLMAPIKey:         values["llm_api_key"],
		LLMModel:          values["llm_model"],
		LLMTimeout:        llmTimeout,
		ReadOnlyMode:      readOnlyMode,
		PageSize:          pageSize,
		MatchMaxRows:      matchMaxRows,
		DefaultTheme:      defaultTheme,
		DefaultDensity:    defaultDensity,
		Port:              port,
		AdminPasswordHash: values["admin_password_hash"],
		AuthMode:          values["auth_mode"],
	}, nil
}

func parseStoredDuration(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	return time.ParseDuration(value)
}

func maskedSecret(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= 6 {
		return "******"
	}
	return s[:3] + "..." + s[len(s)-3:]
}
