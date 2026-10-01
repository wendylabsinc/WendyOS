package commands

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// requireFileIdentity skips a test on platforms without inode and ctime,
// where the digest cache is off (see fileidentity_other.go).
func requireFileIdentity(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("no file identity on %s; the digest cache is off there", runtime.GOOS)
	}
}

func statIdentity(t *testing.T, p string) fileIdentity {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	id, ok := fileIdentityOf(info)
	if !ok {
		t.Fatalf("no identity for %s on %s", p, runtime.GOOS)
	}
	return id
}

func TestFileIdentitySettled(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) int64 { return now.Add(d).UnixNano() }
	for _, tc := range []struct {
		name string
		id   fileIdentity
		want bool
	}{
		{"both old", fileIdentity{MtimeNs: at(-time.Hour), CtimeNs: at(-time.Hour)}, true},
		{"exactly the window", fileIdentity{MtimeNs: at(-digestRacyWindow), CtimeNs: at(-digestRacyWindow)}, true},
		{"mtime inside the window", fileIdentity{MtimeNs: at(-time.Second), CtimeNs: at(-time.Hour)}, false},
		{"ctime inside the window (mtime reset by cp -p)", fileIdentity{MtimeNs: at(-time.Hour), CtimeNs: at(-time.Second)}, false},
		{"mtime in the future", fileIdentity{MtimeNs: at(time.Hour), CtimeNs: at(-time.Hour)}, false},
		{"untracked mtime", fileIdentity{MtimeNs: 0, CtimeNs: at(-time.Hour)}, false},
		{"untracked ctime", fileIdentity{MtimeNs: at(-time.Hour), CtimeNs: 0}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.id.settled(now); got != tc.want {
				t.Fatalf("settled = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFileIdentityOfSeesRewritesAndReplacements: the two edits that keep size
// and mtime must still change the identity.
func TestFileIdentityOfSeesRewritesAndReplacements(t *testing.T) {
	requireFileIdentity(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "app.py")
	if err := os.WriteFile(p, []byte("print('v1')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	base := statIdentity(t, p)
	if again := statIdentity(t, p); again != base {
		t.Fatalf("identity not stable: %+v != %+v", again, base)
	}

	// In place, same size, mtime reset (cp -p). Linux stamps ctime from a
	// coarse per-tick clock, so let a tick pass first.
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(p, []byte("print('v2')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	rewritten := statIdentity(t, p)
	if rewritten.Size != base.Size || rewritten.MtimeNs != base.MtimeNs || rewritten.Ino != base.Ino {
		t.Fatalf("test setup: want same size, mtime and inode, got %+v vs %+v", rewritten, base)
	}
	if rewritten.CtimeNs == base.CtimeNs {
		t.Fatal("an in-place rewrite with a reset mtime kept the same identity")
	}

	// Replaced by a rename, same size and mtime (rsync, editors' atomic save).
	tmp := filepath.Join(dir, "app.py.tmp")
	if err := os.WriteFile(tmp, []byte("print('v3')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
	if replaced := statIdentity(t, p); replaced.Ino == rewritten.Ino {
		t.Fatal("a file replaced by a rename kept its inode")
	}
}

func TestFileIdentityOfIsOffWithoutInodes(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		t.Skip("inode and ctime are available here")
	}
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fileIdentityOf(info); ok {
		t.Fatalf("fileIdentityOf reported an identity on %s", runtime.GOOS)
	}
}
