package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
)

// fakeDiskutil answers each call with the next result queued for its
// arguments, then with the last one; calls with nothing queued succeed.
type fakeDiskutil struct {
	results map[string][]error
	calls   []string
}

func (f *fakeDiskutil) run(args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	f.calls = append(f.calls, key)
	queue := f.results[key]
	if len(queue) == 0 {
		return []byte("ok"), nil
	}
	err := queue[0]
	if len(queue) > 1 {
		f.results[key] = queue[1:]
	}
	if err != nil {
		return []byte("Volume failed to eject: dissented by PID 312 (mds)"), err
	}
	return []byte("ok"), nil
}

type recordedSleeps []time.Duration

func (r *recordedSleeps) sleep(d time.Duration) { *r = append(*r, d) }

var errExit1 = errors.New("exit status 1")

func TestEjectWithDiskutil(t *testing.T) {
	const dev = "/dev/disk9"
	cases := []struct {
		name      string
		results   map[string][]error
		gone      bool
		wantCalls []string
		wantSleep string
		wantErr   string
		wantNote  bool
	}{
		{
			name:      "ejected at once",
			wantCalls: []string{"eject " + dev},
			wantSleep: "[]",
		},
		{
			name:      "ejected on a retry",
			results:   map[string][]error{"eject " + dev: {errExit1, errExit1, nil}},
			wantCalls: []string{"eject " + dev, "eject " + dev, "eject " + dev},
			wantSleep: "[2s 4s]",
		},
		{
			name:      "unmounted when eject keeps failing",
			results:   map[string][]error{"eject " + dev: {errExit1}},
			wantCalls: []string{"eject " + dev, "eject " + dev, "eject " + dev, "unmountDisk " + dev},
			wantSleep: "[2s 4s]",
			wantNote:  true,
		},
		{
			name: "force unmounted",
			results: map[string][]error{
				"eject " + dev:       {errExit1},
				"unmountDisk " + dev: {errExit1},
			},
			wantCalls: []string{"eject " + dev, "eject " + dev, "eject " + dev, "unmountDisk " + dev, "unmountDisk force " + dev},
			wantSleep: "[2s 4s]",
			wantNote:  true,
		},
		{
			name: "still mounted",
			results: map[string][]error{
				"eject " + dev:             {errExit1},
				"unmountDisk " + dev:       {errExit1},
				"unmountDisk force " + dev: {errExit1},
			},
			wantCalls: []string{"eject " + dev, "eject " + dev, "eject " + dev, "unmountDisk " + dev, "unmountDisk force " + dev},
			wantSleep: "[2s 4s]",
			wantErr:   dev + " is still mounted: eject it in Finder before removing it",
		},
		{
			name:      "disconnected",
			results:   map[string][]error{"eject " + dev: {errExit1}},
			gone:      true,
			wantCalls: []string{"eject " + dev, "eject " + dev, "eject " + dev},
			wantSleep: "[2s 4s]",
			wantErr:   dev + " was disconnected before it could be ejected",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			diskutil := &fakeDiskutil{results: c.results}
			var slept recordedSleeps
			var out strings.Builder
			err := ejectWithDiskutil(dev, diskutil.run, slept.sleep, func() bool { return !c.gone }, &out)

			if c.wantErr == "" && err != nil {
				t.Fatalf("ejectWithDiskutil = %v; want nil", err)
			}
			if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("ejectWithDiskutil = %v; want %q", err, c.wantErr)
			}
			if c.wantErr != "" && !strings.Contains(err.Error(), "dissented by PID") {
				t.Errorf("error %q leaves out what diskutil said", err)
			}
			if strings.Join(diskutil.calls, "\n") != strings.Join(c.wantCalls, "\n") {
				t.Errorf("diskutil calls:\n%s\nwant:\n%s", strings.Join(diskutil.calls, "\n"), strings.Join(c.wantCalls, "\n"))
			}
			if fmt.Sprint([]time.Duration(slept)) != c.wantSleep {
				t.Errorf("slept %v; want %s", []time.Duration(slept), c.wantSleep)
			}
			if gotNote := strings.Contains(out.String(), "safe to remove"); gotNote != c.wantNote {
				t.Errorf("printed %q; want the safe-to-remove note: %v", out.String(), c.wantNote)
			}
		})
	}
}

// mountedDir stands in for a mount point that still shows the partition.
func mountedDir(t *testing.T) (dir, file string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "wendyos-config-1")
	file = filepath.Join(dir, "wendy.conf")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("[device]\nname = lab-nano\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, file
}

func TestReleaseMountRemovesTheEmptyMountPoint(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wendyos-config-1")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	calls := 0
	var slept recordedSleeps
	err := releaseMount(dir, func() ([]byte, error) {
		calls++
		if calls < 3 {
			return []byte("umount: target is busy."), errExit1
		}
		return nil, nil
	}, slept.sleep)
	if err != nil {
		t.Fatalf("releaseMount = %v", err)
	}
	if calls != 3 || len(slept) != 2 {
		t.Errorf("unmounted %d times with %d sleeps; want 3 and 2", calls, len(slept))
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("mount point left behind: %v", err)
	}
}

func TestReleaseMountNeverDeletesAStillMountedPartition(t *testing.T) {
	t.Run("unmount fails", func(t *testing.T) {
		dir, file := mountedDir(t)
		calls := 0
		var slept recordedSleeps
		err := releaseMount(dir, func() ([]byte, error) {
			calls++
			return []byte("umount: target is busy."), errExit1
		}, slept.sleep)
		var stillMounted *configStillMountedError
		if !errors.As(err, &stillMounted) || stillMounted.dir != dir || !strings.Contains(err.Error(), "target is busy") {
			t.Fatalf("releaseMount = %v; want a still-mounted error naming %s and the cause", err, dir)
		}
		if calls != unmountAttempts {
			t.Errorf("tried %d times; want %d", calls, unmountAttempts)
		}
		if _, err := os.Stat(file); err != nil {
			t.Errorf("a file of the mounted partition was deleted: %v", err)
		}
	})
	t.Run("unmount claims success", func(t *testing.T) {
		dir, file := mountedDir(t)
		if err := releaseMount(dir, func() ([]byte, error) { return nil, nil }, func(time.Duration) {}); err != nil {
			t.Fatalf("releaseMount = %v", err)
		}
		if _, err := os.Stat(file); err != nil {
			t.Errorf("a file under the mount point was deleted: %v", err)
		}
	})
}

func TestReleaseMountBeforeEjectLeavesARefusedUnmountToTheEject(t *testing.T) {
	dir, file := mountedDir(t)
	var out bytes.Buffer
	err := releaseMountBeforeEject(dir, func() ([]byte, error) {
		return []byte("Volume failed to unmount: dissented by PID 312 (mds)"), errExit1
	}, func(time.Duration) {}, &out)
	if err != nil {
		t.Fatalf("releaseMountBeforeEject = %v; want nil, the eject releases the mount", err)
	}
	if !strings.Contains(out.String(), dir) || !strings.Contains(out.String(), "ejecting the drive") {
		t.Errorf("printed %q; want a note naming %s and the eject", out.String(), dir)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("a file of the mounted partition was deleted: %v", err)
	}
}

// The darwin flow: the CLI's own mount refuses to unmount after a full write.
func TestRefusedUnmountOnMacIsReportedOnlyWhenTheEjectFails(t *testing.T) {
	dir, _ := mountedDir(t)
	withStubbedProvisioning(t, func(drive, []wendyconf.WifiCredential, string, []byte) error {
		return releaseMountBeforeEject(dir, func() ([]byte, error) { return nil, errExit1 }, func(time.Duration) {}, io.Discard)
	}, true, []bool{true})
	retried := false
	confirmProvisioningRetry = func() (bool, error) { retried = true; return true, nil }

	var provErr error
	out, _ := captureCommandStdout(t, func() error {
		_, _, provErr = provisionConfigWithRetry(context.Background(), drive{DevicePath: "/dev/disk4"}, nil, "lab-nano", nil, true, nil)
		return nil
	})
	if provErr != nil || retried || !strings.Contains(out, "Config partition written: name lab-nano") {
		t.Fatalf("provisioning = %v, retry offered %v, output %q; want written, no retry", provErr, retried, out)
	}
	if err := diskInstallResult("WendyOS 1.0", "SD Card", provErr, nil); err != nil {
		t.Fatalf("eject released the mount, but the install failed: %v", err)
	}

	diskutil := &fakeDiskutil{results: map[string][]error{
		"eject /dev/disk4": {errExit1}, "unmountDisk /dev/disk4": {errExit1}, "unmountDisk force /dev/disk4": {errExit1},
	}}
	ejectErr := ejectWithDiskutil("/dev/disk4", diskutil.run, func(time.Duration) {}, func() bool { return true }, io.Discard)
	err := diskInstallResult("WendyOS 1.0", "SD Card", provErr, ejectErr)
	if err == nil || !strings.Contains(err.Error(), "could not be ejected") || !strings.Contains(err.Error(), "still mounted") ||
		strings.Contains(err.Error(), "not applied") {
		t.Fatalf("diskInstallResult = %v; want only the failed eject", err)
	}
}

func TestDiskInstallResult(t *testing.T) {
	stillMounted := &configStillMountedError{dir: "/tmp/wendyos-config-1", cause: errors.New("umount: target is busy.: exit status 32")}
	for _, c := range []struct {
		name                string
		provisionErr, eject error
		want, notWant       string
	}{
		{name: "success"},
		{name: "not applied", provisionErr: errors.New("config partition not found"), want: "the requested provisioning was not applied: config partition not found"},
		{name: "written but still mounted", provisionErr: stillMounted,
			want: "was written to SD Card, but the config partition is still mounted at /tmp/wendyos-config-1: run 'sudo umount /tmp/wendyos-config-1' before removing the drive", notWant: "not applied"},
		{name: "write failed and still mounted", provisionErr: errors.Join(errors.New("disk full"), stillMounted), want: "not applied: disk full"},
		{name: "eject failed", eject: errors.New("disk4 is still mounted"), want: "and its first-boot configuration were written to SD Card, but the drive could not be ejected"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := diskInstallResult("WendyOS 1.0", "SD Card", c.provisionErr, c.eject)
			if c.want == "" {
				if err != nil {
					t.Fatalf("diskInstallResult = %v; want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) || (c.notWant != "" && strings.Contains(err.Error(), c.notWant)) {
				t.Fatalf("diskInstallResult = %v; want %q without %q", err, c.want, c.notWant)
			}
		})
	}
}

func TestProvisionConfigWithRetryNeverRetriesAStillMountedPartition(t *testing.T) {
	stillMounted := &configStillMountedError{dir: "/tmp/wendyos-config-1", cause: errExit1}
	for _, c := range []struct {
		name      string
		err       error
		requested bool
		written   bool
		wantErr   string
	}{
		{name: "only the unmount failed", err: stillMounted, requested: true, written: true, wantErr: "still mounted"},
		{name: "write failed too", err: errors.Join(errors.New("disk full"), stillMounted), requested: true, wantErr: "disk full"},
		{name: "unrequested write failed too", err: errors.Join(errors.New("disk full"), stillMounted), wantErr: "still mounted"},
	} {
		t.Run(c.name, func(t *testing.T) {
			calls := withStubbedProvisioning(t, func(drive, []wendyconf.WifiCredential, string, []byte) error { return c.err }, true, []bool{true})
			var err error
			out, _ := captureCommandStdout(t, func() error {
				_, _, err = provisionConfigWithRetry(context.Background(), drive{DevicePath: "/dev/sdz"}, nil, "lab-nano", nil, c.requested, nil)
				return nil
			})
			if *calls != 1 {
				t.Fatalf("provisioned %d times; want 1, no retry over the old mount", *calls)
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) || !strings.Contains(err.Error(), "still mounted") {
				t.Fatalf("provisionConfigWithRetry = %v; want %q and the mount", err, c.wantErr)
			}
			if got := strings.Contains(out, "Config partition written"); got != c.written {
				t.Fatalf("output %q; written line printed = %v, want %v", out, got, c.written)
			}
		})
	}
}
