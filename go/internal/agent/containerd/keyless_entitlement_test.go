package containerd

import (
	"context"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func TestKeylessDeployRefusesAdminAndBuild(t *testing.T) {
	keyless := trustedkeys.WithKeyless(context.Background())
	for _, e := range []string{appconfig.EntitlementAdmin, appconfig.EntitlementBuild} {
		cfg := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: e}}}
		if keylessEntitlementError(keyless, cfg) == nil {
			t.Errorf("keyless deploy with %s accepted", e)
		}
		if err := keylessEntitlementError(context.Background(), cfg); err != nil {
			t.Errorf("keyed deploy with %s refused: %v", e, err)
		}
	}
	gpu := &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementGPU}}}
	if err := keylessEntitlementError(keyless, gpu); err != nil {
		t.Errorf("keyless deploy with gpu refused: %v", err)
	}
}
