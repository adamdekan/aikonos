package broker

import (
	"context"
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/alerting"
)

type captureNotifier struct{ got []alerting.Alert }

func (c *captureNotifier) Notify(_ context.Context, a alerting.Alert) error {
	c.got = append(c.got, a)
	return nil
}

type mapEmails map[string]string

func (m mapEmails) Email(_ context.Context, _, subject string) string { return m[subject] }

// TestApprovalNotify_DeepLinkToApprovers (AG-07 slice 2): an approval request
// reaches its approvers (known emails only) with a deep link to the Inbox.
func TestApprovalNotify_DeepLinkToApprovers(t *testing.T) {
	n := &captureNotifier{}
	notify := NewApprovalNotify(n, mapEmails{"bob": "bob@example.com"}, "https://aikonos.example.com/inbox")
	notify(context.Background(), "t1", "task-1", []string{"bob", "carol"}, 2)

	if len(n.got) != 1 {
		t.Fatalf("notifications = %d, want 1", len(n.got))
	}
	a := n.got[0]
	if a.Link != "https://aikonos.example.com/inbox?approval=task-1" {
		t.Errorf("Link = %q", a.Link)
	}
	if len(a.To) != 1 || a.To[0] != "bob@example.com" {
		t.Errorf("To = %v, want the one known approver email", a.To)
	}
}
