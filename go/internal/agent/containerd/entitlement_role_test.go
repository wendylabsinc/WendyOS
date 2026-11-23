package containerd

import (
	"context"
	"testing"

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
