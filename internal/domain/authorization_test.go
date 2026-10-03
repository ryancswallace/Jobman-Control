package domain

import (
	"slices"
	"testing"
)

func TestCapabilityUnionAndUnknownRoles(t *testing.T) {
	t.Parallel()
	for _, role := range []string{RoleViewer, RoleSubmitter, RoleOperator, RoleNamespaceAdmin} {
		capabilities := EffectiveCapabilities([]string{role})
		if !slices.IsSorted(capabilities) || !slices.Contains(capabilities, CapabilityLogsRead) || !slices.Contains(capabilities, CapabilityJobsCancelOwn) {
			t.Fatalf("role %s has invalid capabilities: %v", role, capabilities)
		}
	}
	if len(EffectiveCapabilities([]string{"administrator", ""})) != 0 {
		t.Fatal("unknown roles granted capabilities")
	}
	roles := []string{RoleViewer, RoleSubmitter, RoleOperator, RoleViewer}
	union := EffectiveCapabilities(roles)
	expected := append(EffectiveCapabilities([]string{RoleViewer}), EffectiveCapabilities([]string{RoleSubmitter})...)
	expected = append(expected, EffectiveCapabilities([]string{RoleOperator})...)
	slices.Sort(expected)
	expected = slices.Compact(expected)
	if !slices.Equal(union, expected) || slices.Contains(union, CapabilityMembershipsManage) {
		t.Fatalf("unexpected union: %v", union)
	}
	union[0] = "changed"
	if slices.Contains(EffectiveCapabilities(roles), "changed") {
		t.Fatal("capability catalog exposed mutable backing storage")
	}
}
