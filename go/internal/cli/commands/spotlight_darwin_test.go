//go:build darwin

package commands

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestStopTargetIndexing(t *testing.T) {
	mounts := `/dev/disk0s1 on / (apfs, local)
/dev/disk4s1 on /Volumes/boot (msdos, local, fskit)
/dev/disk41s1 on /Volumes/other (msdos, local, fskit)
/dev/disk4s2 on /Volumes/config with spaces (msdos, local, fskit)
/dev/disk4s3 on /Volumes/data (exfat, local)
`
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "continue after failure"}[fail], func(t *testing.T) {
			var calls [][]string
			err := stopTargetIndexing("/dev/disk4", func(timeout time.Duration, name string, args ...string) ([]byte, error) {
				if timeout != markerTimeout {
					t.Fatalf("unbounded command: %s", name)
				}
				calls = append(calls, append([]string{name}, args...))
				if name == "/sbin/mount" {
					return []byte(mounts), nil
				}
				if fail && len(calls) == 2 {
					return nil, errCommandTimedOut
				}
				return nil, nil
			})
			want := [][]string{
				{"/sbin/mount"},
				{"sudo", "-n", "/usr/bin/mdutil", "-i", "off", "/Volumes/boot"},
				{"sudo", "-n", "/usr/bin/mdutil", "-i", "off", "/Volumes/config with spaces"},
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("calls = %v, want %v", calls, want)
			}
			if errors.Is(err, errCommandTimedOut) != fail {
				t.Fatalf("error = %v, fail = %v", err, fail)
			}
		})
	}
}

func TestUnmountDarwinDisk(t *testing.T) {
	for _, tc := range []struct {
		name    string
		errs    []error
		wantErr bool
	}{
		{"normal", []error{nil}, false},
		{"force after failure", []error{errors.New("busy"), nil}, false},
		{"force after timeout", []error{errCommandTimedOut, nil}, false},
		{"both timed out", []error{errCommandTimedOut, errCommandTimedOut}, true},
		{"both failed", []error{errors.New("busy"), errors.New("busy")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := unmountDarwinDisk("/dev/disk4", func(timeout time.Duration, name string, args ...string) ([]byte, error) {
				want := []string{"-n", "diskutil", "unmountDisk", "/dev/disk4"}
				if calls == 1 {
					want = []string{"-n", "diskutil", "unmountDisk", "force", "/dev/disk4"}
				}
				if timeout != unmountTimeout || name != "sudo" || !reflect.DeepEqual(args, want) {
					t.Fatalf("unexpected command: %s %v (%s)", name, args, timeout)
				}
				err := tc.errs[calls]
				calls++
				return []byte("busy"), err
			})
			if (err != nil) != tc.wantErr || calls != len(tc.errs) {
				t.Fatalf("error = %v, calls = %d", err, calls)
			}
			if tc.name == "both timed out" && !errors.Is(err, errCommandTimedOut) {
				t.Fatalf("lost timeout: %v", err)
			}
		})
	}
}

func TestStopTargetIndexingNoMountedFAT(t *testing.T) {
	calls := 0
	if err := stopTargetIndexing("/dev/disk4", func(_ time.Duration, _ string, _ ...string) ([]byte, error) {
		calls++
		return []byte("/dev/disk0s1 on / (apfs, local)\n"), nil
	}); err != nil || calls != 1 {
		t.Fatalf("error = %v, calls = %d", err, calls)
	}
}
