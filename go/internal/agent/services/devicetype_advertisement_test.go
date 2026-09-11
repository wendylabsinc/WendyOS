package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// deviceTypeFixture writes the device-type and avahi service files into a temp
// dir and returns their paths. Empty content means the file is not created.
func deviceTypeFixture(t *testing.T, deviceType, serviceContent string) (deviceTypePath, servicePath string) {
	t.Helper()
	dir := t.TempDir()
	deviceTypePath = filepath.Join(dir, "device-type")
	servicePath = filepath.Join(dir, "wendyos-mdns.service")
	if deviceType != "" {
		if err := os.WriteFile(deviceTypePath, []byte(deviceType), 0o644); err != nil {
			t.Fatalf("writing device-type fixture: %v", err)
		}
	}
	if serviceContent != "" {
		if err := os.WriteFile(servicePath, []byte(serviceContent), 0o644); err != nil {
			t.Fatalf("writing avahi fixture: %v", err)
		}
	}
	return deviceTypePath, servicePath
}

func countRestarts(n *int) func() bool {
	return func() bool {
		*n++
		return true
	}
}

// The board id goes into the advertisement so the CLI can tell what a sighting
// is before it can reach the agent -- a VM's announcement may carry an
// address nothing on the host can dial.
func TestEnsureDeviceTypeAdvertisementAddsTheRecord(t *testing.T) {
	dtPath, svcPath := deviceTypeFixture(t, "BOARD=vm-arm64\nMACHINE=vm-arm64-wendyos\nSTORAGE=disk\n", avahiFixture)

	restarts := 0
	ensureDeviceTypeAdvertisement(zap.NewNop(), dtPath, svcPath, countRestarts(&restarts))

	got := readFile(t, svcPath)
	const record = "<txt-record>devicetype=vm-arm64</txt-record>"
	recordAt := strings.Index(got, record)
	if recordAt < 0 {
		t.Fatalf("devicetype TXT record not added:\n%s", got)
	}
	if typeAt := strings.Index(got, "<type>_wendyos._udp</type>"); typeAt > recordAt || strings.Index(got, "</service>") < recordAt {
		t.Errorf("devicetype record landed outside the _wendyos._udp block:\n%s", got)
	}
	// Records the agent owns elsewhere must survive untouched.
	for _, keep := range []string{"<txt-record>tls=true</txt-record>", "<port>50052</port>", "<txt-record>name=brave-dolphin</txt-record>"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%s was clobbered:\n%s", keep, got)
		}
	}
	if restarts != 1 {
		t.Errorf("avahi restarts = %d, want 1", restarts)
	}
}

// This runs on every agent start. Once the record is in place nothing may be
// written or restarted: a needless avahi restart drops the advertisement.
func TestEnsureDeviceTypeAdvertisementIsQuietWhenCurrent(t *testing.T) {
	current := strings.Replace(avahiFixture, "  </service>",
		"    <txt-record>devicetype=vm-arm64</txt-record>\n  </service>", 1)
	dtPath, svcPath := deviceTypeFixture(t, "BOARD=vm-arm64\n", current)

	restarts := 0
	ensureDeviceTypeAdvertisement(zap.NewNop(), dtPath, svcPath, countRestarts(&restarts))

	if got := readFile(t, svcPath); got != current {
		t.Errorf("service file rewritten although already current:\n%s", got)
	}
	if restarts != 0 {
		t.Errorf("avahi restarts = %d, want 0", restarts)
	}
}

func TestEnsureDeviceTypeAdvertisementReplacesAStaleRecord(t *testing.T) {
	stale := strings.Replace(avahiFixture, "  </service>",
		"    <txt-record>devicetype=old-board</txt-record>\n  </service>", 1)
	dtPath, svcPath := deviceTypeFixture(t, "BOARD=vm-arm64\n", stale)

	restarts := 0
	ensureDeviceTypeAdvertisement(zap.NewNop(), dtPath, svcPath, countRestarts(&restarts))

	got := readFile(t, svcPath)
	if !strings.Contains(got, "<txt-record>devicetype=vm-arm64</txt-record>") || strings.Contains(got, "old-board") {
		t.Errorf("stale devicetype record not replaced:\n%s", got)
	}
	if strings.Count(got, "devicetype=") != 1 {
		t.Errorf("want exactly one devicetype record:\n%s", got)
	}
	if restarts != 1 {
		t.Errorf("avahi restarts = %d, want 1", restarts)
	}
}

// Older images wrote the board as a bare string; the record still has to say
// what it is.
func TestEnsureDeviceTypeAdvertisementReadsLegacyPlainDeviceType(t *testing.T) {
	dtPath, svcPath := deviceTypeFixture(t, "jetson-orin-nano\n", avahiFixture)

	ensureDeviceTypeAdvertisement(zap.NewNop(), dtPath, svcPath, func() bool { return true })

	if got := readFile(t, svcPath); !strings.Contains(got, "<txt-record>devicetype=jetson-orin-nano</txt-record>") {
		t.Errorf("legacy device-type not advertised:\n%s", got)
	}
}

// No device-type file (an image that never wrote one, or a desktop install)
// means nothing to advertise and nothing to touch.
func TestEnsureDeviceTypeAdvertisementWithoutDeviceType(t *testing.T) {
	dtPath, svcPath := deviceTypeFixture(t, "", avahiFixture)

	restarts := 0
	ensureDeviceTypeAdvertisement(zap.NewNop(), dtPath, svcPath, countRestarts(&restarts))

	if got := readFile(t, svcPath); got != avahiFixture {
		t.Errorf("service file changed with no device type known:\n%s", got)
	}
	if restarts != 0 {
		t.Errorf("avahi restarts = %d, want 0", restarts)
	}
}

// A host with no avahi service file (the Linux desktop install) is not an
// error and must not restart anything.
func TestEnsureDeviceTypeAdvertisementMissingServiceFile(t *testing.T) {
	dtPath, svcPath := deviceTypeFixture(t, "BOARD=vm-arm64\n", "")

	restarts := 0
	ensureDeviceTypeAdvertisement(zap.NewNop(), dtPath, svcPath, countRestarts(&restarts))

	if _, err := os.Stat(svcPath); !os.IsNotExist(err) {
		t.Errorf("a service file was created where none existed (stat err %v)", err)
	}
	if restarts != 0 {
		t.Errorf("avahi restarts = %d, want 0", restarts)
	}
}

// Only the WendyOS block is ours; a second service in the same file (SSH,
// say) must not grow a devicetype record.
func TestEnsureDeviceTypeAdvertisementLeavesOtherServiceBlocksAlone(t *testing.T) {
	withSSH := strings.Replace(avahiFixture, "</service-group>",
		"  <service>\n    <type>_ssh._tcp</type>\n    <port>22</port>\n  </service>\n</service-group>", 1)
	dtPath, svcPath := deviceTypeFixture(t, "BOARD=vm-arm64\n", withSSH)

	ensureDeviceTypeAdvertisement(zap.NewNop(), dtPath, svcPath, func() bool { return true })

	got := readFile(t, svcPath)
	if strings.Count(got, "devicetype=") != 1 {
		t.Errorf("want exactly one devicetype record:\n%s", got)
	}
	sshAt := strings.Index(got, "<type>_ssh._tcp</type>")
	if recordAt := strings.Index(got, "devicetype="); recordAt > sshAt {
		t.Errorf("devicetype record landed in the SSH block:\n%s", got)
	}
}
