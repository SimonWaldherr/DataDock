package main

import (
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// AuditLogger writes a minimal, append-only, JSON-lines record of write
// operations DataDock allows through: who (session), what (method/path/
// target), and the outcome (status). It is deliberately narrower than
// VerboseLogger: no request/response body previews, no read traffic, just
// enough to answer "who changed what, when" from the -audit-log file without
// turning on verbose mode (which is meant for diagnostics, not audit).
type AuditLogger struct {
	mu  sync.Mutex
	out *log.Logger
	f   *os.File
}

// AuditEvent is one line of the audit log.
type AuditEvent struct {
	Time      string `json:"time"`
	Session   string `json:"session,omitempty"`
	Username  string `json:"username,omitempty"`
	Method    string `json:"method,omitempty"`
	Path      string `json:"path,omitempty"`
	Operation string `json:"operation,omitempty"`
	Target    string `json:"target,omitempty"`
	Status    int    `json:"status,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// NewAuditLogger opens (creating if needed) path for append and returns a
// logger that writes one JSON object per line. A blank path returns a nil
// logger; nil *AuditLogger is safe to call Log/Close on.
func NewAuditLogger(path string) (*AuditLogger, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &AuditLogger{out: log.New(f, "", 0), f: f}, nil
}

func (a *AuditLogger) Enabled() bool {
	return a != nil && a.out != nil
}

func (a *AuditLogger) Close() error {
	if a == nil || a.f == nil {
		return nil
	}
	return a.f.Close()
}

// Log appends one redacted, structured audit event. Secrets are stripped the
// same way VerboseLogger strips them, since audit entries can otherwise leak
// DSNs or tokens embedded in a request path or error detail.
func (a *AuditLogger) Log(event AuditEvent) {
	if !a.Enabled() {
		return
	}
	event.Time = time.Now().UTC().Format(time.RFC3339Nano)
	event.Path = redactURL(event.Path)
	event.Detail = redactInlineSecrets(event.Detail)
	b, err := json.Marshal(event)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.out.Printf(`{"time":%q,"error":%q}`, event.Time, err.Error())
		return
	}
	a.out.Println(string(b))
}
