package personalskill

import (
	"strings"
	"testing"
)

func TestValidSkillName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"demo", true},
		{"my-skill_v2", true},
		{"UPPER", true}, // case is not restricted, unlike memorybundle concept ids
		{"", false},
		{".", false},
		{"..", false},
		{"...", false},
		{"a/b", false},
		{"../secret", false},
		{"a\\b", false},
		{"./demo", false},
		{"demo/", false},
	}
	for _, c := range cases {
		if got := ValidSkillName(c.name); got != c.want {
			t.Errorf("ValidSkillName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseFrontmatter(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantErr bool
		check   func(t *testing.T, fm Frontmatter)
	}{
		{
			name: "full fields",
			src: "---\n" +
				"name: demo\n" +
				"description: A demo skill.\n" +
				"allowed-tools:\n" +
				"  - web.fetch\n" +
				"  - doc.write\n" +
				"keywords:\n" +
				"  - Foo\n" +
				"  - foo\n" +
				"  - \"  Bar  \"\n" +
				"disable-model-invocation: true\n" +
				"---\n\nbody\n",
			check: func(t *testing.T, fm Frontmatter) {
				if fm.Name != "demo" || fm.Description != "A demo skill." {
					t.Fatalf("fm = %+v", fm)
				}
				if strings.Join(fm.AllowedTools, ",") != "web.fetch,doc.write" {
					t.Fatalf("allowedTools = %v", fm.AllowedTools)
				}
				if strings.Join(fm.Keywords, ",") != "foo,bar" {
					t.Fatalf("keywords = %v, want deduped/normalized [foo bar]", fm.Keywords)
				}
				if !fm.DisableModelInvocation {
					t.Fatalf("disableModelInvocation = false, want true")
				}
			},
		},
		{
			name: "multiline block-scalar description",
			src: "---\n" +
				"name: demo\n" +
				"description: |\n" +
				"  line one\n" +
				"  line two\n" +
				"---\n\nbody\n",
			check: func(t *testing.T, fm Frontmatter) {
				if !strings.Contains(fm.Description, "\n") {
					t.Fatalf("description = %q, want embedded newline preserved", fm.Description)
				}
				if !strings.Contains(fm.Description, "line one") || !strings.Contains(fm.Description, "line two") {
					t.Fatalf("description = %q, missing block content", fm.Description)
				}
			},
		},
		{
			name: "unknown keys ignored",
			src: "---\n" +
				"name: demo\n" +
				"description: d\n" +
				"future_field: 42\n" +
				"nested:\n  a: b\n" +
				"---\n\nbody\n",
			check: func(t *testing.T, fm Frontmatter) {
				if fm.Description != "d" {
					t.Fatalf("description = %q", fm.Description)
				}
			},
		},
		{
			name:    "missing opening fence",
			src:     "name: demo\ndescription: d\n",
			wantErr: true,
		},
		{
			name:    "unterminated fence",
			src:     "---\nname: demo\ndescription: d\n",
			wantErr: true,
		},
		{
			name:    "unparseable yaml",
			src:     "---\nname: [unterminated\n---\n\nbody\n",
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fm, err := ParseFrontmatter([]byte(c.src))
			if c.wantErr {
				if err == nil {
					t.Fatalf("ParseFrontmatter(%q) = nil error, want error", c.name)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseFrontmatter(%q): %v", c.name, err)
			}
			c.check(t, fm)
		})
	}
}

func TestSplitBody(t *testing.T) {
	body, err := SplitBody([]byte("---\nname: demo\ndescription: d\n---\n\n# Demo body\n"))
	if err != nil {
		t.Fatalf("SplitBody: %v", err)
	}
	if body != "# Demo body\n" {
		t.Fatalf("body = %q, want the markdown after the closing fence", body)
	}

	if _, err := SplitBody([]byte("no fence here\n")); err == nil {
		t.Fatalf("SplitBody(missing opening fence) = nil error, want error")
	}
	if _, err := SplitBody([]byte("---\nname: demo\n")); err == nil {
		t.Fatalf("SplitBody(unterminated fence) = nil error, want error")
	}
}

func TestNormalizeKeywordsCaps(t *testing.T) {
	// Over maxKeywords: truncates to the cap, keeping first-seen order.
	in := make([]string, maxKeywords+10)
	for i := range in {
		in[i] = "kw" + string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	out := normalizeKeywords(in)
	if len(out) != maxKeywords {
		t.Fatalf("len(out) = %d, want %d", len(out), maxKeywords)
	}

	// Over maxKeywordChars: truncates the individual keyword rather than
	// dropping it.
	long := strings.Repeat("x", maxKeywordChars+20)
	out = normalizeKeywords([]string{long})
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	if len([]rune(out[0])) != maxKeywordChars {
		t.Fatalf("truncated keyword length = %d, want %d", len([]rune(out[0])), maxKeywordChars)
	}

	// Empty/whitespace-only entries are dropped, not counted.
	out = normalizeKeywords([]string{"", "   ", "real"})
	if strings.Join(out, ",") != "real" {
		t.Fatalf("out = %v, want [real]", out)
	}

	// Nil/empty input never errors, returns nil.
	if out := normalizeKeywords(nil); out != nil {
		t.Fatalf("normalizeKeywords(nil) = %v, want nil", out)
	}
}

func TestBuildEntry(t *testing.T) {
	t.Run("valid, no mismatch", func(t *testing.T) {
		e := buildEntry("demo", Frontmatter{Name: "demo", Description: "d"}, nil)
		if !e.Valid || e.Warning != "" || e.Name != "demo" {
			t.Fatalf("e = %+v", e)
		}
	})
	t.Run("dir identity wins on mismatch, valid with warning", func(t *testing.T) {
		e := buildEntry("demo", Frontmatter{Name: "other-name", Description: "d"}, nil)
		if !e.Valid {
			t.Fatalf("e.Valid = false, want true (mismatch is a warning, not an error)")
		}
		if e.Name != "demo" {
			t.Fatalf("e.Name = %q, want dir name %q to win", e.Name, "demo")
		}
		if e.Warning == "" {
			t.Fatalf("e.Warning empty, want a mismatch warning")
		}
	})
	t.Run("missing description is invalid", func(t *testing.T) {
		e := buildEntry("demo", Frontmatter{Name: "demo", Description: ""}, nil)
		if e.Valid || e.Warning == "" {
			t.Fatalf("e = %+v, want invalid with a warning", e)
		}
	})
	t.Run("parse error is invalid", func(t *testing.T) {
		e := buildEntry("demo", Frontmatter{}, errUnparseable)
		if e.Valid || !strings.Contains(e.Warning, "unparseable") {
			t.Fatalf("e = %+v, want invalid unparseable warning", e)
		}
	})
}

// errUnparseable is a stand-in parse error for TestBuildEntry — the real
// callers pass ParseFrontmatter's own error, exercised end-to-end in
// TestParseFrontmatter and store_test.go's TestScan.
var errUnparseable = &parseError{"boom"}

type parseError struct{ msg string }

func (e *parseError) Error() string { return e.msg }
