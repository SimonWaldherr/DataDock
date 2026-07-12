package main

import "testing"

func TestApplyRuntimeSettingsMatchMaxRowsDefaultsAndValidates(t *testing.T) {
	app := newTestApp(t)

	// Zero means "use the default", not "unlimited" or "zero rows".
	settings := app.currentRuntimeSettings()
	settings.MatchMaxRows = 0
	if err := app.applyRuntimeSettings(settings); err != nil {
		t.Fatalf("applyRuntimeSettings with MatchMaxRows=0: %v", err)
	}
	if got := app.currentMatchMaxRows(); got != defaultMatchMaxRows {
		t.Errorf("currentMatchMaxRows() = %d, want default %d", got, defaultMatchMaxRows)
	}

	settings.MatchMaxRows = -1
	if err := app.applyRuntimeSettings(settings); err == nil {
		t.Error("expected an error for a negative match_max_rows")
	}

	settings.MatchMaxRows = maxMatchMaxRows + 1
	if err := app.applyRuntimeSettings(settings); err == nil {
		t.Error("expected an error for a match_max_rows above the sanity ceiling")
	}

	settings.MatchMaxRows = 5_000_000
	if err := app.applyRuntimeSettings(settings); err != nil {
		t.Fatalf("applyRuntimeSettings with a valid custom value: %v", err)
	}
	if got := app.currentMatchMaxRows(); got != 5_000_000 {
		t.Errorf("currentMatchMaxRows() = %d, want 5000000", got)
	}
}

func TestApplyRuntimeSettingsAuthModeNoneRequiresLoopbackOrOptIn(t *testing.T) {
	app := newTestApp(t)

	// Default (no listenAddr recorded yet, as in a fresh App): nothing to
	// check against, so auth-mode=none is accepted.
	settings := app.currentRuntimeSettings()
	settings.AuthMode = "none"
	if err := app.applyRuntimeSettings(settings); err != nil {
		t.Fatalf("applyRuntimeSettings(auth-mode=none) with no recorded listen address: %v", err)
	}
	if got := app.currentAuthMode(); got != AuthModeNone {
		t.Errorf("currentAuthMode() = %q, want %q", got, AuthModeNone)
	}

	// A non-loopback bind must refuse auth-mode=none...
	app.listenAddr = "0.0.0.0:8080"
	app.allowInsecureRemote = false
	settings.AuthMode = "none"
	if err := app.applyRuntimeSettings(settings); err == nil {
		t.Error("expected an error switching to auth-mode=none on a non-loopback bind without -allow-insecure-remote")
	}
	// ...falling back to auth-mode=local must still work on that same bind.
	settings.AuthMode = "local"
	if err := app.applyRuntimeSettings(settings); err != nil {
		t.Errorf("applyRuntimeSettings(auth-mode=local) on a non-loopback bind: %v", err)
	}

	// ...but is allowed once the operator opts in explicitly.
	app.allowInsecureRemote = true
	settings.AuthMode = "none"
	if err := app.applyRuntimeSettings(settings); err != nil {
		t.Errorf("applyRuntimeSettings(auth-mode=none) with allowInsecureRemote=true: %v", err)
	}

	// A loopback bind is always fine, opt-in or not.
	app.listenAddr = "127.0.0.1:8080"
	app.allowInsecureRemote = false
	settings.AuthMode = "none"
	if err := app.applyRuntimeSettings(settings); err != nil {
		t.Errorf("applyRuntimeSettings(auth-mode=none) on a loopback bind: %v", err)
	}

	// An unknown auth mode is rejected outright.
	settings.AuthMode = "bogus"
	if err := app.applyRuntimeSettings(settings); err == nil {
		t.Error("expected an error for an unknown auth-mode value")
	}
}

func TestRuntimeSettingsWithoutVectorFieldsRemainCompatible(t *testing.T) {
	settings, err := runtimeSettingsFromStoredValues(map[string]string{})
	if err != nil {
		t.Fatalf("load legacy settings: %v", err)
	}
	app := newTestApp(t)
	if err := app.applyRuntimeSettings(settings); err != nil {
		t.Fatalf("apply legacy settings: %v", err)
	}
	if app.currentVectorIndex() != "flat" || app.currentVectorWarmEnabled() {
		t.Fatalf("legacy vector defaults = index %q warm %t", app.currentVectorIndex(), app.currentVectorWarmEnabled())
	}
}
