package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const runtimeSettingsTable = "__datadock_settings"
const managedConnectionsSettingKey = "managed_connections"

type RuntimeSettings struct {
	Dialect        string
	ConnectTimeout time.Duration
	QueryTimeout   time.Duration
	LLMBaseURL     string
	LLMAPIKey      string
	LLMModel       string
	LLMTimeout     time.Duration
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

	cfg := LLMConfig{
		BaseURL: strings.TrimSpace(s.LLMBaseURL),
		APIKey:  strings.TrimSpace(s.LLMAPIKey),
		Model:   strings.TrimSpace(s.LLMModel),
		Timeout: s.LLMTimeout,
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
	return nil
}

func (a *App) saveRuntimeSettings(ctx context.Context) error {
	if err := a.ensureRuntimeSettingsTable(ctx); err != nil {
		return err
	}
	s := a.currentRuntimeSettings()
	values := map[string]string{
		"dialect":         s.Dialect,
		"connect_timeout": s.ConnectTimeout.String(),
		"query_timeout":   s.QueryTimeout.String(),
		"llm_base_url":    s.LLMBaseURL,
		"llm_api_key":     s.LLMAPIKey,
		"llm_model":       s.LLMModel,
		"llm_timeout":     s.LLMTimeout.String(),
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
	if _, err := a.sqlDB.ExecContext(ctx, "DELETE FROM "+runtimeSettingsTable+" WHERE setting_key = ?", key); err != nil {
		return err
	}
	_, err := a.sqlDB.ExecContext(ctx,
		"INSERT INTO "+runtimeSettingsTable+" (setting_key, setting_value) VALUES (?, ?)",
		key, value,
	)
	return err
}

func (a *App) loadRuntimeSettings(ctx context.Context) (RuntimeSettings, bool, error) {
	if err := a.ensureRuntimeSettingsTable(ctx); err != nil {
		return RuntimeSettings{}, false, err
	}
	rows, err := a.sqlDB.QueryContext(ctx, "SELECT setting_key, setting_value FROM "+runtimeSettingsTable)
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
		conn, err := OpenManagedConnection(ctx, cfg.ID, cfg.Name, cfg.Kind, cfg.DSN)
		if err != nil {
			return fmt.Errorf("%s: %w", cfg.ID, err)
		}
		if err := a.conns.Add(conn); err != nil {
			_ = conn.DB.Close()
			return err
		}
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
	err := a.sqlDB.QueryRowContext(ctx, "SELECT setting_value FROM "+runtimeSettingsTable+" WHERE setting_key = ?", key).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || strings.Contains(strings.ToLower(err.Error()), "no rows") {
			return "", false, nil
		}
		return "", false, err
	}
	return value, true, nil
}

func (a *App) ensureRuntimeSettingsTable(ctx context.Context) error {
	_, err := a.sqlDB.ExecContext(ctx, "CREATE TABLE "+runtimeSettingsTable+" (setting_key TEXT, setting_value TEXT)")
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
		Dialect:        a.dialect.Name,
		ConnectTimeout: a.connectTimeout,
		QueryTimeout:   a.queryTimeout,
		LLMBaseURL:     a.llmConfig.BaseURL,
		LLMAPIKey:      a.llmConfig.APIKey,
		LLMModel:       a.llmConfig.Model,
		LLMTimeout:     a.llmConfig.Timeout,
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
	}
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
	return RuntimeSettings{
		Dialect:        values["dialect"],
		ConnectTimeout: connectTimeout,
		QueryTimeout:   queryTimeout,
		LLMBaseURL:     values["llm_base_url"],
		LLMAPIKey:      values["llm_api_key"],
		LLMModel:       values["llm_model"],
		LLMTimeout:     llmTimeout,
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
