// broker/internal/toolproxy/office_client.go
//
// HTTP client for the office-worker service (docx/xlsx/pptx/pdf tools,
// office-worker/API.md). The worker is stateless multipart in/out: POST
// /v1/<format>/<op> with file parts + a "params" JSON field, response is
// either a multipart file+result payload (200) or plain {"error":...} JSON
// (4xx/5xx). Mirrors connector_token.go's doRequest shape (injectable client,
// timeout, bounded response read) — the worker holds no credentials, so the
// office network boundary (not auth) is the control here.
package toolproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
)

const defaultOfficeTimeout = 200 * time.Second

// officeMaxResponseBytes bounds a single response-part read. Per API.md's
// size-cap semantics the worker already refuses to respond with an artifact
// over the max_output_bytes we send (workspacefs.MaxFileBytes) — this is a
// defensive backstop against a misbehaving worker, not the enforcement point.
const officeMaxResponseBytes = workspacefs.MaxFileBytes + 1<<20 // +1 MiB slack for the JSON result part

// officeAvailable reports whether cfg carries a worker URL. Every office
// plugin's Available(cfg) delegates here — a deployment without office-worker
// degrades cleanly (the plugins simply don't register, mirroring web_fetch's
// unconditional-true / gdrive's connectorAvailable gates).
func officeAvailable(cfg Config) bool { return cfg.OfficeWorkerURL != "" }

// officeClient is a bound HTTP client for one office-worker deployment.
type officeClient struct {
	baseURL string
	client  *http.Client
}

func newOfficeClient(cfg Config) *officeClient {
	timeout := cfg.OfficeTimeout
	if timeout <= 0 {
		timeout = defaultOfficeTimeout
	}
	client := cfg.OfficeHTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	return &officeClient{baseURL: strings.TrimRight(cfg.OfficeWorkerURL, "/"), client: client}
}

// officeFile is one multipart file part, in a request or a response.
type officeFile struct {
	Field    string // form field name; worker ops read/write "file" almost universally
	Filename string
	Data     []byte
}

// officeResult is a parsed 200 response: zero or more output files plus the
// op's structured result (the trailing "result" JSON part).
type officeResult struct {
	Files  []officeFile
	Result map[string]any
}

// call POSTs route (e.g. "/v1/docx/create") with files + params. params
// always gains max_output_bytes = workspacefs.MaxFileBytes — the size-cap
// contract office-worker/API.md pins ("Always includes max_output_bytes").
func (c *officeClient) call(ctx context.Context, route string, files []officeFile, params map[string]any) (*officeResult, error) {
	if params == nil {
		params = map[string]any{}
	}
	params["max_output_bytes"] = int64(workspacefs.MaxFileBytes)

	body, contentType, err := buildOfficeRequest(files, params)
	if err != nil {
		return nil, fmt.Errorf("office-worker: build request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+route, body)
	if err != nil {
		return nil, fmt.Errorf("office-worker: %w", err)
	}
	httpReq.Header.Set("Content-Type", contentType)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("office-worker: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, officeErrorFromResponse(resp)
	}
	return parseOfficeMultipartResponse(resp)
}

func buildOfficeRequest(files []officeFile, params map[string]any) (io.Reader, string, error) {
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	for _, f := range files {
		part, err := mw.CreateFormFile(f.Field, f.Filename)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(f.Data); err != nil {
			return nil, "", err
		}
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, "", err
	}
	if err := mw.WriteField("params", string(paramsJSON)); err != nil {
		return nil, "", err
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf, mw.FormDataContentType(), nil
}

// officeErrorFromResponse decodes a non-2xx response as the worker's
// {"error": "..."} JSON shape (API.md), falling back to a raw snippet if the
// body isn't that shape (defense-in-depth against a malformed error body).
func officeErrorFromResponse(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error != "" {
		return fmt.Errorf("office-worker: %s", parsed.Error)
	}
	snippet := strings.TrimSpace(decodeToUTF8(body))
	if snippet == "" {
		snippet = resp.Status
	}
	return fmt.Errorf("office-worker: status %d: %s", resp.StatusCode, snippet)
}

// parseOfficeMultipartResponse reads a 200 response's file parts + trailing
// "result" JSON part (API.md's response shape).
func parseOfficeMultipartResponse(resp *http.Response) (*officeResult, error) {
	mediaType, mparams, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return nil, fmt.Errorf("office-worker: unexpected response content-type %q", resp.Header.Get("Content-Type"))
	}
	boundary := mparams["boundary"]
	if boundary == "" {
		return nil, fmt.Errorf("office-worker: response multipart has no boundary")
	}

	out := &officeResult{}
	mr := multipart.NewReader(resp.Body, boundary)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("office-worker: reading response part: %w", err)
		}
		data, rerr := io.ReadAll(io.LimitReader(part, officeMaxResponseBytes+1))
		name := part.FormName()
		part.Close()
		if rerr != nil {
			return nil, fmt.Errorf("office-worker: reading response part %q: %w", name, rerr)
		}
		if int64(len(data)) > officeMaxResponseBytes {
			return nil, fmt.Errorf("office-worker: response part %q exceeds %d bytes", name, int64(officeMaxResponseBytes))
		}
		if name == "result" {
			if err := json.Unmarshal(data, &out.Result); err != nil {
				return nil, fmt.Errorf("office-worker: malformed result JSON: %w", err)
			}
			continue
		}
		out.Files = append(out.Files, officeFile{Field: name, Filename: part.FileName(), Data: data})
	}
	if out.Result == nil {
		return nil, fmt.Errorf("office-worker: response missing result part")
	}
	return out, nil
}

// officeReadInput reads and cap-checks a workspace-relative input file for an
// office op, routing through cfg per workspaceRead (workspace_seam.go — the
// injected Backend seam when the request carries a known user, else the
// legacy safeJoin/os.ReadFile path), plus an explicit re-check of the
// write-time 10 MiB cap (defense-in-depth: every writer already enforces it,
// but a handler here never trusts that blindly).
func officeReadInput(ctx context.Context, cfg WorkspaceConfig, req Request, rel, toolID string) ([]byte, error) {
	b, err := cfg.workspaceRead(ctx, req, rel, toolID)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > workspacefs.MaxFileBytes {
		return nil, fmt.Errorf("%s: input %q exceeds max size of %d bytes", toolID, rel, int64(workspacefs.MaxFileBytes))
	}
	return b, nil
}

// officeWriteOutput cap-checks and writes output bytes back into the
// workspace at rel (the tool's output_path/output_dir member), routing
// through cfg per workspaceWrite (workspace_seam.go) — the injected Backend
// seam when the request carries a known user, else the legacy
// safeJoin+writeFileAtomic path doc.write uses.
func officeWriteOutput(ctx context.Context, cfg WorkspaceConfig, req Request, rel, toolID string, data []byte) error {
	if int64(len(data)) > workspacefs.MaxFileBytes {
		return fmt.Errorf("%s: output %q exceeds max size of %d bytes", toolID, rel, int64(workspacefs.MaxFileBytes))
	}
	return cfg.workspaceWrite(ctx, req, rel, toolID, data)
}

// newOfficeScriptCreateHandler builds a Handler for a *.create op: no input
// file, a model-authored script param, output written to output_path. Shared
// by docx.create, xlsx.create, pptx.create, pdf.create — identical wire shape
// per API.md (only the interpreter/library differ, worker-side).
func newOfficeScriptCreateHandler(cfg Config, toolID, route string) Handler {
	client := newOfficeClient(cfg)
	ws := cfg.withSeam()
	return func(ctx context.Context, req Request) (map[string]any, int64, error) {
		script := argString(req.Args, "script")
		outputPath, _ := req.Args["output_path"].(string)
		if script == "" {
			return nil, 0, fmt.Errorf("%s: missing required arg %q", toolID, "script")
		}
		if outputPath == "" {
			return nil, 0, fmt.Errorf("%s: missing required arg %q", toolID, "output_path")
		}
		result, err := client.call(ctx, route, nil, map[string]any{"script": script})
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", toolID, err)
		}
		if len(result.Files) == 0 {
			return nil, 0, fmt.Errorf("%s: office-worker returned no output file", toolID)
		}
		out := result.Files[0].Data
		if err := officeWriteOutput(ctx, ws, req, outputPath, toolID, out); err != nil {
			return nil, 0, err
		}
		return map[string]any{"path": outputPath, "size": len(out)}, int64(len(out)/1024) + 1, nil
	}
}

// newOfficeXMLEditHandler builds a Handler for the declarative zip_edit ops
// (find/replace against a named archive member): docx.edit / pptx.edit share
// one worker engine and one wire shape (op.md's "shared" docx/pptx.edit).
func newOfficeXMLEditHandler(cfg Config, toolID, route string) Handler {
	client := newOfficeClient(cfg)
	ws := cfg.withSeam()
	return func(ctx context.Context, req Request) (map[string]any, int64, error) {
		path, _ := req.Args["path"].(string)
		outputPath, _ := req.Args["output_path"].(string)
		ops, _ := req.Args["ops"].([]any)
		if outputPath == "" {
			return nil, 0, fmt.Errorf("%s: missing required arg %q", toolID, "output_path")
		}
		if len(ops) == 0 {
			return nil, 0, fmt.Errorf("%s: missing required arg %q", toolID, "ops")
		}
		data, err := officeReadInput(ctx, ws, req, path, toolID)
		if err != nil {
			return nil, 0, err
		}
		files := []officeFile{{Field: "file", Filename: filepath.Base(path), Data: data}}
		result, err := client.call(ctx, route, files, map[string]any{"ops": ops})
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", toolID, err)
		}
		if len(result.Files) == 0 {
			return nil, 0, fmt.Errorf("%s: office-worker returned no output file", toolID)
		}
		out := result.Files[0].Data
		if err := officeWriteOutput(ctx, ws, req, outputPath, toolID, out); err != nil {
			return nil, 0, err
		}
		return map[string]any{"path": outputPath, "size": len(out)}, int64(len(out)/1024) + 1, nil
	}
}

// newOfficeExtractHandler builds a Handler for the *.extract ops sharing the
// markdown-default + format:"xml" mode (docx.extract, pptx.extract). No
// output file — result is metadata/content only. The extracted "content"
// field is run through the injection-pattern scan (result_scan.go,
// checkpoint 10); a match annotates the result with injection_flags but
// never blocks or alters the content (annotate-only this pass).
func newOfficeExtractHandler(cfg Config, toolID, route string) Handler {
	client := newOfficeClient(cfg)
	ws := cfg.withSeam()
	return func(ctx context.Context, req Request) (map[string]any, int64, error) {
		path, _ := req.Args["path"].(string)
		data, err := officeReadInput(ctx, ws, req, path, toolID)
		if err != nil {
			return nil, 0, err
		}
		params := map[string]any{}
		for _, key := range []string{"format", "member", "pattern", "context_chars", "echo_cap", "track_changes"} {
			if v, ok := req.Args[key]; ok {
				params[key] = v
			}
		}
		files := []officeFile{{Field: "file", Filename: filepath.Base(path), Data: data}}
		result, err := client.call(ctx, route, files, params)
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", toolID, err)
		}
		out := map[string]any{"path": path}
		for k, v := range result.Result {
			out[k] = v
		}
		content, _ := out["content"].(string)
		annotateInjectionFlags(out, content)
		return out, int64(len(content)/1024) + 1, nil
	}
}
