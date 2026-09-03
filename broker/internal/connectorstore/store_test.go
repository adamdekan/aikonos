package connectorstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/workspacepath"
)

func TestUpsertListResolveRemove(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if !s.Enabled() {
		t.Fatal("store should be enabled with a root")
	}

	e := Entry{
		ConnectorID: "gdrive",
		Provider:    "google_drive",
		DisplayName: "Google Drive",
		Scopes:      []string{"gdrive:read"},
		VaultPath:   "workspaces/t/u/connectors/gdrive",
		Status:      "connected",
		ConnectedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
	if err := s.Upsert("t", "u", e); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, ok, err := s.Resolve("t", "u", "gdrive")
	if err != nil || !ok {
		t.Fatalf("resolve: %v %v", ok, err)
	}
	if got.Provider != "google_drive" || got.Status != "connected" {
		t.Fatalf("resolved wrong entry: %+v", got)
	}

	// Upsert same id replaces, doesn't duplicate.
	e.Status = "expired"
	if err := s.Upsert("t", "u", e); err != nil {
		t.Fatal(err)
	}
	list, err := s.List("t", "u")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Status != "expired" {
		t.Fatalf("expected 1 replaced entry, got %+v", list)
	}

	if err := s.Remove("t", "u", "gdrive"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Resolve("t", "u", "gdrive"); ok {
		t.Fatal("entry should be gone after remove")
	}
}

func TestListEmptyWhenNoFile(t *testing.T) {
	s := New(t.TempDir())
	list, err := s.List("t", "u")
	if err != nil || len(list) != 0 {
		t.Fatalf("expected empty list, got %v %v", list, err)
	}
}

func TestTwoProvidersCoexist(t *testing.T) {
	s := New(t.TempDir())
	_ = s.Upsert("t", "u", Entry{ConnectorID: "gdrive", Provider: "google_drive"})
	_ = s.Upsert("t", "u", Entry{ConnectorID: "onedrive", Provider: "onedrive"})
	list, _ := s.List("t", "u")
	if len(list) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(list))
	}
}

// F42r: connectorstore no longer rejects traversal-shaped segments outright —
// it now shares workspacefs/writers' replace-style rule, which sanitizes
// instead of erroring. This pins that the write lands under the tenant root
// (never escaping via "..") rather than failing.
func TestTraversalSegmentSanitizedNotRejected(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if err := s.Upsert("../escape", "u", Entry{ConnectorID: "gdrive"}); err != nil {
		t.Fatalf("expected sanitize-not-reject, got error: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == ".." || strings.Contains(e.Name(), "/") {
			t.Fatalf("escaped tenant root: %q", e.Name())
		}
	}
}

func TestSanitizeMatchesWorkspacePathRule(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if err := s.Upsert("alice@corp.com", "u", Entry{ConnectorID: "gdrive"}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, workspacepath.SanitizeSeg("alice@corp.com"), "u", "config", connectionsFile)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("expected file at shared-rule path %s: %v", p, err)
	}
}

// F42r legacy-read fallback: an entry that only exists at the pre-migration
// (reject-only rule) path is found, served, and forwarded to the new path.
func TestLegacyFallbackFoundAndForwarded(t *testing.T) {
	root := t.TempDir()
	// A special-char user id: the old reject-only rule wrote it verbatim; the
	// new replace-style rule rewrites '@'/'.'  — the two paths diverge.
	tenant, user := "tenant2", "alice@corp.com"
	legacyDir := filepath.Join(root, tenant, user, "config")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fc := fileContents{Connections: []Entry{{ConnectorID: "gdrive", Provider: "google_drive"}}}
	b, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, connectionsFile), b, 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(root)
	list, err := s.List(tenant, user)
	if err != nil {
		t.Fatalf("legacy fallback read: %v", err)
	}
	if len(list) != 1 || list[0].ConnectorID != "gdrive" {
		t.Fatalf("expected legacy entry served, got %+v", list)
	}

	newPath := filepath.Join(root, workspacepath.SanitizeSeg(tenant), workspacepath.SanitizeSeg(user), "config", connectionsFile)
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("expected forward-write to new path %s: %v", newPath, err)
	}
}

// Miss at both the new and legacy path is a normal not-found (empty list),
// not an error.
func TestMissAtBothPathsIsNormalNotFound(t *testing.T) {
	s := New(t.TempDir())
	list, err := s.List("tenant-none", "user-none")
	if err != nil {
		t.Fatalf("expected no error on miss, got %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty list, got %+v", list)
	}
}

// Forward-write failure (new path directory unwritable) still serves the
// legacy data — best-effort forwarding never blocks the read.
func TestLegacyFallbackServedEvenWhenForwardWriteFails(t *testing.T) {
	root := t.TempDir()
	tenant, user := "tenant3", "carol@corp.com"
	legacyDir := filepath.Join(root, tenant, user, "config")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fc := fileContents{Connections: []Entry{{ConnectorID: "onedrive", Provider: "onedrive"}}}
	b, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, connectionsFile), b, 0o644); err != nil {
		t.Fatal(err)
	}

	// Block the forward-write: pre-create the new path's config dir (so the
	// initial read is a clean not-found, not an fs error) then strip write
	// permission so writeAtomic's temp-file creation fails.
	newTenantSeg := workspacepath.SanitizeSeg(tenant)
	newUserSeg := workspacepath.SanitizeSeg(user)
	newConfigDir := filepath.Join(root, newTenantSeg, newUserSeg, "config")
	if err := os.MkdirAll(newConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(newConfigDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(newConfigDir, 0o755) }) // let t.TempDir clean up

	s := New(root)
	list, err := s.List(tenant, user)
	if err != nil {
		t.Fatalf("expected legacy data served despite forward-write failure, got error: %v", err)
	}
	if len(list) != 1 || list[0].ConnectorID != "onedrive" {
		t.Fatalf("expected legacy entry served, got %+v", list)
	}
}

func TestUpdateStatus(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Upsert("t", "u", Entry{ConnectorID: "gdrive", Status: "connected"}); err != nil {
		t.Fatal(err)
	}

	// Failure transition: connected -> reconnect_needed.
	if err := s.UpdateStatus("t", "u", "gdrive", "reconnect_needed"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Resolve("t", "u", "gdrive")
	if err != nil || !ok {
		t.Fatalf("resolve: %v %v", ok, err)
	}
	if got.Status != "reconnect_needed" {
		t.Fatalf("expected reconnect_needed, got %q", got.Status)
	}

	// Recovery transition: reconnect_needed -> connected. Other fields must
	// survive (only Status is touched).
	if err := s.UpdateStatus("t", "u", "gdrive", "connected"); err != nil {
		t.Fatal(err)
	}
	got, ok, err = s.Resolve("t", "u", "gdrive")
	if err != nil || !ok {
		t.Fatalf("resolve: %v %v", ok, err)
	}
	if got.Status != "connected" || got.ConnectorID != "gdrive" {
		t.Fatalf("expected connected/gdrive, got %+v", got)
	}
}

func TestUpdateStatusMissingEntryIsNoop(t *testing.T) {
	s := New(t.TempDir())
	if err := s.UpdateStatus("t", "u", "gdrive", "reconnect_needed"); err != nil {
		t.Fatalf("missing entry should be a no-op, got %v", err)
	}
	list, err := s.List("t", "u")
	if err != nil || len(list) != 0 {
		t.Fatalf("expected no entries created, got %v %v", list, err)
	}
}

func TestFileWrittenAtExpectedPath(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if err := s.Upsert("tenant1", "alice", Entry{ConnectorID: "gdrive"}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "tenant1", "alice", "config", "mcp_connections.json")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("expected file at %s: %v", p, err)
	}
}
