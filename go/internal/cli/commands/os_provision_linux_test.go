//go:build linux

package commands

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func shortenConfigPartitionTimeout(t *testing.T, wait time.Duration) {
	t.Helper()
	origWait, origPoll := configPartitionTimeout, configPartitionPollInterval
	configPartitionTimeout, configPartitionPollInterval = wait, time.Millisecond
	t.Cleanup(func() { configPartitionTimeout, configPartitionPollInterval = origWait, origPoll })
}

// fakeTools stands in for every tool writeConfigPartition runs, with nothing
// else on PATH, so no real sudo or mount can run.
type fakeTools struct {
	t   *testing.T
	bin string
}

// fakeProvisioningTools installs the fakes. lsblk shows the config label only
// after lsblkMisses calls; mount exits with mountExits in turn, then 0.
func fakeProvisioningTools(t *testing.T, lsblkMisses int, mountExits ...int) fakeTools {
	t.Helper()
	f := fakeTools{t: t, bin: t.TempDir()}
	// Scripts find their state through $0, so no path is quoted into them.
	const count = "read n < \"$0.count\"; n=$((n+1)); echo $n > \"$0.count\"\n"
	f.write("sudo", "printf '%s\\n' \"$*\" >> \"$0.log\"\nexec \"$@\"\n")
	for _, tool := range []string{"partprobe", "udevadm", "umount"} {
		f.write(tool, count+"exit 0\n")
	}
	f.write("lsblk", count+fmt.Sprintf(
		"echo sdz\nif [ \"$n\" -gt %d ]; then echo 'sdz1 config'; else echo sdz1; fi\n", lsblkMisses))
	f.write("mount", count+fmt.Sprintf(
		"i=1\nfor code in %s; do\n  if [ \"$i\" -eq \"$n\" ]; then echo 'mount: fake failure' >&2; exit \"$code\"; fi\n  i=$((i+1))\ndone\n",
		strings.Trim(fmt.Sprint(mountExits), "[]")))
	t.Setenv("PATH", f.bin)
	return f
}

func (f fakeTools) write(name, body string) {
	f.t.Helper()
	for path, data := range map[string]string{name: "#!/bin/sh\n" + body, name + ".count": "0\n"} {
		if err := os.WriteFile(filepath.Join(f.bin, path), []byte(data), 0o755); err != nil {
			f.t.Fatal(err)
		}
	}
}

// ran reports how many times tool ran.
func (f fakeTools) ran(tool string) int {
	b, err := os.ReadFile(filepath.Join(f.bin, tool+".count"))
	if err != nil {
		f.t.Fatal(err)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// sudoed lists the commands run through sudo, in order.
func (f fakeTools) sudoed() []string {
	b, err := os.ReadFile(filepath.Join(f.bin, "sudo.log"))
	if err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}
	if s := strings.TrimSpace(string(b)); s != "" {
		return strings.Split(s, "\n")
	}
	return nil
}

// fakeDisk is a device path that exists, so drivePresent reports it attached.
func fakeDisk(t *testing.T) drive {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sdz")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return drive{DevicePath: path}
}

func TestWriteConfigPartitionWaitsForTheLabelAndRetriesTheMount(t *testing.T) {
	shortenConfigPartitionTimeout(t, 5*time.Second)
	tools := fakeProvisioningTools(t, 2, 32)
	disk := fakeDisk(t)
	if err := writeConfigPartition(disk, configAgent{}, nil, "", nil); err != nil {
		t.Fatalf("writeConfigPartition: %v", err)
	}
	// Two lookups before udev has the label, one whose mount finds the node gone, one that mounts.
	if lookups, mounts := tools.ran("lsblk"), tools.ran("mount"); lookups != 4 || mounts != 2 {
		t.Fatalf("lsblk ran %d times and mount %d, want 4 and 2", lookups, mounts)
	}
	got := tools.sudoed()
	if len(got) == 0 {
		t.Fatal("nothing ran through sudo")
	}
	dir := strings.TrimPrefix(got[len(got)-1], "umount ")
	mount := fmt.Sprintf("mount -t vfat -o uid=%d,gid=%d /dev/sdz1 %s", os.Getuid(), os.Getgid(), dir)
	want := []string{"partprobe " + disk.DevicePath, "udevadm settle --timeout=10", mount, mount, "umount " + dir}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sudo ran:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestWriteConfigPartitionDoesNotRetryAFailingSudo(t *testing.T) {
	shortenConfigPartitionTimeout(t, 5*time.Second)
	tools := fakeProvisioningTools(t, 0, 1)
	err := writeConfigPartition(fakeDisk(t), configAgent{}, nil, "", nil)
	if err == nil || tools.ran("mount") != 1 || tools.ran("umount") != 0 {
		t.Fatalf("got %v after %d mounts and %d umounts, want the first failure and no umount",
			err, tools.ran("mount"), tools.ran("umount"))
	}
}

func TestWriteConfigPartitionStopsWhenTheDriveIsGone(t *testing.T) {
	shortenConfigPartitionTimeout(t, 5*time.Second)
	tools := fakeProvisioningTools(t, 1000)
	gone := drive{DevicePath: filepath.Join(t.TempDir(), "sdz")}
	if err := writeConfigPartition(gone, configAgent{}, nil, "", nil); err == nil || tools.ran("lsblk") != 1 {
		t.Fatalf("got %v after %d lookups, want the lookup error at once", err, tools.ran("lsblk"))
	}
}

func TestRetryConfigPartitionRetriesUntilItSucceeds(t *testing.T) {
	shortenConfigPartitionTimeout(t, time.Minute)
	calls := 0
	err := retryConfigPartition(func() (bool, error) {
		calls++
		if calls < 4 {
			return true, errors.New("config partition not found on /dev/mmcblk0")
		}
		return false, nil
	}, func() bool { return true })
	if err != nil || calls != 4 {
		t.Fatalf("got %v after %d attempts, want success on the 4th", err, calls)
	}
}

func TestRetryConfigPartitionGivesUpAfterTheWait(t *testing.T) {
	shortenConfigPartitionTimeout(t, 20*time.Millisecond)
	notFound := errors.New("config partition not found on /dev/mmcblk0")
	start := time.Now()
	err := retryConfigPartition(func() (bool, error) { return true, notFound }, func() bool { return true })
	if !errors.Is(err, notFound) || !strings.Contains(err.Error(), "retried for 20ms") {
		t.Fatalf("got %v, want the attempt's error and how long it retried", err)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Fatalf("gave up after %s, before the wait ran out", elapsed)
	}
}

func TestRetryConfigPartitionStopsOnAPermanentFailure(t *testing.T) {
	shortenConfigPartitionTimeout(t, time.Minute)
	authFailed := errors.New("sudo: a password is required")
	calls := 0
	err := retryConfigPartition(func() (bool, error) {
		calls++
		return false, authFailed
	}, func() bool { return true })
	if !errors.Is(err, authFailed) || calls != 1 {
		t.Fatalf("got %v after %d attempts, want the error at once", err, calls)
	}
}

func TestMountRetryableOnlyOnMountFailure(t *testing.T) {
	for _, tc := range []struct {
		code int
		want bool
	}{{32, true}, {1, false}, {2, false}} {
		err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", tc.code)).Run()
		if got := mountRetryable(fmt.Errorf("mounting: %w", err)); got != tc.want {
			t.Errorf("exit %d: mountRetryable = %v, want %v", tc.code, got, tc.want)
		}
	}
	if mountRetryable(errors.New("sudo: not found")) {
		t.Error("a non-exit error counts as a mount failure")
	}
}

func TestRetryConfigPartitionStopsWhenTheDriveIsGone(t *testing.T) {
	shortenConfigPartitionTimeout(t, time.Minute)
	gone := errors.New("lsblk /dev/sdb: exit status 32")
	calls := 0
	err := retryConfigPartition(func() (bool, error) {
		calls++
		return true, gone
	}, func() bool { return false })
	if !errors.Is(err, gone) || calls != 1 {
		t.Fatalf("got %v after %d attempts, want the error at once", err, calls)
	}
}
