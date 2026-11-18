//go:build linux

package sandbox

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// procStatus returns the child's own /proc/self/status, started by start.
func procStatus(t *testing.T, start func(*exec.Cmd) error) map[string]string {
	t.Helper()
	cmd := exec.Command("/bin/cat", "/proc/self/status")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := start(cmd); err != nil {
		t.Errorf("start: %v", err)
		return nil
	}
	if err := cmd.Wait(); err != nil || stderr.Len() != 0 {
		t.Errorf("wait: %v; stderr %q", err, stderr.String())
	}
	fields := map[string]string{}
	for _, line := range strings.Split(out.String(), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			fields[k] = strings.Join(strings.Fields(v), " ")
		}
	}
	return fields
}

func TestStartDropsRootForTheChildOnly(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("dropping root needs root")
	}
	// The agent's working directory is not searchable by the sandbox user either.
	t.Chdir(t.TempDir())
	video, err := user.LookupGroup("video")
	if err != nil {
		t.Skip("no video group on this host")
	}
	id, err := Lookup("nobody", "video", "no-such-group")
	if err != nil {
		t.Fatal(err)
	}

	got := procStatus(t, id.Start)
	uid := fmt.Sprint(id.cred.Uid)
	gid := fmt.Sprint(id.cred.Gid)
	for field, want := range map[string]string{
		"Uid":        strings.Join([]string{uid, uid, uid, uid}, " "),
		"Gid":        strings.Join([]string{gid, gid, gid, gid}, " "),
		"Groups":     video.Gid,
		"NoNewPrivs": "1",
		"CapPrm":     "0000000000000000",
		"CapEff":     "0000000000000000",
		"CapAmb":     "0000000000000000",
	} {
		if got[field] != want {
			t.Errorf("sandboxed %s = %q; want %q", field, got[field], want)
		}
	}
	if uid == "0" {
		t.Fatal("sandbox identity is root")
	}

	// The flag must stay on the one thread that carries it. A leak would show
	// up as a later, unrelated child of the agent that cannot use sudo or ping,
	// so start plain children while sandboxed ones are starting too.
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 25 {
				if got := procStatus(t, id.Start); got["NoNewPrivs"] != "1" {
					t.Errorf("sandboxed NoNewPrivs = %q; want 1", got["NoNewPrivs"])
				}
			}
		})
		wg.Go(func() {
			for range 25 {
				plain := procStatus(t, (*exec.Cmd).Start)
				if plain["NoNewPrivs"] != "0" || !strings.HasPrefix(plain["Uid"], "0 ") {
					t.Errorf("an unsandboxed child inherited the sandbox: NoNewPrivs=%q Uid=%q", plain["NoNewPrivs"], plain["Uid"])
				}
			}
		})
	}
	wg.Wait()

	// The cache directory is the child's to write, and nobody else's to read.
	if id.cache == "" {
		t.Fatal("no cache directory")
	}
	touch := exec.Command("touch", filepath.Join(id.cache, "written-by-child"))
	if err := id.Start(touch); err != nil {
		t.Fatal(err)
	}
	if err := touch.Wait(); err != nil {
		t.Fatalf("child could not write its cache directory: %v", err)
	}
	if !strings.Contains(strings.Join(touch.Env, "\n"), "XDG_CACHE_HOME="+id.cache) {
		t.Fatalf("XDG_CACHE_HOME not set: %v", touch.Env)
	}
	if fi, err := os.Stat(id.cache); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("cache directory mode = %v, %v; want 0700", fi.Mode(), err)
	}
}

func TestLookupRefusesWhatIsNotASandbox(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("lookups are skipped without root")
	}
	if id, err := Lookup("no-such-user-wendy-sandbox"); err == nil {
		t.Fatalf("unknown user resolved to %v", id)
	}
	if id, err := Lookup("root"); err == nil {
		t.Fatalf("root accepted as a sandbox identity: %v", id)
	}
}
