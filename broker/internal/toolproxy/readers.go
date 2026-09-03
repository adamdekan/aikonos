// broker/internal/toolproxy/readers.go
//
// doc.read (read_only) and workspace.read (read_only) — the read side of the
// per-task workspace, pairing with doc.write so read-after-write plans work.
package toolproxy

import (
	"context"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// docReadPlugin self-registers doc.read, unconditional (no deps).
type docReadPlugin struct{}

func (docReadPlugin) ToolID() string                  { return "doc.read" }
func (docReadPlugin) Available(cfg Config) bool       { return true }
func (docReadPlugin) Build(cfg Config) Handler        { return newDocReadHandler(cfg.withSeam()) }
func (docReadPlugin) Scope() string                   { return "doc:read" }
func (docReadPlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_READ_ONLY }

// workspaceReadPlugin self-registers workspace.read, unconditional (no deps).
type workspaceReadPlugin struct{}

func (workspaceReadPlugin) ToolID() string                  { return "workspace.read" }
func (workspaceReadPlugin) Available(cfg Config) bool       { return true }
func (workspaceReadPlugin) Build(cfg Config) Handler        { return newWorkspaceReadHandler(cfg.withSeam()) }
func (workspaceReadPlugin) Scope() string                   { return "workspace:read" }
func (workspaceReadPlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_READ_ONLY }

func init() {
	RegisterPlugin(docReadPlugin{})
	RegisterPlugin(workspaceReadPlugin{})
}

// newDocReadHandler reads a document back from the caller's workspace: the
// injected Backend seam (cfg.fs) when the request carries a known user, else
// the legacy per-task local directory — see workspace_seam.go.
func newDocReadHandler(cfg WorkspaceConfig) Handler {
	return func(ctx context.Context, req Request) (map[string]any, int64, error) {
		rel, _ := req.Args["path"].(string)
		b, err := cfg.workspaceRead(ctx, req, rel, "doc.read")
		if err != nil {
			return nil, 0, err
		}
		content := decodeToUTF8(b)
		full := len(content)
		content = truncateUTF8(content, maxContentChars)
		cost := int64(full/1024) + 1
		return map[string]any{
			"path":           rel,
			"content":        content,
			"content_length": full,
		}, cost, nil
	}
}

// newWorkspaceReadHandler lists the files in the caller's workspace (relative
// paths + sizes): the injected Backend seam when the request carries a known
// user, else the legacy per-task local directory — see workspace_seam.go.
// Returns an empty list when nothing has been written yet.
func newWorkspaceReadHandler(cfg WorkspaceConfig) Handler {
	return func(ctx context.Context, req Request) (map[string]any, int64, error) {
		files, err := cfg.workspaceList(ctx, req)
		if err != nil {
			return nil, 0, err
		}
		return map[string]any{"files": files, "count": len(files)}, 1, nil
	}
}
