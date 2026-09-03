// Package connectorstore owns the per-user mcp_connections.json reference file
// in the workspace PVC. It records WHICH connectors a user has linked and where
// their tokens live in Vault — never the tokens themselves. The broker writes it
// on CompleteConnectorAuth/RevokeConnector; the frontend reads it (via
// ListConnectors) to show connection status.
package connectorstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/workspacepath"
)

const connectionsFile = "mcp_connections.json"

// Entry is one linked connector. VaultPath is the logical KV path (without the
// KV-v2 "data/" segment) for operator reference; the token is fetched by the
// secrets provider, not from here.
type Entry struct {
	ConnectorID string    `json:"connector_id"`
	Provider    string    `json:"provider"`
	DisplayName string    `json:"display_name"`
	Scopes      []string  `json:"scopes"`
	VaultPath   string    `json:"vault_path"`
	Status      string    `json:"status"`
	ConnectedAt time.Time `json:"connected_at"`
}

type fileContents struct {
	Connections []Entry `json:"connections"`
}

// Store reads and writes mcp_connections.json under a workspace root. Reads take
// a read lock, writes a write lock; writes are atomic (temp file + rename).
type Store struct {
	root string
	mu   sync.RWMutex
}

// New builds a Store rooted at the workspace root (the same Root the Tool Proxy
// writes documents under). An empty root disables persistence.
func New(root string) *Store { return &Store{root: root} }

// Enabled reports whether a workspace root is configured.
func (s *Store) Enabled() bool { return s.root != "" }

// configDir returns the (tenant, user) config directory under the shared
// sanitize rule (F42r). workspacepath.SanitizeSeg never errors, so the error
// return is always nil; kept only so the five call sites below don't need a
// signature change.
func (s *Store) configDir(tenant, user string) (string, error) {
	t := workspacepath.SanitizeSeg(tenant)
	u := workspacepath.SanitizeSeg(user)
	return filepath.Join(s.root, t, u, "config"), nil
}

// legacySanitizeSeg is connectorstore's pre-F42r reject-only rule: segments
// with separators, ".", "..", or empty were rejected outright; everything
// else passed through verbatim. Kept only to compute the pre-migration
// directory for the one-time read fallback below — never used to write.
func legacySanitizeSeg(seg string) (string, bool) {
	if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, "/\\") {
		return "", false
	}
	return seg, true
}

// legacyConfigDir returns the pre-F42r config directory for (tenant, user),
// and whether the pair could have been written there at all (the old rule
// rejected some inputs outright, so no on-disk state could exist for them).
func (s *Store) legacyConfigDir(tenant, user string) (string, bool) {
	t, ok := legacySanitizeSeg(tenant)
	if !ok {
		return "", false
	}
	u, ok := legacySanitizeSeg(user)
	if !ok {
		return "", false
	}
	return filepath.Join(s.root, t, u, "config"), true
}

func (s *Store) readLocked(tenant, user string) (*fileContents, string, error) {
	dir, err := s.configDir(tenant, user)
	if err != nil {
		return nil, "", err
	}
	path := filepath.Join(dir, connectionsFile)
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, "", err
		}
		// F42r migration: miss at the new sanitized path. Some (tenant,user)
		// pairs previously mapped verbatim under the old reject-only rule —
		// try that legacy path once, and if data is found there, serve it and
		// best-effort write it forward to the new path so later reads (and
		// writes, which always target the new path) land in one place.
		if fc, ok := s.tryLegacyFallback(tenant, user, path); ok {
			return fc, path, nil
		}
		return &fileContents{}, path, nil
	}
	var fc fileContents
	if err := json.Unmarshal(b, &fc); err != nil {
		return nil, "", fmt.Errorf("connectorstore: parse %s: %w", path, err)
	}
	return &fc, path, nil
}

// tryLegacyFallback reads the legacy-rule path for (tenant, user); on a hit
// it forwards the data to newPath (best-effort — a forward-write failure
// still returns the data found, ok=true) and reports whether legacy data was
// found at all.
func (s *Store) tryLegacyFallback(tenant, user, newPath string) (*fileContents, bool) {
	legacyDir, ok := s.legacyConfigDir(tenant, user)
	if !ok {
		return nil, false
	}
	legacyPath := filepath.Join(legacyDir, connectionsFile)
	b, err := os.ReadFile(legacyPath)
	if err != nil {
		return nil, false
	}
	var fc fileContents
	if err := json.Unmarshal(b, &fc); err != nil {
		return nil, false
	}
	_ = s.writeAtomic(newPath, &fc) // best-effort: serve regardless of outcome
	return &fc, true
}

func (s *Store) writeAtomic(path string, fc *fileContents) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(fc, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), connectionsFile+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// List returns the user's connector entries (empty when none).
func (s *Store) List(tenant, user string) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fc, _, err := s.readLocked(tenant, user)
	if err != nil {
		return nil, err
	}
	return fc.Connections, nil
}

// Resolve returns the entry for a connector id, and whether it exists.
func (s *Store) Resolve(tenant, user, connectorID string) (Entry, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fc, _, err := s.readLocked(tenant, user)
	if err != nil {
		return Entry{}, false, err
	}
	for _, e := range fc.Connections {
		if e.ConnectorID == connectorID {
			return e, true, nil
		}
	}
	return Entry{}, false, nil
}

// Upsert adds or replaces the entry with the same connector id.
func (s *Store) Upsert(tenant, user string, e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, path, err := s.readLocked(tenant, user)
	if err != nil {
		return err
	}
	replaced := false
	for i := range fc.Connections {
		if fc.Connections[i].ConnectorID == e.ConnectorID {
			fc.Connections[i] = e
			replaced = true
			break
		}
	}
	if !replaced {
		fc.Connections = append(fc.Connections, e)
	}
	return s.writeAtomic(path, fc)
}

// UpdateStatus best-effort updates the Status field of an existing entry,
// leaving every other field untouched. A no-op (not an error) when the entry
// doesn't exist or already carries the target status — the latter keeps the
// hot JIT-refresh path from writing the file on every successful call.
func (s *Store) UpdateStatus(tenant, user, connectorID, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, path, err := s.readLocked(tenant, user)
	if err != nil {
		return err
	}
	for i := range fc.Connections {
		if fc.Connections[i].ConnectorID == connectorID {
			if fc.Connections[i].Status == status {
				return nil
			}
			fc.Connections[i].Status = status
			return s.writeAtomic(path, fc)
		}
	}
	return nil
}

// Remove deletes the entry for a connector id (no-op if absent).
func (s *Store) Remove(tenant, user, connectorID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, path, err := s.readLocked(tenant, user)
	if err != nil {
		return err
	}
	out := fc.Connections[:0]
	for _, e := range fc.Connections {
		if e.ConnectorID != connectorID {
			out = append(out, e)
		}
	}
	fc.Connections = out
	return s.writeAtomic(path, fc)
}
