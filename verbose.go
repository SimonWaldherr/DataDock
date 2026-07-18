package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const verbosePreviewLimit = 2048

var sensitiveJSONKeys = map[string]bool{
	"api_key":       true,
	"apikey":        true,
	"authorization": true,
	"cookie":        true,
	"dsn":           true,
	"key":           true,
	"password":      true,
	"pwd":           true, // SQL Server ADO connection strings (Pwd=...)
	"secret":        true,
	"token":         true,
}

type VerboseLogger struct {
	out *log.Logger
}

type VerboseEvent struct {
	System        string        `json:"system"`
	Direction     string        `json:"direction,omitempty"`
	Operation     string        `json:"operation,omitempty"`
	Method        string        `json:"method,omitempty"`
	Target        string        `json:"target,omitempty"`
	Status        string        `json:"status,omitempty"`
	Duration      time.Duration `json:"-"`
	DurationMs    int64         `json:"duration_ms,omitempty"`
	SQL           string        `json:"sql,omitempty"`
	ArgsCount     int           `json:"args_count,omitempty"`
	RequestBytes  int64         `json:"request_bytes,omitempty"`
	ResponseBytes int64         `json:"response_bytes,omitempty"`
	Preview       string        `json:"preview,omitempty"`
	Error         string        `json:"error,omitempty"`
}

func NewVerboseLogger(enabled bool) *VerboseLogger {
	if !enabled {
		return nil
	}
	return &VerboseLogger{out: log.New(os.Stdout, "datadock verbose ", log.LstdFlags|log.Lmicroseconds)}
}

func (v *VerboseLogger) Enabled() bool {
	return v != nil && v.out != nil
}

func (v *VerboseLogger) Log(event VerboseEvent) {
	if !v.Enabled() {
		return
	}
	event.Target = redactURL(event.Target)
	event.SQL = redactInlineSecrets(event.SQL)
	event.Preview = redactPreview(event.Preview)
	event.Error = redactInlineSecrets(event.Error)
	if event.Duration > 0 {
		event.DurationMs = event.Duration.Milliseconds()
	}
	b, err := json.Marshal(event)
	if err != nil {
		v.out.Printf(`{"system":"verbose","error":%q}`, err.Error())
		return
	}
	v.out.Print(string(b))
}

func (v *VerboseLogger) HTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	return &http.Client{
		Timeout: timeout,
		Transport: verboseRoundTripper{
			base:   http.DefaultTransport,
			logger: v,
		},
	}
}

type verboseRoundTripper struct {
	base   http.RoundTripper
	logger *VerboseLogger
}

func (t verboseRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if !t.logger.Enabled() {
		return base.RoundTrip(req)
	}
	var reqPreview string
	var reqBytes int64
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err == nil {
			reqBytes = int64(len(body))
			reqPreview = previewBytes(body)
			req.Body = io.NopCloser(bytes.NewReader(body))
		}
	}
	start := time.Now()
	t.logger.Log(VerboseEvent{
		System:       "http",
		Direction:    "outbound",
		Operation:    "request",
		Method:       req.Method,
		Target:       req.URL.String(),
		RequestBytes: reqBytes,
		Preview:      reqPreview,
	})
	resp, err := base.RoundTrip(req)
	if err != nil {
		t.logger.Log(VerboseEvent{
			System:    "http",
			Direction: "inbound",
			Operation: "response",
			Method:    req.Method,
			Target:    req.URL.String(),
			Duration:  time.Since(start),
			Error:     err.Error(),
		})
		return nil, err
	}
	t.logger.Log(VerboseEvent{
		System:        "http",
		Direction:     "inbound",
		Operation:     "response",
		Method:        req.Method,
		Target:        req.URL.String(),
		Status:        resp.Status,
		Duration:      time.Since(start),
		ResponseBytes: resp.ContentLength,
	})
	return resp, nil
}

func previewBytes(b []byte) string {
	if len(b) > verbosePreviewLimit {
		return string(b[:verbosePreviewLimit]) + fmt.Sprintf("... [truncated %d bytes]", len(b)-verbosePreviewLimit)
	}
	return string(b)
}

func redactPreview(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var decoded any
	if json.Unmarshal([]byte(s), &decoded) == nil {
		redactJSONValue(decoded)
		if b, err := json.Marshal(decoded); err == nil {
			s = string(b)
		}
	}
	if len(s) > verbosePreviewLimit {
		s = s[:verbosePreviewLimit] + "... [truncated]"
	}
	return redactInlineSecrets(s)
}

func redactJSONValue(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if isSensitiveKey(k) {
				x[k] = "[REDACTED]"
				continue
			}
			redactJSONValue(val)
		}
	case []any:
		for _, val := range x {
			redactJSONValue(val)
		}
	}
}

func isSensitiveKey(k string) bool {
	k = strings.ToLower(strings.TrimSpace(k))
	if sensitiveJSONKeys[k] {
		return true
	}
	return strings.Contains(k, "password") || strings.Contains(k, "secret") || strings.Contains(k, "token")
}

func redactURL(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	if strings.Contains(raw, " ") {
		return redactInlineSecrets(raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return redactInlineSecrets(raw)
	}
	if u.User != nil {
		if name := u.User.Username(); name != "" {
			u.User = url.UserPassword(name, "[REDACTED]")
		} else {
			u.User = url.User("[REDACTED]")
		}
	}
	q := u.Query()
	for key := range q {
		if isSensitiveKey(key) || strings.EqualFold(key, "apikey") {
			q.Set(key, "[REDACTED]")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func redactInlineSecrets(s string) string {
	if s == "" {
		return ""
	}
	replacements := []*regexp.Regexp{
		regexp.MustCompile(`(?i)(authorization\s*[:=]\s*(?:bearer\s+|basic\s+)?)[^\s,;]+`),
		regexp.MustCompile(`(?i)(api[_-]?key\s*[:=]\s*)[^\s,;]+`),
		regexp.MustCompile(`(?i)(password\s*[=:]\s*)[^\s,;]+`),
		// "Pwd" is the ADO/ODBC connection-string alias for the same
		// credential (e.g. SQL Server's "Server=...;Uid=sa;Pwd=secret;").
		regexp.MustCompile(`(?i)(pwd\s*[=:]\s*)[^\s,;]+`),
		regexp.MustCompile(`(?i)(token\s*[=:]\s*)[^\s,;]+`),
		regexp.MustCompile(`(?i)(secret\s*[=:]\s*)[^\s,;]+`),
	}
	for _, re := range replacements {
		s = re.ReplaceAllString(s, `${1}[REDACTED]`)
	}
	return s
}
