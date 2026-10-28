package db

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// TestScimRepo_RoundTrip runs against a migrated DB (migration 060 applied);
// it skips without TEST_DATABASE_URL.
func TestScimRepo_RoundTrip(t *testing.T) {
	pool := openTestPool(t)
	repo := NewScimRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant := uuid.NewString()

	// Tokens: create, resolve, rotate, revoke.
	tok, err := repo.CreateToken(ctx, tenant, "hash-1-"+tenant, "scim_abc", "admin", nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if id, err := repo.ResolveToken(ctx, tenant, "hash-1-"+tenant); err != nil || id != tok.ID {
		t.Fatalf("resolve: %v %v", id, err)
	}
	if _, err := repo.ResolveToken(ctx, uuid.NewString(), "hash-1-"+tenant); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other tenant resolved the token: %v", err)
	}
	tok2, err := repo.CreateToken(ctx, tenant, "hash-2-"+tenant, "scim_def", "admin", &tok.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := repo.ResolveToken(ctx, tenant, "hash-1-"+tenant); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotated token still resolves: %v", err)
	}
	if list, err := repo.ListTokens(ctx, tenant); err != nil || len(list) != 1 || list[0].ID != tok2.ID || list[0].LastUsedAt != nil {
		t.Fatalf("list tokens: %v %v", list, err)
	}
	if err := repo.RevokeToken(ctx, tenant, tok2.ID, "admin"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := repo.RevokeToken(ctx, tenant, tok2.ID, "admin"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double revoke: %v", err)
	}

	// Users: userName unique case-insensitively, externalId unique.
	u := ScimUser{UserName: "Alice@contoso.com", ExternalID: "oid-a", Emails: []ScimEmail{{Value: "alice@contoso.com", Type: "work", Primary: true}}, Active: true}
	if err := repo.CreateUser(ctx, tenant, &u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	dup := ScimUser{UserName: "alice@CONTOSO.com", ExternalID: "oid-b", Active: true}
	if err := repo.CreateUser(ctx, tenant, &dup); !errors.Is(err, ErrScimConflict) {
		t.Fatalf("duplicate userName: %v", err)
	}
	u.Active = false
	if err := repo.UpdateUser(ctx, tenant, &u); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := repo.GetUser(ctx, tenant, u.ID)
	if err != nil || got.Active || len(got.Emails) != 1 || got.Emails[0].Value != "alice@contoso.com" {
		t.Fatalf("get: %+v %v", got, err)
	}

	// Groups: members, fga_group unique, delete user drops membership.
	g := ScimGroup{DisplayName: "Finance", FGAGroup: "finance", Members: []uuid.UUID{u.ID}}
	if err := repo.CreateGroup(ctx, tenant, &g); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := repo.CreateGroup(ctx, tenant, &ScimGroup{DisplayName: "FINANCE", FGAGroup: "finance"}); !errors.Is(err, ErrScimConflict) {
		t.Fatalf("duplicate fga_group: %v", err)
	}
	g.DisplayName = "Finance EU"
	if err := repo.UpdateGroup(ctx, tenant, &g); err != nil {
		t.Fatalf("update group: %v", err)
	}
	if err := repo.DeleteUser(ctx, tenant, u.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	gg, err := repo.GetGroup(ctx, tenant, g.ID)
	if err != nil || len(gg.Members) != 0 || gg.DisplayName != "Finance EU" {
		t.Fatalf("group after user delete: %+v %v", gg, err)
	}
	if err := repo.DeleteGroup(ctx, tenant, g.ID); err != nil {
		t.Fatalf("delete group: %v", err)
	}
	if groups, err := repo.ListGroups(ctx, tenant); err != nil || len(groups) != 0 {
		t.Fatalf("groups left: %v %v", groups, err)
	}
}
