package policy

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestLoadBundle_TakesPolicyFilesOnly(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"a.rego":                   "package aikonos.a\n",
		"sub/b.rego":               "package aikonos.b\n",
		"sub/b_test.rego":          "package aikonos.b_test\n",
		"tests/c_test.rego":        "package aikonos.c_test\n",
		".hidden/d.rego":           "package aikonos.d\n",
		".e.rego":                  "package aikonos.e\n",
		"data.json":                `{"aikonos":{"x":1}}`,
		"sub/data.yaml":            "y: 2\n",
		"conformance/vectors.json": "[]",
		"README.md":                "# policies\n",
	})
	b, err := LoadBundle(dir, []string{"aikonos"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range b.Files {
		got = append(got, f.Path)
	}
	want := []string{"a.rego", "data.json", "sub/b.rego", "sub/data.yaml"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v", got, want)
	}
}

func TestLoadBundle_Errors(t *testing.T) {
	if _, err := LoadBundle(writeTree(t, map[string]string{"README.md": "x"}), []string{"aikonos"}); err == nil {
		t.Error("a directory without policy files must not load")
	}
	if _, err := LoadBundle(writeTree(t, map[string]string{"a.rego": "package aikonos\n"}), nil); err == nil {
		t.Error("a bundle without roots must not load")
	}
}

// The revision is the SHA-256 of a canonical form documented for outside
// tools (jq -cS). Compute it here independently of bundleRevision.
func TestBundleRevision_MatchesDocumentedCanonicalForm(t *testing.T) {
	a, b := "package aikonos.a\nallow := true\n", "package aikonos.b\n# <html> & co\n"
	dir := writeTree(t, map[string]string{"b.rego": b, "a.rego": a})
	bundle, err := LoadBundle(dir, []string{"aikonos"})
	if err != nil {
		t.Fatal(err)
	}
	canon := `{"files":[{"path":"a.rego","sha256":"` + sha(a) + `"},{"path":"b.rego","sha256":"` + sha(b) + `"}],"roots":["aikonos"]}`
	if want := "sha256:" + sha(canon); bundle.Revision != want {
		t.Fatalf("revision = %s, want %s", bundle.Revision, want)
	}
	again, _ := LoadBundle(dir, []string{"aikonos"})
	if again.Revision != bundle.Revision {
		t.Fatal("the same files must give the same revision")
	}
	changed, _ := LoadBundle(writeTree(t, map[string]string{"a.rego": a + "\n", "b.rego": b}), []string{"aikonos"})
	if changed.Revision == bundle.Revision {
		t.Fatal("a changed byte must change the revision")
	}
}

func TestSnapshot_VerifiesAndDetectsTampering(t *testing.T) {
	b, err := LoadBundle(writeTree(t, map[string]string{"a.rego": "package aikonos.a\n"}), []string{"aikonos"})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := b.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if rev, err := VerifySnapshot(snap); err != nil || rev != b.Revision {
		t.Fatalf("VerifySnapshot = %s, %v; want %s", rev, err, b.Revision)
	}

	mutate := func(f func(*BundleSnapshot)) []byte {
		var s BundleSnapshot
		if err := json.Unmarshal(snap, &s); err != nil {
			t.Fatal(err)
		}
		f(&s)
		out, _ := json.Marshal(s)
		return out
	}
	cases := map[string][]byte{
		"content edited":   mutate(func(s *BundleSnapshot) { s.Files[0].Content += "allow := true\n" }),
		"content and hash": mutate(func(s *BundleSnapshot) { s.Files[0].Content += "x"; s.Files[0].SHA256 = sha(s.Files[0].Content) }),
		"revision swapped": mutate(func(s *BundleSnapshot) { s.Revision = "sha256:" + strings.Repeat("0", 64) }),
		"file added": mutate(func(s *BundleSnapshot) {
			s.Files = append(s.Files, BundleFile{Path: "x.rego", SHA256: sha(""), Content: ""})
		}),
		"wrong kind": mutate(func(s *BundleSnapshot) { s.Kind = "other" }),
		"not json":   []byte("{"),
	}
	for name, data := range cases {
		if _, err := VerifySnapshot(data); err == nil {
			t.Errorf("%s: VerifySnapshot accepted a tampered snapshot", name)
		}
	}
}

func TestTarball_CarriesManifestAndFiles(t *testing.T) {
	b, err := LoadBundle(writeTree(t, map[string]string{"a.rego": "package aikonos.a\n", "sub/b.rego": "package aikonos.b\n"}), []string{"aikonos"})
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(b.Tarball()))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	got := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		got[h.Name] = string(data)
	}
	var manifest struct {
		Revision string   `json:"revision"`
		Roots    []string `json:"roots"`
	}
	if err := json.Unmarshal([]byte(got["/.manifest"]), &manifest); err != nil {
		t.Fatalf("manifest: %v (%q)", err, got["/.manifest"])
	}
	if manifest.Revision != b.Revision || len(manifest.Roots) != 1 || manifest.Roots[0] != "aikonos" {
		t.Fatalf("manifest = %+v", manifest)
	}
	if got["/a.rego"] != "package aikonos.a\n" || got["/sub/b.rego"] != "package aikonos.b\n" {
		t.Fatalf("tarball files = %v", got)
	}
	if b.ETag() != `"`+strings.Replace(b.Revision, ":", "-", 1)+`"` {
		t.Fatalf("etag = %s", b.ETag())
	}
}
