package containerd

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func TestROS2AppReadsSensorsOnlyWithTheSensorReadEntitlement(t *testing.T) {
	camera := appconfig.Entitlement{Type: appconfig.EntitlementCamera}
	for name, tc := range map[string]struct {
		entitlements []appconfig.Entitlement
		want         bool
	}{
		"no entitlements":      {nil, false},
		"another entitlement":  {[]appconfig.Entitlement{camera}, false},
		"sensor-read":          {[]appconfig.Entitlement{{Type: appconfig.EntitlementSensorRead}}, true},
		"sensor-read and more": {[]appconfig.Entitlement{camera, {Type: appconfig.EntitlementSensorRead}}, true},
	} {
		// The labels a container is created with are all that is left of its
		// entitlements once it runs.
		labels := wendyLabels("app", "", "1", nil, tc.entitlements, "", nil)
		if got := ros2AppReadsSensors(labels); got != tc.want {
			t.Errorf("%s: reads sensors = %v, want %v", name, got, tc.want)
		}
	}
}
