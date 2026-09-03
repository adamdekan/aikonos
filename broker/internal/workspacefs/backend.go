package workspacefs

import (
	"context"
	"time"
)

// Backend is the workspace storage seam CP3 introduces: *Store satisfies it
// directly (ctx accepted but ignored today — no cancellation support yet),
// and Router (this package) dispatches between a local Store and a remote
// Backend by (tenant, user) preference. A future broker/internal/onedrivefs.Store
// (CP4) satisfies it too, letting Deps.Workspace.Local (broker/internal/broker)
// hold any of the three with zero call-site change.
//
// ReadVerified is deliberately NOT part of this interface — HMAC
// session-sidecar verification is a Store-only concern (session records under
// SessionsDirPrefix always route local; see Router's reserved-prefix
// routing), not something a generic backend needs to implement.
type Backend interface {
	Enabled() bool
	List(ctx context.Context, tenant, user string) ([]FileInfo, error)
	ListDir(ctx context.Context, tenant, user, dir string, recursive bool) ([]FileInfo, error)
	Read(ctx context.Context, tenant, user, rel string) ([]byte, time.Time, error)
	Write(ctx context.Context, tenant, user, rel string, data []byte) (FileInfo, error)
	Delete(ctx context.Context, tenant, user, rel string) error
	Move(ctx context.Context, tenant, user, from, to string) (FileInfo, error)
	Mkdir(ctx context.Context, tenant, user, rel string) error
}

var (
	_ Backend = (*Store)(nil)
	_ Backend = (*Router)(nil)
)
