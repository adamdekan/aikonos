package db

import (
	"testing"

	"github.com/google/uuid"
)

func TestTaskOnBehalfOf(t *testing.T) {
	agent, client, grant := uuid.New(), uuid.New(), uuid.New()
	sender := "u-sender"
	cases := []struct {
		name string
		task Task
		want OnBehalfOf
	}{
		{"human owner", Task{OwnerUserID: "u-1"}, OnBehalfOf{Subject: "u-1"}},
		{"svc- owner", Task{OwnerUserID: "svc-" + agent.String(), AgentID: &agent, ExternalClientID: &client},
			OnBehalfOf{Actor: agent.String(), ServiceRun: true, ExternalClientID: client.String()}},
		{"agent-bound", Task{OwnerUserID: "u-1", AgentID: &agent, GrantID: &grant},
			OnBehalfOf{Subject: "u-1", Actor: agent.String(), GrantID: grant.String()}},
		{"delegated", Task{OwnerUserID: "u-2", DelegatedByUserID: &sender},
			OnBehalfOf{Subject: "u-2", DelegatedBy: sender}},
	}
	for _, tc := range cases {
		if got := tc.task.OnBehalfOf(); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
