package jobops_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/jobops"
)

func TestArtifactsPullVerifiesFilesAndSkipsUnchangedWithoutReturningData(t *testing.T) {
	source := fixture(t)
	payload := []byte(strings.Repeat("not model context\x00", 10000))
	if err := os.WriteFile(filepath.Join(source, "evidence.bin"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	c := connection(t, &host{})
	out := t.TempDir()
	files := []string{"status.json", "evidence.bin"}
	first, err := jobops.Pull(context.Background(), c, source, out, files)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(out, "evidence.bin"))
	if err != nil || string(data) != string(payload) {
		t.Fatal("artifact bytes changed", err)
	}
	encoded, _ := json.Marshal(first)
	if len(encoded) > 700 || strings.Contains(string(encoded), "not model context") {
		t.Fatal("binary data entered tool result")
	}
	second, err := jobops.Pull(context.Background(), c, source, out, files)
	if err != nil {
		t.Fatal(err)
	}
	if second["unchanged_files"] != 2 || second["transferred_bytes"] != int64(0) {
		t.Fatalf("unchanged transfer repeated: %+v", second)
	}
	manifest, err := os.ReadFile(first["manifest_path"].(string))
	if err != nil || !strings.Contains(string(manifest), "sha256") {
		t.Fatal("missing manifest", err)
	}
}
func TestArtifactsPullFailureDoesNotOverwriteExistingFile(t *testing.T) {
	source := fixture(t)
	out := t.TempDir()
	if err := os.WriteFile(filepath.Join(out, "status.json"), []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	c := connection(t, &host{})
	_, err := jobops.Pull(context.Background(), c, source, out, []string{"status.json", "missing.json"})
	if err == nil {
		t.Fatal("missing file accepted")
	}
	data, _ := os.ReadFile(filepath.Join(out, "status.json"))
	if string(data) != "preserve" {
		t.Fatal("partial transfer overwrote old evidence")
	}
}
func TestArtifactsPullRefusesUnsafeAndSymlinkPaths(t *testing.T) {
	h := &host{}
	c := connection(t, h)
	source := fixture(t)
	out := t.TempDir()
	for _, files := range [][]string{{"../escape"}, {"/tmp/absolute"}, {"status.json", "status.json"}, {".wendy-artifacts-manifest.json"}} {
		if _, err := jobops.Pull(context.Background(), c, source, out, files); err == nil {
			t.Fatal("unsafe path accepted", files)
		}
	}
	if h.calls.Load() != 0 {
		t.Fatal("unsafe paths reached host")
	}
	external := t.TempDir()
	os.WriteFile(filepath.Join(external, "secret"), []byte("private"), 0600)
	if err := os.Symlink(external, filepath.Join(source, "link")); err != nil {
		t.Skip(err)
	}
	if _, err := jobops.Pull(context.Background(), c, source, out, []string{"link/secret"}); err == nil {
		t.Fatal("remote symlink followed")
	}
}

func TestArtifactsPullRejectsCorruptedTransportBeforePublishing(t *testing.T) {
	source := fixture(t)
	out := t.TempDir()
	c := connection(t, &host{corrupt: true})
	if _, err := jobops.Pull(context.Background(), c, source, out, []string{"status.json"}); err == nil {
		t.Fatal("corrupt snapshot accepted")
	}
	if _, err := os.Stat(filepath.Join(out, "status.json")); !os.IsNotExist(err) {
		t.Fatal("unverified artifact was published")
	}
}
func TestArtifactsPullRefusesOversizedFileBeforeReadingIt(t *testing.T) {
	source := fixture(t)
	f, err := os.Create(filepath.Join(source, "too-large.bin"))
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(17 * 1024 * 1024)
	f.Close()
	c := connection(t, &host{})
	if _, err = jobops.Pull(context.Background(), c, source, t.TempDir(), []string{"too-large.bin"}); err == nil {
		t.Fatal("oversized artifact accepted")
	}
}

func TestArtifactPullDoesNotBlockOnExistingNamedPipe(t *testing.T) {
	tool, err := exec.LookPath("mkfifo")
	if err != nil {
		t.Skip("POSIX named-pipe contract")
	}
	source := fixture(t)
	out := t.TempDir()
	if data, err := exec.Command(tool, filepath.Join(out, "status.json")).CombinedOutput(); err != nil {
		t.Skipf("mkfifo unavailable: %v %s", err, data)
	}
	c := connection(t, &host{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := jobops.Pull(ctx, c, source, out, []string{"status.json"})
	if err != nil {
		t.Fatal(err)
	}
	if result["verified_files"] != 1 {
		t.Fatal(result)
	}
}
