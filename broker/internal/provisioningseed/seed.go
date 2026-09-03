// broker/internal/provisioningseed/seed.go — idempotent provisioning rule seed-on-boot.
// Reads AIKONOS_PROVISIONING_SEED_FILE (YAML), inserts rules whose matchers
// are not already present. Idempotent: admin edits win; existing matchers are
// never overwritten. Invalid entries are logged and skipped.
package provisioningseed

import (
	"context"
	"os"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/google/uuid"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/provisioning"
)

// RuleStore is the minimal repo surface the seeder needs.
// Satisfied by *db.ProvisioningRepo; injectable in tests.
type RuleStore interface {
	ListRules(ctx context.Context, tenantID string) ([]db.ProvisioningRule, error)
	AddRule(ctx context.Context, tenantID string, rule db.ProvisioningRule) error
}

// seedEntry mirrors the YAML rule entry shape.
type seedEntry struct {
	Matcher string   `yaml:"matcher"`
	Groups  []string `yaml:"groups"`
}

// seedFile is the top-level YAML document.
type seedFile struct {
	Rules []seedEntry `yaml:"rules"`
}

// Seed reads the YAML file at path and for the given tenant inserts rules
// whose matchers are not already present. Idempotent across restarts.
// path comes from viper (provisioning.seed_file); empty means disabled.
func Seed(ctx context.Context, repo RuleStore, tenantID, path string, log *zap.Logger) {
	if path == "" {
		return
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		log.Warn("provisioningseed: cannot read seed file", zap.String("path", path), zap.Error(err))
		return
	}

	var sf seedFile
	if err := yaml.Unmarshal(raw, &sf); err != nil {
		log.Warn("provisioningseed: cannot parse seed YAML", zap.String("path", path), zap.Error(err))
		return
	}

	existing, err := repo.ListRules(ctx, tenantID)
	if err != nil {
		log.Warn("provisioningseed: ListRules failed — skipping seed", zap.Error(err))
		return
	}
	existingMatchers := make(map[string]struct{}, len(existing))
	for _, r := range existing {
		existingMatchers[r.Matcher] = struct{}{}
	}

	for _, entry := range sf.Rules {
		matcher, mErr := provisioning.NormalizeMatcher(entry.Matcher)
		if mErr != nil {
			log.Warn("provisioningseed: invalid matcher, skipping",
				zap.String("matcher", entry.Matcher), zap.Error(mErr))
			continue
		}
		if err := provisioning.ValidateGroups(entry.Groups); err != nil {
			log.Warn("provisioningseed: invalid groups, skipping",
				zap.String("matcher", matcher), zap.Error(err))
			continue
		}
		if _, found := existingMatchers[matcher]; found {
			log.Debug("provisioningseed: matcher already exists, skipping", zap.String("matcher", matcher))
			continue
		}
		rule := db.ProvisioningRule{
			ID:        uuid.New(),
			Matcher:   matcher,
			Groups:    entry.Groups,
			CreatedBy: "seed",
		}
		if err := repo.AddRule(ctx, tenantID, rule); err != nil {
			log.Warn("provisioningseed: AddRule failed", zap.String("matcher", matcher), zap.Error(err))
			continue
		}
		log.Info("provisioningseed: seeded rule", zap.String("matcher", matcher), zap.Strings("groups", entry.Groups))
	}
}
