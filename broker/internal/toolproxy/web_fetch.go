// broker/internal/toolproxy/web_fetch.go
//
// web.fetch — read_only HTTP(S) GET. Mirrors skills/web.fetch/skill.yaml. The
// real platform routes this through an egress proxy with an allowlist + DLP;
// this MVP enforces the SSRF defense-in-depth directly (block private/loopback
// targets, cap size + time) and returns the (optionally text-extracted) body.
package toolproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"

	"github.com/adamdekan/aikonos/broker/internal/netacl"
)

// NetACL is the network access-list the web.fetch handler consults (the Tool
// Proxy is the unbypassable egress enforcement point). Satisfied by
// *netacl.Checker; faked in tests.
type NetACL interface {
	Enabled() bool
	Decide(ctx context.Context, tenant, user, host string) netacl.Action
}

type WebFetchConfig struct {
	// AllowPrivateHosts disables the SSRF guard (private/loopback targets). MUST
	// stay false in production; tests set it true to reach an httptest server.
	AllowPrivateHosts bool
	Timeout           time.Duration
	MaxBytes          int64
	// ACL gates destination hosts (allow-list). Nil → no host filtering (today's
	// behavior; the SSRF guard still applies).
	ACL NetACL
	// HTTPClient is injectable for tests; nil builds a default client.
	HTTPClient *http.Client
}

// aclPrincipal rides on the request context so the shared client's CheckRedirect
// can evaluate each redirect hop's host against the acting principal.
type aclPrincipal struct{ tenant, user string }
type aclPrincipalKey struct{}

const (
	defaultFetchTimeout = 15 * time.Second
	defaultMaxBytes     = 5 * 1024 * 1024 // 5 MiB
	maxContentChars     = 50_000          // keep results context-manageable
)

// webFetchPlugin self-registers web.fetch, unconditional (no external deps).
type webFetchPlugin struct{}

func (webFetchPlugin) ToolID() string                  { return "web.fetch" }
func (webFetchPlugin) Available(cfg Config) bool       { return true }
func (webFetchPlugin) Build(cfg Config) Handler        { return newWebFetchHandler(cfg.WebFetch) }
func (webFetchPlugin) Scope() string                   { return "web:read" }
func (webFetchPlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_READ_ONLY }

func init() { RegisterPlugin(webFetchPlugin{}) }

func newWebFetchHandler(cfg WebFetchConfig) Handler {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultFetchTimeout
	}
	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	client := cfg.HTTPClient
	if client == nil {
		client = hardenedClient(timeout, cfg.AllowPrivateHosts, cfg.ACL)
	}

	return func(ctx context.Context, req Request) (map[string]any, int64, error) {
		rawURL, _ := req.Args["url"].(string)
		if rawURL == "" {
			return nil, 0, fmt.Errorf("web.fetch: missing required arg %q", "url")
		}
		if err := validateFetchURL(rawURL, cfg.AllowPrivateHosts); err != nil {
			return nil, 0, err
		}

		// Access-list: block a denied destination host. ALLOW/ASK proceed (an
		// ASK host was already gated at plan time via NEEDS_HUMAN; redirects to
		// non-ALLOW hosts are blocked separately in CheckRedirect). DENY is the
		// hard, unbypassable boundary enforced here regardless of the plan.
		if cfg.ACL != nil && cfg.ACL.Enabled() {
			host := hostOf(rawURL)
			if cfg.ACL.Decide(ctx, req.TenantID, req.UserID, host) == netacl.Deny {
				return nil, 0, fmt.Errorf("web.fetch: host %q is not permitted by the network access-list", host)
			}
			ctx = context.WithValue(ctx, aclPrincipalKey{}, aclPrincipal{tenant: req.TenantID, user: req.UserID})
		}

		extractText := true
		if v, ok := req.Args["extract_text_only"].(bool); ok {
			extractText = v
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, 0, fmt.Errorf("web.fetch: bad request: %w", err)
		}
		httpReq.Header.Set("User-Agent", "Aikonos/0.1 (Agentic Platform; +https://aikonos.com)")
		httpReq.Header.Set("Accept", "text/html,text/plain,application/json")

		resp, err := client.Do(httpReq)
		if err != nil {
			return nil, 1, fmt.Errorf("web.fetch: request failed: %w", err)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
		if err != nil {
			return nil, 1, fmt.Errorf("web.fetch: read failed: %w", err)
		}

		text := decodeToUTF8(body)
		contentType := resp.Header.Get("Content-Type")
		if extractText && strings.Contains(contentType, "html") {
			text = stripHTML(text)
		}
		fullLen := len(text)
		text = truncateUTF8(text, maxContentChars)

		// Cost: 1 unit per ~1KB fetched, min 1.
		cost := int64(len(body)/1024) + 1
		return map[string]any{
			"url":            rawURL,
			"status_code":    resp.StatusCode,
			"content":        text,
			"content_length": fullLen,
			"fetched_at":     time.Now().UTC().Format(time.RFC3339),
		}, cost, nil
	}
}

// validateFetchURL rejects non-HTTP(S) schemes (file://, ftp://, gopher:// …)
// and, unless private hosts are allowed, targets that resolve to private,
// loopback, link-local or unspecified addresses. This is the fast pre-flight
// check; the connect-time dialer guard (hardenedClient) is what actually closes
// SSRF — it re-checks every TCP connect, so a redirect to a private host or a
// DNS-rebind between this check and connect is also caught.
func validateFetchURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("web.fetch: invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("web.fetch: only http/https permitted, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("web.fetch: URL has no host")
	}
	if allowPrivate {
		return nil
	}

	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return errBlockedTarget
	}
	// If the host is a literal IP, check it directly. If it's a name, resolve
	// and reject if any resolved address is private/loopback (DNS-rebind guard).
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		resolved, err := net.LookupIP(host)
		if err != nil {
			return fmt.Errorf("web.fetch: cannot resolve host %q: %w", host, err)
		}
		ips = resolved
	}
	for _, ip := range ips {
		if isDisallowedIP(ip) {
			return errBlockedTarget
		}
	}
	return nil
}

var errBlockedTarget = fmt.Errorf("web.fetch: loopback/private targets are not permitted")

// hostOf extracts the hostname from a URL for the access-list check (best-effort;
// validateFetchURL already confirmed it parses with a host).
func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Hostname()
	}
	return raw
}

// cgnatNet is RFC 6598 shared (carrier-grade NAT) address space. It is routable
// on some networks but never a legitimate egress-fetch target, and Go's
// net.IP.IsPrivate does not cover it.
var cgnatNet = mustCIDR("100.64.0.0/10")

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// isDisallowedIP reports whether an address is in a range an egress fetch must
// never reach (loopback, RFC1918 private, link-local incl. cloud metadata
// 169.254.169.254, CGNAT shared space, or unspecified).
func isDisallowedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	if v4 := ip.To4(); v4 != nil && cgnatNet.Contains(v4) {
		return true
	}
	return false
}

const maxRedirects = 5

// hardenedClient builds an http.Client whose dialer rejects connecting to a
// disallowed IP — enforced at *every* TCP connect, so it covers the original
// URL, any redirect hop, and DNS rebinding. Redirects are capped, and (when an
// ACL is wired) each redirect hop's host is checked against the access-list so a
// redirect can't jump to a non-allowed host. The acting principal rides on the
// request context (aclPrincipalKey).
func hardenedClient(timeout time.Duration, allowPrivate bool, acl NetACL) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}
	if !allowPrivate {
		dialer.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if isDisallowedIP(net.ParseIP(host)) {
				return errBlockedTarget
			}
			return nil
		}
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DialContext: dialer.DialContext},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("web.fetch: too many redirects (>%d)", maxRedirects)
			}
			if acl != nil && acl.Enabled() {
				if p, ok := req.Context().Value(aclPrincipalKey{}).(aclPrincipal); ok {
					host := req.URL.Hostname()
					if acl.Decide(req.Context(), p.tenant, p.user, host) != netacl.Allow {
						return fmt.Errorf("web.fetch: redirect to %q is not permitted by the network access-list", host)
					}
				}
			}
			return nil
		},
	}
}

// NewHardenedClient returns an http.Client whose dialer rejects connecting to a
// loopback/private/link-local address at every TCP connect (SSRF guard), with
// redirects capped. Exported for callers outside this package (e.g. the broker's
// LLM provider connection test) that need a public-egress-only client. Pass
// allowPrivate=true only in dev/tests where the target may be loopback.
func NewHardenedClient(timeout time.Duration, allowPrivate bool) *http.Client {
	return hardenedClient(timeout, allowPrivate, nil)
}

// ValidatePublicURL runs the SSRF pre-flight check (scheme/host plus a
// DNS-resolution guard that rejects private/loopback targets) used by web.fetch.
// Exported for reuse; pass allowPrivate=true only in dev/tests.
func ValidatePublicURL(raw string, allowPrivate bool) error {
	return validateFetchURL(raw, allowPrivate)
}

var (
	reBlockTags = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script\s*>`),
		regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style\s*>`),
		regexp.MustCompile(`(?is)<head\b[^>]*>.*?</head\s*>`),
		regexp.MustCompile(`(?is)<noscript\b[^>]*>.*?</noscript\s*>`),
	}
	reAnyTag = regexp.MustCompile(`(?s)<[^>]+>`)
	reWS     = regexp.MustCompile(`\n{3,}`)
)

// stripHTML is a minimal HTML→text reducer: drop script/style/etc blocks, strip
// remaining tags, collapse runaway blank lines.
func stripHTML(s string) string {
	for _, re := range reBlockTags {
		s = re.ReplaceAllString(s, " ")
	}
	s = reAnyTag.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "\r", "")
	// Collapse intra-line whitespace runs, then trim and squeeze blank lines.
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimSpace(strings.Join(strings.Fields(ln), " "))
	}
	s = strings.Join(lines, "\n")
	s = reWS.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
