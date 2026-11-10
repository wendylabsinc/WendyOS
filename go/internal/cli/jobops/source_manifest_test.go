package jobops_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/jobops"
)

func TestPinnedSourceManifestRejectsExtraFilesBeforeStartingJob(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "rl"), 0700)
	source := []byte("# protected worker\n")
	os.WriteFile(filepath.Join(root, "rl/worker.py"), source, 0600)
	hash := func(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
	raw, _ := json.Marshal(map[string]any{"schema": "wendy.sources.v1", "files": map[string]string{"rl/worker.py": hash(source)}, "exact_directories": []string{"rl"}})
	os.WriteFile(filepath.Join(root, "sources.json"), raw, 0600)
	spec := jobops.Spec{Name: "source-contract", WorkingDirectory: root, SourceManifest: &jobops.SourceManifest{Path: "sources.json", SHA256: hash(raw)}, Steps: []jobops.Step{{Name: "check", Argv: []string{"python3", "-c", "print('checked')"}, TimeoutSeconds: 2}}}
	c := connection(t, &host{})
	os.WriteFile(filepath.Join(root, "rl/extra.py"), []byte("# not in checkpoint source set\n"), 0600)
	if _, err := jobops.Submit(context.Background(), c, spec); err == nil || !strings.Contains(err.Error(), "source directory differs") {
		t.Fatalf("extra source reached job execution: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".wendy")); !os.IsNotExist(err) {
		t.Fatalf("failed validation created a job: %v", err)
	}
	os.Remove(filepath.Join(root, "rl/extra.py"))
	receipt, err := jobops.SubmitObserved(context.Background(), c, spec, 3)
	if err != nil || receipt["reported_state"] != "completed" {
		t.Fatalf("valid source manifest rejected: %v %+v", err, receipt)
	}
	checked := receipt["source_check"].(map[string]any)
	if checked["verified_files"] != float64(1) || checked["manifest_sha256"] != hash(raw) {
		t.Fatalf("missing source provenance: %+v", receipt)
	}
	again, err := jobops.SubmitObserved(context.Background(), c, spec, 3)
	if err != nil || again["reused"] != true || again["runner_pid"] != receipt["runner_pid"] {
		t.Fatalf("manifest retry restarted job: %v %+v", err, again)
	}
	os.WriteFile(filepath.Join(root, "sources.json"), append(raw, ' '), 0600)
	reused, err := jobops.Submit(context.Background(), c, spec)
	if err != nil || reused["reused"] != true || reused["runner_pid"] != receipt["runner_pid"] {
		t.Fatalf("source edit blocked original-job retrieval: %v %+v", err, reused)
	}
	spec.Name = "changed-source-new-job"
	if _, err = jobops.Submit(context.Background(), c, spec); err == nil || !strings.Contains(err.Error(), "source manifest hash mismatch") {
		t.Fatalf("new job accepted changed manifest: %v", err)
	}
}

func TestSourceManifestReferenceIsValidatedBeforeHostAccess(t *testing.T) {
	for _, name := range []string{"/absolute", "../escape", "a/../escape", ".", "bad\x00name"} {
		spec := jobops.Spec{Name: "invalid-source", WorkingDirectory: "/tmp/project", SourceManifest: &jobops.SourceManifest{Path: name, SHA256: strings.Repeat("a", 64)}, Steps: []jobops.Step{{Name: "check", Argv: []string{"python3"}, TimeoutSeconds: 2}}}
		if _, err := jobops.Submit(context.Background(), nil, spec); err == nil || !strings.Contains(err.Error(), "source_manifest") {
			t.Fatalf("unsafe manifest reached connection: %q %v", name, err)
		}
	}
}

func TestSourceManifestCannotHashFilesOutsideItsRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "private.py"), []byte("private"), 0600)
	if err := os.Symlink(outside, filepath.Join(root, "alias")); err != nil {
		t.Skip(err)
	}
	hash := func(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
	raw, _ := json.Marshal(map[string]any{"schema": "wendy.sources.v1", "files": map[string]string{"alias/private.py": hash([]byte("private"))}})
	os.WriteFile(filepath.Join(root, "sources.json"), raw, 0600)
	spec := jobops.Spec{Name: "confined-source", WorkingDirectory: root, SourceManifest: &jobops.SourceManifest{Path: "sources.json", SHA256: hash(raw)}, Steps: []jobops.Step{{Name: "check", Argv: []string{"python3"}, TimeoutSeconds: 2}}}
	if _, err := jobops.Submit(context.Background(), connection(t, &host{}), spec); err == nil {
		t.Fatal("intermediate source symlink was followed")
	}
	if _, err := os.Stat(filepath.Join(root, ".wendy")); !os.IsNotExist(err) {
		t.Fatal("bad source created a job")
	}
}
