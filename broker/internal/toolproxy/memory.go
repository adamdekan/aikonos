// broker/internal/toolproxy/memory.go
//
// memory.read / memory.write — the two gated tools over an agent's OKF memory
// bundle. A bundle is a directory of markdown
// concepts under <segment>/.agent/Memory/, where the segment is the scope: the
// caller's own subject (user), svc-<agentId> (agent), group-<groupId> (group).
// Format and bundle storage both live in broker/internal/memorybundle (the
// store half is shared with the management RPCs in package broker, so one lock
// map covers every writer); this file is only the plugin, arg-parsing and
// authority wiring around them.
//
// These handlers call Config.WorkspaceFS directly rather than the
// workspaceRead/workspaceWrite seam: the seam hardwires req.UserID as the
// path segment, which can only ever reach the user scope.
package toolproxy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/memorybundle"
	"github.com/adamdekan/aikonos/broker/internal/workspacepath"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// Per-mode id budgets: progressive disclosure means many cheap frontmatter
// reads and only a handful of full bodies.
const (
	memoryMaxFrontmatterIDs = 20
	memoryMaxConceptIDs     = 5
)

type memoryReadPlugin struct{}

func (memoryReadPlugin) ToolID() string                  { return "memory.read" }
func (memoryReadPlugin) Available(cfg Config) bool       { return cfg.WorkspaceFS != nil }
func (memoryReadPlugin) Build(cfg Config) Handler        { return newMemoryReadHandler(cfg) }
func (memoryReadPlugin) Scope() string                   { return "memory:read" }
func (memoryReadPlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_READ_ONLY }

type memoryWritePlugin struct{}

func (memoryWritePlugin) ToolID() string                  { return "memory.write" }
func (memoryWritePlugin) Available(cfg Config) bool       { return cfg.WorkspaceFS != nil }
func (memoryWritePlugin) Build(cfg Config) Handler        { return newMemoryWriteHandler(cfg) }
func (memoryWritePlugin) Scope() string                   { return "memory:write" }
func (memoryWritePlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_WRITE_LOCAL }

func init() {
	RegisterPlugin(memoryReadPlugin{})
	RegisterPlugin(memoryWritePlugin{})
}

// memorySegment maps the call's scope arg onto the workspace path segment
// keying its bundle. The agent branch reads req.AgentID, which InvokeTool
// populates from the task row — a caller-supplied agent id here would be a
// cross-agent memory read.
func memorySegment(req Request) (string, error) {
	switch scope := argString(req.Args, "scope"); scope {
	case "user":
		if req.UserID == "" {
			return "", fmt.Errorf("%s: user scope requires a user-bound call", req.ToolID)
		}
		// The user branch is the only one with no authority check above it, so a
		// subject already shaped like a synthetic segment must not reach it — it
		// would alias onto an agent or group bundle without the group pre-gate.
		for _, reserved := range []string{"svc-", "group-"} {
			if strings.HasPrefix(req.UserID, reserved) {
				return "", fmt.Errorf("%s: user id %q uses the reserved %q prefix", req.ToolID, req.UserID, reserved)
			}
		}
		return req.UserID, nil
	case "agent":
		if req.AgentID == "" {
			return "", errors.New("memory: agent scope requires an agent-bound run")
		}
		return "svc-" + req.AgentID, nil
	case "group":
		gid := argString(req.Args, "group_id")
		if gid == "" {
			return "", fmt.Errorf("%s: group scope requires a %q", req.ToolID, "group_id")
		}
		// Rejected rather than sanitized: silently aliasing "a/b" onto "a_b"
		// would let two distinct group ids share one bundle.
		if workspacepath.SanitizeSeg(gid) != gid {
			return "", fmt.Errorf("%s: group_id %q is not a valid path segment", req.ToolID, gid)
		}
		return "group-" + gid, nil
	default:
		return "", fmt.Errorf("%s: scope must be \"user\", \"agent\" or \"group\", got %q", req.ToolID, scope)
	}
}

// memoryGeneratedBy is the server-side provenance stamp. An agent-bound run
// writes as the agent even into a user-scope bundle: the agent is what
// produced the content. Caller args never reach this value.
func memoryGeneratedBy(req Request) (string, error) {
	switch {
	case req.AgentID != "":
		return "agent:" + req.AgentID, nil
	case req.UserID != "":
		return "user:" + req.UserID, nil
	default:
		return "", fmt.Errorf("%s: no caller identity to stamp generated.by", req.ToolID)
	}
}

func newMemoryReadHandler(cfg Config) Handler {
	return func(ctx context.Context, req Request) (map[string]any, int64, error) {
		seg, err := memorySegment(req)
		if err != nil {
			return nil, 0, err
		}
		switch mode := argString(req.Args, "mode"); mode {
		case "index":
			idx, err := memorybundle.ReadIndex(ctx, cfg.WorkspaceFS, req.TenantID, seg)
			if err != nil {
				return nil, 0, fmt.Errorf("%s: %w", req.ToolID, err)
			}
			ids, err := memorybundle.ListConceptIDs(ctx, cfg.WorkspaceFS, req.TenantID, seg)
			if err != nil {
				return nil, 0, fmt.Errorf("%s: %w", req.ToolID, err)
			}
			out := map[string]any{"index": idx, "concept_count": int64(len(ids))}
			// The index renders every concept's title and description, so it
			// carries the same untrusted text the frontmatter does.
			annotateInjectionFlags(out, idx)
			return out, 0, nil
		case "frontmatter":
			return memoryReadConcepts(ctx, cfg, req, seg, false, memoryMaxFrontmatterIDs)
		case "concept":
			return memoryReadConcepts(ctx, cfg, req, seg, true, memoryMaxConceptIDs)
		default:
			return nil, 0, fmt.Errorf("%s: mode must be \"index\", \"frontmatter\" or \"concept\", got %q", req.ToolID, mode)
		}
	}
}

// memoryReadConcepts backs both the frontmatter and concept modes. A missing,
// malformed or unparseable id is reported in its own entry rather than failing
// the call — recall asks about several ids at once and one bad one must not
// cost the caller the rest.
func memoryReadConcepts(ctx context.Context, cfg Config, req Request, seg string, withBody bool, maxIDs int) (map[string]any, int64, error) {
	ids := memoryIDList(req.Args["ids"])
	if len(ids) == 0 {
		return nil, 0, fmt.Errorf("%s: %q is required for this mode", req.ToolID, "ids")
	}
	if len(ids) > maxIDs {
		return nil, 0, fmt.Errorf("%s: at most %d ids per call, got %d", req.ToolID, maxIDs, len(ids))
	}

	now := time.Now().UTC()
	// Every byte this mode hands back, scanned as one string: frontmatter alone
	// in frontmatter mode, frontmatter plus body in concept mode. fmt sorts map
	// keys, so the rendered frontmatter is deterministic.
	var scanned strings.Builder
	entries := make([]any, 0, len(ids))
	for _, id := range ids {
		entry := map[string]any{"id": id, "found": false}
		c, err := memorybundle.LoadConcept(ctx, cfg.WorkspaceFS, req.TenantID, seg, id)
		if err != nil {
			// LoadConcept's errors are already path-free per-id markers.
			entry["error"] = err.Error()
			entries = append(entries, entry)
			continue
		}
		entry["found"] = true
		entry["frontmatter"] = structpbSafe(c.Frontmatter)
		entry["trust_tier"] = memorybundle.TrustTier(c.Frontmatter)
		entry["stale"] = memorybundle.IsStale(c.Frontmatter, now)
		fmt.Fprintf(&scanned, "%v\n", c.Frontmatter)
		if withBody {
			entry["body"] = c.Body
			scanned.WriteString(c.Body)
			scanned.WriteString("\n")
		}
		entries = append(entries, entry)
	}

	out := map[string]any{"concepts": entries}
	// Recalled frontmatter and bodies are machine-written untrusted text — same
	// annotate-only posture as the office *.extract tools (result_scan.go).
	annotateInjectionFlags(out, scanned.String())
	return out, 0, nil
}

func newMemoryWriteHandler(cfg Config) Handler {
	return func(ctx context.Context, req Request) (map[string]any, int64, error) {
		seg, err := memorySegment(req)
		if err != nil {
			return nil, 0, err
		}
		id := argString(req.Args, "id")
		if err := memorybundle.ValidateID(id); err != nil {
			return nil, 0, fmt.Errorf("%s: %w", req.ToolID, err)
		}
		by, err := memoryGeneratedBy(req)
		if err != nil {
			return nil, 0, err
		}
		spec := memorybundle.ConceptSpec{
			Type:        argString(req.Args, "type"),
			Title:       argString(req.Args, "title"),
			Description: argString(req.Args, "description"),
			Tags:        memoryStringList(req.Args["tags"]),
			Status:      argString(req.Args, "status"),
			StaleAfter:  argString(req.Args, "stale_after"),
			Sources:     memoryMapList(req.Args["sources"]),
			Body:        argString(req.Args, "body"),
		}
		// ConceptSpec has no generated/verified field, so a caller-supplied
		// provenance claim has nowhere to land — it is dropped here by shape.
		data, err := memorybundle.ComposeConcept(spec, by, time.Now())
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", req.ToolID, err)
		}

		defer memorybundle.LockBundle(req.TenantID, seg)()

		ids, err := memorybundle.ListConceptIDs(ctx, cfg.WorkspaceFS, req.TenantID, seg)
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", req.ToolID, err)
		}
		if len(ids) >= memorybundle.MaxConcepts && !memoryHasID(ids, id) {
			return nil, 0, fmt.Errorf("%s: bundle already holds %d concepts (cap %d); update or deprecate an existing one instead",
				req.ToolID, len(ids), memorybundle.MaxConcepts)
		}
		rel := memorybundle.ConceptPath(id)
		if _, err := cfg.WorkspaceFS.Write(ctx, req.TenantID, seg, rel, data); err != nil {
			return nil, 0, fmt.Errorf("%s: write concept: %w", req.ToolID, err)
		}
		skipped, err := memorybundle.RegenerateIndex(ctx, cfg.WorkspaceFS, req.TenantID, seg)
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", req.ToolID, err)
		}
		entry := fmt.Sprintf("wrote `%s` (type: %s) by %s", id, spec.Type, by)
		if err := memorybundle.AppendLogEntry(ctx, cfg.WorkspaceFS, req.TenantID, seg, entry); err != nil {
			return nil, 0, fmt.Errorf("%s: %w", req.ToolID, err)
		}
		out := map[string]any{"id": id, "path": rel, "bytes": int64(len(data))}
		if skipped > 0 {
			// Surfaced only when non-zero: a dropped concept still consumes the
			// concept cap, so silence here is what would make a bundle look like it
			// has room it doesn't.
			out["unparseable_skipped"] = skipped
		}
		return out, 0, nil
	}
}

func memoryHasID(ids []string, id string) bool {
	for _, existing := range ids {
		if existing == id {
			return true
		}
	}
	return false
}

// memoryIDList reads the "ids" arg. Args arrive as structpb-decoded values, so
// a list is always []any.
func memoryIDList(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func memoryStringList(v any) []string {
	out := memoryIDList(v)
	if len(out) == 0 {
		return nil
	}
	return out
}

func memoryMapList(v any) []map[string]any {
	items, _ := v.([]any)
	var out []map[string]any
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// structpbSafe normalizes a parsed-YAML value into something
// structpb.NewStruct accepts (ResultStruct, toolproxy.go): yaml.v3 resolves
// timestamps — generated.at, an unquoted stale_after — to time.Time, which
// structpb rejects, and can yield non-string map keys.
func structpbSafe(v any) any {
	switch t := v.(type) {
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = structpbSafe(vv)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[fmt.Sprint(k)] = structpbSafe(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = structpbSafe(vv)
		}
		return out
	default:
		return v
	}
}
