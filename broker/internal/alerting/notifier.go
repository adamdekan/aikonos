package alerting

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Notifier delivers an Alert to an external sink.
type Notifier interface {
	Notify(ctx context.Context, a Alert) error
}

// AlertRepo persists fired alerts. Implemented by db.AlertRepo; the interface
// lives here so the Engine can take it without an import cycle.
type AlertRepo interface {
	Insert(ctx context.Context, a StoredAlert) error
	List(ctx context.Context, tenantID string, limit int) ([]StoredAlert, error)
}

// StoredAlert is the persisted form of an Alert (alert_id added for DB storage).
type StoredAlert struct {
	ID       string
	TenantID string
	Rule     string
	Severity string
	Summary  map[string]interface{}
	FiredAt  time.Time
}

// alertToStored converts a fired Alert to its StoredAlert form.
// The id is a UUIDv7 string supplied by the caller.
func alertToStored(id string, a Alert) StoredAlert {
	return StoredAlert{
		ID:       id,
		TenantID: a.TenantID,
		Rule:     a.Rule,
		Severity: a.Severity,
		Summary: map[string]interface{}{
			"title":    a.Title,
			"detail":   a.Detail,
			"event_id": a.EventID,
		},
		FiredAt: a.FiredAt,
	}
}

// WebhookNotifier POSTs an Alert as JSON to a configured URL.
// Client should have a timeout set by the caller.
type WebhookNotifier struct {
	URL    string
	Client *http.Client
}

// withoutURL drops the request URL a *url.Error carries in its message: chat
// webhooks (Slack, Teams, Mattermost) put their credential in the URL path,
// and the error ends up in the logs.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func (n *WebhookNotifier) Notify(ctx context.Context, a Alert) error {
	body, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("alerting: marshal alert: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("alerting: build request: %w", withoutURL(err))
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.Client.Do(req)
	if err != nil {
		return fmt.Errorf("alerting: webhook POST: %w", withoutURL(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("alerting: webhook returned %d", resp.StatusCode)
	}
	return nil
}

// defaultSMTPTimeout is the dial+handshake deadline for SMTPNotifier when no
// explicit DialTimeout is set. smtp.SendMail has no timeout; a hung server
// would block the fire-and-forget goroutine indefinitely without this.
const defaultSMTPTimeout = 10 * time.Second

// SMTPNotifier sends an Alert as a plain-text email over SMTP.
// Username/Password are optional; omit both for unauthenticated relays.
// A nil or zero-value SMTPNotifier will error on Notify.
// DialTimeout caps the initial TCP dial + SMTP handshake; defaults to 10 s.
// Logger is optional; when set, SMTP-level warnings (e.g. Quit errors) are
// emitted at Warn rather than silently discarded.
type SMTPNotifier struct {
	Host        string
	Port        string
	From        string
	To          string
	Username    string
	Password    string
	DialTimeout time.Duration // 0 → defaultSMTPTimeout
	Logger      *zap.Logger
}

// Notify composes an RFC 822 message and submits it via SMTP. The connection
// is established fresh per call (fire-and-forget delivery; no connection pool).
func (n *SMTPNotifier) Notify(_ context.Context, a Alert) error {
	if n.Host == "" || n.Port == "" {
		return fmt.Errorf("alerting: SMTPNotifier: host and port are required")
	}

	timeout := n.DialTimeout
	if timeout <= 0 {
		timeout = defaultSMTPTimeout
	}

	addr := net.JoinHostPort(n.Host, n.Port)

	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return fmt.Errorf("alerting: smtp dial: %w", err)
	}

	c, err := smtp.NewClient(conn, n.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("alerting: smtp new client: %w", err)
	}
	defer c.Close()

	// L-08: credentials and approval links never cross the wire in clear.
	// Upgrade whenever the server offers STARTTLS; refuse one that does not,
	// unless it is a loopback relay (a local mail catcher).
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: n.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("alerting: smtp STARTTLS: %w", err)
		}
	} else if !isLoopbackHost(n.Host) {
		return fmt.Errorf("alerting: smtp server %s does not offer STARTTLS", n.Host)
	}

	if n.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", n.Username, n.Password, n.Host)); err != nil {
			return fmt.Errorf("alerting: smtp auth: %w", err)
		}
	}

	if err := c.Mail(n.From); err != nil {
		return fmt.Errorf("alerting: smtp MAIL FROM: %w", err)
	}
	to := a.To
	if len(to) == 0 {
		to = []string{n.To}
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("alerting: smtp RCPT TO: %w", err)
		}
	}

	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("alerting: smtp DATA: %w", err)
	}

	subject := fmt.Sprintf("[aikonOS alert] %s — %s (%s)", a.Rule, a.Severity, a.TenantID)
	if a.Link != "" {
		subject = fmt.Sprintf("[aikonOS] %s", a.Title)
	}
	body := n.composeMessage(a, subject, strings.Join(to, ", "))
	if _, err := fmt.Fprint(wc, body); err != nil {
		wc.Close()
		return fmt.Errorf("alerting: smtp write body: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("alerting: smtp close data: %w", err)
	}

	if err := c.Quit(); err != nil && n.Logger != nil {
		n.Logger.Warn("alerting: smtp QUIT failed",
			zap.String("host", n.Host),
			zap.Error(err))
	}
	return nil
}

// composeMessage builds an RFC 822 email body.
func (n *SMTPNotifier) composeMessage(a Alert, subject, to string) string {
	var b strings.Builder
	b.WriteString("From: " + n.From + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + subject + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString("aikonOS alerting notification\r\n\r\n")
	b.WriteString(fmt.Sprintf("Rule:      %s\r\n", a.Rule))
	b.WriteString(fmt.Sprintf("Severity:  %s\r\n", a.Severity))
	b.WriteString(fmt.Sprintf("Tenant:    %s\r\n", a.TenantID))
	b.WriteString(fmt.Sprintf("Title:     %s\r\n", a.Title))
	b.WriteString(fmt.Sprintf("Detail:    %s\r\n", a.Detail))
	b.WriteString(fmt.Sprintf("Event ID:  %s\r\n", a.EventID))
	b.WriteString(fmt.Sprintf("Fired at:  %s\r\n", a.FiredAt.UTC().Format(time.RFC3339)))
	if a.Link != "" {
		b.WriteString(fmt.Sprintf("Open:      %s\r\n", a.Link))
	}
	return b.String()
}

// isLoopbackHost reports whether host names this machine: "localhost" or a
// loopback IP literal.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// multiNotifier fans out an Alert to all configured sinks. A failing sink is
// logged and skipped — it never prevents other sinks from receiving the alert.
// Notify always returns nil (the "error" is surfaced only via the logger).
type multiNotifier struct {
	sinks []Notifier
	log   *zap.Logger
}

// NewMultiNotifier returns a Notifier that fans out to all sinks, logging and
// skipping any that fail. It always returns nil from Notify.
func NewMultiNotifier(log *zap.Logger, sinks ...Notifier) Notifier {
	return &multiNotifier{sinks: sinks, log: log}
}

func (m *multiNotifier) Notify(ctx context.Context, a Alert) error {
	for _, s := range m.sinks {
		if err := s.Notify(ctx, a); err != nil {
			m.log.Warn("alerting: notifier sink error",
				zap.String("rule", a.Rule),
				zap.Error(err))
		}
	}
	return nil
}
