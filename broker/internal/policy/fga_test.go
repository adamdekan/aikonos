package policy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckFGA_DevStub(t *testing.T) {
	// No store id → allow-all stub, no HTTP calls.
	e := newTestEngine(t, "http://unused")
	allowed, err := e.CheckFGA(context.Background(), "user:alice", "can_approve", "task:1")
	if err != nil || !allowed {
		t.Fatalf("stub should allow: allowed=%v err=%v", allowed, err)
	}
}

func TestCheckFGA_Live(t *testing.T) {
	var gotPath, gotUser, gotRel, gotObj string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		var req fgaCheckRequest
		_ = json.Unmarshal(body, &req)
		gotUser, gotRel, gotObj = req.TupleKey.User, req.TupleKey.Relation, req.TupleKey.Object
		// alice may approve, mallory may not.
		allowed := req.TupleKey.User == "user:alice@example.com"
		_ = json.NewEncoder(w).Encode(fgaCheckResponse{Allowed: allowed})
	}))
	defer srv.Close()

	e, err := NewEngine(context.Background(), Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: srv.URL,
		OpenFGAStoreID:  "store-123",
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	allowed, err := e.CheckFGA(context.Background(), "user:alice@example.com", "can_approve", "task:42")
	if err != nil || !allowed {
		t.Fatalf("alice should be allowed: allowed=%v err=%v", allowed, err)
	}
	if !strings.Contains(gotPath, "/stores/store-123/check") {
		t.Errorf("unexpected path %q", gotPath)
	}
	if gotUser != "user:alice@example.com" || gotRel != "can_approve" || gotObj != "task:42" {
		t.Errorf("tuple shape wrong: %s %s %s", gotUser, gotRel, gotObj)
	}

	denied, err := e.CheckFGA(context.Background(), "user:mallory@example.com", "can_approve", "task:42")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if denied {
		t.Error("mallory should be denied")
	}
}

func TestWriteRelations_DevStubNoop(t *testing.T) {
	// No store id → WriteRelations is a no-op and makes no HTTP call.
	e := newTestEngine(t, "http://unused")
	if err := e.WriteRelations(context.Background(),
		Relation{User: "user:alice", Relation: "owner", Object: "task:1"}); err != nil {
		t.Fatalf("disabled WriteRelations should be a no-op: %v", err)
	}
}

func TestWriteRelations_Live(t *testing.T) {
	var gotPath string
	var gotReq fgaWriteRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotReq)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	e, err := NewEngine(context.Background(), Config{
		OPAEndpoint: "http://unused", OpenFGAEndpoint: srv.URL, OpenFGAStoreID: "store-1",
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	err = e.WriteRelations(context.Background(),
		Relation{User: "user:alice@example.com", Relation: "owner", Object: "task:42"},
		Relation{User: "user:alice@example.com", Relation: "approver", Object: "task:42"},
	)
	if err != nil {
		t.Fatalf("WriteRelations: %v", err)
	}
	if !strings.Contains(gotPath, "/stores/store-1/write") {
		t.Errorf("path = %q", gotPath)
	}
	if len(gotReq.Writes.TupleKeys) != 2 || gotReq.Writes.TupleKeys[1].Relation != "approver" {
		t.Errorf("tuples = %+v", gotReq.Writes.TupleKeys)
	}
}

func TestWriteRelations_AlreadyExistsIsOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"write_failed_due_to_invalid_input"}`))
	}))
	defer srv.Close()

	e, _ := NewEngine(context.Background(), Config{
		OPAEndpoint: "http://unused", OpenFGAEndpoint: srv.URL, OpenFGAStoreID: "store-1",
	})
	if err := e.WriteRelations(context.Background(),
		Relation{User: "user:alice", Relation: "owner", Object: "task:1"}); err != nil {
		t.Errorf("duplicate-tuple write should be treated as success, got %v", err)
	}
}

func TestReadTuples_DevStubNoCall(t *testing.T) {
	// No store id → ReadTuples returns empty and makes no HTTP call.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("disabled ReadTuples must not call OpenFGA (hit %s)", r.URL.Path)
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL) // newTestEngine leaves the store id empty → disabled
	got, err := e.ReadTuples(context.Background(), ReadFilter{Object: "group:"})
	if err != nil || got != nil {
		t.Fatalf("disabled ReadTuples = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestReadTuples_Paginates(t *testing.T) {
	var gotPath string
	var pages int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		var req fgaReadRequest
		_ = json.Unmarshal(body, &req)
		if req.TupleKey.Object != "group:" {
			t.Errorf("filter object = %q, want type prefix group:", req.TupleKey.Object)
		}
		pages++
		// First call returns a page + continuation; second returns the tail.
		if req.ContinuationToken == "" {
			_ = json.NewEncoder(w).Encode(fgaReadResponse{
				Tuples: []struct {
					Key fgaTupleKey `json:"key"`
				}{{Key: fgaTupleKey{User: "user:alice@example.com", Relation: "member", Object: "group:security-team"}}},
				ContinuationToken: "tok-2",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(fgaReadResponse{
			Tuples: []struct {
				Key fgaTupleKey `json:"key"`
			}{{Key: fgaTupleKey{User: "user:bob@example.com", Relation: "member", Object: "group:security-team"}}},
			ContinuationToken: "",
		})
	}))
	defer srv.Close()

	e, err := NewEngine(context.Background(), Config{
		OPAEndpoint: "http://unused", OpenFGAEndpoint: srv.URL, OpenFGAStoreID: "store-1",
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	got, err := e.ReadTuples(context.Background(), ReadFilter{Object: "group:"})
	if err != nil {
		t.Fatalf("ReadTuples: %v", err)
	}
	if pages != 2 {
		t.Errorf("expected 2 pages, got %d", pages)
	}
	if len(got) != 2 || got[0].User != "user:alice@example.com" || got[1].User != "user:bob@example.com" {
		t.Errorf("tuples = %+v", got)
	}
	if !strings.Contains(gotPath, "/stores/store-1/read") {
		t.Errorf("path = %q", gotPath)
	}
}

func TestDeleteRelations_Live(t *testing.T) {
	var gotPath string
	var gotReq fgaDeleteRequest
	var rawHasWrites bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		rawHasWrites = strings.Contains(string(body), "\"writes\"")
		_ = json.Unmarshal(body, &gotReq)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	e, err := NewEngine(context.Background(), Config{
		OPAEndpoint: "http://unused", OpenFGAEndpoint: srv.URL, OpenFGAStoreID: "store-1",
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	err = e.DeleteRelations(context.Background(),
		Relation{User: "user:alice@example.com", Relation: "member", Object: "group:security-team"})
	if err != nil {
		t.Fatalf("DeleteRelations: %v", err)
	}
	if !strings.Contains(gotPath, "/stores/store-1/write") {
		t.Errorf("path = %q", gotPath)
	}
	if rawHasWrites {
		t.Error("delete body must use the deletes field, not writes")
	}
	if len(gotReq.Deletes.TupleKeys) != 1 || gotReq.Deletes.TupleKeys[0].Object != "group:security-team" {
		t.Errorf("deletes = %+v", gotReq.Deletes.TupleKeys)
	}
}

func TestDeleteRelations_AbsentIsOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"write_failed_due_to_invalid_input"}`))
	}))
	defer srv.Close()
	e, _ := NewEngine(context.Background(), Config{
		OPAEndpoint: "http://unused", OpenFGAEndpoint: srv.URL, OpenFGAStoreID: "store-1",
	})
	if err := e.DeleteRelations(context.Background(),
		Relation{User: "user:alice", Relation: "member", Object: "group:x"}); err != nil {
		t.Errorf("deleting an absent tuple should succeed, got %v", err)
	}
}

// TestWriteRelations_MalformedBodyDoesNotPanic confirms that a malformed
// response body on the 400 error path is logged (not panicked or propagated
// as an unexpected error type). Control flow still decides on status code.
func TestWriteRelations_MalformedBodyDoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	defer srv.Close()

	e, err := NewEngine(context.Background(), Config{
		OPAEndpoint: "http://unused", OpenFGAEndpoint: srv.URL, OpenFGAStoreID: "store-1",
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	// Malformed body → code field is empty string → not in the known-benign set →
	// WriteRelations returns a "fga write rejected" error. The important thing is
	// no panic, and the decode error was logged at Warn level internally.
	writeErr := e.WriteRelations(context.Background(),
		Relation{User: "user:alice", Relation: "owner", Object: "task:1"})
	if writeErr == nil {
		t.Fatal("expected a rejection error when response body is malformed")
	}
	if !strings.Contains(writeErr.Error(), "fga write rejected") {
		t.Errorf("unexpected error: %v", writeErr)
	}
}

// TestDeleteRelations_MalformedBodyDoesNotPanic mirrors the write test for the
// delete path.
func TestDeleteRelations_MalformedBodyDoesNotPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	defer srv.Close()

	e, err := NewEngine(context.Background(), Config{
		OPAEndpoint: "http://unused", OpenFGAEndpoint: srv.URL, OpenFGAStoreID: "store-1",
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	deleteErr := e.DeleteRelations(context.Background(),
		Relation{User: "user:alice", Relation: "member", Object: "group:x"})
	if deleteErr == nil {
		t.Fatal("expected a rejection error when response body is malformed")
	}
	if !strings.Contains(deleteErr.Error(), "fga delete rejected") {
		t.Errorf("unexpected error: %v", deleteErr)
	}
}

func TestCheckFGA_FailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	e, err := NewEngine(context.Background(), Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: srv.URL,
		OpenFGAStoreID:  "store-123",
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	allowed, err := e.CheckFGA(context.Background(), "user:alice", "can_approve", "task:1")
	if err == nil {
		t.Fatal("expected error on FGA 500")
	}
	if allowed {
		t.Fatal("must fail closed (deny) on FGA error")
	}
}

func TestListObjects_DevStubNil(t *testing.T) {
	// No store id → disabled, returns (nil, nil) without any HTTP call.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("disabled ListObjects must not call OpenFGA (hit %s)", r.URL.Path)
	}))
	defer srv.Close()
	e := newTestEngine(t, srv.URL) // store id empty → disabled
	got, err := e.ListObjects(context.Background(), "user:alice@example.com", "can_use", "agent")
	if err != nil || got != nil {
		t.Fatalf("disabled ListObjects = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestListObjects_Live(t *testing.T) {
	var gotPath string
	var gotBody fgaListObjectsRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_ = json.NewEncoder(w).Encode(fgaListObjectsResponse{
			Objects: []string{"agent:aaa", "agent:bbb"},
		})
	}))
	defer srv.Close()

	e, err := NewEngine(context.Background(), Config{
		OPAEndpoint: "http://unused", OpenFGAEndpoint: srv.URL, OpenFGAStoreID: "store-1",
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	got, err := e.ListObjects(context.Background(), "user:alice@example.com", "can_use", "agent")
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	if !strings.Contains(gotPath, "/stores/store-1/list-objects") {
		t.Errorf("path = %q, want .../list-objects", gotPath)
	}
	if gotBody.User != "user:alice@example.com" || gotBody.Relation != "can_use" || gotBody.Type != "agent" {
		t.Errorf("request body = %+v", gotBody)
	}
	if len(got) != 2 || got[0] != "agent:aaa" || got[1] != "agent:bbb" {
		t.Errorf("objects = %v", got)
	}
}

func TestListObjects_FailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	e, err := NewEngine(context.Background(), Config{
		OPAEndpoint: "http://unused", OpenFGAEndpoint: srv.URL, OpenFGAStoreID: "store-1",
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	got, err := e.ListObjects(context.Background(), "user:alice", "can_use", "agent")
	if err == nil {
		t.Fatal("expected error on FGA 500")
	}
	if got != nil {
		t.Fatal("must fail closed (nil objects) on FGA error")
	}
}
