package jobs

import "testing"

func TestBuildKeepsMillisecondFields(t *testing.T) {
	job, err := Build(Config{
		Name:         "daily_rollup",
		SQL:          "SELECT 1",
		ScheduleType: "INTERVAL",
		IntervalMs:   24 * 60 * 60 * 1000,
		MaxRuntimeMs: 5 * 60 * 1000,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if got, want := job.IntervalMs, int64(24*60*60*1000); got != want {
		t.Fatalf("IntervalMs = %d, want %d", got, want)
	}
	if got, want := job.MaxRuntimeMs, int64(5*60*1000); got != want {
		t.Fatalf("MaxRuntimeMs = %d, want %d", got, want)
	}
}
