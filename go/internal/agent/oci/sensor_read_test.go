package oci

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func TestSensorReadMountIsNotAdmin(t *testing.T) {
	original := SensorReadSocketHostPath
	t.Cleanup(func() { SensorReadSocketHostPath = original })
	// Darwin's Unix socket path limit is shorter than testing.T.TempDir here.
	dir, err := os.MkdirTemp("/tmp", "sensor-read-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	SensorReadSocketHostPath = filepath.Join(dir, "s")
	cfg := &appconfig.AppConfig{AppID: "reader", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementSensorRead}}}
	missing := DefaultSpec("/rootfs", []string{"/reader"})
	if err := ApplyEntitlements(missing, cfg, ApplyOptions{}); err == nil {
		t.Fatal("missing API silently accepted")
	}
	lis, err := net.Listen("unix", SensorReadSocketHostPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	spec := DefaultSpec("/rootfs", []string{"/reader"})
	if err := ApplyEntitlements(spec, cfg, ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if !hasMount(spec, "/run/wendy/sensors") || hasMount(spec, ctrAgentSocketDir) {
		t.Fatal("incorrect sensor/admin mount")
	}
	if !hasEnv(spec, "WENDY_SENSOR_SOCKET=/run/wendy/sensors/sensors.sock") || hasEnv(spec, "WENDY_AGENT_SOCKET="+ctrAgentSocketPath) {
		t.Fatal("incorrect socket environment")
	}
	if !hasGID(spec, SensorReadSocketGID) {
		t.Fatal("non-root reader lacks socket group")
	}
	for _, m := range spec.Mounts {
		if m.Destination == "/run/wendy/sensors" && m.Source != filepath.Dir(SensorReadSocketHostPath) {
			t.Fatal("must mount persistent sensor-only directory")
		}
	}
}
