//go:build linux

package secretmount

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMountFailureWritesNoPlaintext(t *testing.T) {
	root := filepath.Join(t.TempDir(), "secrets")
	mount := func(_, dir, _ string, flags uintptr, options string) error {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatal("plaintext written before mount")
		}
		if options != "mode=0700,size=1m,nr_inodes=128,noswap" {
			t.Fatal("missing bounded noswap mount")
		}
		return unix.EINVAL
	}
	_, err := prepareAt(root, map[string][]byte{"TOKEN": []byte("fixture")}, uint32(os.Getuid()), uint32(os.Getgid()), mount, unix.Unmount)
	if err == nil {
		t.Fatal("mount failure accepted")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("failed mount left files")
	}
}

func TestSecretMountRejectsUnsafeNames(t *testing.T) {
	for _, name := range []string{"../escape", "a/b", ".", "..", ""} {
		_, err := prepareAt(t.TempDir(), map[string][]byte{name: []byte("fixture")}, 0, 0, func(string, string, string, uintptr, string) error { t.Fatal("invalid name reached mount"); return nil }, unix.Unmount)
		if err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
}

func TestIntegrationSecretMount(t *testing.T) {
	if os.Getenv("WENDY_SECRETS_INTEGRATION") == "" {
		t.Skip("set WENDY_SECRETS_INTEGRATION=1 for real Linux mounts")
	}
	if os.Geteuid() != 0 {
		t.Fatal("mount integration needs root")
	}
	want := []byte(" leading\r\nline\ntrailing ")
	a, err := Prepare(map[string][]byte{"TOKEN": want}, 1234, 1234)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Cleanup(a) })
	b, err := Prepare(map[string][]byte{"TOKEN": []byte("second")}, 2345, 2345)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Cleanup(b) })
	if a == b {
		t.Fatal("apps share secret mount")
	}
	got, err := os.ReadFile(filepath.Join(a, "TOKEN"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("secret bytes changed", err)
	}
	var st unix.Stat_t
	if err = unix.Stat(filepath.Join(a, "TOKEN"), &st); err != nil {
		t.Fatal(err)
	}
	if st.Uid != 1234 || st.Gid != 1234 || st.Mode&0777 != 0400 {
		t.Fatalf("wrong file permissions: %+v", st)
	}
	if err = unix.Stat(a, &st); err != nil || st.Mode&0777 != 0500 {
		t.Fatal("wrong directory permissions", err)
	}
	if err = os.WriteFile(filepath.Join(a, "TOKEN"), []byte("replace"), 0400); !errors.Is(err, unix.EROFS) {
		t.Fatalf("mount writable: %v", err)
	}
	if err = Cleanup(a); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(a); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("mount not removed")
	}
	if got, err = os.ReadFile(filepath.Join(b, "TOKEN")); err != nil || string(got) != "second" {
		t.Fatal("cleanup removed other mount")
	}
	if err = Cleanup(a); err != nil {
		t.Fatal("cleanup not idempotent", err)
	}
}
