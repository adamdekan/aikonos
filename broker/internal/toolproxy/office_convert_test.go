package toolproxy

import (
	"context"
	"net/http"
	"testing"
)

func TestOfficeConvertPlugin_AvailableFalseWhenURLEmpty(t *testing.T) {
	cfg := officeTestConfig(t, "")
	if (officeConvertPlugin{}).Available(cfg) {
		t.Fatal("expected Available=false when OfficeWorkerURL is empty")
	}
}

func TestOfficeConvert_LegacyDocToDocx(t *testing.T) {
	var gotParams map[string]any
	var gotFiles []officeFile
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/office/convert" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		gotParams, gotFiles = parseOfficeTestRequest(t, r)
		writeOfficeTestResponse(t, w, []officeFile{{Field: "file", Filename: "output.docx", Data: []byte("converted-docx")}},
			map[string]any{"filename": "output.docx", "size": 14})
	})
	defer srv.Close()

	cfg := officeTestConfig(t, srv.URL)
	req := officeTestReq(map[string]any{
		"path":          "in/legacy.doc",
		"output_path":   "out/legacy.docx",
		"target_format": "docx",
	})
	writeOfficeTestInput(t, cfg, req, "in/legacy.doc", []byte("legacy-doc-bytes"))

	h := officeConvertPlugin{}.Build(cfg)
	out, _, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if gotParams["target_format"] != "docx" {
		t.Fatalf("expected target_format param sent, got %+v", gotParams)
	}
	if len(gotFiles) != 1 || gotFiles[0].Filename != "legacy.doc" {
		t.Fatalf("expected input filename to carry its extension, got %+v", gotFiles)
	}
	if out["path"] != "out/legacy.docx" {
		t.Fatalf("unexpected output: %+v", out)
	}
	got := readOfficeTestOutput(t, cfg, req, "out/legacy.docx")
	if string(got) != "converted-docx" {
		t.Fatalf("unexpected workspace content: %q", got)
	}
}

func TestOfficeConvert_MissingTargetFormat(t *testing.T) {
	cfg := officeTestConfig(t, "http://unused")
	req := officeTestReq(map[string]any{"path": "in/legacy.doc", "output_path": "out/x.docx"})
	writeOfficeTestInput(t, cfg, req, "in/legacy.doc", []byte("x"))
	h := officeConvertPlugin{}.Build(cfg)
	_, _, err := h(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing target_format")
	}
}

func TestOfficeConvert_WorkerErrorSurfaces(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		writeOfficeTestError(w, http.StatusInternalServerError, "conversion failed")
	})
	defer srv.Close()

	cfg := officeTestConfig(t, srv.URL)
	req := officeTestReq(map[string]any{"path": "in/legacy.doc", "output_path": "out/x.docx", "target_format": "docx"})
	writeOfficeTestInput(t, cfg, req, "in/legacy.doc", []byte("x"))

	h := officeConvertPlugin{}.Build(cfg)
	_, _, err := h(context.Background(), req)
	if err == nil {
		t.Fatal("expected worker error to surface")
	}
}
