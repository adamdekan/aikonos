package baseline

import (
	"testing"
	"time"
)

func TestDetector_Check_LearningPhaseSilence(t *testing.T) {
	d := NewDetector()
	baseline := Baseline{
		ToolSet:       []string{"web.fetch"},
		RpmP95:        10,
		SampleWindows: 5,
	}
	window := WindowSummary{
		Tenant:      "t1",
		Agent:       "agent-1",
		WindowStart: time.Now(),
		Tools:       []string{"unknown.tool"},
		Invocations: 1000, // would trigger both drift kinds if mature
	}
	cfg := DetectorConfig{MinSampleWindows: 30, DriftMultiplier: 2.0}

	drifts := d.Check(baseline, window, cfg)
	if drifts != nil {
		t.Fatalf("expected no drift during learning phase, got %+v", drifts)
	}
}

func TestDetector_Check_UnknownTool(t *testing.T) {
	d := NewDetector()
	baseline := Baseline{
		ToolSet:       []string{"web.fetch", "doc.write"},
		RpmP95:        10,
		SampleWindows: 30,
	}
	window := WindowSummary{
		Tools:       []string{"web.fetch", "email.draft"},
		Invocations: 5,
	}
	cfg := DetectorConfig{MinSampleWindows: 30, DriftMultiplier: 2.0}

	drifts := d.Check(baseline, window, cfg)
	if len(drifts) != 1 {
		t.Fatalf("expected exactly one drift, got %+v", drifts)
	}
	if drifts[0].Kind != "unknown_tool" || drifts[0].Tool != "email.draft" {
		t.Fatalf("drift = %+v, want unknown_tool for email.draft", drifts[0])
	}
}

func TestDetector_Check_RateDriftOverThreshold(t *testing.T) {
	d := NewDetector()
	baseline := Baseline{
		ToolSet:       []string{"web.fetch"},
		RpmP95:        10,
		SampleWindows: 30,
	}
	window := WindowSummary{
		Tools:       []string{"web.fetch"},
		Invocations: 21, // > 10 * 2.0
	}
	cfg := DetectorConfig{MinSampleWindows: 30, DriftMultiplier: 2.0}

	drifts := d.Check(baseline, window, cfg)
	if len(drifts) != 1 {
		t.Fatalf("expected exactly one drift, got %+v", drifts)
	}
	if drifts[0].Kind != "rate" || drifts[0].Observed != 21 || drifts[0].Ceiling != 20 {
		t.Fatalf("drift = %+v, want rate observed=21 ceiling=20", drifts[0])
	}
}

func TestDetector_Check_RateAtThreshold_NoDrift(t *testing.T) {
	d := NewDetector()
	baseline := Baseline{
		ToolSet:       []string{"web.fetch"},
		RpmP95:        10,
		SampleWindows: 30,
	}
	window := WindowSummary{
		Tools:       []string{"web.fetch"},
		Invocations: 20, // == 10 * 2.0, not "over" the ceiling
	}
	cfg := DetectorConfig{MinSampleWindows: 30, DriftMultiplier: 2.0}

	drifts := d.Check(baseline, window, cfg)
	if drifts != nil {
		t.Fatalf("expected no drift exactly at threshold, got %+v", drifts)
	}
}

func TestDetector_Check_RateUnderThreshold_NoDrift(t *testing.T) {
	d := NewDetector()
	baseline := Baseline{
		ToolSet:       []string{"web.fetch"},
		RpmP95:        10,
		SampleWindows: 30,
	}
	window := WindowSummary{
		Tools:       []string{"web.fetch"},
		Invocations: 5,
	}
	cfg := DetectorConfig{MinSampleWindows: 30, DriftMultiplier: 2.0}

	drifts := d.Check(baseline, window, cfg)
	if drifts != nil {
		t.Fatalf("expected no drift under threshold, got %+v", drifts)
	}
}

func TestDetector_Check_EmptyBaseline(t *testing.T) {
	d := NewDetector()
	var baseline Baseline // zero value: SampleWindows=0
	window := WindowSummary{
		Tools:       []string{"web.fetch"},
		Invocations: 5,
	}
	cfg := DetectorConfig{MinSampleWindows: 30, DriftMultiplier: 2.0}

	drifts := d.Check(baseline, window, cfg)
	if drifts != nil {
		t.Fatalf("expected no drift against an empty (immature) baseline, got %+v", drifts)
	}
}
