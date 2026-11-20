//go:build linux

package commands

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
)

// TestHW_WriteConfigPartition drives the real writeConfigPartition against
// WENDY_HW_DISK, a disk carrying a partition labelled "config" (e.g. a freshly
// written Jetson SD card), and overwrites that partition's provisioning files.
// It needs sudo, like `wendy os install`:
//
//	WENDY_HW_DISK=/dev/mmcblk0 go test -count=1 ./go/internal/cli/commands/ -run TestHW_ -v
func TestHW_WriteConfigPartition(t *testing.T) {
	disk := os.Getenv("WENDY_HW_DISK")
	if disk == "" {
		t.Skip("set WENDY_HW_DISK=/dev/<disk> to run against real hardware")
	}

	agent := []byte("\x7fELF hw-test agent")
	creds := []wendyconf.WifiCredential{{SSID: "hw-test-ssid", Password: "hw-test-secret", Security: "wpa2"}}
	const deviceName = "brave-dolphin"

	start := time.Now()
	if err := writeConfigPartition(drive{DevicePath: disk}, configAgent{data: agent}, creds, deviceName, nil); err != nil {
		t.Fatalf("writeConfigPartition: %v", err)
	}
	t.Logf("provisioned in %s", time.Since(start).Round(time.Millisecond))

	partDev, err := findConfigPartition(disk)
	if err != nil {
		t.Fatalf("findConfigPartition after write: %v", err)
	}
	// Not t.TempDir: its RemoveAll would delete the card's files if the umount
	// failed. Read-write, because a desktop automount of the card already holds
	// the volume read-write and the kernel refuses a read-only second mount.
	dir, err := os.MkdirTemp("", "wendy-hw-config-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(dir)
	opts := fmt.Sprintf("uid=%d,gid=%d", os.Getuid(), os.Getgid())
	if out, err := exec.Command("sudo", "mount", "-t", "vfat", "-o", opts, partDev, dir).CombinedOutput(); err != nil {
		t.Fatalf("mounting %s to verify: %s: %v", partDev, strings.TrimSpace(string(out)), err)
	}
	defer exec.Command("sudo", "umount", dir).Run() //nolint:errcheck

	gotAgent, err := os.ReadFile(filepath.Join(dir, "wendy-agent"))
	if err != nil || !bytes.Equal(gotAgent, agent) {
		t.Errorf("wendy-agent: got %q (%v), want %q", gotAgent, err, agent)
	}
	conf, err := os.ReadFile(filepath.Join(dir, "wendy.conf"))
	if err != nil {
		t.Fatalf("reading wendy.conf: %v", err)
	}
	for _, want := range []string{"hw-test-ssid", "name = " + deviceName} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("wendy.conf lacks %q:\n%s", want, conf)
		}
	}
}
