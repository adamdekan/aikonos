package config_test

import (
	"context"
	"errors"
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/config"
)

// fakeRepo is an in-memory configRepo used by all store tests.
type fakeRepo struct {
	data    map[string]string // key → value
	written []writeCall
	err     error // when non-nil, every call returns this error
}

type writeCall struct {
	tenant, key, value, actor string
}

func newFakeRepo() *fakeRepo { return &fakeRepo{data: map[string]string{}} }

func (f *fakeRepo) Get(_ context.Context, _, key string) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	v, ok := f.data[key]
	return v, ok, nil
}

func (f *fakeRepo) Set(_ context.Context, _, key, value, actor string) error {
	if f.err != nil {
		return f.err
	}
	f.data[key] = value
	f.written = append(f.written, writeCall{key: key, value: value, actor: actor})
	return nil
}

func (f *fakeRepo) List(_ context.Context, _ string) (map[string]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]string, len(f.data))
	for k, v := range f.data {
		out[k] = v
	}
	return out, nil
}

// ── 1. GetInt ──────────────────────────────────────────────────────────────

func TestGetInt_StoredValue(t *testing.T) {
	repo := newFakeRepo()
	repo.data["approval_expiry_hours"] = "48"
	s := config.New(repo)

	got := s.GetInt(context.Background(), "t1", "approval_expiry_hours")
	if got != 48 {
		t.Fatalf("want 48, got %d", got)
	}
}

func TestGetInt_SchemaDefaultWhenUnset(t *testing.T) {
	repo := newFakeRepo()
	s := config.New(repo)

	got := s.GetInt(context.Background(), "t1", "approval_expiry_hours")
	if got != 24 {
		t.Fatalf("want default 24, got %d", got)
	}
}

func TestGetInt_DefaultOnRepoError(t *testing.T) {
	repo := newFakeRepo()
	repo.err = errors.New("db down")
	s := config.New(repo)

	// fail-safe: must return schema default, not propagate the error
	got := s.GetInt(context.Background(), "t1", "approval_expiry_hours")
	if got != 24 {
		t.Fatalf("want default 24 on error, got %d", got)
	}
}

// ── 2. Set unknown key ─────────────────────────────────────────────────────

func TestSet_UnknownKey_ReturnsErrUnknownKey(t *testing.T) {
	repo := newFakeRepo()
	s := config.New(repo)

	err := s.Set(context.Background(), "t1", "not_a_real_key", "42", "admin")
	if !errors.Is(err, config.ErrUnknownKey) {
		t.Fatalf("want ErrUnknownKey, got %v", err)
	}
	if len(repo.written) != 0 {
		t.Fatal("repo must not be written for unknown key")
	}
}

// ── 3. Set validation ──────────────────────────────────────────────────────

func TestSet_OutOfRange_Zero(t *testing.T) {
	repo := newFakeRepo()
	s := config.New(repo)

	err := s.Set(context.Background(), "t1", "approval_expiry_hours", "0", "admin")
	if err == nil {
		t.Fatal("want validation error for 0, got nil")
	}
	if len(repo.written) != 0 {
		t.Fatal("repo must not be written on validation failure")
	}
}

func TestSet_OutOfRange_TooLarge(t *testing.T) {
	repo := newFakeRepo()
	s := config.New(repo)

	err := s.Set(context.Background(), "t1", "approval_expiry_hours", "9999", "admin")
	if err == nil {
		t.Fatal("want validation error for 9999, got nil")
	}
	if len(repo.written) != 0 {
		t.Fatal("repo must not be written on validation failure")
	}
}

func TestSet_ValidValue_Written(t *testing.T) {
	repo := newFakeRepo()
	s := config.New(repo)

	err := s.Set(context.Background(), "t1", "approval_expiry_hours", "48", "admin")
	if err != nil {
		t.Fatalf("want nil error, got %v", err)
	}
	if len(repo.written) != 1 {
		t.Fatalf("want 1 write, got %d", len(repo.written))
	}
	if repo.written[0].value != "48" {
		t.Fatalf("want written value 48, got %s", repo.written[0].value)
	}
}

// ── 4. Set invalidates cache ───────────────────────────────────────────────

func TestSet_InvalidatesCache(t *testing.T) {
	repo := newFakeRepo()
	repo.data["approval_expiry_hours"] = "12"
	s := config.New(repo)

	// prime the cache
	if got := s.GetInt(context.Background(), "t1", "approval_expiry_hours"); got != 12 {
		t.Fatalf("pre-set: want 12, got %d", got)
	}

	// mutate via Set
	if err := s.Set(context.Background(), "t1", "approval_expiry_hours", "72", "admin"); err != nil {
		t.Fatalf("set: %v", err)
	}

	// must reflect new value, not stale cached one
	if got := s.GetInt(context.Background(), "t1", "approval_expiry_hours"); got != 72 {
		t.Fatalf("post-set: want 72, got %d", got)
	}
}

// ── 5. Guardrail: Schema is exactly the convenience-only MVP set ───────────

func TestSchema_ExactlyKnownConvenienceKeys(t *testing.T) {
	// This test is the structural guardrail. It will fail if someone adds an
	// authorization-shaped key to Schema without also updating this list.
	// "convenience-only" means the key cannot widen a capability scope, grant
	// access, or lower an authorization bar.
	knownConvenienceKeys := map[string]bool{
		"approval_expiry_hours": true,
		"approval_required_n":   true,
		"disabled_tools":        true,
		"effect_class_routing":  true,
		"llm_model":             true,
	}

	for name := range config.Schema {
		if !knownConvenienceKeys[name] {
			t.Errorf("Schema contains key %q which is NOT in the guardrail's known-convenience set — "+
				"add it here only if it is routing/convenience-only and cannot affect authorization", name)
		}
	}
	for name := range knownConvenienceKeys {
		if _, ok := config.Schema[name]; !ok {
			t.Errorf("guardrail references key %q but it is absent from Schema — update the guardrail", name)
		}
	}
	if len(config.Schema) != len(knownConvenienceKeys) {
		t.Errorf("Schema has %d keys, guardrail has %d — they must match exactly",
			len(config.Schema), len(knownConvenienceKeys))
	}
}

// ── 6. GetString ───────────────────────────────────────────────────────────

func TestGetString_DefaultWhenUnset(t *testing.T) {
	repo := newFakeRepo()
	s := config.New(repo)

	got := s.GetString(context.Background(), "t1", "disabled_tools")
	if got != "" {
		t.Fatalf("want empty string default, got %q", got)
	}
}

func TestGetString_StoredValue(t *testing.T) {
	repo := newFakeRepo()
	repo.data["disabled_tools"] = "web.fetch,doc.write"
	s := config.New(repo)

	got := s.GetString(context.Background(), "t1", "disabled_tools")
	if got != "web.fetch,doc.write" {
		t.Fatalf("want %q, got %q", "web.fetch,doc.write", got)
	}
}

// ── 7. disabled_tools Validate ─────────────────────────────────────────────

func TestDisabledTools_Validate_Empty(t *testing.T) {
	repo := newFakeRepo()
	s := config.New(repo)

	// empty string is valid — means nothing disabled
	if err := s.Set(context.Background(), "t1", "disabled_tools", "", "admin"); err != nil {
		t.Fatalf("want nil for empty string, got %v", err)
	}
}

func TestDisabledTools_Validate_ValidList(t *testing.T) {
	repo := newFakeRepo()
	s := config.New(repo)

	if err := s.Set(context.Background(), "t1", "disabled_tools", "a,b,c", "admin"); err != nil {
		t.Fatalf("want nil for 'a,b,c', got %v", err)
	}
}

func TestDisabledTools_Validate_TooLong(t *testing.T) {
	repo := newFakeRepo()
	s := config.New(repo)

	// 4097-char value must be rejected
	big := make([]byte, 4097)
	for i := range big {
		big[i] = 'a'
	}
	err := s.Set(context.Background(), "t1", "disabled_tools", string(big), "admin")
	if err == nil {
		t.Fatal("want validation error for >4096 char value, got nil")
	}
	if len(repo.written) != 0 {
		t.Fatal("repo must not be written on validation failure")
	}
}

// ── 8. ParseList ───────────────────────────────────────────────────────────

func TestParseList_TrimsAndDropsEmpty(t *testing.T) {
	got := config.ParseList("a, b ,,c")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("index %d: want %q, got %q", i, w, got[i])
		}
	}
}

func TestParseList_EmptyString(t *testing.T) {
	got := config.ParseList("")
	if len(got) != 0 {
		t.Fatalf("want empty slice for empty string, got %v", got)
	}
}

// ── 9. llm_model validate ──────────────────────────────────────────────────

func TestLLMModel_Empty_Valid(t *testing.T) {
	repo := newFakeRepo()
	s := config.New(repo)
	if err := s.Set(context.Background(), "t1", "llm_model", "", "admin"); err != nil {
		t.Fatalf("empty llm_model must be valid, got %v", err)
	}
}

func TestLLMModel_ValidString(t *testing.T) {
	repo := newFakeRepo()
	s := config.New(repo)
	for _, v := range []string{
		"anthropic/claude-sonnet-4.6",
		"openai/gpt-4o",
		"google/gemini-1.5-pro",
		"meta-llama/llama-3.1-70b-instruct",
	} {
		if err := s.Set(context.Background(), "t1", "llm_model", v, "admin"); err != nil {
			t.Fatalf("valid model %q rejected: %v", v, err)
		}
	}
}

func TestLLMModel_InvalidStrings(t *testing.T) {
	repo := newFakeRepo()
	s := config.New(repo)
	for _, v := range []string{
		"no-slash",
		"UPPERCASE/model",
		"/noprefix",
		"provider/",
		"provider/model with spaces",
	} {
		if err := s.Set(context.Background(), "t1", "llm_model", v, "admin"); err == nil {
			t.Fatalf("invalid model %q should have been rejected", v)
		}
	}
}

func TestLLMModel_TooLong(t *testing.T) {
	repo := newFakeRepo()
	s := config.New(repo)
	big := "a/b" + string(make([]byte, 130))
	if err := s.Set(context.Background(), "t1", "llm_model", big, "admin"); err == nil {
		t.Fatal("want validation error for >128 char value, got nil")
	}
}
