package connector

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"go.uber.org/zap"
	"golang.org/x/oauth2"

	"github.com/adamdekan/aikonos/broker/internal/secrets"
)

// tokenLocks serialises the read → refresh → write-back of one stored token
// (and RevokeConnector's read → revoke → delete), so parallel tool calls do
// not spend one rotating refresh token twice and a refresh racing a
// disconnect cannot write the deleted secret back (G22). The broker is a
// singleton (cmd/broker/singleton.go), so a process lock covers it.
// ponytail: per-process; a Vault KV-v2 cas write if replicas ever share tokens.
var tokenLocks = struct {
	mu sync.Mutex
	m  map[string]*tokenLock
}{m: map[string]*tokenLock{}}

type tokenLock struct {
	mu   sync.Mutex
	refs int
}

// LockToken locks the stored token of (tenant, user, connectorID) and returns
// its unlock.
func LockToken(tenant, user, connectorID string) (unlock func()) {
	key := tenant + "\x00" + user + "\x00" + connectorID
	tokenLocks.mu.Lock()
	l := tokenLocks.m[key]
	if l == nil {
		l = &tokenLock{}
		tokenLocks.m[key] = l
	}
	l.refs++
	tokenLocks.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		tokenLocks.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(tokenLocks.m, key)
		}
		tokenLocks.mu.Unlock()
	}
}

// FreshAccessToken returns a usable access token for a user's connection,
// refreshing it against the provider if the stored one has expired and
// persisting the rotated token back to the store. A missing connection yields a
// clear "not connected" error; a failed refresh yields "reconnect needed".
func (r *Registry) FreshAccessToken(ctx context.Context, store secrets.Provider, p Provider, tenant, user string) (string, error) {
	connectorID := p.ConnectorID()
	defer LockToken(tenant, user, connectorID)()
	stored, err := readStored(ctx, store, tenant, user, connectorID, p.DisplayName())
	if err != nil {
		return "", err
	}
	cfg, err := r.OAuthConfig(p, stored.Scopes)
	if err != nil {
		return "", err
	}
	return r.refresh(ctx, store, cfg, stored, tenant, user, connectorID, p.DisplayName(), string(p))
}

func readStored(ctx context.Context, store secrets.Provider, tenant, user, connectorID, label string) (*secrets.ConnectorToken, error) {
	stored, err := store.ReadConnector(ctx, tenant, user, connectorID)
	if err != nil {
		if errors.Is(err, secrets.ErrNotFound) {
			return nil, fmt.Errorf("%s is not connected — link it under Connections first", label)
		}
		return nil, err
	}
	return stored, nil
}

// refresh is the shared refresh + write-back + status step of the cloud
// connectors (FreshAccessToken) and the oauth MCP connections
// (FreshMcpAccessToken).
func (r *Registry) refresh(ctx context.Context, store secrets.Provider, cfg *oauth2.Config, stored *secrets.ConnectorToken, tenant, user, connectorID, label, provider string) (string, error) {
	src := cfg.TokenSource(ctx, &oauth2.Token{
		AccessToken:  stored.AccessToken,
		RefreshToken: stored.RefreshToken,
		Expiry:       stored.ExpiresAt,
	})
	tok, err := src.Token()
	if err != nil {
		r.recordStatus(tenant, user, connectorID, StatusReconnectNeeded)
		return "", fmt.Errorf("%s token refresh failed — reconnect needed: %w", label, err)
	}
	if tok.AccessToken != stored.AccessToken {
		r.recordStatus(tenant, user, connectorID, StatusConnected)
		refresh := tok.RefreshToken
		if refresh == "" {
			refresh = stored.RefreshToken
		}
		// The just-refreshed access token already serves this call, so a failed
		// write-back does not fail the request — but it means the next JIT fetch
		// reloads the stored (possibly already-rotated-out) token, which can brick
		// the connection until re-link. Surface it instead of dropping it silently.
		if werr := store.WriteConnector(ctx, tenant, user, connectorID, &secrets.ConnectorToken{
			AccessToken:  tok.AccessToken,
			RefreshToken: refresh,
			ExpiresAt:    tok.Expiry,
			Scopes:       stored.Scopes,
			TokenURL:     stored.TokenURL,
		}); werr != nil && r.Logger != nil {
			r.Logger.Warn("connector token write-back failed; connection may need re-link",
				zap.String("provider", provider),
				zap.String("connector_id", connectorID),
				zap.Bool("refresh_token_rotated", refresh != stored.RefreshToken),
				zap.Error(werr))
		}
	}
	return tok.AccessToken, nil
}
