package broker

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// stubDelegationScopes is the dev stand-in for a sender's delegable scopes,
// used only while FGA is disabled (dev allow-all mode). Kept narrow so the
// attenuation check remains meaningful. Moved here from service.go — F24
// "implement for real".
var stubDelegationScopes = []string{"siem:read", "slack:write", "doc:write", "workspace:read"}

// senderDelegableScopes returns the capability scopes userID may delegate,
// plus a source marker ("fga-derived" or "dev-stub") recorded on the
// envelope-send audit event.
//
// With FGA enabled, delegable authority is derived from ground truth: what
// the user can actually invoke. OpenFGA's can_invoke/skill grants
// (ListObjects) are mapped through the ToolRegistry's tool→scope table — the
// same table InvokeTool enforces at the Biscuit gate — so the derived set is
// exactly the scopes backing tools the user is allowed to run. Skill ids with
// no RequiredScope entry (mcp:-prefixed tools, capability skills such as
// workflows/scheduler/vision) fall out of the mapping silently: they carry no
// delegable capability scope.
//
// With FGA disabled (nil Policy or !FGAEnabled()), dev stacks keep working
// unseeded via the hardcoded stub list.
//
// A ListObjects transport error fails closed: the caller must deny the send
// rather than fall back to the stub, which would turn an availability
// incident into an authority grant.
func (s *BrokerService) senderDelegableScopes(ctx context.Context, userID string) (scopes []string, source string, err error) {
	if s.deps.Policy == nil || !s.deps.Policy.FGAEnabled() {
		return stubDelegationScopes, "dev-stub", nil
	}

	// A nil ToolRegistry with FGA enabled is a misconfiguration, not a
	// legitimate "no scopes" state: fail closed instead of silently deriving
	// an empty delegable set that would deny every future attenuation.
	if s.deps.ToolRegistry == nil {
		return nil, "", fmt.Errorf("senderDelegableScopes: FGA enabled but ToolRegistry is nil")
	}

	objects, err := s.deps.Policy.ListObjects(ctx, "user:"+userID, "can_invoke", "skill")
	if err != nil {
		return nil, "", err
	}

	seen := make(map[string]struct{}, len(objects))
	for _, obj := range objects {
		toolID := strings.TrimPrefix(obj, "skill:")
		scope, ok := s.deps.ToolRegistry.RequiredScope(toolID)
		if !ok {
			continue
		}
		seen[scope] = struct{}{}
	}
	scopes = make([]string, 0, len(seen))
	for scope := range seen {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	return scopes, "fga-derived", nil
}
