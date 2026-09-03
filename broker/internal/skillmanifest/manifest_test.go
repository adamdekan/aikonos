package skillmanifest_test

import (
	"strings"
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/skillmanifest"
)

func TestLoad_SingleValid(t *testing.T) {
	manifests, err := skillmanifest.Load("testdata/valid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(manifests) != 1 {
		t.Fatalf("want 1 manifest, got %d", len(manifests))
	}
	m := manifests[0]
	if got := m.ToolID(); got != "web.fetch" {
		t.Errorf("ToolID: want web.fetch, got %s", got)
	}
	if got := m.Scope(); got != "web:read" {
		t.Errorf("Scope: want web:read, got %s", got)
	}
	if got := m.EffectClass(); got != "read_only" {
		t.Errorf("EffectClass: want read_only, got %s", got)
	}
}

func TestLoad_MissingCapabilityScope(t *testing.T) {
	_, err := skillmanifest.Load("testdata/err-missing-scope")
	if err == nil {
		t.Fatal("want error for missing capability_scope, got nil")
	}
	if !strings.Contains(err.Error(), "capability_scope") {
		t.Errorf("error should mention capability_scope, got: %v", err)
	}
}

func TestLoad_WrongKind(t *testing.T) {
	_, err := skillmanifest.Load("testdata/err-wrong-kind")
	if err == nil {
		t.Fatal("want error for kind != Skill, got nil")
	}
	if !strings.Contains(err.Error(), "kind") {
		t.Errorf("error should mention kind, got: %v", err)
	}
}

func TestLoad_UnknownEffectClass(t *testing.T) {
	_, err := skillmanifest.Load("testdata/err-unknown-effect")
	if err == nil {
		t.Fatal("want error for unknown effect_class, got nil")
	}
	if !strings.Contains(err.Error(), "effect_class") {
		t.Errorf("error should mention effect_class, got: %v", err)
	}
}

func TestLoad_MalformedYAML(t *testing.T) {
	_, err := skillmanifest.Load("testdata/err-malformed")
	if err == nil {
		t.Fatal("want error for malformed YAML, got nil")
	}
}

func TestLoad_EmptyDir(t *testing.T) {
	manifests, err := skillmanifest.Load("testdata/empty")
	if err != nil {
		t.Fatalf("empty dir must not error, got: %v", err)
	}
	if len(manifests) != 0 {
		t.Errorf("want 0 manifests, got %d", len(manifests))
	}
}
