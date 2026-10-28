package broker

import (
	"context"
	"fmt"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/alerting"
)

// ApprovalNotifyFunc tells an approval request's approvers that it waits for
// them (AG-07 slice 2). SubmitPlan calls it in the background after it builds
// the gate; nil disables notification.
type ApprovalNotifyFunc func(ctx context.Context, tenantID, taskID string, approvers []string, requiresN int)

// NewApprovalNotify sends approval requests through the alerting sinks: the
// webhook gets the deep link, SMTP mails the approvers whose email the user
// directory knows (else the sink's configured recipient). inboxURL is the
// webui Inbox, e.g. https://aikonos.example.com/inbox.
func NewApprovalNotify(n alerting.Notifier, emails interface {
	Email(ctx context.Context, tenantID, subject string) string
}, inboxURL string) ApprovalNotifyFunc {
	return func(ctx context.Context, tenantID, taskID string, approvers []string, requiresN int) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		var to []string
		for _, a := range approvers {
			if e := emails.Email(ctx, tenantID, a); e != "" {
				to = append(to, e)
			}
		}
		_ = n.Notify(ctx, alerting.Alert{
			Rule:     "approval_requested",
			Severity: alerting.SeverityInfo,
			TenantID: tenantID,
			Title:    "approval needed for task " + taskID,
			Detail:   fmt.Sprintf("%d approval(s) required", requiresN),
			EventID:  taskID,
			FiredAt:  time.Now().UTC(),
			Link:     inboxURL + "?approval=" + taskID,
			To:       to,
		})
	}
}
