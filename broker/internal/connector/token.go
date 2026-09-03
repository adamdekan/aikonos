package connector

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"
	"golang.org/x/oauth2"

	"github.com/adamdekan/aikonos/broker/internal/secrets"
)

// FreshAccessToken returns a usable access token for a user's connection,
// refreshing it against the provider if the stored one has expired and
// persisting the rotated token back to the store. A missing connection yields a
// clear "not connected" error; a failed refresh yields "reconnect needed".
func (r *Registry) FreshAccessToken(ctx context.Context, store secrets.Provider, p Provider, tenant, user string) (string, error) {
	connectorID := p.ConnectorID()
	stored, err := store.ReadConnector(ctx, tenant, user, connectorID)
	if err != nil {
		if errors.Is(err, secrets.ErrNotFound) {
			return "", fmt.Errorf("%s is not connected — link it under Connections first", p.DisplayName())
		}
		return "", err
	}
	cfg, err := r.OAuthConfig(p, stored.Scopes)
	if err != nil {
		return "", err
	}
	src := cfg.TokenSource(ctx, &oauth2.Token{
		AccessToken:  stored.AccessToken,
		RefreshToken: stored.RefreshToken,
		Expiry:       stored.ExpiresAt,
	})
	tok, err := src.Token()
	if err != nil {
		r.recordStatus(tenant, user, connectorID, StatusReconnectNeeded)
		return "", fmt.Errorf("%s token refresh failed — reconnect needed: %w", p.DisplayName(), err)
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
		}); werr != nil && r.Logger != nil {
			r.Logger.Warn("connector token write-back failed; connection may need re-link",
				zap.String("provider", string(p)),
				zap.String("connector_id", connectorID),
				zap.Bool("refresh_token_rotated", refresh != stored.RefreshToken),
				zap.Error(werr))
		}
	}
	return tok.AccessToken, nil
}
