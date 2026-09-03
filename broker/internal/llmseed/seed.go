// broker/internal/llmseed/seed.go — idempotent LLM provider seed-on-boot.
// Reads AIKONOS_LLM_PROVIDERS_SEED_FILE (YAML), upserts providers that do not
// yet exist, and marks the one flagged is_default. Admin edits win: existing
// provider ids are never overwritten.
package llmseed

import (
	"context"
	"os"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
)

// LlmProviderStore is the minimal repo surface the seeder needs.
// Satisfied by *db.LlmProviderRepo; injectable in tests via the interface.
type LlmProviderStore interface {
	Get(ctx context.Context, tenant, id string) (db.Provider, error)
	Upsert(ctx context.Context, tenant string, p db.Provider) error
	SetDefault(ctx context.Context, tenant, id string) error
}

// seedModel mirrors the YAML model entry shape.
type seedModel struct {
	ID              string  `yaml:"id"`
	PriceIn         float64 `yaml:"priceIn"`
	PriceOut        float64 `yaml:"priceOut"`
	PriceCacheRead  float64 `yaml:"priceCacheRead"`
	PriceCacheWrite float64 `yaml:"priceCacheWrite"`
	ContextWindow   int     `yaml:"contextWindow"`
	MaxTokens       int     `yaml:"maxTokens"`
}

// seedProvider mirrors the YAML provider entry shape.
// apiKey supports ${ENV} interpolation via os.ExpandEnv.
type seedProvider struct {
	ID         string      `yaml:"id"`
	Name       string      `yaml:"name"`
	Endpoint   string      `yaml:"endpoint"`
	API        string      `yaml:"api"`
	APIVersion string      `yaml:"api_version"` // required when api is azure-openai
	Enabled    bool        `yaml:"enabled"`
	IsDefault  bool        `yaml:"is_default"`
	APIKey     string      `yaml:"apiKey"` // may contain ${ENV_VAR}
	Models     []seedModel `yaml:"models"`
}

// Seed reads the YAML file at path (or AIKONOS_LLM_PROVIDERS_SEED_FILE when
// path is empty) and for the given tenant inserts providers that do not already
// exist. Idempotent: a provider whose id is already in the repo is skipped so
// admin edits are never clobbered.
func Seed(ctx context.Context, repo LlmProviderStore, sec secrets.Provider, tenantID, path string, log *zap.Logger) {
	if path == "" {
		path = os.Getenv("AIKONOS_LLM_PROVIDERS_SEED_FILE")
	}
	if path == "" {
		return
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		log.Warn("llmseed: cannot read seed file", zap.String("path", path), zap.Error(err))
		return
	}

	var providers []seedProvider
	if err := yaml.Unmarshal(raw, &providers); err != nil {
		log.Warn("llmseed: cannot parse seed YAML", zap.String("path", path), zap.Error(err))
		return
	}

	for _, sp := range providers {
		if sp.ID == "" {
			log.Warn("llmseed: skipping provider with empty id")
			continue
		}

		_, err := repo.Get(ctx, tenantID, sp.ID)
		if err == nil {
			// Already present — admin edit wins, skip.
			log.Debug("llmseed: provider already exists, skipping", zap.String("id", sp.ID))
			continue
		}
		if err != db.ErrProviderNotFound {
			log.Warn("llmseed: Get failed, skipping provider", zap.String("id", sp.ID), zap.Error(err))
			continue
		}

		// Resolve api key from env interpolation.
		apiKey := os.ExpandEnv(sp.APIKey)

		hasKey := false
		if apiKey != "" && sec != nil {
			if werr := sec.WriteProviderKey(ctx, tenantID, sp.ID, apiKey); werr != nil {
				log.Warn("llmseed: write provider key failed — provider stored WITHOUT a key and will not be retried on next boot; set the key via the admin UI",
					zap.String("id", sp.ID), zap.Error(werr))
			} else {
				hasKey = true
			}
		}

		models := make([]db.Model, 0, len(sp.Models))
		for _, m := range sp.Models {
			models = append(models, db.Model{
				ID:              m.ID,
				PriceIn:         m.PriceIn,
				PriceOut:        m.PriceOut,
				PriceCacheRead:  m.PriceCacheRead,
				PriceCacheWrite: m.PriceCacheWrite,
				ContextWindow:   m.ContextWindow,
				MaxTokens:       m.MaxTokens,
			})
		}

		p := db.Provider{
			ID:         sp.ID,
			Name:       sp.Name,
			Endpoint:   sp.Endpoint,
			API:        sp.API,
			APIVersion: sp.APIVersion,
			Models:     models,
			Enabled:    sp.Enabled,
			IsDefault:  sp.IsDefault,
			HasKey:     hasKey,
			UpdatedBy:  "seed",
		}
		if err := repo.Upsert(ctx, tenantID, p); err != nil {
			log.Warn("llmseed: Upsert failed", zap.String("id", sp.ID), zap.Error(err))
			continue
		}

		if sp.IsDefault {
			if err := repo.SetDefault(ctx, tenantID, sp.ID); err != nil {
				log.Warn("llmseed: SetDefault failed", zap.String("id", sp.ID), zap.Error(err))
			}
		}

		log.Info("llmseed: seeded provider", zap.String("id", sp.ID), zap.Bool("has_key", hasKey))
	}
}
