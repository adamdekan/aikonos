package alerting

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWebhookNotifier_200(t *testing.T) {
	var got Alert
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("unmarshal body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &WebhookNotifier{URL: srv.URL, Client: &http.Client{Timeout: 5 * time.Second}}
	a := Alert{
		Rule:     "denial",
		Severity: SeverityWarning,
		TenantID: "t1",
		Title:    "tool.invoke",
		Detail:   "task/1",
		EventID:  "evt-1",
		FiredAt:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	if err := n.Notify(context.Background(), a); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if got.Rule != a.Rule {
		t.Errorf("rule: want %q, got %q", a.Rule, got.Rule)
	}
	if got.TenantID != a.TenantID {
		t.Errorf("tenant: want %q, got %q", a.TenantID, got.TenantID)
	}
	if got.EventID != a.EventID {
		t.Errorf("event id: want %q, got %q", a.EventID, got.EventID)
	}
}

func TestWebhookNotifier_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := &WebhookNotifier{URL: srv.URL, Client: &http.Client{Timeout: 5 * time.Second}}
	err := n.Notify(context.Background(), Alert{Rule: "denial"})
	if err == nil {
		t.Fatal("want error for 500 response, got nil")
	}
}
