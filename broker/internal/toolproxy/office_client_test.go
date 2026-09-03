package toolproxy

import (
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
)

// newTestServer starts an httptest server for the given handler, closed by
// the caller — shared by every office_*_test.go fake-worker.
func newTestServer(fn http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(fn)
}

// parseOfficeTestRequest decodes an incoming multipart request the way the
// real office-worker would — shared by every office_*_test.go fake-worker
// handler in this package.
func parseOfficeTestRequest(t *testing.T, r *http.Request) (map[string]any, []officeFile) {
	t.Helper()
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		t.Fatalf("ParseMultipartForm: %v", err)
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(r.FormValue("params")), &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	var files []officeFile
	if r.MultipartForm != nil {
		for field, headers := range r.MultipartForm.File {
			for _, h := range headers {
				f, err := h.Open()
				if err != nil {
					t.Fatalf("open file part: %v", err)
				}
				data, err := io.ReadAll(f)
				f.Close()
				if err != nil {
					t.Fatalf("read file part: %v", err)
				}
				files = append(files, officeFile{Field: field, Filename: h.Filename, Data: data})
			}
		}
	}
	return params, files
}

// writeOfficeTestResponse writes a multipart success response shaped like
// office-worker/API.md: zero or more file parts + a trailing "result" JSON part.
func writeOfficeTestResponse(t *testing.T, w http.ResponseWriter, files []officeFile, result map[string]any) {
	t.Helper()
	mw := multipart.NewWriter(w)
	w.Header().Set("Content-Type", mw.FormDataContentType())
	for _, f := range files {
		part, err := mw.CreateFormFile(f.Field, f.Filename)
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		if _, err := part.Write(f.Data); err != nil {
			t.Fatalf("write file part: %v", err)
		}
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	resultPart, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="result"`},
		"Content-Type":        {"application/json"},
	})
	if err != nil {
		t.Fatalf("CreatePart(result): %v", err)
	}
	if _, err := resultPart.Write(resultJSON); err != nil {
		t.Fatalf("write result part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
}

func writeOfficeTestError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": message})
}

func TestOfficeClient_Call_SendsMaxOutputBytesAndParsesResponse(t *testing.T) {
	var gotParams map[string]any
	var gotFiles []officeFile
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/docx/create" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		gotParams, gotFiles = parseOfficeTestRequest(t, r)
		writeOfficeTestResponse(t, w, []officeFile{{Field: "file", Filename: "output.docx", Data: []byte("docx-bytes")}},
			map[string]any{"filename": "output.docx", "size": 10})
	})
	defer srv.Close()

	c := &officeClient{baseURL: srv.URL, client: srv.Client()}
	result, err := c.call(context.Background(), "/v1/docx/create", nil, map[string]any{"script": "console.log(1)"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if gotParams["max_output_bytes"] == nil {
		t.Fatalf("expected max_output_bytes in request params, got %+v", gotParams)
	}
	if gotParams["script"] != "console.log(1)" {
		t.Fatalf("expected script param round-tripped, got %+v", gotParams)
	}
	if len(gotFiles) != 0 {
		t.Fatalf("expected no input file parts for create, got %d", len(gotFiles))
	}
	if len(result.Files) != 1 || string(result.Files[0].Data) != "docx-bytes" {
		t.Fatalf("unexpected result files: %+v", result.Files)
	}
	if result.Result["filename"] != "output.docx" {
		t.Fatalf("unexpected result: %+v", result.Result)
	}
}

func TestOfficeClient_Call_WorkerErrorSurfaces(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		writeOfficeTestError(w, http.StatusInternalServerError, "script failed: boom")
	})
	defer srv.Close()

	c := &officeClient{baseURL: srv.URL, client: srv.Client()}
	_, err := c.call(context.Background(), "/v1/docx/create", nil, map[string]any{"script": "x"})
	if err == nil || !strings.Contains(err.Error(), "script failed: boom") {
		t.Fatalf("expected worker error to surface, got %v", err)
	}
}

func TestOfficeClient_Call_SendsInputFileParts(t *testing.T) {
	var gotFiles []officeFile
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		_, gotFiles = parseOfficeTestRequest(t, r)
		writeOfficeTestResponse(t, w, nil, map[string]any{"content": "hello"})
	})
	defer srv.Close()

	c := &officeClient{baseURL: srv.URL, client: srv.Client()}
	_, err := c.call(context.Background(), "/v1/docx/extract", []officeFile{{Field: "file", Filename: "report.docx", Data: []byte("zipbytes")}}, nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(gotFiles) != 1 || gotFiles[0].Filename != "report.docx" || string(gotFiles[0].Data) != "zipbytes" {
		t.Fatalf("unexpected uploaded files: %+v", gotFiles)
	}
}
