package toolproxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/netacl"
	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

func newTestProxy(t *testing.T) *Proxy {
	t.Helper()
	return New(Config{
		WebFetch:  WebFetchConfig{AllowPrivateHosts: true}, // reach httptest (127.0.0.1)
		Workspace: WorkspaceConfig{Root: t.TempDir()},
	}, nil)
}

func TestWebFetch_Success_ExtractsText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><head><title>x</title><style>a{}</style></head><body><p>Hello</p><script>bad()</script></body></html>"))
	}))
	defer srv.Close()

	res, err := newTestProxy(t).Invoke(context.Background(), Request{
		ToolID: "web.fetch", EffectClass: planv1.EffectClass_READ_ONLY,
		Args: map[string]any{"url": srv.URL},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got error %q", res.Error)
	}
	if res.Output["status_code"].(int) != 200 {
		t.Errorf("status_code = %v", res.Output["status_code"])
	}
	content := res.Output["content"].(string)
	if !strings.Contains(content, "Hello") {
		t.Errorf("expected extracted text to contain Hello, got %q", content)
	}
	if strings.Contains(content, "bad()") || strings.Contains(content, "a{}") {
		t.Errorf("script/style content should be stripped, got %q", content)
	}
	if res.CostUnits < 1 {
		t.Errorf("expected cost >= 1, got %d", res.CostUnits)
	}
}

func TestWebFetch_SSRFGuardBlocksPrivate(t *testing.T) {
	// SSRF guard ON (default config).
	p := New(Config{Workspace: WorkspaceConfig{Root: t.TempDir()}}, nil)
	for _, u := range []string{"http://localhost/x", "http://127.0.0.1:8080/", "http://169.254.169.254/latest/meta-data"} {
		res, err := p.Invoke(context.Background(), Request{ToolID: "web.fetch", Args: map[string]any{"url": u}})
		if err != nil {
			t.Fatalf("Invoke(%s): unexpected proxy error %v", u, err)
		}
		if res.Success {
			t.Errorf("expected SSRF guard to block %s", u)
		}
	}
}

func TestIsDisallowedIP(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":       true,  // loopback
		"10.0.0.5":        true,  // RFC1918
		"192.168.1.1":     true,  // RFC1918
		"169.254.169.254": true,  // link-local (cloud metadata)
		"0.0.0.0":         true,  // unspecified
		"::1":             true,  // ipv6 loopback
		"8.8.8.8":         false, // public
		"93.184.216.34":   false, // public (example.com)
	}
	for ipStr, want := range cases {
		if got := isDisallowedIP(net.ParseIP(ipStr)); got != want {
			t.Errorf("isDisallowedIP(%s) = %v, want %v", ipStr, got, want)
		}
	}
	if !isDisallowedIP(nil) {
		t.Error("nil IP must be treated as disallowed")
	}
}

// The connect-time dialer guard must block even when validateFetchURL is
// bypassed (the redirect / DNS-rebind path). Hit an httptest server (127.0.0.1)
// directly through a hardened client with private hosts disallowed.
func TestHardenedClient_BlocksPrivateAtConnect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	_, err := hardenedClient(5*time.Second, false, nil).Get(srv.URL)
	if err == nil {
		t.Fatal("expected connect to 127.0.0.1 to be blocked by the dialer guard")
	}
	if !strings.Contains(err.Error(), "loopback/private") {
		t.Errorf("expected blocked-target error, got %v", err)
	}
}

// fakeACL allows everything except hosts in deny.
type fakeACL struct{ deny map[string]bool }

func (f fakeACL) Enabled() bool { return true }
func (f fakeACL) Decide(_ context.Context, _, _, host string) netacl.Action {
	if f.deny[host] {
		return netacl.Deny
	}
	return netacl.Allow
}

func TestWebFetch_ACLBlocksDeniedHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	host := mustHost(t, srv.URL)

	p := New(Config{WebFetch: WebFetchConfig{AllowPrivateHosts: true, ACL: fakeACL{deny: map[string]bool{host: true}}}}, nil)
	res, err := p.Invoke(context.Background(), Request{ToolID: "web.fetch", Args: map[string]any{"url": srv.URL}})
	if err != nil {
		t.Fatalf("proxy error: %v", err)
	}
	if res.Success || !strings.Contains(res.Error, "not permitted") {
		t.Fatalf("expected ACL block, got %+v", res)
	}
}

func TestWebFetch_ACLBlocksRedirectToDeniedHost(t *testing.T) {
	// dest is a "denied" host; src redirects to it. The initial host is allowed,
	// so only the CheckRedirect ACL hop should block the request.
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("secret"))
	}))
	defer dest.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL, http.StatusFound)
	}))
	defer src.Close()

	destHost := mustHost(t, dest.URL)
	p := New(Config{WebFetch: WebFetchConfig{AllowPrivateHosts: true, ACL: fakeACL{deny: map[string]bool{destHost: true}}}}, nil)
	res, err := p.Invoke(context.Background(), Request{ToolID: "web.fetch", Args: map[string]any{"url": src.URL}})
	if err != nil {
		t.Fatalf("proxy error: %v", err)
	}
	if res.Success {
		t.Fatalf("expected redirect to a denied host to be blocked, got %+v", res)
	}
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}

func TestWebFetch_RejectsNonHTTPScheme(t *testing.T) {
	res, _ := newTestProxy(t).Invoke(context.Background(), Request{
		ToolID: "web.fetch", Args: map[string]any{"url": "file:///etc/passwd"},
	})
	if res.Success || !strings.Contains(res.Error, "http/https") {
		t.Errorf("expected scheme rejection, got %+v", res)
	}
}

func TestWebFetch_MissingURL(t *testing.T) {
	res, _ := newTestProxy(t).Invoke(context.Background(), Request{ToolID: "web.fetch", Args: map[string]any{}})
	if res.Success || !strings.Contains(res.Error, "url") {
		t.Errorf("expected missing-url error, got %+v", res)
	}
}

func TestDocWrite_WritesFileWithinWorkspace(t *testing.T) {
	root := t.TempDir()
	p := New(Config{Workspace: WorkspaceConfig{Root: root}}, nil)
	res, err := p.Invoke(context.Background(), Request{
		ToolID: "doc.write", TenantID: "ten1", TaskID: "task1",
		Args: map[string]any{"path": "notes/out.md", "body": "hello world"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got %q", res.Error)
	}
	if res.Output["bytes_written"].(int) != len("hello world") {
		t.Errorf("bytes_written = %v", res.Output["bytes_written"])
	}
	want := filepath.Join(root, "ten1", "task1", "notes", "out.md")
	b, readErr := os.ReadFile(want)
	if readErr != nil {
		t.Fatalf("expected file at %s: %v", want, readErr)
	}
	if string(b) != "hello world" {
		t.Errorf("file content = %q", b)
	}
}

func TestDocWrite_RejectsTraversal(t *testing.T) {
	p := New(Config{Workspace: WorkspaceConfig{Root: t.TempDir()}}, nil)
	for _, bad := range []string{"../../../etc/passwd", "/etc/passwd"} {
		res, _ := p.Invoke(context.Background(), Request{
			ToolID: "doc.write", TenantID: "t", TaskID: "k",
			Args: map[string]any{"path": bad, "body": "x"},
		})
		if res.Success {
			t.Errorf("expected traversal rejection for %q", bad)
		}
	}
}

func TestDocWrite_RejectsOverCapBody(t *testing.T) {
	root := t.TempDir()
	p := New(Config{Workspace: WorkspaceConfig{Root: root}}, nil)
	oversized := strings.Repeat("x", workspacefs.MaxFileBytes+1)
	res, err := p.Invoke(context.Background(), Request{
		ToolID: "doc.write", TenantID: "ten1", TaskID: "task1",
		Args: map[string]any{"path": "notes/too-big.md", "body": oversized},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Success {
		t.Fatalf("expected over-cap write to be rejected")
	}
	want := filepath.Join(root, "ten1", "task1", "notes", "too-big.md")
	if _, statErr := os.Stat(want); !os.IsNotExist(statErr) {
		t.Errorf("expected no destination file at %s, stat err = %v", want, statErr)
	}
}

func TestDocWrite_NoLeftoverTempFile(t *testing.T) {
	root := t.TempDir()
	p := New(Config{Workspace: WorkspaceConfig{Root: root}}, nil)
	res, err := p.Invoke(context.Background(), Request{
		ToolID: "doc.write", TenantID: "ten1", TaskID: "task1",
		Args: map[string]any{"path": "notes/out.md", "body": "hello world"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got %q", res.Error)
	}
	dir := filepath.Join(root, "ten1", "task1", "notes")
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("ReadDir(%s): %v", dir, readErr)
	}
	if len(entries) != 1 || entries[0].Name() != "out.md" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("expected only out.md in %s, got %v", dir, names)
	}
}

func TestDocReadWrite_RoundTrip(t *testing.T) {
	p := New(Config{Workspace: WorkspaceConfig{Root: t.TempDir()}}, nil)
	req := func(tool string, args map[string]any) *Result {
		r, err := p.Invoke(context.Background(), Request{ToolID: tool, TenantID: "t", TaskID: "k", Args: args})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return r
	}
	if w := req("doc.write", map[string]any{"path": "a/b.md", "body": "round trip"}); !w.Success {
		t.Fatalf("write failed: %q", w.Error)
	}
	r := req("doc.read", map[string]any{"path": "a/b.md"})
	if !r.Success || r.Output["content"] != "round trip" {
		t.Fatalf("read-after-write mismatch: %+v", r)
	}
	ls := req("workspace.read", nil)
	if !ls.Success || ls.Output["count"].(int) != 1 {
		t.Errorf("workspace.read expected 1 file, got %+v", ls.Output)
	}
}

func TestDocRead_NotFound(t *testing.T) {
	p := New(Config{Workspace: WorkspaceConfig{Root: t.TempDir()}}, nil)
	res, _ := p.Invoke(context.Background(), Request{ToolID: "doc.read", TenantID: "t", TaskID: "k", Args: map[string]any{"path": "missing.md"}})
	if res.Success || !strings.Contains(res.Error, "not found") {
		t.Errorf("expected not-found, got %+v", res)
	}
}

func TestDocRead_RejectsTraversal(t *testing.T) {
	p := New(Config{Workspace: WorkspaceConfig{Root: t.TempDir()}}, nil)
	res, _ := p.Invoke(context.Background(), Request{ToolID: "doc.read", TenantID: "t", TaskID: "k", Args: map[string]any{"path": "../../../etc/passwd"}})
	if res.Success {
		t.Error("expected traversal rejection")
	}
}

func TestWorkspaceRead_EmptyWhenNothingWritten(t *testing.T) {
	p := New(Config{Workspace: WorkspaceConfig{Root: t.TempDir()}}, nil)
	res, err := p.Invoke(context.Background(), Request{ToolID: "workspace.read", TenantID: "t", TaskID: "fresh"})
	if err != nil || !res.Success || res.Output["count"].(int) != 0 {
		t.Errorf("empty workspace should list 0 files, got %+v (err %v)", res.Output, err)
	}
}

func TestEmailDraft_ComposesDraft(t *testing.T) {
	res, err := newTestProxy(t).Invoke(context.Background(), Request{
		ToolID: "email.draft",
		Args:   map[string]any{"to": "bob@example.com", "subject": "hi", "body": "body text"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Success || res.Output["to"] != "bob@example.com" || res.Output["draft_id"] == "" {
		t.Errorf("unexpected draft result %+v", res.Output)
	}
}

func TestEmailDraft_RequiresToAndSubject(t *testing.T) {
	res, _ := newTestProxy(t).Invoke(context.Background(), Request{
		ToolID: "email.draft", Args: map[string]any{"to": "bob@example.com"},
	})
	if res.Success {
		t.Error("expected error when subject is missing")
	}
}

func TestInvoke_UnknownToolErrors(t *testing.T) {
	_, err := newTestProxy(t).Invoke(context.Background(), Request{ToolID: "totally.unknown"})
	if !errors.Is(err, ErrUnknownTool) {
		t.Errorf("expected ErrUnknownTool, got %v", err)
	}
}
