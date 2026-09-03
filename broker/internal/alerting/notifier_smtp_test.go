package alerting

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// fakeSmtpServer listens on a random local port and records the raw SMTP session.
// It speaks just enough SMTP to let SMTPNotifier complete a single DATA exchange.
// Set quitError to make the server respond to QUIT with a 500 error instead of 221.
type fakeSmtpServer struct {
	ln        net.Listener
	mu        sync.Mutex
	sessions  []string // full DATA body of each completed message
	quitError bool     // if true, respond to QUIT with "500 error" instead of "221 bye"
}

func newFakeSmtpServer(t *testing.T) *fakeSmtpServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakeSmtpServer listen: %v", err)
	}
	s := &fakeSmtpServer{ln: ln}
	go s.serve()
	return s
}

func (s *fakeSmtpServer) Addr() string { return s.ln.Addr().String() }

func (s *fakeSmtpServer) Close() { s.ln.Close() }

func (s *fakeSmtpServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeSmtpServer) handle(conn net.Conn) {
	defer conn.Close()
	w := bufio.NewWriter(conn)
	r := bufio.NewReader(conn)

	send := func(line string) {
		w.WriteString(line + "\r\n")
		w.Flush()
	}

	send("220 fakemail ESMTP")

	var dataLines []string
	inData := false

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")

		if inData {
			if line == "." {
				s.mu.Lock()
				s.sessions = append(s.sessions, strings.Join(dataLines, "\n"))
				s.mu.Unlock()
				send("250 OK")
				inData = false
				dataLines = nil
			} else {
				dataLines = append(dataLines, line)
			}
			continue
		}

		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			send("250-fakemail")
			send("250 OK")
		case strings.HasPrefix(upper, "AUTH"):
			send("235 OK")
		case strings.HasPrefix(upper, "MAIL FROM"):
			send("250 OK")
		case strings.HasPrefix(upper, "RCPT TO"):
			send("250 OK")
		case upper == "DATA":
			send("354 go ahead")
			inData = true
		case upper == "QUIT":
			if s.quitError {
				send("500 internal error")
			} else {
				send("221 bye")
			}
			return
		default:
			send("500 unrecognised")
		}
	}
}

func (s *fakeSmtpServer) received() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]string, len(s.sessions))
	copy(cp, s.sessions)
	return cp
}

// ── SMTPNotifier tests ────────────────────────────────────────────────────────

func TestSMTPNotifier_DeliversMessage(t *testing.T) {
	srv := newFakeSmtpServer(t)
	defer srv.Close()

	host, port, _ := net.SplitHostPort(srv.Addr())
	n := &SMTPNotifier{
		Host: host,
		Port: port,
		From: "alerts@example.com",
		To:   "admin@example.com",
	}

	a := Alert{
		Rule:     "denial",
		Severity: SeverityWarning,
		TenantID: "t1",
		Title:    "tool.invoke",
		Detail:   "task/99",
		EventID:  "evt-smtp-1",
		FiredAt:  time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := n.Notify(ctx, a); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	// Wait for message to arrive
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(srv.received()) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	msgs := srv.received()
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}

	body := msgs[0]
	// RFC 822 headers
	if !strings.Contains(body, "From: alerts@example.com") {
		t.Errorf("message missing From header, got:\n%s", body)
	}
	if !strings.Contains(body, "To: admin@example.com") {
		t.Errorf("message missing To header, got:\n%s", body)
	}
	// Subject line must mention rule name
	if !strings.Contains(body, "denial") {
		t.Errorf("message subject/body missing rule name, got:\n%s", body)
	}
	// Body must mention EventID
	if !strings.Contains(body, "evt-smtp-1") {
		t.Errorf("message body missing event id, got:\n%s", body)
	}
}

func TestSMTPNotifier_UnreachableHostReturnsError(t *testing.T) {
	n := &SMTPNotifier{
		Host:        "127.0.0.1",
		Port:        "1", // nothing listening — connection refused
		From:        "a@b.com",
		To:          "c@d.com",
		DialTimeout: 2 * time.Second, // must return within the timeout, not hang
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	err := n.Notify(ctx, Alert{Rule: "denial"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("want error when SMTP host unreachable, got nil")
	}
	// connection refused is near-instant; verify we didn't hang past the dial timeout
	if elapsed > 3*time.Second {
		t.Errorf("Notify took %v; expected to return within dial timeout", elapsed)
	}
}

// ── multiNotifier tests ───────────────────────────────────────────────────────

func TestMultiNotifier_AllSinksReceive(t *testing.T) {
	fn1 := &fakeNotifier{}
	fn2 := &fakeNotifier{}
	log := zap.NewNop()

	mn := NewMultiNotifier(log, fn1, fn2)
	a := Alert{Rule: "denial", Severity: SeverityWarning}

	if err := mn.Notify(context.Background(), a); err != nil {
		// multiNotifier must not return error even if all succeed
		t.Fatalf("Notify: %v", err)
	}

	if len(fn1.received()) != 1 {
		t.Errorf("sink1: want 1 alert, got %d", len(fn1.received()))
	}
	if len(fn2.received()) != 1 {
		t.Errorf("sink2: want 1 alert, got %d", len(fn2.received()))
	}
}

func TestMultiNotifier_FailingSinkDoesNotBlockOthers(t *testing.T) {
	fn1 := &fakeNotifier{errOn: 1} // always errors
	fn2 := &fakeNotifier{}
	log := zap.NewNop()

	mn := NewMultiNotifier(log, fn1, fn2)
	a := Alert{Rule: "denial", Severity: SeverityWarning}

	// must not return error; fn2 still receives
	if err := mn.Notify(context.Background(), a); err != nil {
		t.Fatalf("Notify returned error: %v", err)
	}

	if len(fn2.received()) != 1 {
		t.Errorf("sink2: want 1 alert despite sink1 error, got %d", len(fn2.received()))
	}
}

func TestMultiNotifier_Empty(t *testing.T) {
	log := zap.NewNop()
	mn := NewMultiNotifier(log)
	if err := mn.Notify(context.Background(), Alert{Rule: "denial"}); err != nil {
		t.Fatalf("empty multiNotifier: %v", err)
	}
}

// TestSMTPNotifier_QuitFailureLogsWarn verifies that a 500 response to QUIT
// triggers the B.7 Warn path rather than silently discarding the error.
func TestSMTPNotifier_QuitFailureLogsWarn(t *testing.T) {
	srv := newFakeSmtpServer(t)
	srv.quitError = true
	defer srv.Close()

	core, logs := observer.New(zapcore.WarnLevel)
	logger := zap.New(core)

	host, port, _ := net.SplitHostPort(srv.Addr())
	n := &SMTPNotifier{
		Host:   host,
		Port:   port,
		From:   "alerts@example.com",
		To:     "admin@example.com",
		Logger: logger,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Notify must still succeed — message was delivered; QUIT is teardown only.
	if err := n.Notify(ctx, Alert{
		Rule:    "denial",
		EventID: "evt-quit-fail",
		FiredAt: time.Now(),
	}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	// The Warn entry for smtp QUIT failure must have been emitted.
	entries := logs.FilterMessage("alerting: smtp QUIT failed").All()
	if len(entries) == 0 {
		t.Error("expected at least one Warn log entry for smtp QUIT failure, got none")
	}
}
