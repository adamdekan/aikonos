// broker/internal/db/workspace_prefs.go
// Repository for the per-user workspace-backend preference (user_workspace_prefs
// table, migration 037,  CP5). Unlike
// org_settings' JSONB-merge document, this is a plain per-row upsert — the
// full set of columns is always supplied by the caller (broker/internal/broker's
// WorkspacePrefResolver owns the default-rule logic for an absent row; this
// repo only reads/writes what's actually stored). All queries set
// app.current_tenant for RLS. No raw SQL outside this package.
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// WorkspacePrefs is the typed view over one user_workspace_prefs row.
type WorkspacePrefs struct {
	Backend            string // "local" | "onedrive"
	OneDriveFolderPath string
	DriveID            string
	RootItemID         string
	UpdatedAt          time.Time
}

// WorkspacePrefsRepo persists user_workspace_prefs rows. Every query wraps its
// connection in withTenant for RLS.
type WorkspacePrefsRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

// NewWorkspacePrefsRepo constructs the repo.
func NewWorkspacePrefsRepo(pool *pgxpool.Pool, logger *zap.Logger) *WorkspacePrefsRepo {
	return &WorkspacePrefsRepo{pool: pool, logger: logger}
}

// Get returns the user's stored preference. A user with no row yet resolves
// to the zero-value WorkspacePrefs and found=false — NOT an error; the
// caller (WorkspacePrefResolver) applies the default rule in that case.
func (r *WorkspacePrefsRepo) Get(ctx context.Context, tenantID, userID string) (WorkspacePrefs, bool, error) {
	return withConn2(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (WorkspacePrefs, bool, error) {
		var out WorkspacePrefs
		err := conn.QueryRow(ctx, `
		SELECT backend, onedrive_folder_path, drive_id, root_item_id, updated_at
		  FROM user_workspace_prefs
		 WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID,
		).Scan(&out.Backend, &out.OneDriveFolderPath, &out.DriveID, &out.RootItemID, &out.UpdatedAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return WorkspacePrefs{}, false, nil
			}
			return WorkspacePrefs{}, false, fmt.Errorf("get user_workspace_prefs: %w", err)
		}
		return out, true, nil
	})
}

// Set upserts the user's full preference row.
func (r *WorkspacePrefsRepo) Set(ctx context.Context, tenantID, userID string, p WorkspacePrefs) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		_, err := conn.Exec(ctx, `
		INSERT INTO user_workspace_prefs (tenant_id, user_id, backend, onedrive_folder_path, drive_id, root_item_id, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (tenant_id, user_id) DO UPDATE SET
		    backend              = EXCLUDED.backend,
		    onedrive_folder_path = EXCLUDED.onedrive_folder_path,
		    drive_id             = EXCLUDED.drive_id,
		    root_item_id         = EXCLUDED.root_item_id,
		    updated_at           = NOW()`,
			tenantID, userID, p.Backend, p.OneDriveFolderPath, p.DriveID, p.RootItemID,
		)
		if err != nil {
			return fmt.Errorf("set user_workspace_prefs: %w", err)
		}
		return nil
	})
}
