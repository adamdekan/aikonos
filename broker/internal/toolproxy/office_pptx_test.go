package toolproxy

import (
	"context"
	"net/http"
	"testing"
)

func TestPptxThumbnailPlugin_AvailableFalseWhenURLEmpty(t *testing.T) {
	cfg := officeTestConfig(t, "")
	if (pptxThumbnailPlugin{}).Available(cfg) {
		t.Fatal("expected Available=false when OfficeWorkerURL is empty")
	}
}

func TestPptxThumbnail_WritesEachPageUnderOutputDir(t *testing.T) {
	var gotParams map[string]any
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pptx/thumbnail" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		gotParams, _ = parseOfficeTestRequest(t, r)
		writeOfficeTestResponse(t, w, []officeFile{
			{Field: "file", Filename: "page-1.jpg", Data: []byte("page1")},
			{Field: "file", Filename: "page-2.jpg", Data: []byte("page2")},
		}, map[string]any{"count": 2, "skipped": 0, "total_pages": 2, "dpi": 150})
	})
	defer srv.Close()

	cfg := officeTestConfig(t, srv.URL)
	req := officeTestReq(map[string]any{"path": "in/deck.pptx", "output_dir": "out/thumbs", "dpi": 150, "max_pages": 20})
	writeOfficeTestInput(t, cfg, req, "in/deck.pptx", []byte("orig-pptx"))

	h := pptxThumbnailPlugin{}.Build(cfg)
	out, _, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if gotParams["dpi"] != float64(150) && gotParams["dpi"] != 150 {
		t.Fatalf("expected dpi param sent, got %+v", gotParams)
	}
	paths, _ := out["paths"].([]any)
	if len(paths) != 2 {
		t.Fatalf("expected 2 output paths, got %+v", out)
	}
	got1 := readOfficeTestOutput(t, cfg, req, "out/thumbs/page-1.jpg")
	if string(got1) != "page1" {
		t.Fatalf("unexpected page 1 content: %q", got1)
	}
	got2 := readOfficeTestOutput(t, cfg, req, "out/thumbs/page-2.jpg")
	if string(got2) != "page2" {
		t.Fatalf("unexpected page 2 content: %q", got2)
	}
	if out["count"] != float64(2) && out["count"] != 2 {
		t.Fatalf("expected count in result, got %+v", out)
	}
}

func TestPptxThumbnail_MissingOutputDir(t *testing.T) {
	cfg := officeTestConfig(t, "http://unused")
	h := pptxThumbnailPlugin{}.Build(cfg)
	req := officeTestReq(map[string]any{"path": "in/deck.pptx"})
	writeOfficeTestInput(t, cfg, req, "in/deck.pptx", []byte("orig-pptx"))
	_, _, err := h(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing output_dir")
	}
}

func TestPptxEdit_SharesXMLEditEngine(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pptx/edit" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		writeOfficeTestResponse(t, w, []officeFile{{Field: "file", Filename: "output.pptx", Data: []byte("edited-pptx")}},
			map[string]any{"filename": "output.pptx", "size": 11})
	})
	defer srv.Close()

	cfg := officeTestConfig(t, srv.URL)
	req := officeTestReq(map[string]any{
		"path":        "in/deck.pptx",
		"output_path": "out/deck.pptx",
		"ops":         []any{map[string]any{"member": "ppt/slides/slide1.xml", "find": "A", "replace": "B"}},
	})
	writeOfficeTestInput(t, cfg, req, "in/deck.pptx", []byte("orig-pptx"))

	h := pptxEditPlugin{}.Build(cfg)
	out, _, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if out["path"] != "out/deck.pptx" {
		t.Fatalf("unexpected output: %+v", out)
	}
}

func TestPptxExtract_SharesExtractEngine(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pptx/extract" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		writeOfficeTestResponse(t, w, nil, map[string]any{"content": "slide markdown"})
	})
	defer srv.Close()

	cfg := officeTestConfig(t, srv.URL)
	req := officeTestReq(map[string]any{"path": "in/deck.pptx"})
	writeOfficeTestInput(t, cfg, req, "in/deck.pptx", []byte("orig-pptx"))

	h := pptxExtractPlugin{}.Build(cfg)
	out, _, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if out["content"] != "slide markdown" {
		t.Fatalf("unexpected output: %+v", out)
	}
}
