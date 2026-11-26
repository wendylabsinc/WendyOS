package containerd

import (
	"context"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"

	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func TestDeployEntitlementsByCallerRole(t *testing.T) {
	bg := context.Background()
	as := func(r trustedkeys.Role) context.Context { return trustedkeys.WithCallerRole(bg, r) }
	for _, c := range []struct {
		name string
		ctx  context.Context
		ent  string
		ok   bool
	}{
		{"keyless admin", as(trustedkeys.RoleNone), appconfig.EntitlementAdmin, false},
		{"keyless build", as(trustedkeys.RoleNone), appconfig.EntitlementBuild, false},
		{"keyless gpu", as(trustedkeys.RoleNone), appconfig.EntitlementGPU, true},
		{"operator admin", as(trustedkeys.RoleOperator), appconfig.EntitlementAdmin, true},
		{"operator build", as(trustedkeys.RoleOperator), appconfig.EntitlementBuild, false},
		{"owner build", as(trustedkeys.RoleOwner), appconfig.EntitlementBuild, true},
		{"open or enrolled build", bg, appconfig.EntitlementBuild, true},
	} {
		cfg := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: c.ent}}}
		if err := entitlementRoleError(c.ctx, cfg); (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want allowed=%v", c.name, err, c.ok)
		}
	}
}

// Exec and attach run with the app's privileges, so they need the role that
// could have deployed its entitlements, read back from its labels.
func TestExecAndAttachNeedTheRoleTheAppsEntitlementsNeed(t *testing.T) {
	bg := context.Background()
	as := func(r trustedkeys.Role) context.Context { return trustedkeys.WithCallerRole(bg, r) }
	build := retirementContainerForTest{id: "app", labels: wendyLabels("app", "", "1", nil,
		[]appconfig.Entitlement{{Type: appconfig.EntitlementBuild}}, "", nil)}
	for _, c := range []struct {
		name string
		ctx  context.Context
		ok   bool
	}{
		{"keyless", as(trustedkeys.RoleNone), false},
		{"operator", as(trustedkeys.RoleOperator), false},
		{"owner", as(trustedkeys.RoleOwner), true},
		{"open or enrolled", bg, true},
	} {
		if err := roleMayDrive(c.ctx, build); (err == nil) != c.ok {
			t.Errorf("%s into a build app: err=%v, want allowed=%v", c.name, err, c.ok)
		}
	}
	plain := retirementContainerForTest{id: "app", labels: wendyLabels("app", "", "1", nil, nil, "", nil)}
	if err := roleMayDrive(as(trustedkeys.RoleOperator), plain); err != nil {
		t.Errorf("operator into a plain app: %v", err)
	}
}

// A push that re-points the image an app was made from runs on the app's
// next restart with the app's entitlements, so it needs the role that could
// have deployed them.
func TestPushOverAnAppNeedsTheRoleItsEntitlementsNeed(t *testing.T) {
	bg := context.Background()
	as := func(r trustedkeys.Role) context.Context { return trustedkeys.WithCallerRole(bg, r) }
	app := func(ent string) []containerd.Container {
		var ents []appconfig.Entitlement
		if ent != "" {
			ents = []appconfig.Entitlement{{Type: ent}}
		}
		return []containerd.Container{retirementContainerForTest{id: "app", labels: wendyLabels("app", "", "1", nil, ents, "", nil)}}
	}
	for _, c := range []struct {
		name string
		ctx  context.Context
		ent  string
		ok   bool
	}{
		{"keyless over admin", as(trustedkeys.RoleNone), appconfig.EntitlementAdmin, false},
		{"keyless over plain", as(trustedkeys.RoleNone), "", true},
		{"keyless over gpu", as(trustedkeys.RoleNone), appconfig.EntitlementGPU, true},
		{"operator over build", as(trustedkeys.RoleOperator), appconfig.EntitlementBuild, false},
		{"owner over admin", as(trustedkeys.RoleOwner), appconfig.EntitlementAdmin, true},
		{"owner over build", as(trustedkeys.RoleOwner), appconfig.EntitlementBuild, true},
	} {
		if err := imageReplaceError(c.ctx, app(c.ent)); (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want allowed=%v", c.name, err, c.ok)
		}
	}
	// Open and Enrolled devices carry no role and look nothing up.
	if err := ImageReplaceError(bg, nil, "localhost/app:latest"); err != nil {
		t.Errorf("open or enrolled: %v", err)
	}
}
