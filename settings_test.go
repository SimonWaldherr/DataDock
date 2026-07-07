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
