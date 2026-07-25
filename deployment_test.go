package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestVersionEndpoint verifies the unauthenticated /version endpoint returns
// the running build identity as JSON — used by monitoring to detect version
// drift and by bug reports to pin the exact build.
func TestVersionEndpoint(t *testing.T) {
	rec := httptest.NewRecorder()
	versionHandler(rec, httptest.NewRequest(http.MethodGet, "/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("expected application/json, got %q", ct)
	}
	var body struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, rec.Body.String())
	}
	if strings.TrimSpace(body.Version) == "" {
		t.Fatalf("expected a non-empty version, got %q", rec.Body.String())
	}
}

// TestResolveVersionPrefersStamped confirms an -ldflags-stamped version wins
// over the build-info fallback.
func TestResolveVersionPrefersStamped(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })

	version = "1.2.3"
	if got := resolveVersion(); got != "1.2.3" {
		t.Fatalf("expected stamped version to win, got %q", got)
	}

	// With the default "dev", resolveVersion must still return something
	// identifiable (module version or VCS revision), never empty.
	version = "dev"
	if got := strings.TrimSpace(resolveVersion()); got == "" {
		t.Fatal("resolveVersion must never return empty")
	}
}
