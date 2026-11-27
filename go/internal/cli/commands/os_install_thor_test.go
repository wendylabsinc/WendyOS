package commands

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/flashpack"
)

// The usbAccessHintBox tells tarball users to install usbUdevRule verbatim, and
// the deb/rpm install packaging/linux/udev/70-wendy-jetson.rules. The two must
// stay identical: /etc/udev/rules.d overrides /usr/lib/udev/rules.d for the same
// filename, so a stale hint-box copy would permanently mask the packaged rule.
func TestUsbUdevRuleMatchesPackagedRule(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "packaging", "linux", "udev", "70-wendy-jetson.rules")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading packaged udev rule: %v", err)
	}
	var rules []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			rules = append(rules, line)
		}
	}
	if len(rules) != 1 || rules[0] != usbUdevRule {
		t.Fatalf("packaged udev rule diverged from usbUdevRule:\npackaged: %q\nconst:    %q", rules, usbUdevRule)
	}
}

func TestUsbAccessHintLines(t *testing.T) {
	// macOS: a claim-time seize denial. The device enumerated, so the guidance must
	// point at the two fixes that work — running as root and re-entering recovery —
	// not the Linux udev rule.
	mac := strings.ToLower(strings.Join(usbAccessHintLines("darwin"), "\n"))
	if !strings.Contains(mac, "sudo") {
		t.Errorf("darwin hint should mention sudo, got:\n%s", mac)
	}
	if !strings.Contains(mac, "recovery mode") {
		t.Errorf("darwin hint should tell the user to re-enter recovery mode, got:\n%s", mac)
	}
	if strings.Contains(mac, usbUdevRule) {
		t.Errorf("darwin hint should not contain the Linux udev rule, got:\n%s", mac)
	}

	// Linux: keep the udev-rule guidance verbatim so the parity test stays meaningful.
	lin := strings.Join(usbAccessHintLines("linux"), "\n")
	if !strings.Contains(lin, usbUdevRule) {
		t.Errorf("linux hint should contain the udev rule %q, got:\n%s", usbUdevRule, lin)
	}
}

func TestStopADBServer(t *testing.T) {
	// No server listening → no-op, false.
	if stopADBServer("127.0.0.1:1") {
		t.Fatal("stopADBServer should return false when nothing is listening")
	}

	// A fake adb server: accept one connection, record the request, reply OKAY.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			got <- ""
			return
		}
		defer conn.Close()
		buf := make([]byte, len("0009host:kill"))
		io.ReadFull(conn, buf)
		conn.Write([]byte("OKAY"))
		got <- string(buf)
	}()

	if !stopADBServer(ln.Addr().String()) {
		t.Fatal("stopADBServer should return true when a server is contacted")
	}
	if req := <-got; req != "0009host:kill" {
		t.Fatalf("server received %q, want %q", req, "0009host:kill")
	}
}

func TestByteProgress(t *testing.T) {
	const gib = 1 << 30
	if got := byteProgress(gib*3/2, gib*3); got != "50% · 1.5/3.0 GiB" {
		t.Errorf("byteProgress = %q, want %q", got, "50% · 1.5/3.0 GiB")
	}
	// Unknown total: report only what's transferred.
	if got := byteProgress(gib/2, 0); got != "0.5 GiB" {
		t.Errorf("byteProgress unknown total = %q, want %q", got, "0.5 GiB")
	}
	// At (or past) the estimated total the percent pins at 99: the ✓ marks done.
	if got := byteProgress(gib*4, gib*3); got != "99% · 4.0/3.0 GiB" {
		t.Errorf("byteProgress overshoot = %q, want %q", got, "99% · 4.0/3.0 GiB")
	}
}

func TestFlashpackCached(t *testing.T) {
	dir := t.TempDir()
	const version = "nightly-20260701T135030"

	if flashpackCached(dir, version) {
		t.Fatal("empty cache should not report cached")
	}

	// A downloaded tarball counts as cached.
	tarball := flashpack.TarballCachePath(dir, version)
	if err := os.WriteFile(tarball, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !flashpackCached(dir, version) {
		t.Fatal("tarball present should report cached")
	}

	// An already-extracted tree also counts.
	os.Remove(tarball)
	if err := os.MkdirAll(filepath.Join(dir, flashpack.FlashpackName(version)), 0o755); err != nil {
		t.Fatal(err)
	}
	if !flashpackCached(dir, version) {
		t.Fatal("extracted tree should report cached")
	}
}

func TestOfflineThorVersion(t *testing.T) {
	cases := []struct {
		version string
		pr      int
		want    string
	}{
		{"0.17.0", 0, "0.17.0"}, // explicit version wins
		{"0.17.0", 5, "0.17.0"}, // ...even alongside --pr
		{"", 189, "pr-189"},     // --pr resolves to the conventional tag
		{"", 0, ""},             // bare latest/nightly needs the manifest
	}
	for _, c := range cases {
		if got := offlineThorVersion(c.version, c.pr); got != c.want {
			t.Errorf("offlineThorVersion(%q, %d) = %q, want %q", c.version, c.pr, got, c.want)
		}
	}
}

func TestThorFlashpackSpaceNeeded(t *testing.T) {
	dir := t.TempDir()
	const version = "0.16.1"

	// Nothing cached, no manifest info: unknown size, skip the check.
	if got := thorFlashpackSpaceNeeded(dir, thorFlashPlan{version: version}); got != 0 {
		t.Fatalf("unknown size should return 0, got %d", got)
	}

	// Nothing cached, manifest size known: download + extraction.
	plan := thorFlashPlan{version: version, info: &thorFlashpackInfo{SizeBytes: 1000}}
	if got := thorFlashpackSpaceNeeded(dir, plan); got != 3500 {
		t.Fatalf("download+extract estimate = %d, want 3500", got)
	}

	// Tarball cached: extraction only, sized from the tarball on disk.
	tarball := flashpack.TarballCachePath(dir, version)
	if err := os.WriteFile(tarball, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := thorFlashpackSpaceNeeded(dir, plan); got != 250 {
		t.Fatalf("extract-only estimate = %d, want 250", got)
	}

	// Extracted tree present: nothing left to write.
	if err := os.MkdirAll(filepath.Join(dir, flashpack.FlashpackName(version)), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := thorFlashpackSpaceNeeded(dir, plan); got != 0 {
		t.Fatalf("extracted tree should return 0, got %d", got)
	}
}

func TestCheckThorDiskSpace(t *testing.T) {
	dir := t.TempDir()
	const version = "0.16.1"

	// Plenty of room (tiny requirement) passes.
	plan := thorFlashPlan{version: version, info: &thorFlashpackInfo{SizeBytes: 1}}
	if err := checkThorDiskSpace(dir, plan); err != nil {
		t.Fatalf("tiny flashpack should fit: %v", err)
	}

	// An absurd requirement fails with a readable message.
	plan.info.SizeBytes = 1 << 60
	err := checkThorDiskSpace(dir, plan)
	if err == nil {
		t.Fatal("1 EiB flashpack should not fit")
	}
	if !strings.Contains(err.Error(), "not enough free disk space") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestRunFlashStepsPlain_Sequencing(t *testing.T) {
	// runFlashSteps falls back to the plain runner in a non-TTY test env.
	var ran []int
	steps := []flashStep{
		{id: stepDownload, label: "Download flashpack", run: func(out io.Writer, detail func(string)) (bool, error) {
			ran = append(ran, stepDownload)
			return true, nil // cached
		}},
		{id: stepStage1, label: "Stage 1", run: func(out io.Writer, detail func(string)) (bool, error) {
			ran = append(ran, stepStage1)
			return false, nil
		}},
		{id: stepStage2, label: "Stage 2", run: func(out io.Writer, detail func(string)) (bool, error) {
			ran = append(ran, stepStage2)
			return false, nil
		}},
	}

	failedID, err := runFlashSteps("Flashing", steps, func() {}, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if failedID != -1 {
		t.Fatalf("failedID = %d, want -1", failedID)
	}
	if len(ran) != 3 || ran[0] != stepDownload || ran[2] != stepStage2 {
		t.Fatalf("steps ran out of order: %v", ran)
	}
}

func TestRunFlashStepsPlain_StopsOnFailure(t *testing.T) {
	sentinel := errors.New("stage 1 boom")
	var reachedStage2 bool
	steps := []flashStep{
		{id: stepDownload, label: "Download flashpack", run: func(out io.Writer, detail func(string)) (bool, error) {
			return false, nil
		}},
		{id: stepStage1, label: "Stage 1", run: func(out io.Writer, detail func(string)) (bool, error) {
			return false, sentinel
		}},
		{id: stepStage2, label: "Stage 2", run: func(out io.Writer, detail func(string)) (bool, error) {
			reachedStage2 = true
			return false, nil
		}},
	}

	failedID, err := runFlashSteps("Flashing", steps, func() {}, io.Discard)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if failedID != stepStage1 {
		t.Fatalf("failedID = %d, want %d (stepStage1)", failedID, stepStage1)
	}
	if reachedStage2 {
		t.Fatal("stage 2 should not run after stage 1 fails")
	}
}
