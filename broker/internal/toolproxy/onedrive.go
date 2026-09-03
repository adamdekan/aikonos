// broker/internal/toolproxy/onedrive.go
//
// onedrive.read (read_only) and onedrive.write (write_external) — governed
// access to the calling user's OneDrive via Microsoft Graph. Like gdrive.*, the
// OAuth token is fetched just-in-time from Vault and the call is already gated by
// a capability token before reaching here. Simple uploads only (<=4MB); resumable
// upload sessions are deferred.
package toolproxy

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"

	"github.com/adamdekan/aikonos/broker/internal/connector"
)

// Overridable in tests; production points at Microsoft Graph.
var graphAPIBase = "https://graph.microsoft.com/v1.0"

type graphItem struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	Size                 int64  `json:"size,omitempty"`
	LastModifiedDateTime string `json:"lastModifiedDateTime,omitempty"`
	WebURL               string `json:"webUrl,omitempty"`
	File                 *struct {
		MimeType string `json:"mimeType,omitempty"`
	} `json:"file,omitempty"`
	// Folder is present (non-nil) iff the item is a directory — the DriveItem
	// "folder" facet. Used to route a path lookup to list-children instead of
	// fetch-content.
	Folder *struct {
		ChildCount int `json:"childCount"`
	} `json:"folder,omitempty"`
}

func (it graphItem) mimeType() string {
	if it.File != nil {
		return it.File.MimeType
	}
	return ""
}

func (it graphItem) meta() cloudFileMeta {
	return cloudFileMeta{FileID: it.ID, Name: it.Name, MimeType: it.mimeType(), ModifiedTime: it.LastModifiedDateTime, WebURL: it.WebURL}
}

var onedriveOps = cloudFileOps{
	provider: connector.ProviderOneDrive,
	errTag:   "onedrive",
	readOne:  onedriveReadOne,
	list:     onedriveListOp,
	validateWrite: func(args map[string]any) error {
		if argString(args, "path", "name") == "" {
			return fmt.Errorf("onedrive.write: missing required arg %q", "path")
		}
		return nil
	},
	write: onedriveWriteOp,
}

// onedriveReadPlugin self-registers onedrive.read, gated on connector wiring
// (see connectorAvailable in gdrive.go).
type onedriveReadPlugin struct{}

func (onedriveReadPlugin) ToolID() string            { return "onedrive.read" }
func (onedriveReadPlugin) Available(cfg Config) bool { return connectorAvailable(cfg) }
func (onedriveReadPlugin) Build(cfg Config) Handler {
	return newOneDriveReadHandler(connectorDepsFrom(cfg), cfg.withSeam())
}
func (onedriveReadPlugin) Scope() string                   { return "onedrive:read" }
func (onedriveReadPlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_READ_ONLY }

// onedriveWritePlugin self-registers onedrive.write, gated on connector wiring.
type onedriveWritePlugin struct{}

func (onedriveWritePlugin) ToolID() string            { return "onedrive.write" }
func (onedriveWritePlugin) Available(cfg Config) bool { return connectorAvailable(cfg) }
func (onedriveWritePlugin) Build(cfg Config) Handler {
	return newOneDriveWriteHandler(connectorDepsFrom(cfg), cfg.withSeam())
}
func (onedriveWritePlugin) Scope() string { return "onedrive:write" }
func (onedriveWritePlugin) EffectClass() planv1.EffectClass {
	return planv1.EffectClass_WRITE_EXTERNAL
}

func init() {
	RegisterPlugin(onedriveReadPlugin{})
	RegisterPlugin(onedriveWritePlugin{})
}

func newOneDriveReadHandler(d connectorDeps, ws WorkspaceConfig) Handler {
	return newCloudReadHandler(d, ws, onedriveOps)
}

func onedriveReadOne(ctx context.Context, d connectorDeps, token string, args map[string]any, maxBytes int64) (cloudFileMeta, []byte, bool, error) {
	if itemID := argString(args, "item_id", "itemId"); itemID != "" {
		base := fmt.Sprintf("%s/me/drive/items/%s", graphAPIBase, url.PathEscape(itemID))
		return onedriveFetchItem(ctx, d, token, base, base+"/content", maxBytes)
	}
	if path := argString(args, "path"); path != "" {
		base := fmt.Sprintf("%s/me/drive/root:/%s", graphAPIBase, escapeGraphPath(path))
		meta, err := onedriveResolveMeta(ctx, d, token, base)
		if err != nil {
			return cloudFileMeta{}, nil, false, err
		}
		if meta.Folder != nil {
			// path addresses a directory: fall through to list, which resolves
			// the same path arg via the :/children endpoint.
			return cloudFileMeta{}, nil, false, nil
		}
		raw, err := d.fetchContent(ctx, base+":/content", token, maxBytes)
		if err != nil {
			return cloudFileMeta{}, nil, false, err
		}
		return meta.meta(), raw, true, nil
	}
	return cloudFileMeta{}, nil, false, nil
}

// onedriveResolveMeta fetches a DriveItem's metadata (file or folder) by its
// item or path base URL, without touching content.
func onedriveResolveMeta(ctx context.Context, d connectorDeps, token, metaURL string) (graphItem, error) {
	sep := "?"
	if strings.Contains(metaURL, "?") {
		sep = "&"
	}
	var meta graphItem
	err := d.doJSON(ctx, "GET", metaURL+sep+"$select=id,name,size,lastModifiedDateTime,webUrl,file,folder", token, "", nil, &meta)
	return meta, err
}

func onedriveFetchItem(ctx context.Context, d connectorDeps, token, metaURL, contentURL string, maxBytes int64) (cloudFileMeta, []byte, bool, error) {
	meta, err := onedriveResolveMeta(ctx, d, token, metaURL)
	if err != nil {
		return cloudFileMeta{}, nil, false, err
	}
	raw, err := d.fetchContent(ctx, contentURL, token, maxBytes)
	if err != nil {
		return cloudFileMeta{}, nil, false, err
	}
	return meta.meta(), raw, true, nil
}

func onedriveListOp(ctx context.Context, d connectorDeps, token string, args map[string]any) ([]cloudFileMeta, error) {
	var listURL string
	if path := argString(args, "path"); path != "" {
		listURL = fmt.Sprintf("%s/me/drive/root:/%s:/children?$select=id,name,size,lastModifiedDateTime,file&$top=%d",
			graphAPIBase, escapeGraphPath(path), clampPageSize(args, 25))
	} else {
		listURL = fmt.Sprintf("%s/me/drive/root/children?$select=id,name,size,lastModifiedDateTime,file&$top=%d",
			graphAPIBase, clampPageSize(args, 25))
	}
	var out struct {
		Value []graphItem `json:"value"`
	}
	if err := d.doJSON(ctx, "GET", listURL, token, "", nil, &out); err != nil {
		return nil, err
	}
	items := make([]cloudFileMeta, 0, len(out.Value))
	for _, it := range out.Value {
		items = append(items, it.meta())
	}
	return items, nil
}

func onedriveWriteOp(ctx context.Context, d connectorDeps, token string, args map[string]any) (cloudFileMeta, int, error) {
	path := argString(args, "path", "name")
	content := argString(args, "content", "body")
	ct := argString(args, "content_type", "mime_type")
	if ct == "" {
		ct = "text/plain"
	}
	uploadURL := fmt.Sprintf("%s/me/drive/root:/%s:/content", graphAPIBase, escapeGraphPath(path))
	var created graphItem
	if err := d.doJSON(ctx, "PUT", uploadURL, token, ct, []byte(content), &created); err != nil {
		return cloudFileMeta{}, 0, err
	}
	return created.meta(), len(content), nil
}

func newOneDriveWriteHandler(d connectorDeps, ws WorkspaceConfig) Handler {
	return newCloudWriteHandler(d, ws, onedriveOps)
}

// escapeGraphPath escapes each path segment but preserves the slashes Graph uses
// to address nested folders under /me/drive/root:/.
func escapeGraphPath(p string) string {
	p = strings.Trim(p, "/")
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}
	return strings.Join(parts, "/")
}
