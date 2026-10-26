//go:build linux

package nancontrol

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func permissionTestSocket(t *testing.T) (string, string) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("socket group permissions require root in a disposable Linux container")
	}
	dir, err := os.MkdirTemp("/tmp", "nan-perm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "nan0")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path, filepath.Join(dir, "receipt.json")
}

func TestSocketPermissionLeaseRetainsIdentityAndRestoresOriginalAccess(t *testing.T) {
	path, receipt := permissionTestSocket(t)
	original, err := socketIdentity(path, receipt)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := grantSocketAccess(path, receipt, 2001); err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0770 || info.Sys().(*syscall.Stat_t).Gid != 2001 {
			t.Fatalf("socket access not granted: %v", info)
		}
		if got, err := socketIdentity(path, receipt); err != nil || got != original {
			t.Fatalf("grant invalidated provider ownership: %v %v", got, err)
		}
		if err := grantSocketAccess(path, receipt, 2001); err != nil {
			t.Fatal(err)
		}
		if err := restoreSocketAccess(path, receipt); err != nil {
			t.Fatal(err)
		}
		info, _ = os.Stat(path)
		if info.Mode().Perm() != 0700 || info.Sys().(*syscall.Stat_t).Gid != uint32(os.Getgid()) {
			t.Fatalf("original socket access not restored: %v", info)
		}
		if got, err := socketIdentity(path, receipt); err != nil || got != original {
			t.Fatalf("restore invalidated provider ownership: %v %v", got, err)
		}
	}
	state, _ := os.Stat(receipt)
	if state.Mode().Perm() != 0600 {
		t.Fatalf("receipt permissions = %v", state.Mode())
	}
}

func TestSocketPermissionRestorePreservesForeignChanges(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{true: "replacement", false: "administrator chmod"}[replace], func(t *testing.T) {
			path, receipt := permissionTestSocket(t)
			original, _ := socketIdentity(path, receipt)
			if err := grantSocketAccess(path, receipt, 2001); err != nil {
				t.Fatal(err)
			}
			if replace {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
			}
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			before, _ := os.Stat(path)
			if got, err := socketIdentity(path, receipt); err != nil || got == original {
				t.Fatalf("foreign change inherited provider authority: %v %v", got, err)
			}
			if err := restoreSocketAccess(path, receipt); err != nil {
				t.Fatal(err)
			}
			after, _ := os.Stat(path)
			if after.Mode() != before.Mode() || after.Sys().(*syscall.Stat_t).Gid != before.Sys().(*syscall.Stat_t).Gid {
				t.Fatal("restoration changed a foreign socket")
			}
		})
	}
}

func TestSocketPermissionReceiptCannotAuthorizeRegularFile(t *testing.T) {
	path, receipt := permissionTestSocket(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := grantSocketAccess(path, receipt, 2001); err == nil {
		t.Fatal("regular file granted as NAN control socket")
	}
}

func TestSocketGrantFailsBeforeWideningAccessWithoutWritableReceipt(t *testing.T) {
	path, _ := permissionTestSocket(t)
	before, _ := os.Stat(path)
	if err := grantSocketAccess(path, filepath.Join(path, "not-a-directory"), 2001); err == nil {
		t.Fatal("grant succeeded without an ownership receipt")
	}
	after, _ := os.Stat(path)
	if after.Mode() != before.Mode() || after.Sys().(*syscall.Stat_t).Gid != before.Sys().(*syscall.Stat_t).Gid {
		t.Fatal("failed grant widened host socket access")
	}
}

func TestPinnedPermissionChangeCannotAffectReplacementSocket(t *testing.T) {
	path, _ := permissionTestSocket(t)
	pinned, err := openSocket(path)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	replacement, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	before, _ := os.Stat(path)
	if err := changeSocketPermissions(pinned, 0, 2001, 0770); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if after.Mode() != before.Mode() || after.Sys().(*syscall.Stat_t).Gid != before.Sys().(*syscall.Stat_t).Gid {
		t.Fatal("permission change followed the replaced socket pathname")
	}
}
