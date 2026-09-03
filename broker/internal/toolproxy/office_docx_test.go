package toolproxy

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
)

func officeTestConfig(t *testing.T, workerURL string) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{
		Workspace:       WorkspaceConfig{Root: dir},
		OfficeWorkerURL: workerURL,
	}
}

func officeTestReq(args map[string]any) Request {
	return Request{TenantID: "t1", UserID: "u1", Args: args}
}

func writeOfficeTestInput(t *testing.T, cfg Config, req Request, rel string, data []byte) {
	t.Helper()
	if err := officeWriteOutput(context.Background(), cfg.Workspace, req, rel, "test-setup", data); err != nil {
		t.Fatalf("seed input %q: %v", rel, err)
	}
}

func readOfficeTestOutput(t *testing.T, cfg Config, req Request, rel string) []byte {
	t.Helper()
	clean, err := safeJoin(cfg.Workspace.root(), req.TenantID, req.wsKey(), rel)
	if err != nil {
		t.Fatalf("safeJoin: %v", err)
	}
	b, err := os.ReadFile(clean)
	if err != nil {
		t.Fatalf("read output %q: %v", rel, err)
	}
	return b
}

func TestDocxCreatePlugin_AvailableFalseWhenURLEmpty(t *testing.T) {
	cfg := officeTestConfig(t, "")
	if (docxCreatePlugin{}).Available(cfg) {
		t.Fatal("expected Available=false when OfficeWorkerURL is empty")
	}
}

func TestDocxCreate_WritesOutputAndSendsScript(t *testing.T) {
	var gotParams map[string]any
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/docx/create" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		gotParams, _ = parseOfficeTestRequest(t, r)
		writeOfficeTestResponse(t, w, []officeFile{{Field: "file", Filename: "output.docx", Data: []byte("docx-content")}},
			map[string]any{"filename": "output.docx", "size": 12})
	})
	defer srv.Close()

	cfg := officeTestConfig(t, srv.URL)
	h := docxCreatePlugin{}.Build(cfg)
	req := officeTestReq(map[string]any{"script": "writeFileSync(...)", "output_path": "out/report.docx"})

	out, _, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if out["path"] != "out/report.docx" {
		t.Fatalf("unexpected output: %+v", out)
	}
	if gotParams["script"] != "writeFileSync(...)" {
		t.Fatalf("expected script param sent, got %+v", gotParams)
	}
	got := readOfficeTestOutput(t, cfg, req, "out/report.docx")
	if string(got) != "docx-content" {
		t.Fatalf("unexpected workspace content: %q", got)
	}
}

func TestDocxCreate_MissingScript(t *testing.T) {
	cfg := officeTestConfig(t, "http://unused")
	h := docxCreatePlugin{}.Build(cfg)
	_, _, err := h(context.Background(), officeTestReq(map[string]any{"output_path": "out/x.docx"}))
	if err == nil {
		t.Fatal("expected error for missing script")
	}
}

func TestDocxCreate_WorkerErrorSurfacesAsToolError(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		writeOfficeTestError(w, http.StatusInternalServerError, "script threw")
	})
	defer srv.Close()

	cfg := officeTestConfig(t, srv.URL)
	h := docxCreatePlugin{}.Build(cfg)
	_, _, err := h(context.Background(), officeTestReq(map[string]any{"script": "bad()", "output_path": "out/x.docx"}))
	if err == nil {
		t.Fatal("expected worker error to surface")
	}
}

func TestDocxEdit_ReadsInputSendsOpsWritesOutput(t *testing.T) {
	var gotParams map[string]any
	var gotFiles []officeFile
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/docx/edit" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		gotParams, gotFiles = parseOfficeTestRequest(t, r)
		writeOfficeTestResponse(t, w, []officeFile{{Field: "file", Filename: "output.docx", Data: []byte("edited-bytes")}},
			map[string]any{"filename": "output.docx", "size": 12})
	})
	defer srv.Close()

	cfg := officeTestConfig(t, srv.URL)
	req := officeTestReq(map[string]any{
		"path":        "in/report.docx",
		"output_path": "out/report.docx",
		"ops": []any{
			map[string]any{"member": "word/document.xml", "find": "OLD", "replace": "NEW"},
		},
	})
	writeOfficeTestInput(t, cfg, req, "in/report.docx", []byte("original-zip-bytes"))

	h := docxEditPlugin{}.Build(cfg)
	out, _, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if out["path"] != "out/report.docx" {
		t.Fatalf("unexpected output: %+v", out)
	}
	if len(gotFiles) != 1 || string(gotFiles[0].Data) != "original-zip-bytes" {
		t.Fatalf("expected input bytes uploaded, got %+v", gotFiles)
	}
	ops, _ := gotParams["ops"].([]any)
	if len(ops) != 1 {
		t.Fatalf("expected ops param round-tripped, got %+v", gotParams)
	}
	got := readOfficeTestOutput(t, cfg, req, "out/report.docx")
	if string(got) != "edited-bytes" {
		t.Fatalf("unexpected workspace content: %q", got)
	}
}

func TestDocxEdit_InputExceedsWorkspaceCap(t *testing.T) {
	cfg := officeTestConfig(t, "http://unused")
	req := officeTestReq(map[string]any{
		"path":        "in/huge.docx",
		"output_path": "out/x.docx",
		"ops":         []any{map[string]any{"member": "a", "find": "b", "replace": "c"}},
	})
	// Bypass the write-side cap check to simulate a file that somehow exceeds
	// the workspace cap (defense-in-depth: the read side must not trust it).
	clean, err := safeJoin(cfg.Workspace.root(), req.TenantID, req.wsKey(), "in/huge.docx")
	if err != nil {
		t.Fatalf("safeJoin: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(clean), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	big := make([]byte, workspacefs.MaxFileBytes+1)
	if err := os.WriteFile(clean, big, 0o640); err != nil {
		t.Fatalf("write huge file: %v", err)
	}

	h := docxEditPlugin{}.Build(cfg)
	_, _, err = h(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for input exceeding workspace cap")
	}
}

func TestDocxExtract_ReturnsWorkerResult(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/docx/extract" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		writeOfficeTestResponse(t, w, nil, map[string]any{"content": "# heading\n"})
	})
	defer srv.Close()

	cfg := officeTestConfig(t, srv.URL)
	req := officeTestReq(map[string]any{"path": "in/report.docx"})
	writeOfficeTestInput(t, cfg, req, "in/report.docx", []byte("zipbytes"))

	h := docxExtractPlugin{}.Build(cfg)
	out, _, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if out["content"] != "# heading\n" {
		t.Fatalf("unexpected output: %+v", out)
	}
}
