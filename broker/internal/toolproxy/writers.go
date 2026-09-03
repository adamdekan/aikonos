// broker/internal/toolproxy/writers.go
//
// doc.write (write_local) and email.draft (write_local) handlers.
package toolproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"

	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
	"github.com/adamdekan/aikonos/broker/internal/workspacepath"
)

type WorkspaceConfig struct {
	// Root is the base dir for doc.write output. Empty → <tmp>/aikonos-workspace.
	// Files land under Root/<tenant>/<task>/<path>; in MVP this is the broker's
	// ephemeral fs (a real deployment mounts a per-task workspace volume).
	Root string

	// fs is the injected workspace storage seam, populated from Config.WorkspaceFS via withSeam below — typically a
	// *workspacefs.Router. Every file-touching handler routes through it
	// instead of Root above when the request carries a known user
	// (req.UserID != "") and fs is non-nil; nil (the default until main.go
	// wires one), or a task-only request (no user — the server-side executor
	// path), always takes the legacy branch. See workspace_seam.go.
	fs workspacefs.Backend
}

func (c WorkspaceConfig) root() string {
	if c.Root != "" {
		return c.Root
	}
	return filepath.Join(os.TempDir(), "aikonos-workspace")
}

// docWritePlugin self-registers doc.write, unconditional (workspace root
// always has a default even when Config.Workspace is zero-valued).
type docWritePlugin struct{}

func (docWritePlugin) ToolID() string                  { return "doc.write" }
func (docWritePlugin) Available(cfg Config) bool       { return true }
func (docWritePlugin) Build(cfg Config) Handler        { return newDocWriteHandler(cfg.withSeam()) }
func (docWritePlugin) Scope() string                   { return "doc:write" }
func (docWritePlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_WRITE_LOCAL }

// emailDraftPlugin self-registers email.draft, unconditional (no deps).
type emailDraftPlugin struct{}

func (emailDraftPlugin) ToolID() string            { return "email.draft" }
func (emailDraftPlugin) Available(cfg Config) bool { return true }
func (emailDraftPlugin) Build(cfg Config) Handler  { return emailDraftHandler }
func (emailDraftPlugin) Scope() string             { return "email:write" }
func (emailDraftPlugin) EffectClass() planv1.EffectClass {
	return planv1.EffectClass_WRITE_EXTERNAL
}

func init() {
	RegisterPlugin(docWritePlugin{})
	RegisterPlugin(emailDraftPlugin{})
}

// newDocWriteHandler writes a document into the caller's workspace: the
// injected Backend seam (cfg.fs) when the request carries a known user, else
// the legacy per-task local directory (no traversal, no absolute path) — see
// workspace_seam.go.
func newDocWriteHandler(cfg WorkspaceConfig) Handler {
	return func(ctx context.Context, req Request) (map[string]any, int64, error) {
		rel, _ := req.Args["path"].(string)
		if rel == "" {
			return nil, 0, fmt.Errorf("doc.write: missing required arg %q", "path")
		}
		body := argString(req.Args, "body", "content")
		if int64(len(body)) > workspacefs.MaxFileBytes {
			return nil, 0, fmt.Errorf("doc.write: body exceeds max size of %d bytes", workspacefs.MaxFileBytes)
		}

		if err := cfg.workspaceWrite(ctx, req, rel, "doc.write", []byte(body)); err != nil {
			return nil, 0, err
		}
		sum := sha256.Sum256([]byte(body))
		cost := int64(len(body)/1024) + 1
		return map[string]any{
			"path":           rel,
			"bytes_written":  len(body),
			"content_sha256": hex.EncodeToString(sum[:]),
			"written_at":     time.Now().UTC().Format(time.RFC3339),
		}, cost, nil
	}
}

// emailDraft composes (does NOT send) an email. Sending is a separate
// write_external tool gated by approval; drafting is write_local, so returning
// the composed draft is the correct, complete result.
func emailDraftHandler(ctx context.Context, req Request) (map[string]any, int64, error) {
	to := argString(req.Args, "to", "recipient")
	subject := argString(req.Args, "subject")
	body := argString(req.Args, "body", "content")
	if to == "" || subject == "" {
		return nil, 0, fmt.Errorf("email.draft: %q and %q are required", "to", "subject")
	}
	preview := body
	if len(preview) > 280 {
		preview = preview[:280]
	}
	return map[string]any{
		"draft_id":     uuid.NewString(),
		"to":           to,
		"subject":      subject,
		"body_preview": preview,
		"body_length":  len(body),
		"created_at":   time.Now().UTC().Format(time.RFC3339),
	}, 1, nil
}

// writeFileAtomic writes data to a temp file in the same directory as path,
// then renames it into place — mirroring workspacefs.Write so a crash
// mid-write never leaves a partial destination file. The temp file is
// best-effort removed on any error before the rename.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// argString returns the first non-empty string value among keys.
func argString(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// safeJoin builds base/tenant/task/rel, rejecting absolute paths and any rel
// that escapes the per-task directory via .. traversal. The escape check
// delegates to workspacefs.CleanRel — the same single source of truth
// router.go/store.go use — instead of re-implementing its own HasPrefix check.
func safeJoin(base, tenant, task, rel string) (string, error) {
	clean, err := workspacefs.CleanRel(rel)
	if err != nil {
		return "", fmt.Errorf("doc.write: %w", err)
	}
	taskDir := filepath.Join(base, sanitizeSeg(tenant), sanitizeSeg(task))
	return filepath.Join(taskDir, clean), nil
}

// sanitizeSeg delegates to the one shared (tenant, user)→path rule; kept as a
// thin unexported wrapper so readers.go's call sites don't need to change.
func sanitizeSeg(s string) string {
	return workspacepath.SanitizeSeg(s)
}
