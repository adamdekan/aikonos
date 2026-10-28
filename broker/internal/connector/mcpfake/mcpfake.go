// Package mcpfake is a test double for AG-09: one httptest server that is both
// an OAuth-protected MCP server (RFC 9728 metadata, a 401 challenge, a
// "whoami" tool that echoes the token's user) and its authorization server
// (RFC 8414 metadata, dynamic registration, authorization code + PKCE S256,
// refresh, RFC 8707 resource checks). Only tests import it.
package mcpfake

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
)

// Server is the fake MCP + authorization server.
type Server struct {
	*httptest.Server

	// NoS256 drops PKCE S256 from the metadata (discovery must refuse).
	NoS256 bool

	mu         sync.Mutex
	n          int
	codes      map[string][2]string // code → {user, challenge}
	access     map[string]string    // access token → user
	refresh    map[string]string    // refresh token → user
	tokenForms []url.Values
	registered int
}

// New starts the fake. Close it with Server.Close.
func New() *Server {
	s := &Server{codes: map[string][2]string{}, access: map[string]string{}, refresh: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.mcp)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"resource":              s.MCPURL(),
			"authorization_servers": []string{s.URL},
			"scopes_supported":      []string{"mcp.read"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		methods := []string{"S256"}
		if s.NoS256 {
			methods = []string{"plain"}
		}
		writeJSON(w, map[string]any{
			"issuer":                           s.URL,
			"authorization_endpoint":           s.URL + "/authorize",
			"token_endpoint":                   s.URL + "/token",
			"registration_endpoint":            s.URL + "/register",
			"code_challenge_methods_supported": methods,
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.registered++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"client_id": "dcr-client", "client_secret": "dcr-secret"})
	})
	mux.HandleFunc("/token", s.token)
	s.Server = httptest.NewServer(mux)
	return s
}

// MCPURL is the MCP endpoint (and the RFC 8707 resource).
func (s *Server) MCPURL() string { return s.URL + "/mcp" }

// Registered counts dynamic client registrations.
func (s *Server) Registered() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.registered
}

// TokenForms returns every token request form received.
func (s *Server) TokenForms() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]url.Values(nil), s.tokenForms...)
}

// Authorize plays the user consenting at the authorize URL: it checks the
// request the way the real server would and returns the code and state the
// browser would carry to the callback.
func (s *Server) Authorize(authorizeURL, user string) (code, state string, err error) {
	u, err := url.Parse(authorizeURL)
	if err != nil {
		return "", "", err
	}
	q := u.Query()
	switch {
	case q.Get("response_type") != "code":
		return "", "", fmt.Errorf("response_type %q", q.Get("response_type"))
	case q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "":
		return "", "", fmt.Errorf("missing PKCE S256")
	case q.Get("resource") != s.MCPURL():
		return "", "", fmt.Errorf("resource %q", q.Get("resource"))
	case q.Get("client_id") == "":
		return "", "", fmt.Errorf("missing client_id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	code = fmt.Sprintf("code-%d", s.n)
	s.codes[code] = [2]string{user, q.Get("code_challenge")}
	return code, q.Get("state"), nil
}

// Revoke makes an access token invalid (a dead session at the server).
func (s *Server) Revoke(accessToken string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.access, accessToken)
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenForms = append(s.tokenForms, r.PostForm)
	if r.PostForm.Get("resource") != s.MCPURL() {
		oauthError(w, "invalid_target")
		return
	}
	var user string
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		c, ok := s.codes[r.PostForm.Get("code")]
		delete(s.codes, r.PostForm.Get("code"))
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != c[1] {
			oauthError(w, "invalid_grant")
			return
		}
		user = c[0]
	case "refresh_token":
		u, ok := s.refresh[r.PostForm.Get("refresh_token")]
		delete(s.refresh, r.PostForm.Get("refresh_token"))
		if !ok {
			oauthError(w, "invalid_grant")
			return
		}
		user = u
	default:
		oauthError(w, "unsupported_grant_type")
		return
	}
	s.n++
	at, rt := fmt.Sprintf("at-%d", s.n), fmt.Sprintf("rt-%d", s.n)
	s.access[at] = user
	s.refresh[rt] = user
	writeJSON(w, map[string]any{"access_token": at, "refresh_token": rt, "token_type": "Bearer", "expires_in": 3600})
}

func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	tok := ""
	if h := r.Header.Get("Authorization"); len(h) > 7 && h[:7] == "Bearer " {
		tok = h[7:]
	}
	s.mu.Lock()
	user, ok := s.access[tok]
	s.mu.Unlock()
	if !ok {
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp"`, s.URL))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var req struct {
		ID     *json.RawMessage `json:"id"`
		Method string           `json:"method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	var result any
	switch req.Method {
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "fake")
		result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
			"serverInfo": map[string]any{"name": "mcpfake", "version": "0"}}
	case "tools/list":
		result = map[string]any{"tools": []any{map[string]any{"name": "whoami", "description": "who the token is",
			"inputSchema": map[string]any{"type": "object"}}}}
	case "tools/call":
		result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "user:" + user}}}
	}
	writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func oauthError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	writeJSON(w, map[string]any{"error": code})
}

func writeJSON(w http.ResponseWriter, v any) {
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	_ = json.NewEncoder(w).Encode(v)
}
