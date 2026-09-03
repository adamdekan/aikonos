package alerting

import "time"

const (
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// Alert is a security-relevant event produced by the Evaluator.
type Alert struct {
	Rule     string
	Severity string
	TenantID string
	Title    string
	Detail   string
	EventID  string
	FiredAt  time.Time
}
