// broker/internal/broker/toolregistry_helper_test.go
//
// C11 made toolproxy plugins the single
// source of truth for their own scope + effect class: toolregistry.Registry
// no longer hand-carries a baseline entry for a plugin-backed id (doc.write,
// web.fetch, gdrive.*, the office tools, …), it derives one via
// LoadFromPlugins. Production wires this in main.go; test call sites in this
// package need the same wiring, both for a fresh Registry (newTestToolRegistry)
// and for the nil-Deps.ToolRegistry fallback path (toolregistry.PkgDefault(),
// seeded once here via TestMain).
package broker

import (
	"os"
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
)

// newTestToolRegistry returns a Registry seeded the way main.go seeds toolReg
// in production: NewRegistry() + LoadFromPlugins(toolproxy.
// RegisteredPluginBaselines()). Use this instead of a bare
// toolregistry.NewRegistry() wherever a test exercises a plugin-backed tool id.
func newTestToolRegistry() *toolregistry.Registry {
	reg := toolregistry.NewRegistry()
	reg.LoadFromPlugins(toolproxy.RegisteredPluginBaselines())
	return reg
}

// TestMain seeds toolregistry's package-default Registry before any test
// runs. Many tests in this package build a Deps with no ToolRegistry set,
// relying on the SandboxService/BrokerService nil-safe fallback to
// toolregistry.PkgDefault() — that fallback must reflect the same
// plugin-derived baseline production gets from main.go, or every plugin-backed
// tool id looks unknown.
func TestMain(m *testing.M) {
	toolregistry.PkgDefault().LoadFromPlugins(toolproxy.RegisteredPluginBaselines())
	os.Exit(m.Run())
}
