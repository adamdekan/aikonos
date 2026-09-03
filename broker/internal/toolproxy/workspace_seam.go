// broker/internal/toolproxy/workspace_seam.go
//
// The CP7 seam: doc.write/doc.read/
// workspace.read, and every office/cloudfile tool via officeReadInput/
// officeWriteOutput, route through an injected workspacefs.Backend instead of
// the legacy per-task local directory when the calling request carries a
// known user. Centralized here so the routing decision, the Backend call,
// and the sentinel→tool-error mapping are implemented exactly once.
package toolproxy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
)

// withSeam returns cfg.Workspace carrying the injected Backend (Config.
// WorkspaceFS) — the single place that combines the legacy per-task root
// with the optional per-user seam, so every plugin Build method threads one
// value through instead of two.
func (cfg Config) withSeam() WorkspaceConfig {
	ws := cfg.Workspace
	ws.fs = cfg.WorkspaceFS
	return ws
}

// useFS reports whether req should route through cfg.fs (the injected
// Backend) rather than the legacy local task-dir path: the caller must be a
// known user (req.UserID != "" — a task-only server-side executor run has no
// user) AND a Backend must actually be wired (nil ⇒ pure legacy, the default
// until main.go configures one).
func (cfg WorkspaceConfig) useFS(req Request) bool {
	return req.UserID != "" && cfg.fs != nil
}

// mapBackendErr maps an error from the Backend seam to a tool-facing error
// for toolID. A missing file reads exactly like the legacy "not found"
// phrasing every handler already uses; every other error (including every
// workspacefs sentinel — ErrReconnectRequired, ErrUnavailable, ErrForbidden,
// ErrTooLarge, ErrInvalidPath) keeps its message text intact via %w, since
// "reconnect_needed" in particular must stay grep-stable
//.
func mapBackendErr(toolID, rel string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %q not found in workspace", toolID, rel)
	}
	return fmt.Errorf("%s: %w", toolID, err)
}

// workspaceRead reads rel for toolID (error messages read "<toolID>: ..."),
// routing per useFS. A missing rel is rejected identically on both branches
// — every existing read call site names its arg "path" in this message.
func (cfg WorkspaceConfig) workspaceRead(ctx context.Context, req Request, rel, toolID string) ([]byte, error) {
	if rel == "" {
		return nil, fmt.Errorf("%s: missing required arg %q", toolID, "path")
	}
	if cfg.useFS(req) {
		data, _, err := cfg.fs.Read(ctx, req.TenantID, req.UserID, rel)
		if err != nil {
			return nil, mapBackendErr(toolID, rel, err)
		}
		return data, nil
	}
	clean, err := safeJoin(cfg.root(), req.TenantID, req.wsKey(), rel)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", toolID, err)
	}
	b, err := os.ReadFile(clean)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s: %q not found in workspace", toolID, rel)
		}
		return nil, fmt.Errorf("%s: read failed: %w", toolID, err)
	}
	return b, nil
}

// agentDirGuard rejects a write target under .agent/ — the server-maintained
// tree (memory bundles, Sessions). WHY here rather than per handler: every seam
// writer (doc.write, every office output, cloudfile save_to) routes through
// workspaceWrite, and a raw concept file dropped into .agent/Memory/ would
// carry caller-authored `generated`/`verified` frontmatter that memory.read
// then reports as human-reviewed — provenance forgery through a tool that never
// gated on memory:write. The prefix test (cleaning included) lives in
// workspacefs.UnderAgentDir, shared with the user-facing file RPCs; unlike
// those, this seam has NO Sessions carve-out — a tool may never write a session
// record either.
func agentDirGuard(rel, toolID string) error {
	if workspacefs.UnderAgentDir(rel) {
		return fmt.Errorf("%s: writing under .agent/ is not permitted", toolID)
	}
	return nil
}

// workspaceWrite writes data at rel for toolID, routing per useFS. Callers
// apply their own size precheck first (each names the cap slightly
// differently) — this only performs the write.
func (cfg WorkspaceConfig) workspaceWrite(ctx context.Context, req Request, rel, toolID string, data []byte) error {
	if err := agentDirGuard(rel, toolID); err != nil {
		return err
	}
	if cfg.useFS(req) {
		if _, err := cfg.fs.Write(ctx, req.TenantID, req.UserID, rel, data); err != nil {
			return mapBackendErr(toolID, rel, err)
		}
		return nil
	}
	clean, err := safeJoin(cfg.root(), req.TenantID, req.wsKey(), rel)
	if err != nil {
		return fmt.Errorf("%s: %w", toolID, err)
	}
	if err := os.MkdirAll(filepath.Dir(clean), 0o750); err != nil {
		return fmt.Errorf("%s: mkdir failed: %w", toolID, err)
	}
	if err := writeFileAtomic(clean, data); err != nil {
		return fmt.Errorf("%s: write failed: %w", toolID, err)
	}
	return nil
}

// workspaceList returns the {path,bytes} entries workspace.read reports,
// routing per useFS. The FS-routed listing skips directory entries so the
// shape stays identical to the legacy WalkDir path (files only, never
// directories).
//
// Accepted divergence (pinned by test): the FS-routed listing also inherits
// workspacefs.List's own "config/" exclusion, whereas the legacy per-task
// WalkDir below has no such concept — deliberate hardening (agent tools now
// see the same user workspace the Files explorer does), not a bug.
func (cfg WorkspaceConfig) workspaceList(ctx context.Context, req Request) ([]any, error) {
	if cfg.useFS(req) {
		infos, err := cfg.fs.List(ctx, req.TenantID, req.UserID)
		if err != nil {
			return nil, mapBackendErr("workspace.read", "", err)
		}
		out := make([]any, 0, len(infos))
		for _, fi := range infos {
			if fi.IsDir {
				continue
			}
			out = append(out, map[string]any{"path": fi.Path, "bytes": fi.Size})
		}
		return out, nil
	}
	base := cfg.root()
	taskDir := filepath.Join(base, sanitizeSeg(req.TenantID), sanitizeSeg(req.wsKey()))
	var out []any
	err := filepath.WalkDir(taskDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // empty workspace
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(taskDir, path)
		info, ierr := d.Info()
		var size int64
		if ierr == nil {
			size = info.Size()
		}
		out = append(out, map[string]any{"path": rel, "bytes": size})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("workspace.read: %w", err)
	}
	return out, nil
}
