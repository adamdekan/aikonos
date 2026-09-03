// broker/internal/toolproxy/plugin_test.go
package toolproxy

import (
	"reflect"
	"sort"
	"testing"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"

	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
)

var allNineToolIDs = []string{
	"doc.read", "doc.write", "email.draft",
	"gdrive.read", "gdrive.write",
	"onedrive.read", "onedrive.write",
	"web.fetch", "web.search", "workspace.read",
}

var fiveUnconditionalToolIDs = []string{
	"doc.read", "doc.write", "email.draft", "web.fetch", "workspace.read",
}

// allRegisteredToolIDs is every statically self-registered plugin id
// (RegisteredToolIDs is a package-level registry, independent of any Config —
// see plugin.go), the nine pre-office-doc-tools ids plus the 15 office ids
// plus the two memory ids
//, sorted.
var allRegisteredToolIDs = []string{
	"doc.read", "doc.write", "docx.create", "docx.edit", "docx.extract",
	"email.draft", "gdrive.read", "gdrive.write",
	"memory.read", "memory.write", "office.convert",
	"onedrive.read", "onedrive.write", "pdf.create", "pdf.extract", "pdf.transform",
	"pptx.create", "pptx.edit", "pptx.extract", "pptx.thumbnail",
	"web.fetch", "web.search", "workspace.read",
	"xlsx.create", "xlsx.edit", "xlsx.extract", "xlsx.recalc",
}

func TestRegisteredToolIDs_ContainsExpectedIDs(t *testing.T) {
	got := RegisteredToolIDs()
	if !reflect.DeepEqual(got, allRegisteredToolIDs) {
		t.Fatalf("RegisteredToolIDs() = %v, want %v", got, allRegisteredToolIDs)
	}
}

func TestNew_BareConfig_RegistersOnlyFiveUnconditionalHandlers(t *testing.T) {
	p := New(Config{Workspace: WorkspaceConfig{Root: t.TempDir()}}, nil)
	got := p.Tools()
	sort.Strings(got)
	if !reflect.DeepEqual(got, fiveUnconditionalToolIDs) {
		t.Fatalf("Tools() = %v, want %v", got, fiveUnconditionalToolIDs)
	}
}

func TestNew_SecretsAndOAuth_RegistersAllNineHandlers(t *testing.T) {
	p := New(Config{
		Workspace: WorkspaceConfig{Root: t.TempDir()},
		Secrets:   fakeStore{},
		OAuth: &connector.Registry{
			Credentials: map[connector.Provider]connector.AppCredentials{
				connector.ProviderGoogleDrive: {ClientID: "g-id", ClientSecret: "g-secret"},
				connector.ProviderOneDrive:    {ClientID: "m-id", ClientSecret: "m-secret"},
			},
		},
	}, nil)
	got := p.Tools()
	sort.Strings(got)
	if !reflect.DeepEqual(got, allNineToolIDs) {
		t.Fatalf("Tools() = %v, want %v", got, allNineToolIDs)
	}
}

func TestRegisterPlugin_DuplicateToolID_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected RegisterPlugin to panic on duplicate ToolID")
		}
	}()
	RegisterPlugin(fakeDuplicatePlugin{})
	RegisterPlugin(fakeDuplicatePlugin{})
}

// fakeDuplicatePlugin is a throwaway id used only to exercise the panic path;
// it never collides with a real tool id.
type fakeDuplicatePlugin struct{}

func (fakeDuplicatePlugin) ToolID() string                  { return "test.duplicate.plugin" }
func (fakeDuplicatePlugin) Available(cfg Config) bool       { return false }
func (fakeDuplicatePlugin) Build(cfg Config) Handler        { return nil }
func (fakeDuplicatePlugin) Scope() string                   { return "test:duplicate" }
func (fakeDuplicatePlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_READ_ONLY }

// TestRegisteredPluginBaselines_MatchesToolIDs proves RegisteredPluginBaselines
// derives one entry per RegisteredToolIDs() id, each carrying a non-empty
// scope — the behavior-preservation half of C11.
func TestRegisteredPluginBaselines_MatchesToolIDs(t *testing.T) {
	baselines := RegisteredPluginBaselines()
	ids := RegisteredToolIDs()
	if len(baselines) != len(ids) {
		t.Fatalf("RegisteredPluginBaselines() has %d entries, want %d (len(RegisteredToolIDs()))", len(baselines), len(ids))
	}
	for i, b := range baselines {
		if b.ToolID != ids[i] {
			t.Fatalf("RegisteredPluginBaselines()[%d].ToolID = %q, want %q", i, b.ToolID, ids[i])
		}
		if b.Scope == "" {
			t.Errorf("RegisteredPluginBaselines(): %q has empty Scope", b.ToolID)
		}
	}
}

// TestLoadFromPlugins_DerivationIsLoadBearing is the red-first proof: a
// plugin-backed id absent from toolregistry's static maps (doc.write is no
// longer in staticBaseline/staticBaselineEffectClass post-C11) resolves
// nothing until LoadFromPlugins runs, and resolves correctly once it does —
// proving the derivation is load-bearing, not dead code.
func TestLoadFromPlugins_DerivationIsLoadBearing(t *testing.T) {
	reg := toolregistry.NewRegistry()
	if _, ok := reg.RequiredScope("doc.write"); ok {
		t.Fatal("doc.write resolves a scope before LoadFromPlugins — it should be absent from the static baseline post-C11")
	}
	if _, ok := reg.AuthoritativeEffectClass("doc.write"); ok {
		t.Fatal("doc.write resolves an effect class before LoadFromPlugins — it should be absent from the static baseline post-C11")
	}

	reg.LoadFromPlugins(RegisteredPluginBaselines())

	scope, ok := reg.RequiredScope("doc.write")
	if !ok || scope != "doc:write" {
		t.Fatalf("doc.write scope after LoadFromPlugins = (%q, %v), want (\"doc:write\", true)", scope, ok)
	}
	ec, ok := reg.AuthoritativeEffectClass("doc.write")
	if !ok || ec != planv1.EffectClass_WRITE_LOCAL {
		t.Fatalf("doc.write effect class after LoadFromPlugins = (%v, %v), want (WRITE_LOCAL, true)", ec, ok)
	}
}
