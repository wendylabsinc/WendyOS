package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
)

const (
	testConfigPartBytes  = 262144 * 1024 // what the real descriptor declares
	testConfigSectorSize = 4096          // ditto — the UFS logical block size
)

// buildTestConfigImage seeds an image the way a flash would, minus the network
// step (seedConfigFS downloads the agent), so the agent is passed directly.
func buildTestConfigImage(t *testing.T, creds []wendyconf.WifiCredential, name string, prov []byte) string {
	t.Helper()
	img := filepath.Join(t.TempDir(), dragonwingConfigImageName)
	d, fs, err := newConfigFS(img, testConfigPartBytes, testConfigSectorSize)
	if err != nil {
		t.Fatalf("creating config image: %v", err)
	}
	if err := writeConfigFilesTo(fatWriter{fs}, []byte("\x7fELF-opaque"), creds, name, prov); err != nil {
		t.Fatalf("seeding config image: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("finishing config image: %v", err)
	}
	return img
}

func TestDragonwingConfigImageIsMountableAsConfig(t *testing.T) {
	// wendyos-config-init.sh stands down only when blkid reports vfat and the
	// label "config"; anything else is reformatted and the seed is lost. blkid
	// reads both from the boot sector, so assert the boot sector.
	img := buildTestConfigImage(t, nil, "", nil)

	if info, err := os.Stat(img); err != nil || info.Size() != testConfigPartBytes {
		t.Fatalf("image is %v bytes (err %v), want %d", info.Size(), err, testConfigPartBytes)
	}
	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	boot := make([]byte, 512)
	if _, err := f.ReadAt(boot, 0); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimRight(string(boot[71:82]), " "); got != dragonwingConfigLabel {
		t.Errorf("volume label = %q, want %q", got, dragonwingConfigLabel)
	}
	// Linux refuses a FAT whose logical sector size is under the device's.
	if got := int(boot[11]) | int(boot[12])<<8; got != testConfigSectorSize {
		t.Errorf("bytes per sector = %d, want %d", got, testConfigSectorSize)
	}
}

func TestDragonwingConfigImageCarriesTheSeed(t *testing.T) {
	creds := []wendyconf.WifiCredential{{SSID: "lab-5g", Password: "hunter2", Priority: 100}}
	img := buildTestConfigImage(t, creds, "bench-board", []byte(`{"enrolled":true}`))

	conf, err := readFATFile(t, img, "wendy.conf")
	if err != nil {
		t.Fatalf("reading wendy.conf: %v", err)
	}
	for _, want := range []string{"[wifi]", "ssid = lab-5g", "password = hunter2", "[device]", "name = bench-board"} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("wendy.conf missing %q:\n%s", want, conf)
		}
	}
	agent, err := readFATFile(t, img, "wendy-agent")
	if err != nil || string(agent) != "\x7fELF-opaque" {
		t.Errorf("agent binary = %q (err %v)", agent, err)
	}
	// Dragonwing has no usable RTC, so the clock floor is what keeps the agent
	// from opening TLS months in the past before NTP lands.
	if floor, err := readFATFile(t, img, "clock_floor"); err != nil || len(floor) == 0 {
		t.Errorf("clock_floor = %q (err %v)", floor, err)
	}
}
