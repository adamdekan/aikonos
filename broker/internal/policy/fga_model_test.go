package policy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeFGAModels serves the latest-model listing and records the model id each
// Check names.
type fakeFGAModels struct {
	mu      sync.Mutex
	latest  string
	checked []string
}

func (f *fakeFGAModels) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/stores/s1/authorization-models":
			models := []any{}
			if f.latest != "" {
				models = append(models, map[string]any{"id": f.latest})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"authorization_models": models})
		case r.Method == http.MethodPost && r.URL.Path == "/stores/s1/check":
			var req fgaCheckRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.checked = append(f.checked, req.AuthorizationModelID)
			_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true})
		default:
			http.NotFound(w, r)
		}
	}))
}

func (f *fakeFGAModels) lastChecked() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checked[len(f.checked)-1]
}

func TestRefreshFGAModel_PinsChecksToTheLatestModel(t *testing.T) {
	f := &fakeFGAModels{latest: "m1"}
	srv := f.server(t)
	defer srv.Close()
	eng, _ := NewEngine(context.Background(), Config{OpenFGAEndpoint: srv.URL, OpenFGAStoreID: "s1"})
	ctx := context.Background()

	if eng.FGAModelID() != "" {
		t.Fatal("no model before the first refresh")
	}
	if prev, next, err := eng.RefreshFGAModel(ctx); err != nil || prev != "" || next != "m1" {
		t.Fatalf("first refresh = %q, %q, %v", prev, next, err)
	}
	_, _ = eng.CheckFGA(ctx, "user:a", "can_invoke", "skill:x")
	if f.lastChecked() != "m1" {
		t.Fatalf("check named model %q, want m1", f.lastChecked())
	}

	f.mu.Lock()
	f.latest = "m2"
	f.mu.Unlock()
	if prev, next, err := eng.RefreshFGAModel(ctx); err != nil || prev != "m1" || next != "m2" {
		t.Fatalf("refresh after a new model = %q, %q, %v", prev, next, err)
	}
	_, _ = eng.CheckFGA(ctx, "user:a", "can_invoke", "skill:x")
	if f.lastChecked() != "m2" || eng.FGAModelID() != "m2" {
		t.Fatalf("after refresh checks name %q, engine reports %q", f.lastChecked(), eng.FGAModelID())
	}
}

func TestRefreshFGAModel_ConfiguredModelIsNeverReplaced(t *testing.T) {
	f := &fakeFGAModels{latest: "m9"}
	srv := f.server(t)
	defer srv.Close()
	eng, _ := NewEngine(context.Background(), Config{OpenFGAEndpoint: srv.URL, OpenFGAStoreID: "s1", OpenFGAModelID: "pinned"})
	if prev, next, err := eng.RefreshFGAModel(context.Background()); err != nil || prev != "pinned" || next != "pinned" {
		t.Fatalf("refresh = %q, %q, %v; want the configured model kept", prev, next, err)
	}
	_, _ = eng.CheckFGA(context.Background(), "user:a", "can_invoke", "skill:x")
	if f.lastChecked() != "pinned" {
		t.Fatalf("check named model %q, want pinned", f.lastChecked())
	}
}

func TestRefreshFGAModel_NoStoreNoModel(t *testing.T) {
	eng, _ := NewEngine(context.Background(), Config{})
	if prev, next, err := eng.RefreshFGAModel(context.Background()); err != nil || prev != "" || next != "" {
		t.Fatalf("refresh without a store = %q, %q, %v", prev, next, err)
	}
}

func TestRefreshFGAModel_EmptyStoreKeepsCurrent(t *testing.T) {
	f := &fakeFGAModels{}
	srv := f.server(t)
	defer srv.Close()
	eng, _ := NewEngine(context.Background(), Config{OpenFGAEndpoint: srv.URL, OpenFGAStoreID: "s1"})
	if _, next, err := eng.RefreshFGAModel(context.Background()); err != nil || next != "" {
		t.Fatalf("refresh with no models yet = %q, %v", next, err)
	}
}
