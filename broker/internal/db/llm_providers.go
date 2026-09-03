package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

var ErrProviderNotFound = errors.New("llm provider not found")

// Capability names in llm_provider_defaults. Only these three have a legacy
// boolean twin on llm_providers that List/Get recompute; the remaining
// modality capabilities are validated at the RPC boundary
// (broker/providers_admin.go), the only place they carry meaning.
const (
	CapabilityChat     = "chat"
	CapabilityVision   = "vision"
	CapabilityFallback = "fallback"
)

// PricingUnitPerMTok is the only pricing unit with a token-costing basis, so
// it is the only one costMicrosFor can price from token counts.
const PricingUnitPerMTok = "per_mtok"

// PricingTier is a context-length-tiered rate override: once a call's input
// side reaches MinContextTokens these rates replace the top-level in/out rates.
type PricingTier struct {
	MinContextTokens int64 `json:"min_context_tokens"`
	InMicros         int64 `json:"in_micros,omitempty"`
	OutMicros        int64 `json:"out_micros,omitempty"`
}

// ModelPricing is one model's unit-discriminated price record. Unit decides
// which amounts mean anything: per_mtok uses all four token rates and may
// carry Tiers; every other unit uses InMicros as its single per-unit amount.
type ModelPricing struct {
	Unit             string        `json:"unit"`
	InMicros         int64         `json:"in_micros,omitempty"`
	OutMicros        int64         `json:"out_micros,omitempty"`
	CacheReadMicros  int64         `json:"cache_read_micros,omitempty"`
	CacheWriteMicros int64         `json:"cache_write_micros,omitempty"`
	Tiers            []PricingTier `json:"tiers,omitempty"`
}

// Model is one entry in a Provider's models JSONB array.
type Model struct {
	ID string `json:"id"`
	// PriceIn/PriceOut/PriceCacheRead/PriceCacheWrite are the legacy float
	// price fields — dead since migration 042 moved costing to integer micro
	// rates. Kept so old rows keep round-tripping; new pricing goes in Pricing.
	PriceIn         float64       `json:"priceIn"`
	PriceOut        float64       `json:"priceOut"`
	PriceCacheRead  float64       `json:"priceCacheRead"`
	PriceCacheWrite float64       `json:"priceCacheWrite"`
	ContextWindow   int           `json:"contextWindow"`
	MaxTokens       int           `json:"maxTokens"`
	Mode            string        `json:"mode,omitempty"` // empty = "chat"
	Pricing         *ModelPricing `json:"pricing,omitempty"`
}

// Provider is a per-tenant LLM provider config row.
type Provider struct {
	ID         string
	Name       string
	Endpoint   string
	API        string
	APIVersion string
	Models     []Model
	Enabled    bool
	// IsDefault/IsDefaultVision/IsFallback are computed on every List/Get from
	// llm_provider_defaults (chat/vision/fallback rows), not read from the
	// frozen boolean columns. Upsert ignores them.
	IsDefault             bool
	VisionCapable         bool
	IsDefaultVision       bool
	IsFallback            bool
	HasKey                bool
	PriceInMicrosPerMTok  int64 // cost fallback: micro-USD per 1M input tokens
	PriceOutMicrosPerMTok int64 // cost fallback: micro-USD per 1M output tokens
	Config                map[string]string
	UpdatedBy             string
	UpdatedAt             time.Time
}

type LlmProviderRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

func NewLlmProviderRepo(pool *pgxpool.Pool, logger *zap.Logger) *LlmProviderRepo {
	return &LlmProviderRepo{pool: pool, logger: logger}
}

// providerCols is scanned positionally by scanProvider — column order and
// scan order must stay in lockstep (guarded by adjacency tests).
const providerCols = `id, name, endpoint, api, api_version, models, enabled, is_default, vision_capable, is_default_vision, is_fallback, has_key, price_in_micros_per_mtok, price_out_micros_per_mtok, updated_by, updated_at, config`

func scanProvider(row interface {
	Scan(dest ...any) error
}) (Provider, error) {
	var p Provider
	var modelsJSON, configJSON []byte
	if err := row.Scan(&p.ID, &p.Name, &p.Endpoint, &p.API, &p.APIVersion, &modelsJSON,
		&p.Enabled, &p.IsDefault, &p.VisionCapable, &p.IsDefaultVision, &p.IsFallback, &p.HasKey,
		&p.PriceInMicrosPerMTok, &p.PriceOutMicrosPerMTok, &p.UpdatedBy, &p.UpdatedAt, &configJSON); err != nil {
		return Provider{}, err
	}
	if err := json.Unmarshal(modelsJSON, &p.Models); err != nil {
		return Provider{}, fmt.Errorf("unmarshal models: %w", err)
	}
	if p.Models == nil {
		p.Models = []Model{}
	}
	if len(configJSON) > 0 {
		if err := json.Unmarshal(configJSON, &p.Config); err != nil {
			return Provider{}, fmt.Errorf("unmarshal config: %w", err)
		}
	}
	return p, nil
}

// applyDefaults recomputes the three legacy boolean views from the tenant's
// defaults map. Unconditional in both directions: a provider that lost a
// capability must read back false, not keep the frozen column's stale true.
func applyDefaults(p *Provider, defaults map[string]string) {
	p.IsDefault = p.ID != "" && defaults[CapabilityChat] == p.ID
	p.IsDefaultVision = p.ID != "" && defaults[CapabilityVision] == p.ID
	p.IsFallback = p.ID != "" && defaults[CapabilityFallback] == p.ID
}

func (r *LlmProviderRepo) List(ctx context.Context, tenant string) ([]Provider, error) {
	return withConn(ctx, r.pool, tenant, func(conn *pgxpool.Conn) ([]Provider, error) {
		defaults, err := defaultsOn(ctx, conn, tenant)
		if err != nil {
			return nil, err
		}
		rows, err := conn.Query(ctx, `SELECT `+providerCols+` FROM llm_providers WHERE tenant_id = $1 ORDER BY id`, tenant)
		if err != nil {
			return nil, fmt.Errorf("list providers: %w", err)
		}
		defer rows.Close()
		var out []Provider
		for rows.Next() {
			p, err := scanProvider(rows)
			if err != nil {
				return nil, err
			}
			applyDefaults(&p, defaults)
			out = append(out, p)
		}
		return out, rows.Err()
	})
}

func (r *LlmProviderRepo) Get(ctx context.Context, tenant, id string) (Provider, error) {
	return withConn(ctx, r.pool, tenant, func(conn *pgxpool.Conn) (Provider, error) {
		defaults, err := defaultsOn(ctx, conn, tenant)
		if err != nil {
			return Provider{}, err
		}
		row := conn.QueryRow(ctx,
			`SELECT `+providerCols+` FROM llm_providers WHERE tenant_id = $1 AND id = $2`,
			tenant, id)
		p, err := scanProvider(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return Provider{}, ErrProviderNotFound
		}
		if err != nil {
			return Provider{}, fmt.Errorf("get provider: %w", err)
		}
		applyDefaults(&p, defaults)
		return p, nil
	})
}

// Upsert inserts or updates a provider row. Does not touch the frozen
// is_default / is_default_vision / is_fallback columns — capability defaults
// live in llm_provider_defaults (use SetDefaultFor).
func (r *LlmProviderRepo) Upsert(ctx context.Context, tenant string, p Provider) error {
	return withConnErr(ctx, r.pool, tenant, func(conn *pgxpool.Conn) error {
		modelsJSON, err := json.Marshal(p.Models)
		if err != nil {
			return fmt.Errorf("marshal models: %w", err)
		}
		// config is NOT NULL: a nil map must land as an empty object, not null.
		configJSON := []byte(`{}`)
		if p.Config != nil {
			if configJSON, err = json.Marshal(p.Config); err != nil {
				return fmt.Errorf("marshal config: %w", err)
			}
		}
		_, err = conn.Exec(ctx, `
		INSERT INTO llm_providers (tenant_id, id, name, endpoint, api, api_version, models, enabled, vision_capable, has_key, price_in_micros_per_mtok, price_out_micros_per_mtok, updated_by, config, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, NOW())
		ON CONFLICT (tenant_id, id) DO UPDATE
		SET name=$3, endpoint=$4, api=$5, api_version=$6, models=$7, enabled=$8, vision_capable=$9, has_key=$10, price_in_micros_per_mtok=$11, price_out_micros_per_mtok=$12, updated_by=$13, config=$14, updated_at=NOW()`,
			tenant, p.ID, p.Name, p.Endpoint, p.API, p.APIVersion, modelsJSON, p.Enabled, p.VisionCapable, p.HasKey, p.PriceInMicrosPerMTok, p.PriceOutMicrosPerMTok, p.UpdatedBy, configJSON,
		)
		if err != nil {
			return fmt.Errorf("upsert provider: %w", err)
		}
		return nil
	})
}

func (r *LlmProviderRepo) Delete(ctx context.Context, tenant, id string) error {
	return withConnErr(ctx, r.pool, tenant, func(conn *pgxpool.Conn) error {
		_, err := conn.Exec(ctx, `DELETE FROM llm_providers WHERE tenant_id = $1 AND id = $2`, tenant, id)
		if err != nil {
			return fmt.Errorf("delete provider: %w", err)
		}
		return nil
	})
}

// SetDefaultFor points one capability at a provider — the single write path for
// every default. An empty providerID clears the capability. A provider that
// does not exist in the tenant trips the composite foreign key, which is what
// makes ErrProviderNotFound here a database fact rather than a racy
// check-then-act.
func (r *LlmProviderRepo) SetDefaultFor(ctx context.Context, tenant, capability, providerID string) error {
	return withConnErr(ctx, r.pool, tenant, func(conn *pgxpool.Conn) error {
		if providerID == "" {
			if _, err := conn.Exec(ctx,
				`DELETE FROM llm_provider_defaults WHERE tenant_id=$1 AND capability=$2`,
				tenant, capability); err != nil {
				return fmt.Errorf("clear %s default: %w", capability, err)
			}
			return nil
		}
		_, err := conn.Exec(ctx, `
		INSERT INTO llm_provider_defaults (tenant_id, capability, provider_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (tenant_id, capability) DO UPDATE
		SET provider_id=$3, updated_at=NOW()`, tenant, capability, providerID)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign_key_violation
			return ErrProviderNotFound
		}
		if err != nil {
			return fmt.Errorf("set %s default: %w", capability, err)
		}
		return nil
	})
}

// DefaultsFor returns the tenant's capability → provider-id map. A capability
// with no default is absent from the map.
func (r *LlmProviderRepo) DefaultsFor(ctx context.Context, tenant string) (map[string]string, error) {
	return withConn(ctx, r.pool, tenant, func(conn *pgxpool.Conn) (map[string]string, error) {
		return defaultsOn(ctx, conn, tenant)
	})
}

// defaultsOn reads the defaults map on an already-scoped connection, so
// List/Get overlay their booleans without a second Acquire.
func defaultsOn(ctx context.Context, conn *pgxpool.Conn, tenant string) (map[string]string, error) {
	rows, err := conn.Query(ctx,
		`SELECT capability, provider_id FROM llm_provider_defaults WHERE tenant_id = $1`, tenant)
	if err != nil {
		return nil, fmt.Errorf("list provider defaults: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var capability, providerID string
		if err := rows.Scan(&capability, &providerID); err != nil {
			return nil, fmt.Errorf("scan provider default: %w", err)
		}
		out[capability] = providerID
	}
	return out, rows.Err()
}

// SetDefault / SetDefaultVision / SetFallback are the legacy per-capability
// entry points. They write llm_provider_defaults like everything else — the
// is_default / is_default_vision / is_fallback columns are frozen and never
// written again (migration 046).
func (r *LlmProviderRepo) SetDefault(ctx context.Context, tenant, id string) error {
	return r.SetDefaultFor(ctx, tenant, CapabilityChat, id)
}

func (r *LlmProviderRepo) SetDefaultVision(ctx context.Context, tenant, id string) error {
	return r.SetDefaultFor(ctx, tenant, CapabilityVision, id)
}

func (r *LlmProviderRepo) SetFallback(ctx context.Context, tenant, id string) error {
	return r.SetDefaultFor(ctx, tenant, CapabilityFallback, id)
}
