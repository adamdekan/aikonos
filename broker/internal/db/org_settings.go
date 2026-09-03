// broker/internal/db/org_settings.go
// Repository for the tenant-scoped org governance control plane (org_settings
// table, migration 035). The row holds a single JSONB `settings` document; the
// typed OrgSettings struct is the schema view over it. Partial updates merge
// with `settings || $patch` so a caller need only send the fields it owns.
// All queries set app.current_tenant for RLS. No raw SQL outside this package.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// OrgSettings is the typed view over the org_settings JSONB document plus its
// provenance columns. JSON tags match the proto field names (snake_case) so the
// stored document and the wire format stay aligned. New governance fields are
// added here (and to the proto OrgSettings message) with no migration required.
type OrgSettings struct {
	// A1 — org-global instruction preamble.
	InstructionPreamble string `json:"instruction_preamble,omitempty"`

	// A8 — unattended/auto-approve master. Pointer so "unset" (nil) is
	// distinguishable from an explicit false; unset defaults to allowed.
	UnattendedAllowed *bool `json:"unattended_allowed,omitempty"`

	// A6 — workflow-sharing master. Unset defaults to allowed.
	WorkflowSharingAllowed *bool `json:"workflow_sharing_allowed,omitempty"`

	// A7 — connector install allowlist. Enabled=nil/false → no restriction.
	ConnectorAllowlistEnabled *bool    `json:"connector_allowlist_enabled,omitempty"`
	ConnectorAllowlist        []string `json:"connector_allowlist,omitempty"`

	// A2 — org capability kill-switches: EffectClass enum names that are denied
	// org-wide at InvokeTool. Empty = nothing disabled.
	DisabledEffectClasses []string `json:"disabled_effect_classes,omitempty"`

	// M365 tenant-wide OneDrive connection. Metadata only — the client secret lives in Vault at
	// secrets.M365AppPath(tenant). Deliberately NOT mapped by orgSettingsToProto/
	// orgSettingsPatch (broker/internal/broker/org_settings.go): the M365
	// connection has its own dedicated M365Connection proto + admin RPCs
	// (broker/internal/broker/m365_admin.go).
	M365TenantID string `json:"m365_tenant_id,omitempty"`
	M365ClientID string `json:"m365_client_id,omitempty"`
	M365Enabled  *bool  `json:"m365_enabled,omitempty"`

	// web.search tenant config. Metadata only — the
	// engine api_key lives in Vault at secrets.WebSearchAppPath(tenant).
	// Deliberately NOT mapped by orgSettingsToProto/orgSettingsPatch: web
	// search has its own dedicated WebSearchConfig proto + admin RPCs
	// (broker/internal/broker/websearch_admin.go), same precedent as M365.
	WebsearchEngine     string `json:"websearch_engine,omitempty"`
	WebsearchMaxResults int    `json:"websearch_max_results,omitempty"`

	// Provenance (from columns, never the JSON blob).
	UpdatedBy string    `json:"-"`
	UpdatedAt time.Time `json:"-"`
}

// UnattendedAllowedOrDefault resolves the A8 flag: unset means allowed (true).
func (s OrgSettings) UnattendedAllowedOrDefault() bool {
	if s.UnattendedAllowed == nil {
		return true
	}
	return *s.UnattendedAllowed
}

// WorkflowSharingAllowedOrDefault resolves the A6 flag: unset means allowed.
func (s OrgSettings) WorkflowSharingAllowedOrDefault() bool {
	if s.WorkflowSharingAllowed == nil {
		return true
	}
	return *s.WorkflowSharingAllowed
}

// EffectClassDisabled reports whether the A2 org kill-switch set contains the
// given EffectClass enum name (e.g. "NETWORK_EGRESS").
func (s OrgSettings) EffectClassDisabled(className string) bool {
	for _, c := range s.DisabledEffectClasses {
		if c == className {
			return true
		}
	}
	return false
}

// ConnectorAllowed resolves the A7 allowlist for a provider id. When the
// allowlist is disabled (nil/false) any provider is allowed; when enabled, only
// ids present in the list are allowed.
func (s OrgSettings) ConnectorAllowed(providerID string) bool {
	if s.ConnectorAllowlistEnabled == nil || !*s.ConnectorAllowlistEnabled {
		return true
	}
	for _, id := range s.ConnectorAllowlist {
		if id == providerID {
			return true
		}
	}
	return false
}

// OrgSettingsRepo persists org_settings rows. Every query wraps its connection
// in withTenant for RLS.
type OrgSettingsRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

// NewOrgSettingsRepo constructs the repo.
func NewOrgSettingsRepo(pool *pgxpool.Pool, logger *zap.Logger) *OrgSettingsRepo {
	return &OrgSettingsRepo{pool: pool, logger: logger}
}

// Get returns the tenant's settings. A tenant with no row yet resolves to the
// zero-value OrgSettings (all documented defaults) — not an error.
func (r *OrgSettingsRepo) Get(ctx context.Context, tenantID string) (OrgSettings, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (OrgSettings, error) {
		var raw []byte
		var updatedBy string
		var updatedAt time.Time
		err := conn.QueryRow(ctx, `
		SELECT settings, updated_by, updated_at
		  FROM org_settings
		 WHERE tenant_id = $1`, tenantID).Scan(&raw, &updatedBy, &updatedAt)
		if err != nil {
			// No row yet for this tenant → documented defaults, not an error.
			if errors.Is(err, pgx.ErrNoRows) {
				return OrgSettings{}, nil
			}
			return OrgSettings{}, fmt.Errorf("get org_settings: %w", err)
		}

		var out OrgSettings
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &out); err != nil {
				return OrgSettings{}, fmt.Errorf("decode org_settings: %w", err)
			}
		}
		out.UpdatedBy = updatedBy
		out.UpdatedAt = updatedAt
		return out, nil
	})
}

// Merge applies a partial patch (only the keys present are changed) and returns
// the full post-merge settings. `patch` is a JSON-shaped map whose keys match
// the OrgSettings JSON tags; callers build it from only the fields they own.
func (r *OrgSettingsRepo) Merge(ctx context.Context, tenantID string, patch map[string]any, updatedBy string) (OrgSettings, error) {
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return OrgSettings{}, fmt.Errorf("encode patch: %w", err)
	}

	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (OrgSettings, error) {
		var raw []byte
		var uBy string
		var uAt time.Time
		// `||` on jsonb is a shallow key-merge: existing keys not in the patch are
		// preserved, patch keys overwrite. Concurrent writers to disjoint keys are
		// serialized by the row lock the upsert takes.
		err := conn.QueryRow(ctx, `
		INSERT INTO org_settings (tenant_id, settings, updated_by, updated_at)
		VALUES ($1, $2::jsonb, $3, NOW())
		ON CONFLICT (tenant_id) DO UPDATE SET
		    settings   = org_settings.settings || EXCLUDED.settings,
		    updated_by = EXCLUDED.updated_by,
		    updated_at = NOW()
		RETURNING settings, updated_by, updated_at`,
			tenantID, string(patchJSON), updatedBy,
		).Scan(&raw, &uBy, &uAt)
		if err != nil {
			return OrgSettings{}, fmt.Errorf("merge org_settings: %w", err)
		}

		var out OrgSettings
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &out); err != nil {
				return OrgSettings{}, fmt.Errorf("decode org_settings: %w", err)
			}
		}
		out.UpdatedBy = uBy
		out.UpdatedAt = uAt
		return out, nil
	})
}
