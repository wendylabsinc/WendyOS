//go:build linux

package nanprovider

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNDIRulesRemovePartialWrite(t *testing.T) {
	// Restrict only a subprocess so this exercises a real short file write,
	// including when native hardware tests run as root.
	if os.Getenv("WENDY_TEST_PARTIAL_NAN_RULE") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestNDIRulesRemovePartialWrite$", "-test.v")
		cmd.Env = append(os.Environ(), "WENDY_TEST_PARTIAL_NAN_RULE=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("partial-write subprocess: %v\n%s", err, out)
		}
		return
	}
	dir := t.TempDir()
	partial, foreign := filepath.Join(dir, "owned.network"), filepath.Join(dir, "foreign.network")
	const foreignContent = "administrator-owned rule\n"
	if err := os.WriteFile(foreign, []byte(foreignContent), 0600); err != nil {
		t.Fatal(err)
	}
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &unix.Rlimit{Cur: 4, Max: limit.Max}); err != nil {
		t.Fatal(err)
	}
	defer unix.Setrlimit(unix.RLIMIT_FSIZE, &limit)
	rules := []ownedRule{{path: partial, content: "[Match]\nName=wnanndi0\n"}, {path: foreign, content: "our intended rule\n"}}
	release, err := reserveNDIRules(context.Background(), filepath.Join(dir, "lock"), rules, func(context.Context) error { return nil })
	if err == nil {
		release()
		t.Fatal("file-size limit did not cause a partial write")
	}
	if data, err := os.ReadFile(partial); !os.IsNotExist(err) {
		t.Errorf("failed startup retained a partial ownership rule: %q, %v", data, err)
	}
	if data, err := os.ReadFile(foreign); err != nil || string(data) != foreignContent {
		t.Fatalf("failed startup changed an administrator's rule: %q, %v", data, err)
	}
}

func TestNDIRulesPreserveForeignAndEditedFiles(t *testing.T) {
	dir := t.TempDir()
	path, lock := filepath.Join(dir, "owned.network"), filepath.Join(dir, "lock")
	rules := func() []ownedRule { return []ownedRule{{path: path, content: "our rule\n"}} }
	reload := func(context.Context) error { return nil }
	release, err := reserveNDIRules(context.Background(), lock, rules(), reload)
	if err != nil {
		t.Fatal(err)
	}
	if duplicateRelease, err := reserveNDIRules(context.Background(), lock, rules(), reload); err == nil {
		duplicateRelease()
		t.Error("a second provider acquired the exclusive reservation")
	}
	if err := os.WriteFile(path, []byte("administrator changed rule\n"), 0600); err != nil {
		release()
		t.Fatal(err)
	}
	release()
	if foreignRelease, err := reserveNDIRules(context.Background(), lock, rules(), reload); err == nil {
		foreignRelease()
		t.Fatal("a foreign rule was adopted")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "administrator changed rule\n" {
		t.Fatalf("administrator's rule was removed or changed: %q, %v", data, err)
	}
}
