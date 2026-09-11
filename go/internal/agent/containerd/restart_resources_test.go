package containerd

import (
	"context"
	"errors"
	"reflect"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/dbusproxy"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

type recordingProxyManager struct {
	started []string
	stopped []string
	dir     string
	err     error
}

func (m *recordingProxyManager) Start(_ context.Context, appID string) (string, error) {
	m.started = append(m.started, appID)
	if m.err != nil {
		return "", m.err
	}
	if m.dir != "" {
		return m.dir, nil
	}
	return dbusproxy.SocketDir(appID), nil
}

func (m *recordingProxyManager) Stop(appID string) error {
	m.stopped = append(m.stopped, appID)
	return nil
}

func (m *recordingProxyManager) StopAll() {}

func TestEnsureDBusProxyForStartRecreatesPersistedProxy(t *testing.T) {
	proxy := &recordingProxyManager{}
	client := &Client{logger: zap.NewNop(), proxyManager: proxy}
	mounts := []specs.Mount{{
		Destination: "/var/run/dbus",
		Source:      dbusproxy.SocketDir("rumble_ui"),
		Type:        "bind",
	}}

	started, err := client.ensureDBusProxyForStart(context.Background(), "rumble_ui", mounts)
	if err != nil {
		t.Fatalf("ensureDBusProxyForStart() error = %v", err)
	}
	if !started {
		t.Fatal("ensureDBusProxyForStart() started = false, want true")
	}
	if !reflect.DeepEqual(proxy.started, []string{"rumble_ui"}) {
		t.Fatalf("proxy starts = %v, want [rumble_ui]", proxy.started)
	}
}

func TestEnsureDBusProxyForStartRejectsMissingManager(t *testing.T) {
	client := &Client{logger: zap.NewNop()}
	mounts := []specs.Mount{{
		Destination: "/var/run/dbus",
		Source:      dbusproxy.SocketDir("rumble_ui"),
		Type:        "bind",
	}}

	if _, err := client.ensureDBusProxyForStart(context.Background(), "rumble_ui", mounts); err == nil {
		t.Fatal("ensureDBusProxyForStart() error = nil, want missing proxy manager error")
	}
}

func TestEnsureDBusProxyForStartIgnoresContainersWithoutBluetoothMount(t *testing.T) {
	proxy := &recordingProxyManager{}
	client := &Client{logger: zap.NewNop(), proxyManager: proxy}

	started, err := client.ensureDBusProxyForStart(context.Background(), "plain", nil)
	if err != nil {
		t.Fatalf("ensureDBusProxyForStart() error = %v", err)
	}
	if started || len(proxy.started) != 0 {
		t.Fatalf("non-Bluetooth start unexpectedly touched proxy: started=%v calls=%v", started, proxy.started)
	}
}

func TestRestoreDBusProxyForRunningTaskAfterAgentRestart(t *testing.T) {
	proxy := &recordingProxyManager{}
	client := &Client{logger: zap.NewNop(), proxyManager: proxy}
	mounts := []specs.Mount{{
		Destination: "/var/run/dbus",
		Source:      dbusproxy.SocketDir("rumble_ui"),
		Type:        "bind",
	}}

	started, err := client.restoreDBusProxyForRunningTask(context.Background(), "rumble_ui", true, mounts)
	if err != nil {
		t.Fatalf("restoreDBusProxyForRunningTask() error = %v", err)
	}
	if !started || !reflect.DeepEqual(proxy.started, []string{"rumble_ui"}) {
		t.Fatalf("running task proxy restore = started %v, calls %v", started, proxy.started)
	}

	started, err = client.restoreDBusProxyForRunningTask(context.Background(), "stopped", false, mounts)
	if err != nil || started || len(proxy.started) != 1 {
		t.Fatalf("stopped task unexpectedly restored proxy: started=%v calls=%v err=%v", started, proxy.started, err)
	}
}

func TestCaptureAndRefreshSerialIdentityAfterRenumber(t *testing.T) {
	origGlob := globSerialByID
	origEval := evalSerialPath
	origStat := statSerialPath
	defer func() {
		globSerialByID = origGlob
		evalSerialPath = origEval
		statSerialPath = origStat
	}()

	globSerialByID = func(string) ([]string, error) {
		return []string{"/dev/serial/by-id/usb-Espressif_display-if00"}, nil
	}
	evalSerialPath = func(path string) (string, error) {
		switch path {
		case "/dev/serial/by-id/usb-Espressif_display-if00":
			return "/dev/ttyACM0", nil
		default:
			return "", errors.New("unexpected path")
		}
	}
	statSerialPath = func(path string) (int64, int64, error) {
		if path == "/dev/ttyACM0" {
			return 166, 0, nil
		}
		return 0, 0, errors.New("not found")
	}

	identities := captureSerialIdentities([]appconfig.Entitlement{{
		Type:   appconfig.EntitlementSerial,
		Device: "ttyACM0",
	}})
	if got := identities["ttyACM0"]; got != "usb-Espressif_display-if00" {
		t.Fatalf("captured identity = %q, want usb-Espressif_display-if00", got)
	}

	// The same USB device is now ttyACM1. The container destination stays
	// ttyACM0 for compatibility, while its host source and cgroup minor move.
	evalSerialPath = func(path string) (string, error) {
		if path == "/dev/serial/by-id/usb-Espressif_display-if00" {
			return "/dev/ttyACM1", nil
		}
		return "", errors.New("unexpected path")
	}
	statSerialPath = func(path string) (int64, int64, error) {
		if path == "/dev/ttyACM1" {
			return 166, 1, nil
		}
		return 0, 0, errors.New("not found")
	}
	major, oldMinor := int64(166), int64(0)
	spec := &specs.Spec{
		Mounts: []specs.Mount{{
			Destination: "/dev/ttyACM0",
			Source:      "/dev/ttyACM0",
			Type:        "bind",
		}},
		Linux: &specs.Linux{
			Resources: &specs.LinuxResources{
				Devices: []specs.LinuxDeviceCgroup{{
					Allow:  true,
					Type:   "c",
					Major:  &major,
					Minor:  &oldMinor,
					Access: "rw",
				}},
			},
		},
	}

	changed, err := refreshSerialMountsForStart(spec, identities)
	if err != nil {
		t.Fatalf("refreshSerialMountsForStart() error = %v", err)
	}
	if !changed {
		t.Fatal("refreshSerialMountsForStart() changed = false, want true")
	}
	if got := spec.Mounts[0].Source; got != "/dev/ttyACM1" {
		t.Fatalf("mount source = %q, want /dev/ttyACM1", got)
	}
	if got := spec.Mounts[0].Destination; got != "/dev/ttyACM0" {
		t.Fatalf("mount destination = %q, want stable /dev/ttyACM0", got)
	}
	if got := *spec.Linux.Resources.Devices[0].Minor; got != 1 {
		t.Fatalf("cgroup minor = %d, want 1", got)
	}
}

func TestRefreshSerialMountsFailsClearlyWhenIdentityIsUnavailable(t *testing.T) {
	origStat := statSerialPath
	defer func() { statSerialPath = origStat }()
	statSerialPath = func(string) (int64, int64, error) {
		return 0, 0, errors.New("not found")
	}
	spec := &specs.Spec{Mounts: []specs.Mount{{
		Destination: "/dev/ttyACM0",
		Source:      "/dev/ttyACM0",
		Type:        "bind",
	}}}

	if _, err := refreshSerialMountsForStart(spec, nil); err == nil {
		t.Fatal("refreshSerialMountsForStart() error = nil, want missing stable identity error")
	}
}

func TestRefreshSerialMountsWithoutIdentityKeepsCompatibleExistingPath(t *testing.T) {
	origStat := statSerialPath
	defer func() { statSerialPath = origStat }()
	statSerialPath = func(path string) (int64, int64, error) {
		if path != "/dev/ttyUSB0" {
			return 0, 0, errors.New("unexpected path")
		}
		return 188, 0, nil
	}
	spec := &specs.Spec{Mounts: []specs.Mount{{
		Destination: "/dev/ttyUSB0",
		Source:      "/dev/ttyUSB0",
		Type:        "bind",
	}}}

	changed, err := refreshSerialMountsForStart(spec, nil)
	if err != nil {
		t.Fatalf("refreshSerialMountsForStart() error = %v", err)
	}
	if changed {
		t.Fatal("refreshSerialMountsForStart() changed = true, want unchanged fallback path")
	}
}
