// broker/internal/broker/connector_authstore.go
// Adapter exposing the DB connector-state repo as a connector.PendingAuthStore,
// so connector OAuth round-trips survive across broker replicas. Maps between
// the connector domain type and the db row type (keeps internal/db free of a
// dependency on internal/connector).
package broker

import (
	"context"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/db"
)

type dbPendingAuthStore struct {
	repo *db.ConnectorStateRepo
	ttl  time.Duration
}

// NewDBPendingAuthStore wires the DB-backed connector OAuth state store.
func NewDBPendingAuthStore(repo *db.ConnectorStateRepo, ttl time.Duration) connector.PendingAuthStore {
	return &dbPendingAuthStore{repo: repo, ttl: ttl}
}

func (d *dbPendingAuthStore) Put(ctx context.Context, state string, a connector.PendingAuth) error {
	return d.repo.Put(ctx, db.ConnectorState{
		State:     state,
		TenantID:  a.Tenant,
		UserID:    a.User,
		Provider:  string(a.Provider),
		Verifier:  a.Verifier,
		Scopes:    a.Scopes,
		ExpiresAt: time.Now().Add(d.ttl),
	})
}

func (d *dbPendingAuthStore) Take(ctx context.Context, tenant, state string) (connector.PendingAuth, bool, error) {
	st, ok, err := d.repo.Take(ctx, tenant, state)
	if err != nil || !ok {
		return connector.PendingAuth{}, ok, err
	}
	return connector.PendingAuth{
		Provider: connector.Provider(st.Provider),
		Tenant:   st.TenantID,
		User:     st.UserID,
		Verifier: st.Verifier,
		Scopes:   st.Scopes,
	}, true, nil
}
