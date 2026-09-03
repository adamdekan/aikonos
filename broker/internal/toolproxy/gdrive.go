// broker/internal/toolproxy/gdrive.go
//
// gdrive.read (read_only) and gdrive.write (write_external) — governed access to
// the calling user's Google Drive via Drive v3. The OAuth token is fetched
// just-in-time from Vault (see connector_token.go); Aikonos's own capability
// token already gated the call before it reached here.
package toolproxy

import (
	"context"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"net/url"
	"strings"

	"bytes"
	"encoding/json"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"

	"github.com/adamdekan/aikonos/broker/internal/connector"
)

// Overridable in tests; production points at the real Google endpoints.
var (
	googleDriveAPIBase    = "https://www.googleapis.com/drive/v3"
	googleDriveUploadBase = "https://www.googleapis.com/upload/drive/v3"
)

type gdriveFile struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MimeType     string `json:"mimeType"`
	Size         string `json:"size,omitempty"`
	ModifiedTime string `json:"modifiedTime,omitempty"`
	WebViewLink  string `json:"webViewLink,omitempty"`
}

func (f gdriveFile) meta() cloudFileMeta {
	return cloudFileMeta{FileID: f.ID, Name: f.Name, MimeType: f.MimeType, ModifiedTime: f.ModifiedTime, WebURL: f.WebViewLink}
}

var gdriveOps = cloudFileOps{
	provider: connector.ProviderGoogleDrive,
	errTag:   "gdrive",
	readOne:  gdriveReadOne,
	list:     gdriveListOp,
	validateWrite: func(args map[string]any) error {
		if argString(args, "name", "filename") == "" {
			return fmt.Errorf("gdrive.write: missing required arg %q", "name")
		}
		return nil
	},
	write: gdriveWriteOp,
}

// connectorAvailable reports whether cfg carries the Vault + OAuth-app deps
// the four cloud-storage connector handlers need. Shared by gdrive/onedrive
// plugins so the "wired or not" gate lives in exactly one place.
func connectorAvailable(cfg Config) bool { return cfg.Secrets != nil && cfg.OAuth != nil }

func connectorDepsFrom(cfg Config) connectorDeps {
	return connectorDeps{secrets: cfg.Secrets, oauth: cfg.OAuth, obo: cfg.OBO}
}

// gdriveReadPlugin self-registers gdrive.read, gated on connector wiring.
type gdriveReadPlugin struct{}

func (gdriveReadPlugin) ToolID() string            { return "gdrive.read" }
func (gdriveReadPlugin) Available(cfg Config) bool { return connectorAvailable(cfg) }
func (gdriveReadPlugin) Build(cfg Config) Handler {
	return newGDriveReadHandler(connectorDepsFrom(cfg), cfg.withSeam())
}
func (gdriveReadPlugin) Scope() string                   { return "gdrive:read" }
func (gdriveReadPlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_READ_ONLY }

// gdriveWritePlugin self-registers gdrive.write, gated on connector wiring.
type gdriveWritePlugin struct{}

func (gdriveWritePlugin) ToolID() string            { return "gdrive.write" }
func (gdriveWritePlugin) Available(cfg Config) bool { return connectorAvailable(cfg) }
func (gdriveWritePlugin) Build(cfg Config) Handler {
	return newGDriveWriteHandler(connectorDepsFrom(cfg), cfg.withSeam())
}
func (gdriveWritePlugin) Scope() string { return "gdrive:write" }
func (gdriveWritePlugin) EffectClass() planv1.EffectClass {
	return planv1.EffectClass_WRITE_EXTERNAL
}

func init() {
	RegisterPlugin(gdriveReadPlugin{})
	RegisterPlugin(gdriveWritePlugin{})
}

func newGDriveReadHandler(d connectorDeps, ws WorkspaceConfig) Handler {
	return newCloudReadHandler(d, ws, gdriveOps)
}

func gdriveReadOne(ctx context.Context, d connectorDeps, token string, args map[string]any, maxBytes int64) (cloudFileMeta, []byte, bool, error) {
	fileID := argString(args, "file_id", "fileId")
	if fileID == "" {
		return cloudFileMeta{}, nil, false, nil
	}

	metaURL := fmt.Sprintf("%s/files/%s?fields=id,name,mimeType,size,modifiedTime,webViewLink",
		googleDriveAPIBase, url.PathEscape(fileID))
	var meta gdriveFile
	if err := d.doJSON(ctx, "GET", metaURL, token, "", nil, &meta); err != nil {
		return cloudFileMeta{}, nil, false, err
	}
	if meta.MimeType == "application/vnd.google-apps.folder" {
		// file_id addresses a folder: fall through to list, scoped to this
		// folder's children (see the dispatch rule documented in cloudfile.go).
		return cloudFileMeta{}, nil, false, nil
	}

	native := strings.HasPrefix(meta.MimeType, "application/vnd.google-apps")
	if native && maxBytes > 0 {
		// maxBytes>0 means save_to was set (staging requested). Staging a native
		// Google file would silently write the lossy text/plain export under a
		// binary filename — refuse instead.
		return cloudFileMeta{}, nil, false, fmt.Errorf(
			"native Google file %q (%s) cannot be staged via save_to this pass — Docs/Sheets/Slides export is deferred, staging covers uploaded binary files only",
			meta.Name, meta.MimeType)
	}

	var contentURL string
	if native {
		// Native Google docs must be exported; plain text is the lossy-but-safe pick.
		contentURL = fmt.Sprintf("%s/files/%s/export?mimeType=text/plain", googleDriveAPIBase, url.PathEscape(fileID))
	} else {
		contentURL = fmt.Sprintf("%s/files/%s?alt=media", googleDriveAPIBase, url.PathEscape(fileID))
	}
	raw, err := d.fetchContent(ctx, contentURL, token, maxBytes)
	if err != nil {
		return cloudFileMeta{}, nil, false, err
	}
	return meta.meta(), raw, true, nil
}

func gdriveListOp(ctx context.Context, d connectorDeps, token string, args map[string]any) ([]cloudFileMeta, error) {
	q := url.Values{}
	q.Set("fields", "files(id,name,mimeType,modifiedTime)")
	q.Set("pageSize", fmt.Sprintf("%d", clampPageSize(args, 25)))
	if query := argString(args, "query", "q"); query != "" {
		q.Set("q", query)
	} else if folderID := argString(args, "file_id", "fileId"); folderID != "" {
		// Same folder selector readOne used to detect the folder — scope the
		// listing to its direct children.
		q.Set("q", fmt.Sprintf("%q in parents", folderID))
	}
	listURL := googleDriveAPIBase + "/files?" + q.Encode()
	var out struct {
		Files []gdriveFile `json:"files"`
	}
	if err := d.doJSON(ctx, "GET", listURL, token, "", nil, &out); err != nil {
		return nil, err
	}
	files := make([]cloudFileMeta, 0, len(out.Files))
	for _, f := range out.Files {
		files = append(files, f.meta())
	}
	return files, nil
}

func gdriveWriteOp(ctx context.Context, d connectorDeps, token string, args map[string]any) (cloudFileMeta, int, error) {
	name := argString(args, "name", "filename")
	content := argString(args, "content", "body")
	mimeType := argString(args, "mime_type", "mimeType")
	if mimeType == "" {
		mimeType = "text/plain"
	}

	meta := map[string]any{"name": name}
	if folder := argString(args, "folder_id", "folderId"); folder != "" {
		meta["parents"] = []string{folder}
	}
	body, contentType, err := buildMultipartUpload(meta, mimeType, []byte(content))
	if err != nil {
		return cloudFileMeta{}, 0, err
	}
	uploadURL := googleDriveUploadBase + "/files?uploadType=multipart&fields=id,name,webViewLink"
	var created gdriveFile
	if err := d.doJSON(ctx, "POST", uploadURL, token, contentType, body, &created); err != nil {
		return cloudFileMeta{}, 0, err
	}
	return created.meta(), len(content), nil
}

func newGDriveWriteHandler(d connectorDeps, ws WorkspaceConfig) Handler {
	return newCloudWriteHandler(d, ws, gdriveOps)
}

// buildMultipartUpload assembles a Drive multipart/related body: a JSON metadata
// part followed by the media part.
func buildMultipartUpload(meta map[string]any, mediaType string, media []byte) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, "", err
	}
	metaHdr := textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}}
	mp, err := mw.CreatePart(metaHdr)
	if err != nil {
		return nil, "", err
	}
	if _, err := mp.Write(metaJSON); err != nil {
		return nil, "", err
	}
	mediaHdr := textproto.MIMEHeader{"Content-Type": {mediaType}}
	dp, err := mw.CreatePart(mediaHdr)
	if err != nil {
		return nil, "", err
	}
	if _, err := dp.Write(media); err != nil {
		return nil, "", err
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	// Drive requires multipart/related, not multipart/form-data.
	contentType := "multipart/related; boundary=" + mw.Boundary()
	return buf.Bytes(), contentType, nil
}

func clampPageSize(args map[string]any, def int) int {
	n := def
	switch v := args["max_results"].(type) {
	case float64:
		n = int(v)
	case int:
		n = v
	}
	if n <= 0 || n > 100 {
		return def
	}
	return n
}
