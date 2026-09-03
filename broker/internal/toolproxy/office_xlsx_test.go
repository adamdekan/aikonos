package toolproxy

import (
	"context"
	"net/http"
	"testing"
)

func TestXlsxEditPlugin_AvailableFalseWhenURLEmpty(t *testing.T) {
	cfg := officeTestConfig(t, "")
	if (xlsxEditPlugin{}).Available(cfg) {
		t.Fatal("expected Available=false when OfficeWorkerURL is empty")
	}
}

func TestXlsxEdit_ReadsInputSendsScriptWritesOutput(t *testing.T) {
	var gotParams map[string]any
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/xlsx/edit" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		gotParams, _ = parseOfficeTestRequest(t, r)
		writeOfficeTestResponse(t, w, []officeFile{{Field: "file", Filename: "output.xlsx", Data: []byte("xlsx-edited")}},
			map[string]any{"filename": "output.xlsx", "size": 11})
	})
	defer srv.Close()

	cfg := officeTestConfig(t, srv.URL)
	req := officeTestReq(map[string]any{
		"path":        "in/book.xlsx",
		"output_path": "out/book.xlsx",
		"script":      "ws['A1']='x'",
	})
	writeOfficeTestInput(t, cfg, req, "in/book.xlsx", []byte("orig-xlsx"))

	h := xlsxEditPlugin{}.Build(cfg)
	out, _, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if out["path"] != "out/book.xlsx" {
		t.Fatalf("unexpected output: %+v", out)
	}
	if gotParams["script"] != "ws['A1']='x'" {
		t.Fatalf("expected script param sent, got %+v", gotParams)
	}
	got := readOfficeTestOutput(t, cfg, req, "out/book.xlsx")
	if string(got) != "xlsx-edited" {
		t.Fatalf("unexpected workspace content: %q", got)
	}
}

func TestXlsxExtract_ReturnsWorkerResult(t *testing.T) {
	var gotParams map[string]any
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/xlsx/extract" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		gotParams, _ = parseOfficeTestRequest(t, r)
		writeOfficeTestResponse(t, w, nil, map[string]any{"sheet": "Sheet1", "rows": 3, "format": "csv", "content": "a,b\n1,2\n"})
	})
	defer srv.Close()

	cfg := officeTestConfig(t, srv.URL)
	req := officeTestReq(map[string]any{"path": "in/book.xlsx", "sheet": "Sheet1", "format": "csv"})
	writeOfficeTestInput(t, cfg, req, "in/book.xlsx", []byte("orig-xlsx"))

	h := xlsxExtractPlugin{}.Build(cfg)
	out, _, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if out["content"] != "a,b\n1,2\n" {
		t.Fatalf("unexpected output: %+v", out)
	}
	if gotParams["sheet"] != "Sheet1" || gotParams["format"] != "csv" {
		t.Fatalf("expected sheet/format params sent, got %+v", gotParams)
	}
}

func TestXlsxRecalc_WritesOutputAndSurfacesErrorReport(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/xlsx/recalc" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		writeOfficeTestResponse(t, w, []officeFile{{Field: "file", Filename: "output.xlsx", Data: []byte("recalced")}},
			map[string]any{
				"filename":    "output.xlsx",
				"size":        8,
				"errors":      []any{map[string]any{"sheet": "Sheet1", "cell": "B2", "error": "#DIV/0!"}},
				"error_count": 1,
			})
	})
	defer srv.Close()

	cfg := officeTestConfig(t, srv.URL)
	req := officeTestReq(map[string]any{"path": "in/book.xlsx", "output_path": "out/book.xlsx"})
	writeOfficeTestInput(t, cfg, req, "in/book.xlsx", []byte("orig-xlsx"))

	h := xlsxRecalcPlugin{}.Build(cfg)
	out, _, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if out["error_count"] != float64(1) && out["error_count"] != 1 {
		t.Fatalf("expected error_count in result, got %+v", out)
	}
	errs, _ := out["errors"].([]any)
	if len(errs) != 1 {
		t.Fatalf("expected errors report in result, got %+v", out)
	}
	got := readOfficeTestOutput(t, cfg, req, "out/book.xlsx")
	if string(got) != "recalced" {
		t.Fatalf("unexpected workspace content: %q", got)
	}
}

func TestXlsxRecalc_WorkerErrorSurfaces(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		writeOfficeTestError(w, http.StatusInternalServerError, "soffice crashed")
	})
	defer srv.Close()

	cfg := officeTestConfig(t, srv.URL)
	req := officeTestReq(map[string]any{"path": "in/book.xlsx", "output_path": "out/book.xlsx"})
	writeOfficeTestInput(t, cfg, req, "in/book.xlsx", []byte("orig-xlsx"))

	h := xlsxRecalcPlugin{}.Build(cfg)
	_, _, err := h(context.Background(), req)
	if err == nil {
		t.Fatal("expected worker error to surface")
	}
}
