package workspacefs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// fakeBackend is a recording Backend fake used as Router.Remote. It never
// touches disk — calls are just appended to calls for assertion.
type fakeBackend struct {
	enabled       bool
	readData      []byte
	listDirResult []FileInfo
	calls         []string
}

func (f *fakeBackend) Enabled() bool { return f.enabled }

func (f *fakeBackend) List(ctx context.Context, tenant, user string) ([]FileInfo, error) {
	f.calls = append(f.calls, "List")
	return nil, nil
}

func (f *fakeBackend) ListDir(ctx context.Context, tenant, user, dir string, recursive bool) ([]FileInfo, error) {
	f.calls = append(f.calls, "ListDir:"+dir)
	return f.listDirResult, nil
}

func (f *fakeBackend) Read(ctx context.Context, tenant, user, rel string) ([]byte, time.Time, error) {
	f.calls = append(f.calls, "Read:"+rel)
	return f.readData, time.Time{}, nil
}

func (f *fakeBackend) Write(ctx context.Context, tenant, user, rel string, data []byte) (FileInfo, error) {
	f.calls = append(f.calls, "Write:"+rel)
	return FileInfo{Path: rel, Size: int64(len(data))}, nil
}

func (f *fakeBackend) Delete(ctx context.Context, tenant, user, rel string) error {
	f.calls = append(f.calls, "Delete:"+rel)
	return nil
}

func (f *fakeBackend) Move(ctx context.Context, tenant, user, from, to string) (FileInfo, error) {
	f.calls = append(f.calls, "Move:"+from+"->"+to)
	return FileInfo{Path: to}, nil
}

func (f *fakeBackend) Mkdir(ctx context.Context, tenant, user, rel string) error {
	f.calls = append(f.calls, "Mkdir:"+rel)
	return nil
}

// countingPrefs is a fake PrefResolver that reports how many times it was
// consulted — used to prove a reserved path never triggers a Prefs call.
type countingPrefs struct {
	kind  BackendKind
	err   error
	calls int
}

func (p *countingPrefs) WorkspaceBackend(ctx context.Context, tenant, user string) (BackendKind, error) {
	p.calls++
	return p.kind, p.err
}

// ── CleanRel ─────────────────────────────────────────────────────────────

func TestCleanRel(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "reports/q1.csv", want: "reports/q1.csv"},
		{in: "notes.txt", want: "notes.txt"},
		{in: ".", want: "."},
		{in: "config", want: "config"}, // CleanRel itself does not reject "config" — resolve() layers that
		{in: "a/../.agent/Sessions/x.json", want: ".agent/Sessions/x.json"},
		{in: "./references/y.png", want: "references/y.png"},
		{in: "", wantErr: true},
		{in: "   ", wantErr: true},
		{in: "/abs/path", wantErr: true},
		{in: "../escape", wantErr: true},
		{in: "../../etc/passwd", wantErr: true},
		{in: "reports/../../x", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := CleanRel(tc.in)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidPath) {
					t.Fatalf("CleanRel(%q): want ErrInvalidPath, got value=%q err=%v", tc.in, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CleanRel(%q): unexpected error %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("CleanRel(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ── Router: reserved-prefix routing ──────────────────────────────────────

func TestRouter_ReservedPrefixAlwaysLocal_PrefsNeverConsulted(t *testing.T) {
	paths := []string{
		".agent/Sessions/x.json",
		"a/../.agent/Sessions/x.json",
		"./references/y.png",
		".Agent/z", // case-insensitive
		"config/mcp_connections.json",
		"Skills/demo/SKILL.md",
		"SKILLS/demo/SKILL.md", // case-insensitive, same as .Agent above
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			local := New(t.TempDir())
			remote := &fakeBackend{enabled: true}
			prefs := &countingPrefs{kind: KindOneDrive}
			r := &Router{Local: local, Remote: remote, Prefs: prefs}

			// The operation itself may fail (e.g. "config/..." is rejected by
			// Local's own reserved-subtree guard) — that's an existing,
			// unrelated invariant. What this test pins is the ROUTING
			// decision: Local was chosen, and Prefs/Remote were never asked.
			_, _, _ = r.Read(context.Background(), tenant, user, p)

			if prefs.calls != 0 {
				t.Fatalf("Prefs must never be consulted for reserved path %q, got %d call(s)", p, prefs.calls)
			}
			if len(remote.calls) != 0 {
				t.Fatalf("Remote must never be called for reserved path %q: %v", p, remote.calls)
			}
		})
	}
}

// ── Router: non-reserved routing ─────────────────────────────────────────

func TestRouter_NilPrefs_RoutesLocalWithNoCall(t *testing.T) {
	local := New(t.TempDir())
	remote := &fakeBackend{enabled: true}
	r := &Router{Local: local, Remote: remote} // Prefs nil

	if _, err := r.Write(context.Background(), tenant, user, "notes.txt", []byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(remote.calls) != 0 {
		t.Fatalf("Remote must not be called when Prefs is nil: %v", remote.calls)
	}
	data, _, err := local.Read(context.Background(), tenant, user, "notes.txt")
	if err != nil || string(data) != "hi" {
		t.Fatalf("expected the write to have landed on Local: data=%q err=%v", data, err)
	}
}

func TestRouter_PrefsError_ReturnsErrUnavailable(t *testing.T) {
	local := New(t.TempDir())
	remote := &fakeBackend{enabled: true}
	r := &Router{Local: local, Remote: remote, Prefs: &countingPrefs{err: errors.New("db unreachable")}}

	_, _, err := r.Read(context.Background(), tenant, user, "notes.txt")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
}

func TestRouter_PrefsOneDrive_RoutesToRemote(t *testing.T) {
	local := New(t.TempDir())
	remote := &fakeBackend{enabled: true}
	r := &Router{Local: local, Remote: remote, Prefs: &countingPrefs{kind: KindOneDrive}}

	if _, err := r.Write(context.Background(), tenant, user, "notes.txt", []byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(remote.calls) != 1 || remote.calls[0] != "Write:notes.txt" {
		t.Fatalf("expected Remote to receive the write, got %v", remote.calls)
	}
	if _, _, err := local.Read(context.Background(), tenant, user, "notes.txt"); err == nil {
		t.Fatalf("Local should not have received a onedrive-routed write")
	}
}

func TestRouter_PrefsOneDrive_NoRemote_ReturnsErrUnavailable(t *testing.T) {
	local := New(t.TempDir())
	r := &Router{Local: local, Prefs: &countingPrefs{kind: KindOneDrive}} // Remote nil

	_, _, err := r.Read(context.Background(), tenant, user, "notes.txt")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
}

func TestRouter_ListDir_EmptyAndDotRouteOnActiveBackend(t *testing.T) {
	local := New(t.TempDir())
	remote := &fakeBackend{enabled: true}
	r := &Router{Local: local, Remote: remote, Prefs: &countingPrefs{kind: KindOneDrive}}

	for _, dir := range []string{"", "."} {
		remote.calls = nil
		if _, err := r.ListDir(context.Background(), tenant, user, dir, true); err != nil {
			t.Fatalf("ListDir(%q): %v", dir, err)
		}
		if len(remote.calls) != 1 {
			t.Fatalf("ListDir(%q) should route to Remote when active backend is onedrive, got %v", dir, remote.calls)
		}
	}
}

// ── Router: root listing merges Local's reserved subtrees ────────────────

// TestRouter_ListDir_Root_OneDrive_MergesLocalReservedSubtrees pins CP3 fix
// #1: single-path ops on .agent/references still route to Local even when
// the active backend is OneDrive (isReserved), so a root listing that only
// asked Remote would make that content invisible in the explorer/#-mention
// palette. The root scope must merge Local's reserved-first-segment entries
// into the Remote listing.
func TestRouter_ListDir_Root_OneDrive_MergesLocalReservedSubtrees(t *testing.T) {
	local := New(t.TempDir())
	ctx := context.Background()
	if _, err := local.Write(ctx, tenant, user, ".agent/Sessions/x.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := local.Write(ctx, tenant, user, "references/pic.png", []byte("img")); err != nil {
		t.Fatal(err)
	}
	// Orphaned pre-switch local content: must NOT resurrect into the listing.
	if _, err := local.Write(ctx, tenant, user, "old-notes.txt", []byte("stale")); err != nil {
		t.Fatal(err)
	}
	remote := &fakeBackend{enabled: true}
	r := &Router{Local: local, Remote: remote, Prefs: &countingPrefs{kind: KindOneDrive}}

	for _, dir := range []string{"", "."} {
		got, err := r.ListDir(ctx, tenant, user, dir, true)
		if err != nil {
			t.Fatalf("ListDir(%q): %v", dir, err)
		}
		paths := map[string]bool{}
		for _, fi := range got {
			paths[fi.Path] = true
		}
		if !paths["references/pic.png"] {
			t.Fatalf("ListDir(%q): expected references/pic.png to be merged in, got %v", dir, paths)
		}
		if !paths[".agent/Sessions/x.json"] {
			t.Fatalf("ListDir(%q): expected .agent/Sessions/x.json to be merged in, got %v", dir, paths)
		}
		if paths["old-notes.txt"] {
			t.Fatalf("ListDir(%q): orphaned non-reserved Local content must not resurrect, got %v", dir, paths)
		}
	}
}

// TestRouter_ListDir_Root_OneDrive_RemoteReservedNameFiltered pins CP3 fix #2:
// a Remote folder literally named ".agent" (or any reserved top segment) is
// unreachable for single-path ops (isReserved always pins those to Local), so
// it must not appear in a root listing at all — Local's reserved entry for
// the same name is the only one that survives, with no duplicate Path.
func TestRouter_ListDir_Root_OneDrive_RemoteReservedNameFiltered(t *testing.T) {
	local := New(t.TempDir())
	ctx := context.Background()
	if _, err := local.Write(ctx, tenant, user, ".agent/Sessions/x.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	remote := &fakeBackend{
		enabled: true,
		listDirResult: []FileInfo{
			{Path: ".agent", IsDir: true},              // shadows Local's reserved subtree, must be dropped
			{Path: "references", IsDir: true},          // also reserved, must be dropped
			{Path: "Projects/plan.docx", IsDir: false}, // ordinary remote content, must survive
		},
	}
	r := &Router{Local: local, Remote: remote, Prefs: &countingPrefs{kind: KindOneDrive}}

	got, err := r.ListDir(ctx, tenant, user, "", true)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	seen := map[string]int{}
	for _, fi := range got {
		seen[fi.Path]++
	}
	// ".agent" is expected exactly ONCE: Local's own reservedEntries emits the
	// directory entry itself, and Remote's same-named entry must be dropped
	// rather than duplicating it.
	if seen[".agent"] != 1 {
		t.Fatalf("expected exactly one %q entry (Local's, Remote's duplicate dropped), got count %d in %v", ".agent", seen[".agent"], got)
	}
	// "references" has no Local content in this test, so Remote's entry being
	// filtered means it must not appear at all.
	if seen["references"] != 0 {
		t.Fatalf("Remote's reserved-named %q entry must be filtered out, got count %d in %v", "references", seen["references"], got)
	}
	if seen[".agent/Sessions/x.json"] != 1 {
		t.Fatalf("expected Local's reserved subtree entry to survive exactly once, got %v", got)
	}
	if seen["Projects/plan.docx"] != 1 {
		t.Fatalf("expected ordinary Remote content to survive, got %v", got)
	}
	for path, count := range seen {
		if count > 1 {
			t.Fatalf("duplicate Path %q appeared %d times in merged listing: %v", path, count, got)
		}
	}
}

// TestRouter_ListDir_Root_OneDrive_RemoteSkillsNameFiltered_WarnsOnDrop pins
// the case-insensitive "Skills" reserved-name drop (personal-skills, docs/
// spec/personal-skills.md) AND that the drop is observable via a warn-level
// log — a silently dropped remote entry previously left no trace.
func TestRouter_ListDir_Root_OneDrive_RemoteSkillsNameFiltered_WarnsOnDrop(t *testing.T) {
	local := New(t.TempDir())
	remote := &fakeBackend{
		enabled: true,
		listDirResult: []FileInfo{
			{Path: "Skills", IsDir: true}, // reserved case-insensitively, must be dropped + logged
			{Path: "Projects/plan.docx", IsDir: false},
		},
	}
	r := &Router{Local: local, Remote: remote, Prefs: &countingPrefs{kind: KindOneDrive}}

	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(prevLogger)

	got, err := r.ListDir(context.Background(), tenant, user, "", true)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	for _, fi := range got {
		if fi.Path == "Skills" {
			t.Fatalf("remote's reserved-named %q entry must be dropped, got %v", "Skills", got)
		}
	}
	var sawOrdinary bool
	for _, fi := range got {
		if fi.Path == "Projects/plan.docx" {
			sawOrdinary = true
		}
	}
	if !sawOrdinary {
		t.Fatalf("ordinary remote content must survive, got %v", got)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, "Skills") {
		t.Fatalf("expected a warn-level log naming the dropped entry, got log output: %q", logged)
	}
}

// TestRouter_ListDir_NonRootOneDrive_NoLocalMerge proves the merge is scoped
// to the root: a listing of a specific non-reserved OneDrive subdirectory
// stays Remote-only.
func TestRouter_ListDir_NonRootOneDrive_NoLocalMerge(t *testing.T) {
	local := New(t.TempDir())
	ctx := context.Background()
	if _, err := local.Write(ctx, tenant, user, ".agent/Sessions/x.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	remote := &fakeBackend{enabled: true}
	r := &Router{Local: local, Remote: remote, Prefs: &countingPrefs{kind: KindOneDrive}}

	got, err := r.ListDir(ctx, tenant, user, "Projects/Q1", true)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	for _, fi := range got {
		if fi.Path == ".agent/Sessions/x.json" {
			t.Fatalf("non-root OneDrive listing must not merge Local's reserved subtree, got %v", got)
		}
	}
}

// ── Router: cross-backend Move ───────────────────────────────────────────

func TestRouter_Move_CrossBackendRejectedWhenRemoteActive(t *testing.T) {
	local := New(t.TempDir())
	remote := &fakeBackend{enabled: true}
	r := &Router{Local: local, Remote: remote, Prefs: &countingPrefs{kind: KindOneDrive}}

	_, err := r.Move(context.Background(), tenant, user, "notes.txt", ".agent/Sessions/x.json")
	if !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("want ErrInvalidPath for a cross-backend move, got %v", err)
	}
	if len(remote.calls) != 0 {
		t.Fatalf("Remote must not be called for a rejected cross-backend move: %v", remote.calls)
	}
}

func TestRouter_Move_MixedButLocalActive_RoutesLocal(t *testing.T) {
	local := New(t.TempDir())
	if _, err := local.Write(context.Background(), tenant, user, "draft.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	r := &Router{Local: local} // Remote/Prefs nil -> everything routes local

	if _, err := r.Move(context.Background(), tenant, user, "draft.txt", ".agent/Sessions/y.json"); err != nil {
		t.Fatalf("a mixed reserved/non-reserved move while local is active should succeed (one disk today), got %v", err)
	}
}

// ── Router: ReadVerified ──────────────────────────────────────────────────

func TestRouter_ReadVerified_RemoteRouted_LegacyFalse(t *testing.T) {
	local := New(t.TempDir())
	remote := &fakeBackend{enabled: true, readData: []byte("remote-data")}
	r := &Router{Local: local, Remote: remote, Prefs: &countingPrefs{kind: KindOneDrive}}

	data, _, legacy, err := r.ReadVerified(context.Background(), tenant, user, "notes.txt")
	if err != nil {
		t.Fatalf("ReadVerified: %v", err)
	}
	if legacy {
		t.Fatalf("a remote-routed ReadVerified must report legacy=false")
	}
	if string(data) != "remote-data" {
		t.Fatalf("expected data from Remote.Read, got %q", data)
	}
	var sawRead bool
	for _, c := range remote.calls {
		if c == "Read:notes.txt" {
			sawRead = true
		}
	}
	if !sawRead {
		t.Fatalf("expected Remote.Read to have been called, got %v", remote.calls)
	}
}

// ── Router: Enabled ────────────────────────────────────────────────────────

func TestRouter_Enabled_DelegatesToLocal(t *testing.T) {
	if (&Router{Local: New("")}).Enabled() {
		t.Fatalf("expected disabled when Local's root is empty")
	}
	if !(&Router{Local: New(t.TempDir())}).Enabled() {
		t.Fatalf("expected enabled when Local's root is set")
	}
}
