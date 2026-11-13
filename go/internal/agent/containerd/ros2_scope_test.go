package containerd

import (
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func withPeripheralInterfaces(t *testing.T, names ...string) {
	t.Helper()
	previous := ros2PeripheralInterfaces
	ros2PeripheralInterfaces = func() ([]string, error) { return names, nil }
	t.Cleanup(func() { ros2PeripheralInterfaces = previous })
}

// lastEnv returns the effective (last) value of key in env, or "".
func lastEnv(env []string, key string) string {
	value := ""
	for _, entry := range env {
		if k, v, _ := strings.Cut(entry, "="); k == key {
			value = v
		}
	}
	return value
}

func TestBuildROS2Env_Scopes(t *testing.T) {
	withPeripheralInterfaces(t, "eth1")
	zero := 0
	networkCyclone := `<CycloneDDS><Domain><General><Interfaces><NetworkInterface name="lo" multicast="true"/><NetworkInterface autodetermine="true"/></Interfaces></General>` +
		`<Discovery><ParticipantIndex>auto</ParticipantIndex><Peers><Peer address="127.0.0.1"/></Peers></Discovery>` +
		`<SharedMemory><Enable>false</Enable></SharedMemory></Domain></CycloneDDS>`
	for _, tc := range []struct {
		name      string
		ros2      appconfig.ROS2Config
		localhost string
		cyclone   string
		domain    string
	}{
		{"app gets its own domain on loopback", appconfig.ROS2Config{Scope: "app"}, "1", cycloneDDSInlineConfig, ""},
		{"device shares loopback", appconfig.ROS2Config{Scope: "device", DomainID: &zero}, "1", cycloneDDSInlineConfig, "0"},
		{"default is device", appconfig.ROS2Config{DomainID: &zero}, "1", cycloneDDSInlineConfig, "0"},
		{"deprecated app is device", appconfig.ROS2Config{DiscoveryScope: "app", DomainID: &zero}, "1", cycloneDDSInlineConfig, "0"},
		{"peripherals adds the hardware interfaces", appconfig.ROS2Config{Scope: "peripherals", DomainID: &zero}, "0",
			`<CycloneDDS><Domain><General><Interfaces><NetworkInterface name="lo" multicast="true"/><NetworkInterface name="eth1"/></Interfaces></General>` +
				`<Discovery><ParticipantIndex>auto</ParticipantIndex><Peers><Peer address="127.0.0.1"/></Peers></Discovery>` +
				`<SharedMemory><Enable>false</Enable></SharedMemory></Domain></CycloneDDS>`, "0"},
		{"network adds loopback to the device's network", appconfig.ROS2Config{Scope: "network", DomainID: &zero}, "0", networkCyclone, "0"},
		{"deprecated host is network", appconfig.ROS2Config{DiscoveryScope: "host", DomainID: &zero}, "0", networkCyclone, "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ros2 := tc.ros2
			env := mustBuildROS2Env(t, &appconfig.AppConfig{Frameworks: &appconfig.FrameworksConfig{ROS2: &ros2}}, "com.example.app", "")
			if got := lastEnv(env, "ROS_LOCALHOST_ONLY"); got != tc.localhost {
				t.Errorf("ROS_LOCALHOST_ONLY = %q, want %q", got, tc.localhost)
			}
			wantRange := map[string]string{"1": "LOCALHOST", "0": "SUBNET"}[tc.localhost]
			if got := lastEnv(env, "ROS_AUTOMATIC_DISCOVERY_RANGE"); got != wantRange {
				t.Errorf("ROS_AUTOMATIC_DISCOVERY_RANGE = %q, want %q", got, wantRange)
			}
			if got := lastEnv(env, "CYCLONEDDS_URI"); got != tc.cyclone {
				t.Errorf("CYCLONEDDS_URI = %q, want %q", got, tc.cyclone)
			}
			wantDomain := tc.domain
			if wantDomain == "" {
				wantDomain = strings.TrimPrefix(appconfig.ROS2AnnotationValue(&ros2, "com.example.app"), "distro=humble,domain_id=")
			}
			if got := lastEnv(env, "ROS_DOMAIN_ID"); got != wantDomain {
				t.Errorf("ROS_DOMAIN_ID = %q, want %q", got, wantDomain)
			}
		})
	}
}

// A device with nothing attached, such as a Jetson whose only wired interface is
// its uplink, runs a peripherals app exactly like a device app.
func TestBuildROS2Env_PeripheralsWithoutHardwareIsDevice(t *testing.T) {
	withPeripheralInterfaces(t)
	zero := 0
	peripherals := mustBuildROS2Env(t, &appconfig.AppConfig{Frameworks: &appconfig.FrameworksConfig{
		ROS2: &appconfig.ROS2Config{Scope: "peripherals", DomainID: &zero}}}, "com.example.a", "")
	device := mustBuildROS2Env(t, &appconfig.AppConfig{Frameworks: &appconfig.FrameworksConfig{
		ROS2: &appconfig.ROS2Config{Scope: "device", DomainID: &zero}}}, "com.example.b", "")
	if !reflect.DeepEqual(peripherals, device) {
		t.Errorf("peripherals without hardware = %v, want device %v", peripherals, device)
	}
}

func TestROS2SidecarKeepsPeripheralInterfaces(t *testing.T) {
	limited := `<CycloneDDS><Domain><General><Interfaces><NetworkInterface name="lo"/><NetworkInterface name="eth1"/></Interfaces></General></Domain></CycloneDDS>`
	discovery := ros2AppDiscoveryEnv([]string{"ROS_LOCALHOST_ONLY=0", "ROS_AUTOMATIC_DISCOVERY_RANGE=SUBNET", "CYCLONEDDS_URI=" + limited})
	got := ros2WithDiscoveryEnv([]string{"PATH=/usr/bin", "CYCLONEDDS_URI=stale"}, discovery)
	if lastEnv(got, "CYCLONEDDS_URI") != limited || strings.Count(strings.Join(got, "\n"), "CYCLONEDDS_URI=") != 1 {
		t.Errorf("sidecar env = %v, want only the app's interface limit", got)
	}
	// A loopback app's CycloneDDS config is not needed: loopback already limits it.
	local := ros2AppDiscoveryEnv([]string{"ROS_LOCALHOST_ONLY=1", "CYCLONEDDS_URI=" + limited})
	if lastEnv(local, "CYCLONEDDS_URI") != "" {
		t.Errorf("loopback sidecar discovery = %v, want no CycloneDDS override", local)
	}
}
