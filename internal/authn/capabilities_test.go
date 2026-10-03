package authn

import (
	"slices"
	"testing"

	"github.com/tgdrive/teldrive/v2/internal/db/sqlcgen"
)

// TestCapabilitiesAreDerivedFromTheRoleAlone pins the permission strings every
// client uses to decide which controls to offer. The frontend hides a button when
// the capability is absent, so a missing entry here is a control an ordinary user
// can still reach by hand.
func TestCapabilitiesAreDerivedFromTheRoleAlone(t *testing.T) {
	t.Parallel()
	fileCapabilities := []string{"files.read", "files.write", "files.share"}
	adminCapabilities := []string{
		"system.manageUsers", "system.manageJobs", "system.manageQueues",
		"system.localImport", "system.maintenance",
	}

	for _, testCase := range []struct {
		name string
		role sqlcgen.UserRole
		want []string
	}{
		{name: "user", role: sqlcgen.UserRole("user"), want: fileCapabilities},
		{name: "admin", role: sqlcgen.UserRoleAdmin, want: append(slices.Clone(fileCapabilities), adminCapabilities...)},
		{name: "owner", role: sqlcgen.UserRoleOwner, want: append(slices.Clone(fileCapabilities), append(slices.Clone(adminCapabilities), "system.owner")...)},
		{name: "unknown role gets the file capabilities only", role: sqlcgen.UserRole("wizard"), want: fileCapabilities},
		{name: "empty role gets the file capabilities only", role: sqlcgen.UserRole(""), want: fileCapabilities},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got := Capabilities(testCase.role)
			if !slices.Equal(got, testCase.want) {
				t.Fatalf("Capabilities(%q) = %v, want %v", testCase.role, got, testCase.want)
			}
		})
	}

	// A disabled account keeps its capabilities: the capability list describes the
	// role, and a disabled account is rejected during authentication instead.
	if !slices.Contains(Capabilities(sqlcgen.UserRoleOwner), "system.owner") {
		t.Fatal("Capabilities(owner) lost system.owner")
	}
}
