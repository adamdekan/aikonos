// broker/internal/toolproxy/workspace_seam_test.go
//
// CP7: doc.write/doc.read/workspace.read,
// and one office tool + one cloudfile save_to as proof the seam automatically
// carries through officeReadInput/officeWriteOutput, route through the
// injected Backend when the request carries a known user; a task-only
// request always takes the legacy path.
package toolproxy

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
)

// fakeWorkspaceBackend is a recording workspacefs.Backend fake used as
// Config.WorkspaceFS below. It never touches disk — calls are appended for
// assertion, and each method's outcome is configurable per test.
type fakeWorkspaceBackend struct {
	calls []string

	readData []byte
	readErr  error
	writeErr error
	listInfo []workspacefs.FileInfo
	listErr  error
}

func (f *fakeWorkspaceBackend) Enabled() bool { return true }

func (f *fakeWorkspaceBackend) List(ctx context.Context, tenant, user string) ([]workspacefs.FileInfo, error) {
	f.calls = append(f.calls, "List:"+tenant+"/"+user)
	return f.listInfo, f.listErr
}

func (f *fakeWorkspaceBackend) ListDir(ctx context.Context, tenant, user, dir string, recursive bool) ([]workspacefs.FileInfo, error) {
	f.calls = append(f.calls, "ListDir:"+tenant+"/"+user+"/"+dir)
	return f.listInfo, f.listErr
}

func (f *fakeWorkspaceBackend) Read(ctx context.Context, tenant, user, rel string) ([]byte, time.Time, error) {
	f.calls = append(f.calls, "Read:"+tenant+"/"+user+"/"+rel)
	return f.readData, time.Time{}, f.readErr
}

func (f *fakeWorkspaceBackend) Write(ctx context.Context, tenant, user, rel string, data []byte) (workspacefs.FileInfo, error) {
	f.calls = append(f.calls, "Write:"+tenant+"/"+user+"/"+rel)
	if f.writeErr != nil {
		return workspacefs.FileInfo{}, f.writeErr
	}
	return workspacefs.FileInfo{Path: rel, Size: int64(len(data))}, nil
}

func (f *fakeWorkspaceBackend) Delete(ctx context.Context, tenant, user, rel string) error {
	f.calls = append(f.calls, "Delete:"+rel)
	return nil
}

func (f *fakeWorkspaceBackend) Move(ctx context.Context, tenant, user, from, to string) (workspacefs.FileInfo, error) {
	f.calls = append(f.calls, "Move:"+from+"->"+to)
	return workspacefs.FileInfo{Path: to}, nil
}

func (f *fakeWorkspaceBackend) Mkdir(ctx context.Context, tenant, user, rel string) error {
	f.calls = append(f.calls, "Mkdir:"+rel)
	return nil
}

var _ workspacefs.Backend = (*fakeWorkspaceBackend)(nil)

// ── .agent/ provenance guard ─────────

// TestWorkspaceWrite_RejectsAgentPrefix pins the provenance protection every
// seam writer inherits: memory bundles live under .agent/Memory/ and are
// writable only through memory.write, which stamps generated/verified
// server-side. A seam writer that could drop a raw concept file there would let
// a model forge its own `verified:` entries and read them back as
// human-reviewed.
func TestWorkspaceWrite_RejectsAgentPrefix(t *testing.T) {
	blocked := []string{
		".agent/Memory/facts/x.md",
		".agent/x",
		".agent",
		"./.agent/x",
		"notes/../.agent/x",
	}
	for _, rel := range blocked {
		t.Run("blocked/"+rel, func(t *testing.T) {
			// Both branches of the seam: the injected Backend and the legacy local dir.
			for _, branch := range []string{"seam", "legacy"} {
				fake := &fakeWorkspaceBackend{}
				cfg := WorkspaceConfig{Root: t.TempDir()}
				req := Request{TenantID: "ten1", TaskID: "task1"}
				if branch == "seam" {
					cfg.fs = fake
					req.UserID = "user1"
				}
				err := cfg.workspaceWrite(context.Background(), req, rel, "doc.write", []byte("x"))
				if err == nil {
					t.Fatalf("%s: %q accepted", branch, rel)
				}
				if !strings.Contains(err.Error(), "doc.write:") || !strings.Contains(err.Error(), ".agent/") {
					t.Errorf("%s: error does not name the tool and the rule: %v", branch, err)
				}
				if len(fake.calls) != 0 {
					t.Errorf("%s: backend touched on a rejected write: %v", branch, fake.calls)
				}
			}
		})
	}

	// Normal paths, the reserved-but-legitimate config/ prefix, and a name that
	// merely starts with the same letters all stay writable.
	for _, rel := range []string{"notes/out.md", "config/x", ".agentic/x", "a/.agent/x"} {
		fake := &fakeWorkspaceBackend{}
		cfg := WorkspaceConfig{Root: t.TempDir(), fs: fake}
		if err := cfg.workspaceWrite(context.Background(),
			Request{TenantID: "ten1", UserID: "user1"}, rel, "doc.write", []byte("x")); err != nil {
			t.Errorf("%q rejected: %v", rel, err)
		}
	}
}

// TestDocWrite_RejectsAgentPrefix proves the guard is reached through a real
// handler, not only by calling the seam helper directly.
func TestDocWrite_RejectsAgentPrefix(t *testing.T) {
	fake := &fakeWorkspaceBackend{}
	p := New(Config{Workspace: WorkspaceConfig{Root: t.TempDir()}, WorkspaceFS: fake}, nil)

	res, err := p.Invoke(context.Background(), Request{
		ToolID: "doc.write", TenantID: "ten1", UserID: "user1",
		Args: map[string]any{"path": ".agent/Memory/facts/x.md", "body": "---\ntype: Fact\n---\nforged\n"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Success {
		t.Fatal("doc.write wrote into .agent/")
	}
	if len(fake.calls) != 0 {
		t.Errorf("backend touched: %v", fake.calls)
	}
}

// ── doc.write ────────────────────────────────────────────────────────────

func TestDocWrite_UserKnown_RoutesThroughSeam(t *testing.T) {
	fake := &fakeWorkspaceBackend{}
	p := New(Config{Workspace: WorkspaceConfig{Root: t.TempDir()}, WorkspaceFS: fake}, nil)

	res, err := p.Invoke(context.Background(), Request{
		ToolID: "doc.write", TenantID: "ten1", UserID: "user1",
		Args: map[string]any{"path": "notes/out.md", "body": "hello"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got %q", res.Error)
	}
	want := []string{"Write:ten1/user1/notes/out.md"}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
}

func TestDocWrite_TaskOnly_NeverTouchesSeam(t *testing.T) {
	fake := &fakeWorkspaceBackend{}
	root := t.TempDir()
	p := New(Config{Workspace: WorkspaceConfig{Root: root}, WorkspaceFS: fake}, nil)

	res, err := p.Invoke(context.Background(), Request{
		ToolID: "doc.write", TenantID: "ten1", TaskID: "task1",
		Args: map[string]any{"path": "notes/out.md", "body": "hello"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got %q", res.Error)
	}
	if len(fake.calls) != 0 {
		t.Errorf("expected no seam calls for a task-only request, got %v", fake.calls)
	}
	want := filepath.Join(root, "ten1", "task1", "notes", "out.md")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected legacy file at %s: %v", want, err)
	}
}

// ── doc.read ─────────────────────────────────────────────────────────────

func TestDocRead_UserKnown_RoutesThroughSeam(t *testing.T) {
	fake := &fakeWorkspaceBackend{readData: []byte("seam content")}
	p := New(Config{Workspace: WorkspaceConfig{Root: t.TempDir()}, WorkspaceFS: fake}, nil)

	res, err := p.Invoke(context.Background(), Request{
		ToolID: "doc.read", TenantID: "ten1", UserID: "user1",
		Args: map[string]any{"path": "notes/out.md"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Success || res.Output["content"] != "seam content" {
		t.Fatalf("expected seam content, got %+v", res)
	}
	want := []string{"Read:ten1/user1/notes/out.md"}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
}

func TestDocRead_TaskOnly_NeverTouchesSeam(t *testing.T) {
	fake := &fakeWorkspaceBackend{readData: []byte("seam content")}
	root := t.TempDir()
	p := New(Config{Workspace: WorkspaceConfig{Root: root}, WorkspaceFS: fake}, nil)

	// Task-only write lands in the legacy dir; a task-only read must find it
	// there too, never touching the fake.
	if w, _ := p.Invoke(context.Background(), Request{
		ToolID: "doc.write", TenantID: "t", TaskID: "k",
		Args: map[string]any{"path": "a.md", "body": "legacy content"},
	}); w == nil || !w.Success {
		t.Fatalf("seed write failed: %+v", w)
	}
	res, err := p.Invoke(context.Background(), Request{ToolID: "doc.read", TenantID: "t", TaskID: "k", Args: map[string]any{"path": "a.md"}})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Success || res.Output["content"] != "legacy content" {
		t.Fatalf("expected legacy content, got %+v", res)
	}
	if len(fake.calls) != 0 {
		t.Errorf("expected no seam calls for a task-only request, got %v", fake.calls)
	}
}

// TestDocRead_Latin1Content_DecodesAndMarshals proves the encoding fix
// end-to-end: a Windows-1252/Latin-1 file (raw bytes invalid UTF-8) reads back
// as correctly-decoded UTF-8 AND the result now marshals into a structpb
// Struct — before decodeToUTF8 the raw bytes made ResultStruct fail.
func TestDocRead_Latin1Content_DecodesAndMarshals(t *testing.T) {
	// 0xE9=é, 0xEF=ï, 0xE1=á in Windows-1252 (Latin-1 in this byte range).
	fake := &fakeWorkspaceBackend{readData: []byte("caf\xe9 na\xefve M\xe1jer")}
	p := New(Config{Workspace: WorkspaceConfig{Root: t.TempDir()}, WorkspaceFS: fake}, nil)

	res, err := p.Invoke(context.Background(), Request{
		ToolID: "doc.read", TenantID: "ten1", UserID: "user1",
		Args: map[string]any{"path": "notes/latin1.txt"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got %q", res.Error)
	}
	if got := res.Output["content"]; got != "café naïve Májer" {
		t.Fatalf("content = %q, want %q", got, "café naïve Májer")
	}
	if _, err := ResultStruct(res.Output); err != nil {
		t.Fatalf("ResultStruct on decoded doc.read output failed: %v", err)
	}
}

// ── workspace.read ───────────────────────────────────────────────────────

func TestWorkspaceRead_UserKnown_RoutesThroughSeamGoldenShape(t *testing.T) {
	fake := &fakeWorkspaceBackend{listInfo: []workspacefs.FileInfo{
		{Path: "a/b.md", Size: 5},
		{Path: "a", Size: 0, IsDir: true}, // directories never surface — same shape as legacy
	}}
	p := New(Config{Workspace: WorkspaceConfig{Root: t.TempDir()}, WorkspaceFS: fake}, nil)

	res, err := p.Invoke(context.Background(), Request{ToolID: "workspace.read", TenantID: "ten1", UserID: "user1"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Success || res.Output["count"].(int) != 1 {
		t.Fatalf("expected 1 file (dir entry excluded), got %+v", res.Output)
	}
	files, _ := res.Output["files"].([]any)
	entry, _ := files[0].(map[string]any)
	if entry["path"] != "a/b.md" || entry["bytes"] != int64(5) {
		t.Errorf("unexpected entry shape (want the same {path,bytes} shape as legacy): %+v", entry)
	}
	want := []string{"List:ten1/user1"}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
}

// TestWorkspaceRead_ConfigExclusionDivergesBetweenSeamAndLegacy pins the
// accepted divergence (workspace_seam.go's workspaceList doc comment): via
// the real workspacefs.Store, "config/" is excluded from every listing;
// legacy toolproxy's own WalkDir has no such concept and lists it. Both
// paths resolve to the exact same on-disk directory here (wsKey() ==
// UserID when set), so the divergence is proven against identical bytes on
// disk, not two different directories.
func TestWorkspaceRead_ConfigExclusionDivergesBetweenSeamAndLegacy(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "ten1", "user1", "config")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{}"), 0o640); err != nil {
		t.Fatal(err)
	}

	store := workspacefs.New(root)
	p := New(Config{Workspace: WorkspaceConfig{Root: root}, WorkspaceFS: store}, nil)

	t.Run("seam excludes config/", func(t *testing.T) {
		res, err := p.Invoke(context.Background(), Request{ToolID: "workspace.read", TenantID: "ten1", UserID: "user1"})
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if res.Output["count"].(int) != 0 {
			t.Errorf("expected the seam listing to exclude config/, got %+v", res.Output)
		}
	})

	t.Run("legacy lists config/", func(t *testing.T) {
		res, err := p.Invoke(context.Background(), Request{ToolID: "workspace.read", TenantID: "ten1", TaskID: "user1"})
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if res.Output["count"].(int) != 1 {
			t.Errorf("expected the legacy WalkDir listing to include config/, got %+v", res.Output)
		}
	})
}

// ── sentinel surfacing ───────────────────────────────────────────────────

func TestDocRead_UserKnown_SentinelReconnectRequired(t *testing.T) {
	fake := &fakeWorkspaceBackend{readErr: workspacefs.ErrReconnectRequired}
	p := New(Config{Workspace: WorkspaceConfig{Root: t.TempDir()}, WorkspaceFS: fake}, nil)

	res, err := p.Invoke(context.Background(), Request{
		ToolID: "doc.read", TenantID: "t", UserID: "u", Args: map[string]any{"path": "x.md"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Success || !strings.Contains(res.Error, "reconnect_needed") {
		t.Errorf("expected reconnect_needed in the tool error, got %+v", res)
	}
}

func TestDocRead_UserKnown_SentinelTooLarge(t *testing.T) {
	backendErr := fmt.Errorf("%w: read exceeds %d bytes", workspacefs.ErrTooLarge, int64(workspacefs.MaxFileBytes))
	fake := &fakeWorkspaceBackend{readErr: backendErr}
	p := New(Config{Workspace: WorkspaceConfig{Root: t.TempDir()}, WorkspaceFS: fake}, nil)

	res, err := p.Invoke(context.Background(), Request{
		ToolID: "doc.read", TenantID: "t", UserID: "u", Args: map[string]any{"path": "x.md"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Success || !strings.Contains(res.Error, "exceeds") || !strings.Contains(res.Error, "bytes") {
		t.Errorf("expected size-error phrasing (\"exceeds ... bytes\"), got %+v", res)
	}
}

// ── office / cloudfile inherit the seam automatically ───────────────────

func TestDocxCreate_UserKnown_RoutesOutputThroughSeam(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		writeOfficeTestResponse(t, w, []officeFile{{Field: "file", Filename: "output.docx", Data: []byte("docx-bytes")}},
			map[string]any{"filename": "output.docx", "size": 10})
	})
	defer srv.Close()

	fake := &fakeWorkspaceBackend{}
	cfg := Config{
		Workspace:       WorkspaceConfig{Root: t.TempDir()},
		WorkspaceFS:     fake,
		OfficeWorkerURL: srv.URL,
	}
	h := (docxCreatePlugin{}).Build(cfg)
	out, _, err := h(context.Background(), Request{
		ToolID: "docx.create", TenantID: "ten1", UserID: "user1",
		Args: map[string]any{"script": "make()", "output_path": "out/report.docx"},
	})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if out["path"] != "out/report.docx" {
		t.Fatalf("unexpected output: %+v", out)
	}
	want := []string{"Write:ten1/user1/out/report.docx"}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
}

func TestGDriveRead_SaveTo_UserKnown_RoutesThroughSeam(t *testing.T) {
	srv := fakeDrive(t)
	defer srv.Close()
	withDriveBase(t, srv)

	fake := &fakeWorkspaceBackend{}
	ws := WorkspaceConfig{Root: t.TempDir(), fs: fake}
	h := newGDriveReadHandler(gdriveDeps(fakeStore{at: "test-at"}), ws)
	out, _, err := h(context.Background(), req(map[string]any{"file_id": "FID", "save_to": "in/report.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	if out["path"] != "in/report.txt" {
		t.Fatalf("unexpected output: %+v", out)
	}
	want := []string{"Write:t/u/in/report.txt"}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
}
