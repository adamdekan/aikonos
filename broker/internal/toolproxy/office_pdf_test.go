package toolproxy

import (
	"context"
	"net/http"
	"testing"
)

func TestPdfTransformPlugin_AvailableFalseWhenURLEmpty(t *testing.T) {
	cfg := officeTestConfig(t, "")
	if (pdfTransformPlugin{}).Available(cfg) {
		t.Fatal("expected Available=false when OfficeWorkerURL is empty")
	}
}

func TestPdfTransform_Merge_SendsFilesInOrderWritesOutput(t *testing.T) {
	var gotParams map[string]any
	var gotFiles []officeFile
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pdf/transform" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		gotParams, gotFiles = parseOfficeTestRequest(t, r)
		writeOfficeTestResponse(t, w, []officeFile{{Field: "file", Filename: "output.pdf", Data: []byte("merged-pdf")}},
			map[string]any{"filename": "output.pdf", "size": 10})
	})
	defer srv.Close()

	cfg := officeTestConfig(t, srv.URL)
	req := officeTestReq(map[string]any{
		"op":          "merge",
		"paths":       []any{"in/a.pdf", "in/b.pdf"},
		"output_path": "out/merged.pdf",
	})
	writeOfficeTestInput(t, cfg, req, "in/a.pdf", []byte("pdf-a"))
	writeOfficeTestInput(t, cfg, req, "in/b.pdf", []byte("pdf-b"))

	h := pdfTransformPlugin{}.Build(cfg)
	out, _, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if gotParams["op"] != "merge" {
		t.Fatalf("expected op=merge sent, got %+v", gotParams)
	}
	if len(gotFiles) != 2 || string(gotFiles[0].Data) != "pdf-a" || string(gotFiles[1].Data) != "pdf-b" {
		t.Fatalf("expected both input files in order, got %+v", gotFiles)
	}
	if out["path"] != "out/merged.pdf" {
		t.Fatalf("unexpected output: %+v", out)
	}
	got := readOfficeTestOutput(t, cfg, req, "out/merged.pdf")
	if string(got) != "merged-pdf" {
		t.Fatalf("unexpected workspace content: %q", got)
	}
}

func TestPdfTransform_Split_WritesEachRangeOutput(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		gotParams, _ := parseOfficeTestRequest(t, r)
		ranges, _ := gotParams["ranges"].([]any)
		if len(ranges) != 2 {
			t.Fatalf("expected 2 ranges sent, got %+v", gotParams)
		}
		writeOfficeTestResponse(t, w, []officeFile{
			{Field: "file", Filename: "split-1.pdf", Data: []byte("part1")},
			{Field: "file", Filename: "split-2.pdf", Data: []byte("part2")},
		}, map[string]any{"files": []any{
			map[string]any{"filename": "split-1.pdf", "size": 5, "pages": "1-2"},
			map[string]any{"filename": "split-2.pdf", "size": 5, "pages": "3-4"},
		}})
	})
	defer srv.Close()

	cfg := officeTestConfig(t, srv.URL)
	req := officeTestReq(map[string]any{
		"op":    "split",
		"paths": []any{"in/doc.pdf"},
		"outputs": []any{
			map[string]any{"pages": "1-2", "output_path": "out/part1.pdf"},
			map[string]any{"pages": "3-4", "output_path": "out/part2.pdf"},
		},
	})
	writeOfficeTestInput(t, cfg, req, "in/doc.pdf", []byte("orig-pdf"))

	h := pdfTransformPlugin{}.Build(cfg)
	out, _, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	paths, _ := out["paths"].([]any)
	if len(paths) != 2 {
		t.Fatalf("expected 2 output paths, got %+v", out)
	}
	got1 := readOfficeTestOutput(t, cfg, req, "out/part1.pdf")
	if string(got1) != "part1" {
		t.Fatalf("unexpected part1 content: %q", got1)
	}
	got2 := readOfficeTestOutput(t, cfg, req, "out/part2.pdf")
	if string(got2) != "part2" {
		t.Fatalf("unexpected part2 content: %q", got2)
	}
}

func TestPdfTransform_UnknownOp(t *testing.T) {
	cfg := officeTestConfig(t, "http://unused")
	req := officeTestReq(map[string]any{"op": "bogus", "paths": []any{"in/a.pdf"}})
	writeOfficeTestInput(t, cfg, req, "in/a.pdf", []byte("x"))
	h := pdfTransformPlugin{}.Build(cfg)
	_, _, err := h(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for unknown op")
	}
}

func TestPdfTransform_WorkerErrorSurfaces(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		writeOfficeTestError(w, http.StatusInternalServerError, "wrong password")
	})
	defer srv.Close()

	cfg := officeTestConfig(t, srv.URL)
	req := officeTestReq(map[string]any{"op": "decrypt", "paths": []any{"in/a.pdf"}, "password": "wrong", "output_path": "out/a.pdf"})
	writeOfficeTestInput(t, cfg, req, "in/a.pdf", []byte("encrypted"))

	h := pdfTransformPlugin{}.Build(cfg)
	_, _, err := h(context.Background(), req)
	if err == nil {
		t.Fatal("expected worker error to surface")
	}
}

func TestPdfExtract_ReturnsWorkerResult(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pdf/extract" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		writeOfficeTestResponse(t, w, nil, map[string]any{"text": "hello pdf", "tables": []any{}, "method": "pdfplumber"})
	})
	defer srv.Close()

	cfg := officeTestConfig(t, srv.URL)
	req := officeTestReq(map[string]any{"path": "in/doc.pdf"})
	writeOfficeTestInput(t, cfg, req, "in/doc.pdf", []byte("orig-pdf"))

	h := pdfExtractPlugin{}.Build(cfg)
	out, _, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if out["text"] != "hello pdf" || out["method"] != "pdfplumber" {
		t.Fatalf("unexpected output: %+v", out)
	}
}
