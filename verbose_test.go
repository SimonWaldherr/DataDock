package main

import (
	"strings"
	"testing"
)

func TestVerboseRedactionMasksSensitiveValues(t *testing.T) {
	raw := `{"authorization":"Bearer abc123","messages":[{"content":"hello"}],"dsn":"postgres://user:secret@example/db","nested":{"api_key":"top-secret"}}`
	redacted := redactPreview(raw)
	for _, leak := range []string{"abc123", "top-secret", "user:secret"} {
		if strings.Contains(redacted, leak) {
			t.Fatalf("redacted preview leaked %q in %s", leak, redacted)
		}
	}
	for _, want := range []string{"[REDACTED]", "hello"} {
		if !strings.Contains(redacted, want) {
			t.Fatalf("redacted preview missing %q in %s", want, redacted)
		}
	}
}

func TestVerboseRedactionMasksURLCredentials(t *testing.T) {
	got := redactURL("postgres://user:secret@example.test/db?sslmode=require&password=secret")
	for _, leak := range []string{"user:secret", "password=secret"} {
		if strings.Contains(got, leak) {
			t.Fatalf("redacted URL leaked %q in %s", leak, got)
		}
	}
	if !strings.Contains(got, "%5BREDACTED%5D") {
		t.Fatalf("redacted URL missing marker: %s", got)
	}
}
