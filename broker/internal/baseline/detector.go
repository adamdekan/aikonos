// broker/internal/baseline/detector.go
// Pure drift detection for automated agent behavioral baseline learning
//. Check never touches the
// DB, OTel, or the clock — it is a plain function of its three arguments, so
// CP-B3 can call it at flush time with a real db.Baseline (adapted to the
// Baseline view below) and log/emit whatever it returns.
package baseline

import "time"

// Baseline is the detector's view of a learned envelope — deliberately
// decoupled from db.Baseline so this package's pure logic never depends on
// the db package's shape.
type Baseline struct {
	ToolSet       []string
	RpmP95        float64
	CostP95       float64
	SampleWindows int
}

// WindowSummary is one just-closed window's observed activity, as reported
// by DBRecorder.Flush via its ClosedWindow keys (CP-B3 fills in Tools and
// Invocations from the flushed deltas before calling Check).
type WindowSummary struct {
	Tenant      string
	Agent       string
	WindowStart time.Time
	Tools       []string
	Invocations int64
}

// Drift is one detected deviation from an agent's learned envelope.
type Drift struct {
	Kind     string // "unknown_tool" or "rate"
	Tool     string // set for "unknown_tool"
	Observed float64
	Ceiling  float64
}

// DetectorConfig tunes drift sensitivity.
type DetectorConfig struct {
	MinSampleWindows int
	DriftMultiplier  float64
}

// Detector is stateless; its methods are pure functions of their arguments.
type Detector struct{}

// NewDetector constructs a Detector.
func NewDetector() *Detector {
	return &Detector{}
}

// Check evaluates one window against a learned baseline and returns every
// drift found. During the learning phase (baseline.SampleWindows below the
// configured minimum) it always returns nil — drift is never emitted before
// the baseline is mature.
func (d *Detector) Check(baseline Baseline, window WindowSummary, cfg DetectorConfig) []Drift {
	if baseline.SampleWindows < cfg.MinSampleWindows {
		return nil
	}

	var drifts []Drift

	known := make(map[string]bool, len(baseline.ToolSet))
	for _, t := range baseline.ToolSet {
		known[t] = true
	}
	for _, t := range window.Tools {
		if !known[t] {
			drifts = append(drifts, Drift{Kind: "unknown_tool", Tool: t})
		}
	}

	ceiling := baseline.RpmP95 * cfg.DriftMultiplier
	observed := float64(window.Invocations)
	if observed > ceiling {
		drifts = append(drifts, Drift{Kind: "rate", Observed: observed, Ceiling: ceiling})
	}

	return drifts
}
