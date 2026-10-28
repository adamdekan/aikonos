package alerting

import "time"

const (
	SeverityInfo     = "info"
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
	// Link is a deep link for the reader to act on (an approval request in
	// the Inbox, AG-07); empty for rule alerts.
	Link string `json:",omitempty"`
	// To overrides the SMTP sink's configured recipient (the approvers of an
	// approval request). Kept off the webhook body.
	To []string `json:"-"`
}
