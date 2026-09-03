package memorybundle

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
)

func storeEnv(t *testing.T) *workspacefs.Store {
	t.Helper()
	return workspacefs.New(t.TempDir())
}

func seedConcept(t *testing.T, st *workspacefs.Store, seg, id, typ, body string) {
	t.Helper()
	data, err := ComposeConcept(ConceptSpec{Type: typ, Title: id + " title", Body: body}, "user:u1", time.Now())
	if err != nil {
		t.Fatalf("compose %q: %v", id, err)
	}
	if _, err := st.Write(context.Background(), "t1", seg, ConceptPath(id), data); err != nil {
		t.Fatalf("seed %q: %v", id, err)
	}
}

func TestListConceptIDs(t *testing.T) {
	ctx := context.Background()
	st := storeEnv(t)

	ids, err := ListConceptIDs(ctx, st, "t1", "u1")
	if err != nil {
		t.Fatalf("list absent bundle: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("absent bundle listed %v, want empty", ids)
	}

	seedConcept(t, st, "u1", "zeta", "Fact", "z")
	seedConcept(t, st, "u1", "alpha", "Fact", "a")
	seedConcept(t, st, "u1", "people/bob", "Person", "b")
	// The server-maintained files must never list as concepts.
	if err := AppendLogEntry(ctx, st, "t1", "u1", "seeded"); err != nil {
		t.Fatalf("append log: %v", err)
	}
	if _, err := RegenerateIndex(ctx, st, "t1", "u1"); err != nil {
		t.Fatalf("regenerate index: %v", err)
	}

	ids, err = ListConceptIDs(ctx, st, "t1", "u1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"alpha", "people/bob", "zeta"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("ListConceptIDs = %v, want %v", ids, want)
	}
}

func TestLoadConceptErrorsAreOpaqueMarkers(t *testing.T) {
	ctx := context.Background()
	st := storeEnv(t)
	seedConcept(t, st, "u1", "good", "Fact", "a fact")
	if _, err := st.Write(ctx, "t1", "u1", ConceptPath("broken"), []byte("no frontmatter\n")); err != nil {
		t.Fatalf("seed broken: %v", err)
	}

	c, err := LoadConcept(ctx, st, "t1", "u1", "good")
	if err != nil {
		t.Fatalf("load good: %v", err)
	}
	if c.Body != "a fact" || c.Frontmatter["type"] != "Fact" {
		t.Fatalf("loaded %+v, want body/type of the seeded concept", c)
	}

	// These strings reach the model as per-id result markers, so they must stay
	// short and carry no server path.
	for _, tc := range []struct{ id, want string }{
		{"Bad Id", "invalid concept id"},
		{"missing", "not found"},
		{"broken", "unparseable concept file"},
	} {
		_, err := LoadConcept(ctx, st, "t1", "u1", tc.id)
		if err == nil || err.Error() != tc.want {
			t.Fatalf("LoadConcept(%q) error = %v, want %q", tc.id, err, tc.want)
		}
	}
}

func TestRegenerateIndexCountsUnparseable(t *testing.T) {
	ctx := context.Background()
	st := storeEnv(t)
	seedConcept(t, st, "u1", "good", "Fact", "a fact")

	skipped, err := RegenerateIndex(ctx, st, "t1", "u1")
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	idx, err := ReadIndex(ctx, st, "t1", "u1")
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if !strings.Contains(idx, "`good`") || !strings.Contains(idx, "## Fact") {
		t.Fatalf("index missing the seeded concept: %q", idx)
	}

	if _, err := st.Write(ctx, "t1", "u1", ConceptPath("broken"), []byte("no frontmatter\n")); err != nil {
		t.Fatalf("seed broken: %v", err)
	}
	skipped, err = RegenerateIndex(ctx, st, "t1", "u1")
	if err != nil {
		t.Fatalf("regenerate with broken: %v", err)
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1", skipped)
	}
	idx, _ = ReadIndex(ctx, st, "t1", "u1")
	if strings.Contains(idx, "`broken`") {
		t.Fatalf("unparseable concept reached the index: %q", idx)
	}
}

func TestReadIndexAbsentBundleIsEmpty(t *testing.T) {
	idx, err := ReadIndex(context.Background(), storeEnv(t), "t1", "u1")
	if err != nil {
		t.Fatalf("read absent index: %v", err)
	}
	if idx != "" {
		t.Fatalf("absent index = %q, want empty", idx)
	}
}

func TestAppendLogEntryPrependsNewest(t *testing.T) {
	ctx := context.Background()
	st := storeEnv(t)
	for _, e := range []string{"first entry", "second entry"} {
		if err := AppendLogEntry(ctx, st, "t1", "u1", e); err != nil {
			t.Fatalf("append %q: %v", e, err)
		}
	}
	data, _, err := st.Read(ctx, "t1", "u1", Dir+"/"+LogFile)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	log := string(data)
	if !strings.HasPrefix(log, logHeader) {
		t.Fatalf("log lost its header: %q", log)
	}
	if strings.Index(log, "second entry") > strings.Index(log, "first entry") {
		t.Fatalf("entries are not newest-first: %q", log)
	}
}

// TestLockBundleSerializesPerBundle pins the two properties the shared lock map
// exists for: one bundle's writers serialize (run under -race), and two
// different bundles never block each other.
func TestLockBundleSerializesPerBundle(t *testing.T) {
	var n int
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer LockBundle("t1", "u1")()
			n++
		}()
	}
	wg.Wait()
	if n != 16 {
		t.Fatalf("n = %d, want 16", n)
	}

	// Held together in one goroutine: a single global lock would deadlock here.
	release := LockBundle("t1", "u1")
	releaseOther := LockBundle("t1", "group-x")
	releaseOther()
	release()
}

// TestConcurrentBundleWritesKeepIndexConsistent is the store-level twin of the
// toolproxy race test: the lock is what makes index regeneration's
// read-modify-write safe.
func TestConcurrentBundleWritesKeepIndexConsistent(t *testing.T) {
	ctx := context.Background()
	st := storeEnv(t)
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer LockBundle("t1", "u1")()
			seedConcept(t, st, "u1", fmt.Sprintf("c%d", i), "Fact", "body")
			if _, err := RegenerateIndex(ctx, st, "t1", "u1"); err != nil {
				t.Errorf("regenerate: %v", err)
			}
		}(i)
	}
	wg.Wait()

	idx, err := ReadIndex(ctx, st, "t1", "u1")
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	for i := 0; i < n; i++ {
		if !strings.Contains(idx, fmt.Sprintf("`c%d`", i)) {
			t.Fatalf("index lost concept c%d: %q", i, idx)
		}
	}
}
