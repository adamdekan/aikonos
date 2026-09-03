// Package onedrivefs is a Microsoft Graph-backed store mirroring
// broker/internal/workspacefs.Store's operation shapes (ListDir/Read/Write/
// Delete/Move/Mkdir, all ctx-first) so CP5's (tenant,user)->prefs->WithRoot->op
// adapter is a thin dispatch, not a rewrite. It deliberately does NOT
// implement workspacefs.Backend itself (no Enabled/List) — wiring this store
// into that seam is CP5's job.
//
// OneDrive/SharePoint item names are case-INsensitive server-side (Graph
// treats "Report.md" and "report.md" as the same item); this package does
// NOT emulate that locally. A caller creating both sees the second create
// surface as an ordinary 409-derived ErrExists from Graph itself, not from
// any local case-folding check here — see 's
// Risks section.
package onedrivefs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
)

// graphBase is overridable in tests (mirrors toolproxy's graphAPIBase
// pattern); production points at Microsoft Graph.
var graphBase = "https://graph.microsoft.com/v1.0"

// simpleUploadMax is the largest payload written via a single PUT :/content
// call; anything bigger uses the chunked upload-session path. Graph's
// historical simple-PUT limit is 4 MiB. Overridable in tests so the
// session-upload path can be exercised without a multi-megabyte payload.
var simpleUploadMax = int64(4 << 20)

// uploadChunkSize is the per-PUT chunk size for the upload-session path.
// Graph requires chunk sizes to be a multiple of 320 KiB (the last chunk may
// be shorter). Overridable in tests for a fast, small-payload exercise of the
// chunking mechanism; TestUploadChunkSize_DefaultIsMultipleOf320KiB pins the
// production default's shape.
var uploadChunkSize = int64(320 * 1024 * 3) // ~960 KiB

// TokenSource resolves a fresh Graph access token for (tenant, user) — the
// production implementation (CP5 main.go) wraps the OBO broker
// (broker/internal/broker/m365_obo.go) and maps its
// connector.ErrOBOReconnectRequired into workspacefs.ErrReconnectRequired;
// that mapping is intentionally NOT done here (see the CP4 contract) so this
// package never imports broker/internal/connector.
type TokenSource interface {
	FreshAccessToken(ctx context.Context, tenant, user string) (string, error)
}

// Store is the OneDrive Graph client. Zero-value HTTP/Logger degrade to
// http.DefaultClient / slog.Default(); Tokens must be set.
type Store struct {
	Tokens TokenSource
	HTTP   *http.Client
	Logger *slog.Logger
}

func (s *Store) httpClient() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return http.DefaultClient
}

func (s *Store) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// workspaceSentinels lists every workspacefs sentinel a TokenSource error
// might already wrap — see token()'s doc comment.
var workspaceSentinels = []error{
	workspacefs.ErrInvalidPath,
	workspacefs.ErrTooLarge,
	workspacefs.ErrExists,
	workspacefs.ErrNotEmpty,
	workspacefs.ErrUnavailable,
	workspacefs.ErrReconnectRequired,
	workspacefs.ErrForbidden,
}

func wrapsWorkspaceSentinel(err error) bool {
	for _, sentinel := range workspaceSentinels {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// token resolves a fresh access token. A TokenSource error that already
// wraps a workspacefs sentinel (e.g. the production adapter's
// ErrReconnectRequired mapping) propagates unchanged; anything else is
// wrapped in ErrUnavailable rather than surfacing an opaque transport error.
func (s *Store) token(ctx context.Context, tenant, user string) (string, error) {
	if s.Tokens == nil {
		return "", fmt.Errorf("onedrivefs: no TokenSource configured: %w", workspacefs.ErrUnavailable)
	}
	tok, err := s.Tokens.FreshAccessToken(ctx, tenant, user)
	if err != nil {
		if wrapsWorkspaceSentinel(err) {
			return "", err
		}
		return "", fmt.Errorf("onedrivefs: resolve token: %w: %v", workspacefs.ErrUnavailable, err)
	}
	return tok, nil
}

// do issues one HTTP request. A transport-level failure (DNS, connection
// refused, timeout) is wrapped in ErrUnavailable; the caller inspects
// resp.StatusCode for everything else.
func (s *Store) do(ctx context.Context, method, rawURL, token, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, fmt.Errorf("onedrivefs: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("onedrivefs: %w: %v", workspacefs.ErrUnavailable, err)
	}
	return resp, nil
}

// parseRetryAfter parses a Retry-After header as an integer seconds count
// (the form Graph sends); an unparseable or negative value reports ok=false.
func parseRetryAfter(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

// doWithRetryFor429 honors a 429's Retry-After exactly once, and only for an
// idempotent GET with a wait of 5s or less — every other case (non-GET, no
// Retry-After, too long a wait, or a second 429) surfaces ErrUnavailable
// rather than retrying further.
func (s *Store) doWithRetryFor429(ctx context.Context, method, rawURL, token, contentType string, body io.Reader) (*http.Response, error) {
	resp, err := s.do(ctx, method, rawURL, token, contentType, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		return resp, nil
	}
	retryAfter, ok := parseRetryAfter(resp.Header.Get("Retry-After"))
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if method != http.MethodGet || !ok || retryAfter > 5*time.Second {
		return nil, workspacefs.ErrUnavailable
	}
	select {
	case <-time.After(retryAfter):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// GET requests never carry a body, so retrying with nil is exact — see
	// the doc comment above (idempotent GET only).
	resp2, err := s.do(ctx, method, rawURL, token, contentType, nil)
	if err != nil {
		return nil, err
	}
	if resp2.StatusCode == http.StatusTooManyRequests {
		_, _ = io.Copy(io.Discard, resp2.Body)
		resp2.Body.Close()
		return nil, workspacefs.ErrUnavailable
	}
	return resp2, nil
}

// classifyStatus maps a status shared across every op the same way (401/403/
// 429/5xx); 404 and 409 are op-dependent and left for the caller to interpret
// from the returned status code. 429 is included here (not just in
// doWithRetryFor429) so a caller that issues its own raw HTTP request outside
// that helper — sessionUpload's chunk PUTs, which cannot retry a non-
// idempotent write the way doWithRetryFor429 retries a GET — still maps a
// mid-upload 429 to the same ErrUnavailable sentinel rather than a generic,
// non-sentinel error. Every path that already goes through
// doWithRetryFor429 never reaches this function with a 429 status at all (it
// resolves 429 itself first), so this addition changes no existing behavior.
func classifyStatus(status int) error {
	switch {
	case status == http.StatusUnauthorized:
		return workspacefs.ErrReconnectRequired
	case status == http.StatusForbidden:
		return workspacefs.ErrForbidden
	case status == http.StatusTooManyRequests, status >= 500:
		return workspacefs.ErrUnavailable
	default:
		return nil
	}
}

// sendJSON issues method/url with an optional JSON body, decodes a JSON
// response into out (nil skips decoding), and returns the raw status code
// alongside a nil error for a 404/409 so op-specific callers can interpret
// those two codes themselves; every other 401/403/429/5xx is turned into the
// matching sentinel via classifyStatus/doWithRetryFor429, and any other
// non-2xx becomes a plain wrapped error.
func (s *Store) sendJSON(ctx context.Context, method, rawURL, token, contentType string, body []byte, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	resp, err := s.doWithRetryFor429(ctx, method, rawURL, token, contentType, reader)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if err := classifyStatus(resp.StatusCode); err != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, err
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusConflict {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, fmt.Errorf("onedrivefs: unexpected status %d: %s", resp.StatusCode, string(b))
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("onedrivefs: decode response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

func (s *Store) getJSON(ctx context.Context, rawURL, token string, out any) (int, error) {
	return s.sendJSON(ctx, http.MethodGet, rawURL, token, "", nil, out)
}

func (s *Store) postJSON(ctx context.Context, rawURL, token string, body []byte, out any) (int, error) {
	return s.sendJSON(ctx, http.MethodPost, rawURL, token, "application/json", body, out)
}

func (s *Store) putJSON(ctx context.Context, rawURL, token, contentType string, body []byte, out any) (int, error) {
	return s.sendJSON(ctx, http.MethodPut, rawURL, token, contentType, body, out)
}

func (s *Store) patchJSON(ctx context.Context, rawURL, token string, body []byte, out any) (int, error) {
	return s.sendJSON(ctx, http.MethodPatch, rawURL, token, "application/json", body, out)
}

func (s *Store) deleteReq(ctx context.Context, rawURL, token string) (int, error) {
	return s.sendJSON(ctx, http.MethodDelete, rawURL, token, "", nil, nil)
}

// graphItem is the subset of a Graph DriveItem this package cares about.
// Folder is present (non-nil) iff the item is a directory — the "folder"
// facet — and its ChildCount backs Delete's non-empty-directory pre-check.
type graphItem struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	Size                 int64  `json:"size"`
	LastModifiedDateTime string `json:"lastModifiedDateTime"`
	Folder               *struct {
		ChildCount int `json:"childCount"`
	} `json:"folder,omitempty"`
}

func parseGraphTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// escapeGraphPath escapes each "/"-separated segment but preserves the
// slashes Graph uses to address nested items in id-anchored/path-anchored
// URLs (mirrors toolproxy/onedrive.go's helper of the same name/shape).
func escapeGraphPath(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}
	return strings.Join(parts, "/")
}

func pathEscape(s string) string { return url.PathEscape(s) }

// forbiddenNameChars are rejected in any path segment before any HTTP call —
// see validateSegment.
const forbiddenNameChars = "\"*:<>?/\\|"

// reservedDeviceNames are Windows/OneDrive-reserved names, checked
// case-insensitively against a segment's base name (the part before its
// first '.', so "CON.txt" is reserved too, matching Windows semantics).
var reservedDeviceNames = buildReservedDeviceNames()

func buildReservedDeviceNames() map[string]bool {
	m := map[string]bool{"con": true, "prn": true, "aux": true, "nul": true}
	for i := 1; i <= 9; i++ {
		m[fmt.Sprintf("com%d", i)] = true
		m[fmt.Sprintf("lpt%d", i)] = true
	}
	return m
}

// validateSegment checks one cleaned path segment against OneDrive's naming
// rules, before any HTTP call is made.
func validateSegment(seg string) error {
	if seg == "" {
		return fmt.Errorf("%w: empty path segment", workspacefs.ErrInvalidPath)
	}
	if strings.TrimSpace(seg) != seg {
		return fmt.Errorf("%w: %q has leading/trailing whitespace", workspacefs.ErrInvalidPath, seg)
	}
	if strings.HasSuffix(seg, ".") {
		return fmt.Errorf("%w: %q has a trailing dot", workspacefs.ErrInvalidPath, seg)
	}
	if strings.ContainsAny(seg, forbiddenNameChars) {
		return fmt.Errorf("%w: %q contains a forbidden character", workspacefs.ErrInvalidPath, seg)
	}
	base := seg
	if i := strings.IndexByte(seg, '.'); i >= 0 {
		base = seg[:i]
	}
	if reservedDeviceNames[strings.ToLower(base)] {
		return fmt.Errorf("%w: %q is a reserved device name", workspacefs.ErrInvalidPath, seg)
	}
	return nil
}

// validateRel validates every segment of an already-CleanRel'd relative
// path. "." (the root itself) has no segment to validate.
func validateRel(clean string) error {
	if clean == "." {
		return nil
	}
	for _, seg := range strings.Split(clean, "/") {
		if err := validateSegment(seg); err != nil {
			return err
		}
	}
	return nil
}

// cleanAndValidateRel cleans rel (workspacefs.CleanRel — traversal/absolute
// rejection) and then validates its segments against OneDrive naming rules.
// Both checks run before any HTTP call.
func cleanAndValidateRel(rel string) (string, error) {
	clean, err := workspacefs.CleanRel(rel)
	if err != nil {
		return "", err
	}
	if err := validateRel(clean); err != nil {
		return "", err
	}
	return clean, nil
}

// cleanDir is cleanAndValidateRel's ListDir-flavored twin: "" and "."
// both denote the root scope itself (no CleanRel call, since CleanRel
// rejects an empty string but ListDir's dir="" means "the root").
func cleanDir(dir string) (string, error) {
	if dir == "" || dir == "." {
		return ".", nil
	}
	return cleanAndValidateRel(dir)
}

// EnsureFolder resolves the caller's drive and creates path segment-by-
// segment under its root (POST children with conflictBehavior:fail,
// tolerating a 409 on ANY segment by resolving the existing child's id and
// continuing) — idempotent: a second call for the same path returns the
// same (driveID, itemID) with no error.
func (s *Store) EnsureFolder(ctx context.Context, tenant, user, path string) (driveID, itemID string, err error) {
	clean, err := cleanAndValidateRel(path)
	if err != nil {
		return "", "", err
	}
	token, err := s.token(ctx, tenant, user)
	if err != nil {
		return "", "", err
	}
	driveID, err = s.resolveDriveID(ctx, token)
	if err != nil {
		return "", "", err
	}

	itemID = "root"
	if clean == "." {
		return driveID, itemID, nil
	}
	for _, seg := range strings.Split(clean, "/") {
		itemID, err = s.ensureChildFolder(ctx, token, driveID, itemID, seg)
		if err != nil {
			return "", "", err
		}
	}
	return driveID, itemID, nil
}

// ensureChildFolder POSTs a folder-create for name under (driveID,parentID),
// tolerating a 409 (name already taken) by resolving the existing child's
// id instead of failing.
func (s *Store) ensureChildFolder(ctx context.Context, token, driveID, parentID, name string) (childID string, err error) {
	createURL := fmt.Sprintf("%s/drives/%s/items/%s/children", graphBase, pathEscape(driveID), pathEscape(parentID))
	body, _ := json.Marshal(map[string]any{
		"name":                              name,
		"folder":                            map[string]any{},
		"@microsoft.graph.conflictBehavior": "fail",
	})
	var created graphItem
	status, err := s.postJSON(ctx, createURL, token, body, &created)
	if err != nil {
		return "", err
	}
	if status == http.StatusConflict {
		return s.resolveChildIDByName(ctx, token, driveID, parentID, name)
	}
	return created.ID, nil
}

// resolveChildIDByName resolves an existing child's id by name — used after
// a 409 on the create-folder call above.
func (s *Store) resolveChildIDByName(ctx context.Context, token, driveID, parentID, name string) (string, error) {
	getURL := fmt.Sprintf("%s/drives/%s/items/%s:/%s", graphBase, pathEscape(driveID), pathEscape(parentID), escapeGraphPath(name))
	var item graphItem
	status, err := s.getJSON(ctx, getURL, token, &item)
	if err != nil {
		return "", err
	}
	if status == http.StatusNotFound {
		return "", fmt.Errorf("onedrivefs: resolve child %q after conflict: not found", name)
	}
	return item.ID, nil
}

// resolveDriveID resolves the caller's own OneDrive drive id — shared by
// EnsureFolder and ListFolders, both of which need to address the caller's
// drive root before any (driveID, itemID) pair (EnsureFolder's own working
// folder, or ListFolders' drive root for the folder picker) exists.
func (s *Store) resolveDriveID(ctx context.Context, token string) (string, error) {
	var drive struct {
		ID string `json:"id"`
	}
	status, err := s.getJSON(ctx, graphBase+"/me/drive?$select=id", token, &drive)
	if err != nil {
		return "", err
	}
	if status == http.StatusNotFound || status == http.StatusConflict || drive.ID == "" {
		return "", fmt.Errorf("onedrivefs: resolve caller drive: %w (status %d)", workspacefs.ErrUnavailable, status)
	}
	return drive.ID, nil
}

// FolderInfo is one folder entry returned by ListFolders — the working-folder
// picker's drive-root browse, distinct from Rooted.ListDir's FileInfo listing
// (which is scoped to one already-resolved working folder and includes files).
type FolderInfo struct {
	Name string
	Path string
}

// ListFolders lists the folder-only immediate children of path, addressed
// from the CALLER'S DRIVE ROOT — not any particular Rooted working-folder
// scope, since the picker browses the whole drive to let the user choose a
// new working folder. path="" or "." means the drive root itself. Reuses
// Rooted.listChildren (pagination + error mapping) by scoping a throwaway
// Rooted to (driveID, "root").
func (s *Store) ListFolders(ctx context.Context, tenant, user, path string) ([]FolderInfo, error) {
	clean, err := cleanDir(path)
	if err != nil {
		return nil, err
	}
	token, err := s.token(ctx, tenant, user)
	if err != nil {
		return nil, err
	}
	driveID, err := s.resolveDriveID(ctx, token)
	if err != nil {
		return nil, err
	}
	root := s.WithRoot(driveID, "root")
	items, found, err := root.listChildren(ctx, token, clean)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	out := make([]FolderInfo, 0, len(items))
	for _, it := range items {
		if it.Folder == nil {
			continue
		}
		out = append(out, FolderInfo{Name: it.Name, Path: childPath(clean, it.Name)})
	}
	return out, nil
}
