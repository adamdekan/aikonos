package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ── DB-backed tests (require TEST_DATABASE_URL; skip when absent) ─────────────
//
// Mirrors the openTestPool skip pattern in workflows_test.go (openTestPool is
// defined there, shared package-wide) — no live Postgres is required for
// `go test` to pass; this only exercises the repo layer when
// TEST_DATABASE_URL is set.

// TestTaskRepo_CountEnvelopesByPayloadRef_OnlyCountsActiveStates is the
// integration counterpart to the broker package's fake-store coverage
// (personal_skills_test.go's memEnvelopeStore.CountEnvelopesByPayloadRef) for
// the real SQL query the skill-transfer staging GC refcount
// (personal_skills.go's gcStagingIfUnreferenced) depends on: it must count
// only PENDING/DELIVERED envelopes sharing payload_ref, excluding any that
// have since settled to a terminal state (DISMISSED, ACCEPTED, ...).
func TestTaskRepo_CountEnvelopesByPayloadRef_OnlyCountsActiveStates(t *testing.T) {
	pool := openTestPool(t)
	repo := NewTaskRepo(pool, zap.NewNop())
	ctx := context.Background()

	tenantID := uuid.New()
	tenant := tenantID.String()
	payloadRef := "xfer-" + uuid.New().String()

	newEnv := func() *Envelope {
		return &Envelope{
			TenantID:   tenantID,
			FromUserID: "alice@example.com",
			ToTarget:   map[string]any{"type": "user", "id": "bob@example.com"},
			TaskSpec:   map[string]any{"payload_ref": payloadRef, "kind": "skill_transfer"},
			ExpiresAt:  time.Now().UTC().Add(time.Hour),
		}
	}

	toDismiss, err := repo.CreateEnvelope(ctx, newEnv(), EnvelopeDelivered)
	if err != nil {
		t.Fatalf("create envelope to dismiss: %v", err)
	}
	toAccept, err := repo.CreateEnvelope(ctx, newEnv(), EnvelopeDelivered)
	if err != nil {
		t.Fatalf("create envelope to accept: %v", err)
	}
	if _, err := repo.CreateEnvelope(ctx, newEnv(), EnvelopePending); err != nil {
		t.Fatalf("create still-pending envelope: %v", err)
	}

	if n, err := repo.CountEnvelopesByPayloadRef(ctx, tenant, payloadRef); err != nil {
		t.Fatalf("CountEnvelopesByPayloadRef (before settling): %v", err)
	} else if n != 3 {
		t.Fatalf("count before settling any envelope: got %d, want 3", n)
	}

	if _, err := repo.DismissEnvelope(ctx, tenant, toDismiss.String(), "bob@example.com"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if _, err := repo.RespondEnvelope(ctx, tenant, toAccept.String(), "bob@example.com", true); err != nil {
		t.Fatalf("respond accepted: %v", err)
	}

	n, err := repo.CountEnvelopesByPayloadRef(ctx, tenant, payloadRef)
	if err != nil {
		t.Fatalf("CountEnvelopesByPayloadRef (after settling): %v", err)
	}
	if n != 1 {
		t.Fatalf("count after dismissing one and accepting another: got %d, want 1 (the still-pending one)", n)
	}
}
