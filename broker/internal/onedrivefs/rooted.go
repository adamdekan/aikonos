package onedrivefs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
)

// bfsMaxDepth/bfsMaxEntries bound a recursive ListDir's Graph fan-out — no
// folder-scoped delta query exists yet (see the spec's non-goals), so an
// unbounded BFS over a deeply-nested or very wide OneDrive tree could issue
// unbounded requests. Both caps truncate the listing silently (no error) —
// callers see a partial-but-correct-so-far result, matching the workspacefs
// v1 floor documented in the design.
const (
	bfsMaxDepth   = 5
	bfsMaxEntries = 500
)

// Rooted is a root-scoped view over one OneDrive folder (a (driveID,itemID)
// pair, normally the folder EnsureFolder returns). Every op below mirrors
// workspacefs.Backend's method shape but Rooted does NOT implement Backend
// itself — see the package doc comment.
type Rooted struct {
	store   *Store
	driveID string
	itemID  string
}

// WithRoot scopes this Store to one OneDrive folder.
func (s *Store) WithRoot(driveID, itemID string) *Rooted {
	return &Rooted{store: s, driveID: driveID, itemID: itemID}
}

// itemBase is the id-anchored Graph URL for the root item itself.
func (r *Rooted) itemBase() string {
	return fmt.Sprintf("%s/drives/%s/items/%s", graphBase, pathEscape(r.driveID), pathEscape(r.itemID))
}

// itemPath builds the id-anchored Graph URL for rel with the given verb
// suffix (e.g. "/children", "/content", "/createUploadSession", or "" for a
// bare metadata/PATCH/DELETE target). rel=="" or "." addresses the root item
// itself, per the CP4 contract's op->Graph mapping table.
func (r *Rooted) itemPath(rel, suffix string) string {
	if rel == "" || rel == "." {
		return r.itemBase() + suffix
	}
	if suffix == "" {
		return r.itemBase() + ":/" + escapeGraphPath(rel)
	}
	return r.itemBase() + ":/" + escapeGraphPath(rel) + ":" + suffix
}

type childrenPage struct {
	Value    []graphItem `json:"value"`
	NextLink string      `json:"@odata.nextLink"`
}

// listChildren fetches every page of rel's children (via @odata.nextLink).
// found=false means rel itself does not exist (404) — the caller decides
// what that means for its op (missing dir lists empty).
func (r *Rooted) listChildren(ctx context.Context, token, rel string) (items []graphItem, found bool, err error) {
	listURL := r.itemPath(rel, "/children") + "?$select=name,size,folder,file,lastModifiedDateTime&$top=200"
	for listURL != "" {
		var page childrenPage
		status, err := r.store.getJSON(ctx, listURL, token, &page)
		if err != nil {
			return nil, false, err
		}
		if status == http.StatusNotFound {
			return nil, false, nil
		}
		items = append(items, page.Value...)
		listURL = page.NextLink
	}
	return items, true, nil
}

func childPath(dir string, name string) string {
	if dir == "." {
		return name
	}
	return dir + "/" + name
}

func toFileInfos(dir string, items []graphItem) []workspacefs.FileInfo {
	out := make([]workspacefs.FileInfo, 0, len(items))
	for _, it := range items {
		out = append(out, workspacefs.FileInfo{
			Path:    childPath(dir, it.Name),
			Size:    it.Size,
			ModTime: parseGraphTime(it.LastModifiedDateTime),
			IsDir:   it.Folder != nil,
		})
	}
	return out
}

type bfsNode struct {
	rel   string
	depth int
}

// listRecursive walks the tree breadth-first from root, bounded by
// bfsMaxDepth/bfsMaxEntries (see their doc comment).
func (r *Rooted) listRecursive(ctx context.Context, token, root string) ([]workspacefs.FileInfo, error) {
	var out []workspacefs.FileInfo
	queue := []bfsNode{{rel: root, depth: 0}}
	truncated := false
	for len(queue) > 0 {
		if len(out) >= bfsMaxEntries {
			truncated = true
			break
		}
		node := queue[0]
		queue = queue[1:]
		items, found, err := r.listChildren(ctx, token, node.rel)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		for _, it := range items {
			if len(out) >= bfsMaxEntries {
				truncated = true
				break
			}
			p := childPath(node.rel, it.Name)
			isDir := it.Folder != nil
			out = append(out, workspacefs.FileInfo{
				Path:    p,
				Size:    it.Size,
				ModTime: parseGraphTime(it.LastModifiedDateTime),
				IsDir:   isDir,
			})
			if isDir && node.depth < bfsMaxDepth {
				queue = append(queue, bfsNode{rel: p, depth: node.depth + 1})
			} else if isDir {
				truncated = true
			}
		}
	}
	if truncated {
		r.store.logger().Debug("onedrivefs: recursive listing truncated", "root", root, "depth_cap", bfsMaxDepth, "entry_cap", bfsMaxEntries, "returned", len(out))
	}
	return out, nil
}

// ListDir mirrors workspacefs.Store.ListDir's contract: dir=="" or "."
// means the root scope; a non-existent dir returns an empty slice with a
// nil error (never an error) so callers treat "not there yet" and "empty"
// identically.
func (r *Rooted) ListDir(ctx context.Context, tenant, user, dir string, recursive bool) ([]workspacefs.FileInfo, error) {
	clean, err := cleanDir(dir)
	if err != nil {
		return nil, err
	}
	token, err := r.store.token(ctx, tenant, user)
	if err != nil {
		return nil, err
	}
	if !recursive {
		items, found, err := r.listChildren(ctx, token, clean)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, nil
		}
		return toFileInfos(clean, items), nil
	}
	return r.listRecursive(ctx, token, clean)
}

// metadata fetches rel's DriveItem metadata (file or folder facet), without
// touching content. found=false means a 404 (rel does not exist).
func (r *Rooted) metadata(ctx context.Context, token, rel string) (graphItem, bool, error) {
	metaURL := r.itemPath(rel, "") + "?$select=id,name,size,folder,file,lastModifiedDateTime"
	var item graphItem
	status, err := r.store.getJSON(ctx, metaURL, token, &item)
	if err != nil {
		return graphItem{}, false, err
	}
	if status == http.StatusNotFound {
		return graphItem{}, false, nil
	}
	return item, true, nil
}

// fetchContent streams rel's content through an io.LimitReader — defense in
// depth only; Read's size precheck against metadata always runs first, so
// this cap should never actually trigger in practice.
func (r *Rooted) fetchContent(ctx context.Context, token, rel string) ([]byte, error) {
	contentURL := r.itemPath(rel, "/content")
	resp, err := r.store.doWithRetryFor429(ctx, http.MethodGet, contentURL, token, "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := classifyStatus(resp.StatusCode); err != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fs.ErrNotExist
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("onedrivefs: content status %d: %s", resp.StatusCode, string(b))
	}
	limited := io.LimitReader(resp.Body, workspacefs.MaxFileBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("onedrivefs: read content: %w", err)
	}
	if int64(len(data)) > workspacefs.MaxFileBytes {
		return nil, fmt.Errorf("%w: content exceeds %d bytes", workspacefs.ErrTooLarge, int64(workspacefs.MaxFileBytes))
	}
	return data, nil
}

// Read fetches rel's metadata first — a size over the cap or a folder facet
// is rejected BEFORE any content fetch — then streams the content.
func (r *Rooted) Read(ctx context.Context, tenant, user, rel string) ([]byte, time.Time, error) {
	clean, err := cleanAndValidateRel(rel)
	if err != nil {
		return nil, time.Time{}, err
	}
	token, err := r.store.token(ctx, tenant, user)
	if err != nil {
		return nil, time.Time{}, err
	}
	meta, found, err := r.metadata(ctx, token, clean)
	if err != nil {
		return nil, time.Time{}, err
	}
	if !found {
		return nil, time.Time{}, fs.ErrNotExist
	}
	if meta.Folder != nil {
		return nil, time.Time{}, fmt.Errorf("%w: %q is a directory", workspacefs.ErrInvalidPath, rel)
	}
	if meta.Size > workspacefs.MaxFileBytes {
		return nil, time.Time{}, fmt.Errorf("%w: %d bytes", workspacefs.ErrTooLarge, meta.Size)
	}
	data, err := r.fetchContent(ctx, token, clean)
	if err != nil {
		return nil, time.Time{}, err
	}
	return data, parseGraphTime(meta.LastModifiedDateTime), nil
}

// simplePut uploads data via a single PUT :/content call (<=simpleUploadMax).
func (r *Rooted) simplePut(ctx context.Context, token, rel string, data []byte) (graphItem, error) {
	putURL := r.itemPath(rel, "/content")
	var item graphItem
	status, err := r.store.putJSON(ctx, putURL, token, "application/octet-stream", data, &item)
	if err != nil {
		return graphItem{}, err
	}
	if status == http.StatusNotFound {
		return graphItem{}, fs.ErrNotExist
	}
	return item, nil
}

type uploadSession struct {
	UploadURL string `json:"uploadUrl"`
}

// sessionUpload uploads data over Graph's resumable upload-session protocol:
// POST createUploadSession, then chunked PUTs to the returned uploadUrl with
// Content-Range set per chunk (uploadChunkSize — a 320 KiB multiple in
// production; see its doc comment for the test-override rationale). The
// session's uploadUrl is pre-signed by Graph — no Authorization header is
// sent on the chunk PUTs, matching Graph's documented behavior.
func (r *Rooted) sessionUpload(ctx context.Context, token, rel string, data []byte) (graphItem, error) {
	createURL := r.itemPath(rel, "/createUploadSession")
	body, _ := json.Marshal(map[string]any{
		"item": map[string]any{"@microsoft.graph.conflictBehavior": "replace"},
	})
	var session uploadSession
	if _, err := r.store.postJSON(ctx, createURL, token, body, &session); err != nil {
		return graphItem{}, err
	}
	if session.UploadURL == "" {
		return graphItem{}, fmt.Errorf("onedrivefs: createUploadSession returned no uploadUrl")
	}

	total := int64(len(data))
	var final graphItem
	for offset := int64(0); offset < total; offset += uploadChunkSize {
		end := offset + uploadChunkSize
		if end > total {
			end = total
		}
		chunk := data[offset:end]
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, session.UploadURL, bytes.NewReader(chunk))
		if err != nil {
			return graphItem{}, fmt.Errorf("onedrivefs: build chunk request: %w", err)
		}
		req.Header.Set("Content-Length", strconv.FormatInt(int64(len(chunk)), 10))
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, end-1, total))
		resp, err := r.store.httpClient().Do(req)
		if err != nil {
			return graphItem{}, fmt.Errorf("onedrivefs: %w: %v", workspacefs.ErrUnavailable, err)
		}
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if cerr := classifyStatus(resp.StatusCode); cerr != nil {
				return graphItem{}, cerr
			}
			return graphItem{}, fmt.Errorf("onedrivefs: chunk upload status %d: %s", resp.StatusCode, string(b))
		}
		if end == total {
			if err := json.NewDecoder(resp.Body).Decode(&final); err != nil {
				resp.Body.Close()
				return graphItem{}, fmt.Errorf("onedrivefs: decode final chunk response: %w", err)
			}
		}
		resp.Body.Close()
	}
	return final, nil
}

// Write enforces the size cap (ErrTooLarge, checked BEFORE any HTTP call),
// then uploads via simplePut or the chunked session path depending on size.
func (r *Rooted) Write(ctx context.Context, tenant, user, rel string, data []byte) (workspacefs.FileInfo, error) {
	clean, err := cleanAndValidateRel(rel)
	if err != nil {
		return workspacefs.FileInfo{}, err
	}
	if int64(len(data)) > workspacefs.MaxFileBytes {
		return workspacefs.FileInfo{}, fmt.Errorf("%w: %d bytes", workspacefs.ErrTooLarge, int64(len(data)))
	}
	token, err := r.store.token(ctx, tenant, user)
	if err != nil {
		return workspacefs.FileInfo{}, err
	}
	var item graphItem
	if int64(len(data)) <= simpleUploadMax {
		item, err = r.simplePut(ctx, token, clean, data)
	} else {
		item, err = r.sessionUpload(ctx, token, clean, data)
	}
	if err != nil {
		return workspacefs.FileInfo{}, err
	}
	modTime := parseGraphTime(item.LastModifiedDateTime)
	if modTime.IsZero() {
		modTime = time.Now().UTC()
	}
	return workspacefs.FileInfo{Path: clean, Size: int64(len(data)), ModTime: modTime}, nil
}

// Delete checks metadata first (folder.childCount > 0 -> ErrNotEmpty; Graph's
// own DELETE recurses on a non-empty folder, which this pre-check must NOT
// inherit), then issues the DELETE.
func (r *Rooted) Delete(ctx context.Context, tenant, user, rel string) error {
	clean, err := cleanAndValidateRel(rel)
	if err != nil {
		return err
	}
	token, err := r.store.token(ctx, tenant, user)
	if err != nil {
		return err
	}
	meta, found, err := r.metadata(ctx, token, clean)
	if err != nil {
		return err
	}
	if !found {
		return fs.ErrNotExist
	}
	if meta.Folder != nil && meta.Folder.ChildCount > 0 {
		return fmt.Errorf("%w: %q is not empty", workspacefs.ErrNotEmpty, rel)
	}
	status, err := r.store.deleteReq(ctx, r.itemPath(clean, ""), token)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return fs.ErrNotExist
	}
	return nil
}

// resolveParentID returns dir's item id (r.itemID for the root itself).
func (r *Rooted) resolveParentID(ctx context.Context, token, dir string) (string, error) {
	if dir == "." || dir == "" {
		return r.itemID, nil
	}
	meta, found, err := r.metadata(ctx, token, dir)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fs.ErrNotExist
	}
	return meta.ID, nil
}

// Move resolves the source (its absence -> fs.ErrNotExist), resolves the
// destination's parent, then PATCHes name/parentReference with
// conflictBehavior:fail — a destination collision surfaces as ErrExists.
func (r *Rooted) Move(ctx context.Context, tenant, user, from, to string) (workspacefs.FileInfo, error) {
	fromClean, err := cleanAndValidateRel(from)
	if err != nil {
		return workspacefs.FileInfo{}, err
	}
	toClean, err := cleanAndValidateRel(to)
	if err != nil {
		return workspacefs.FileInfo{}, err
	}
	token, err := r.store.token(ctx, tenant, user)
	if err != nil {
		return workspacefs.FileInfo{}, err
	}

	_, found, err := r.metadata(ctx, token, fromClean)
	if err != nil {
		return workspacefs.FileInfo{}, err
	}
	if !found {
		return workspacefs.FileInfo{}, fs.ErrNotExist
	}

	toDir := filepath.ToSlash(filepath.Dir(toClean))
	toName := filepath.Base(toClean)
	parentID, err := r.resolveParentID(ctx, token, toDir)
	if err != nil {
		return workspacefs.FileInfo{}, err
	}

	patchURL := r.itemPath(fromClean, "")
	body, _ := json.Marshal(map[string]any{
		"name":                              toName,
		"parentReference":                   map[string]any{"id": parentID},
		"@microsoft.graph.conflictBehavior": "fail",
	})
	var updated graphItem
	status, err := r.store.patchJSON(ctx, patchURL, token, body, &updated)
	if err != nil {
		return workspacefs.FileInfo{}, err
	}
	if status == http.StatusConflict {
		return workspacefs.FileInfo{}, fmt.Errorf("%w: %q", workspacefs.ErrExists, to)
	}
	if status == http.StatusNotFound {
		return workspacefs.FileInfo{}, fs.ErrNotExist
	}
	return workspacefs.FileInfo{
		Path:    toClean,
		Size:    updated.Size,
		ModTime: parseGraphTime(updated.LastModifiedDateTime),
		IsDir:   updated.Folder != nil,
	}, nil
}

// Mkdir creates a (possibly nested) directory: intermediate segments
// tolerate a 409 (resolve the existing id, continue) but the LEAF segment's
// 409 is a real conflict — ErrExists, no clobber.
func (r *Rooted) Mkdir(ctx context.Context, tenant, user, rel string) error {
	clean, err := cleanAndValidateRel(rel)
	if err != nil {
		return err
	}
	token, err := r.store.token(ctx, tenant, user)
	if err != nil {
		return err
	}

	segs := strings.Split(clean, "/")
	parentID := r.itemID
	for i, seg := range segs {
		if i == len(segs)-1 {
			createURL := fmt.Sprintf("%s/drives/%s/items/%s/children", graphBase, pathEscape(r.driveID), pathEscape(parentID))
			body, _ := json.Marshal(map[string]any{
				"name":                              seg,
				"folder":                            map[string]any{},
				"@microsoft.graph.conflictBehavior": "fail",
			})
			status, err := r.store.postJSON(ctx, createURL, token, body, new(graphItem))
			if err != nil {
				return err
			}
			if status == http.StatusConflict {
				return fmt.Errorf("%w: %q", workspacefs.ErrExists, rel)
			}
			return nil
		}
		nextID, err := r.store.ensureChildFolder(ctx, token, r.driveID, parentID, seg)
		if err != nil {
			return err
		}
		parentID = nextID
	}
	return nil
}
