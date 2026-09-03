// broker/internal/toolproxy/cloudfile.go
//
// Shared read/list/write template for the cloud-storage connector tools
// (gdrive.*, onedrive.*). Both providers expose the same shape of operation —
// resolve a single file, list files, or write a file — over different wire
// protocols. This file normalizes their output into one key schema so the
// consuming LLM never has to know which provider produced a result; gdrive.go
// and onedrive.go supply the provider-specific HTTP/API logic via cloudFileOps.
package toolproxy

import (
	"context"
	"fmt"
	"mime"
	"path/filepath"

	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
)

// contentTypeForPath resolves a binary-safe content type for a staged
// (from_path) upload from its extension, falling back to the generic binary
// type rather than assuming text/plain.
func contentTypeForPath(path string) string {
	if ct := mime.TypeByExtension(filepath.Ext(path)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// cloudFileMeta is the provider-normalized subset of a file/item's metadata.
type cloudFileMeta struct {
	FileID       string
	Name         string
	MimeType     string
	ModifiedTime string
	WebURL       string
}

func (m cloudFileMeta) readMap(content string, contentLength int) map[string]any {
	return map[string]any{
		"file_id":        m.FileID,
		"name":           m.Name,
		"mime_type":      m.MimeType,
		"modified_time":  m.ModifiedTime,
		"content":        content,
		"content_length": contentLength,
	}
}

func (m cloudFileMeta) listMap() map[string]any {
	return map[string]any{
		"file_id":       m.FileID,
		"name":          m.Name,
		"mime_type":     m.MimeType,
		"modified_time": m.ModifiedTime,
	}
}

// stagedReadMap is the save_to result shape: metadata only, deliberately no
// "content"/"content_length" — echoing the bytes into the model defeats the
// point of staging them straight to the workspace instead.
func (m cloudFileMeta) stagedReadMap(path string, bytesWritten int) map[string]any {
	return map[string]any{
		"file_id":       m.FileID,
		"name":          m.Name,
		"mime_type":     m.MimeType,
		"modified_time": m.ModifiedTime,
		"path":          path,
		"bytes_written": bytesWritten,
	}
}

func (m cloudFileMeta) writeMap(bytesWritten int) map[string]any {
	return map[string]any{
		"file_id":       m.FileID,
		"name":          m.Name,
		"web_url":       m.WebURL,
		"bytes_written": bytesWritten,
	}
}

// cloudFileOps is the provider-specific logic the shared handlers delegate to.
type cloudFileOps struct {
	provider connector.Provider
	errTag   string // "gdrive" / "onedrive" — preserved in wrapped error messages

	// readOne resolves req.Args to a single file (by id/path). ok=false (with a
	// nil error) means no file selector was given and the caller should list
	// instead. Dispatch rule (both providers, same rule): a selector that
	// resolves to a folder also returns ok=false — the caller falls through
	// to list, scoped to that folder's children, so a path/id can address a
	// folder listing. gdriveReadOne detects this via mimeType ==
	// "application/vnd.google-apps.folder"; onedriveReadOne via a non-nil
	// DriveItem "folder" facet.
	//
	// maxBytes is 0 for a normal content-echo read (silent 5 MiB clip via
	// fetchContent) and workspacefs.MaxFileBytes when save_to is set (loud
	// overflow instead of truncation) — its non-zero-ness doubles as "this is
	// a staged read", which is what lets gdriveReadOne refuse to stage a
	// native Google file's lossy text/plain export under a binary filename.
	readOne func(ctx context.Context, d connectorDeps, token string, args map[string]any, maxBytes int64) (meta cloudFileMeta, content []byte, ok bool, err error)

	// list returns the normalized metadata for every matching file.
	list func(ctx context.Context, d connectorDeps, token string, args map[string]any) ([]cloudFileMeta, error)

	// validateWrite checks required write args before the token fetch, and
	// must return the exact provider error (already carrying the "<tag>.write:"
	// prefix) on failure so error text stays byte-identical to before the split.
	validateWrite func(args map[string]any) error

	// write performs the upload and returns normalized metadata + bytes written.
	write func(ctx context.Context, d connectorDeps, token string, args map[string]any) (meta cloudFileMeta, bytesWritten int, err error)
}

func newCloudReadHandler(d connectorDeps, ws WorkspaceConfig, ops cloudFileOps) Handler {
	return func(ctx context.Context, req Request) (map[string]any, int64, error) {
		tenant, user, err := connectorIdentity(req)
		if err != nil {
			return nil, 0, err
		}
		token, err := d.freshAccessToken(ctx, ops.provider, tenant, user)
		if err != nil {
			return nil, 0, err
		}

		saveTo := argString(req.Args, "save_to")
		var maxBytes int64
		if saveTo != "" {
			maxBytes = workspacefs.MaxFileBytes
		}

		meta, raw, ok, err := ops.readOne(ctx, d, token, req.Args, maxBytes)
		if err != nil {
			return nil, 0, fmt.Errorf("%s.read: %w", ops.errTag, err)
		}
		if ok {
			if saveTo != "" {
				if err := officeWriteOutput(ctx, ws, req, saveTo, ops.errTag+".read", raw); err != nil {
					return nil, 0, err
				}
				cost := int64(len(raw)/1024) + 1
				return meta.stagedReadMap(saveTo, len(raw)), cost, nil
			}
			content := decodeToUTF8(raw)
			full := len(content)
			content = truncateUTF8(content, maxContentChars)
			cost := int64(full/1024) + 1
			return meta.readMap(content, full), cost, nil
		}

		files, err := ops.list(ctx, d, token, req.Args)
		if err != nil {
			return nil, 0, fmt.Errorf("%s.read: %w", ops.errTag, err)
		}
		out := make([]any, 0, len(files))
		for _, f := range files {
			out = append(out, f.listMap())
		}
		return map[string]any{"files": out, "count": len(out)}, 1, nil
	}
}

func newCloudWriteHandler(d connectorDeps, ws WorkspaceConfig, ops cloudFileOps) Handler {
	return func(ctx context.Context, req Request) (map[string]any, int64, error) {
		tenant, user, err := connectorIdentity(req)
		if err != nil {
			return nil, 0, err
		}

		fromPath := argString(req.Args, "from_path")
		hasContent := argString(req.Args, "content", "body") != ""
		if fromPath != "" && hasContent {
			return nil, 0, fmt.Errorf("%s.write: %q and %q are mutually exclusive", ops.errTag, "content", "from_path")
		}

		args := req.Args
		if fromPath != "" {
			data, err := officeReadInput(ctx, ws, req, fromPath, ops.errTag+".write")
			if err != nil {
				return nil, 0, err
			}
			args = cloudWriteArgsFromStaged(req.Args, data, fromPath)
		}

		if err := ops.validateWrite(args); err != nil {
			return nil, 0, err
		}
		token, err := d.freshAccessToken(ctx, ops.provider, tenant, user)
		if err != nil {
			return nil, 0, err
		}
		meta, n, err := ops.write(ctx, d, token, args)
		if err != nil {
			return nil, 0, fmt.Errorf("%s.write: %w", ops.errTag, err)
		}
		cost := int64(n/1024) + 1
		return meta.writeMap(n), cost, nil
	}
}

// cloudWriteArgsFromStaged returns a copy of args with "content" set to the
// staged file's bytes (string/[]byte round-trip in Go preserves every byte,
// so this stays binary-safe) and a binary-safe content type filled in under
// every key either provider's write op reads, unless the caller already gave
// one explicitly — never assume text/plain for a staged upload.
func cloudWriteArgsFromStaged(args map[string]any, data []byte, path string) map[string]any {
	out := make(map[string]any, len(args)+3)
	for k, v := range args {
		out[k] = v
	}
	out["content"] = string(data)
	delete(out, "body")
	if argString(out, "mime_type", "mimeType", "content_type") == "" {
		ct := contentTypeForPath(path)
		out["mime_type"] = ct
		out["mimeType"] = ct
		out["content_type"] = ct
	}
	return out
}
