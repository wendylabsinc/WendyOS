package configpartition

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/network"
	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
)

// fakeApply stands in for NetworkManager and /etc. addErr returns the error
// for each call registering an SSID; nil means success.
type fakeApply struct {
	addErr    func(ssid string, call int) error
	nameErr   error
	added     []network.SavedCredential
	activated []string
	names     []string
	slept     []time.Duration
}

func stubApply(t *testing.T, f *fakeApply) {
	t.Helper()
	prevAdd, prevActivate, prevName, prevSleep := addProfileFn, activateProfileFn, applyDeviceNameFn, sleepFn
	t.Cleanup(func() {
		addProfileFn, activateProfileFn, applyDeviceNameFn, sleepFn = prevAdd, prevActivate, prevName, prevSleep
	})
	calls := map[string]int{}
	addProfileFn = func(_ context.Context, c network.SavedCredential) error {
		f.added = append(f.added, c)
		calls[c.SSID]++
		if f.addErr == nil {
			return nil
		}
		return f.addErr(c.SSID, calls[c.SSID])
	}
	activateProfileFn = func(_ context.Context, ssid string) error {
		f.activated = append(f.activated, ssid)
		return nil
	}
	applyDeviceNameFn = func(_ *zap.Logger, name string) error {
		f.names = append(f.names, name)
		return f.nameErr
	}
	sleepFn = func(d time.Duration) { f.slept = append(f.slept, d) }
}

// nmNotRunning is what AddOrUpdateProfile returns while NetworkManager is down:
// nmcli's exit status 8, wrapped.
func nmNotRunning(t *testing.T) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit 8").Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("sh exit 8 = %v; want an exit error", err)
	}
	return fmt.Errorf("nmcli connection show: %w", err)
}

const threeNetworks = `[wifi]
ssid = Home
password = home-pass
priority = 30

[wifi.2]
ssid = Office
password = office-pass
priority = 20
hidden = true
security = wpa3

[wifi.3]
ssid = Cafe
password = cafe-pass
priority = 10

[device]
name = lab-nano
`

func writeConf(t *testing.T, content string) (cfgDir, confPath string) {
	t.Helper()
	cfgDir = t.TempDir()
	confPath = filepath.Join(cfgDir, "wendy.conf")
	if err := os.WriteFile(confPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgDir, confPath
}

func readAttempts(t *testing.T, configPath string) (int, string, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(configPath, wendyConfAttemptsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	var n int
	var sum string
	if _, err := fmt.Sscanf(string(raw), "%d %s", &n, &sum); err != nil {
		t.Fatalf("attempt counter %q: %v", raw, err)
	}
	return n, sum, true
}

func assertGone(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s still exists (err=%v)", p, err)
		}
	}
}

func TestApplyWendyConfDeletesFileWhenEverythingApplied(t *testing.T) {
	f := &fakeApply{}
	stubApply(t, f)
	cfgDir, confPath := writeConf(t, threeNetworks)
	configPath := t.TempDir()
	// A counter from an earlier partial apply goes with the file.
	if err := os.WriteFile(filepath.Join(configPath, wendyConfAttemptsFile), []byte("2 -\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	applyWendyConf(zap.NewNop(), cfgDir, configPath)

	assertGone(t, confPath, confPath+".new", filepath.Join(configPath, wendyConfAttemptsFile))
	if len(f.added) != 3 || f.added[1].SSID != "Office" || !f.added[1].Hidden || f.added[1].Security != "wpa3" {
		t.Errorf("registered %+v; want Home, Office (hidden, wpa3), Cafe", f.added)
	}
	if strings.Join(f.activated, ",") != "Home" {
		t.Errorf("activated %v; want only the highest priority, Home", f.activated)
	}
	if strings.Join(f.names, ",") != "lab-nano" {
		t.Errorf("applied names %v; want lab-nano", f.names)
	}
	if len(f.slept) != 0 {
		t.Errorf("slept %v with NetworkManager up", f.slept)
	}
}

func TestApplyWendyConfKeepsOnlyWhatNetworkManagerCouldNotTake(t *testing.T) {
	notRunning := nmNotRunning(t)
	f := &fakeApply{addErr: func(ssid string, _ int) error {
		switch ssid {
		case "Office":
			return notRunning
		case "Cafe":
			return errors.New("nmcli connection add Cafe: Error: invalid psk: exit status 2")
		}
		return nil
	}}
	stubApply(t, f)
	cfgDir, confPath := writeConf(t, threeNetworks)
	configPath := t.TempDir()

	applyWendyConf(zap.NewNop(), cfgDir, configPath)

	data, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("wendy.conf was not kept: %v", err)
	}
	sections := parseINI(data)
	got := wendyconf.UnmarshalWiFi(sections)
	want := []wendyconf.WifiCredential{{SSID: "Office", Password: "office-pass", Priority: 20, Hidden: true, Security: "wpa3"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("kept networks %+v; want only %+v", got, want)
	}
	if _, ok := sections["device"]; ok {
		t.Errorf("the applied device name was kept:\n%s", data)
	}
	assertGone(t, confPath+".new")
	n, sum, ok := readAttempts(t, configPath)
	if !ok || n != 1 || sum != wendyConfSum(data) {
		t.Errorf("attempt counter = %d %q (present %v); want 1 and the kept file's hash", n, sum, ok)
	}
	var total time.Duration
	for _, d := range f.slept {
		total += d
	}
	if total == 0 || total > nmWaitBudget {
		t.Errorf("waited %v for NetworkManager; want some wait within %v", total, nmWaitBudget)
	}
}

func TestApplyWendyConfWaitsForNetworkManagerToStart(t *testing.T) {
	notRunning := nmNotRunning(t)
	f := &fakeApply{addErr: func(_ string, call int) error {
		if call <= 2 {
			return notRunning
		}
		return nil
	}}
	stubApply(t, f)
	cfgDir, confPath := writeConf(t, "[wifi]\nssid = Home\npassword = home-pass\n")
	configPath := t.TempDir()

	applyWendyConf(zap.NewNop(), cfgDir, configPath)

	assertGone(t, confPath, filepath.Join(configPath, wendyConfAttemptsFile))
	if fmt.Sprint(f.slept) != "[1s 2s]" {
		t.Errorf("slept %v; want [1s 2s]", f.slept)
	}
}

func TestApplyWendyConfKeepsWithoutWaitingWhenNMCLIIsMissing(t *testing.T) {
	f := &fakeApply{addErr: func(string, int) error { return network.ErrNMCLINotFound }}
	stubApply(t, f)
	cfgDir, confPath := writeConf(t, "[wifi]\nssid = Home\npassword = home-pass\n")
	configPath := t.TempDir()

	applyWendyConf(zap.NewNop(), cfgDir, configPath)

	if len(f.slept) != 0 || len(f.added) != 1 {
		t.Errorf("tried %d times and slept %v; want one try and no wait", len(f.added), f.slept)
	}
	if _, err := os.Stat(confPath); err != nil {
		t.Fatalf("wendy.conf was not kept for a later boot: %v", err)
	}
	if n, _, _ := readAttempts(t, configPath); n != 1 {
		t.Errorf("attempt counter = %d; want 1", n)
	}
}

func TestApplyWendyConfBoundsTheWaitAcrossTheFile(t *testing.T) {
	notRunning := nmNotRunning(t)
	f := &fakeApply{addErr: func(string, int) error { return notRunning }}
	stubApply(t, f)
	cfgDir, confPath := writeConf(t, threeNetworks)

	applyWendyConf(zap.NewNop(), cfgDir, t.TempDir())

	var total time.Duration
	for _, d := range f.slept {
		total += d
	}
	if total > nmWaitBudget {
		t.Errorf("waited %v in total; want at most %v", total, nmWaitBudget)
	}
	tried := map[string]bool{}
	for _, c := range f.added {
		tried[c.SSID] = true
	}
	if len(tried) != 3 {
		t.Errorf("tried %v; want every network tried at least once", tried)
	}
	data, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := wendyconf.UnmarshalWiFi(parseINI(data)); len(got) != 3 {
		t.Errorf("kept %d networks; want all 3", len(got))
	}
}

func TestApplyWendyConfDeviceName(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantKept bool
	}{
		{"applied", nil, false},
		{"invalid name is dropped", fmt.Errorf("%w %q", errInvalidDeviceName, "lab-nano"), false},
		{"write failure is kept", fmt.Errorf("writing device name: %w", fs.ErrPermission), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stubApply(t, &fakeApply{nameErr: c.err})
			cfgDir, confPath := writeConf(t, "[wifi]\nssid = Home\npassword = home-pass\n\n[device]\nname = lab-nano\n")
			configPath := t.TempDir()

			applyWendyConf(zap.NewNop(), cfgDir, configPath)

			data, err := os.ReadFile(confPath)
			if !c.wantKept {
				assertGone(t, confPath, filepath.Join(configPath, wendyConfAttemptsFile))
				return
			}
			if err != nil {
				t.Fatalf("wendy.conf was not kept: %v", err)
			}
			if string(data) != "[device]\nname = lab-nano\n" {
				t.Errorf("kept wendy.conf:\n%s\nwant only the device name", data)
			}
			if n, _, _ := readAttempts(t, configPath); n != 1 {
				t.Errorf("attempt counter = %d; want 1", n)
			}
		})
	}
}

func TestApplyDeviceNameInvalidIsPermanent(t *testing.T) {
	if err := applyDeviceName(zap.NewNop(), "BAD NAME"); !errors.Is(err, errInvalidDeviceName) {
		t.Errorf("applyDeviceName(BAD NAME) = %v; want errInvalidDeviceName", err)
	}
}

func TestApplyWendyConfGivesUpAfterMaxAttempts(t *testing.T) {
	notRunning := nmNotRunning(t)
	stubApply(t, &fakeApply{addErr: func(string, int) error { return notRunning }})
	cfgDir, confPath := writeConf(t, threeNetworks)
	configPath := t.TempDir()

	var first []byte
	for boot := 1; boot < maxWendyConfAttempts; boot++ {
		applyWendyConf(zap.NewNop(), cfgDir, configPath)
		data, err := os.ReadFile(confPath)
		if err != nil {
			t.Fatalf("boot %d: wendy.conf was not kept: %v", boot, err)
		}
		if first == nil {
			first = data
		} else if string(data) != string(first) {
			t.Fatalf("boot %d rewrote wendy.conf differently:\n%s\nwant\n%s", boot, data, first)
		}
		if n, _, _ := readAttempts(t, configPath); n != boot {
			t.Fatalf("boot %d: attempt counter = %d", boot, n)
		}
	}
	applyWendyConf(zap.NewNop(), cfgDir, configPath)
	assertGone(t, confPath, filepath.Join(configPath, wendyConfAttemptsFile))
}

func TestApplyWendyConfReadErrorCountsAsAttempt(t *testing.T) {
	stubApply(t, &fakeApply{})
	cfgDir := t.TempDir()
	// A directory in its place fails the read without being absent.
	confPath := filepath.Join(cfgDir, "wendy.conf")
	if err := os.Mkdir(confPath, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := t.TempDir()

	for boot := 1; boot < maxWendyConfAttempts; boot++ {
		applyWendyConf(zap.NewNop(), cfgDir, configPath)
		if _, err := os.Stat(confPath); err != nil {
			t.Fatalf("boot %d removed the unreadable wendy.conf: %v", boot, err)
		}
		if n, _, _ := readAttempts(t, configPath); n != boot {
			t.Fatalf("boot %d: attempt counter = %d", boot, n)
		}
	}
	applyWendyConf(zap.NewNop(), cfgDir, configPath)
	assertGone(t, confPath, filepath.Join(configPath, wendyConfAttemptsFile))
}

func TestApplyWendyConfCounterFromAnotherFileStartsOver(t *testing.T) {
	notRunning := nmNotRunning(t)
	stubApply(t, &fakeApply{addErr: func(string, int) error { return notRunning }})
	cfgDir, confPath := writeConf(t, threeNetworks)
	configPath := t.TempDir()
	stale := fmt.Sprintf("%d %s\n", maxWendyConfAttempts-1, wendyConfSum([]byte("an earlier install")))
	if err := os.WriteFile(filepath.Join(configPath, wendyConfAttemptsFile), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}

	applyWendyConf(zap.NewNop(), cfgDir, configPath)

	if _, err := os.Stat(confPath); err != nil {
		t.Fatalf("a stale counter dropped the new wendy.conf: %v", err)
	}
	if n, _, _ := readAttempts(t, configPath); n != 1 {
		t.Errorf("attempt counter = %d; want 1", n)
	}
}

func TestApplyWendyConfKeepsWholeFileWhenRewriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only directory")
	}
	notRunning := nmNotRunning(t)
	stubApply(t, &fakeApply{addErr: func(ssid string, _ int) error {
		if ssid == "Home" {
			return notRunning
		}
		return nil
	}})
	cfgDir, confPath := writeConf(t, threeNetworks)
	if err := os.Chmod(cfgDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(cfgDir, 0o700) }) //nolint:errcheck
	configPath := t.TempDir()

	applyWendyConf(zap.NewNop(), cfgDir, configPath)

	data, err := os.ReadFile(confPath)
	if err != nil || string(data) != threeNetworks {
		t.Fatalf("wendy.conf = %q, %v; want it untouched", data, err)
	}
	// The count continues on the next boot, which reads the same whole file.
	if n, sum, _ := readAttempts(t, configPath); n != 1 || sum != wendyConfSum(data) {
		t.Errorf("attempt counter = %d %q; want 1 and the whole file's hash", n, sum)
	}
}

func TestApplyWendyConfDropsWhenAttemptCannotBeCounted(t *testing.T) {
	notRunning := nmNotRunning(t)
	stubApply(t, &fakeApply{addErr: func(string, int) error { return notRunning }})
	cfgDir, confPath := writeConf(t, threeNetworks)
	// A file where the config directory should be: the counter cannot be written.
	configPath := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(configPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	applyWendyConf(zap.NewNop(), cfgDir, filepath.Join(configPath, "sub"))

	assertGone(t, confPath, confPath+".new")
}
