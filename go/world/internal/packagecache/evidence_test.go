package packagecache

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
)

func mediaGetter(data []byte, offsets *[]int64) Getter {
	base := getter(data, offsets)
	return func(ctx context.Context, r evidence.GetRequest) (evidence.GetResponse, error) {
		v, e := base(ctx, r)
		v.ContentType = "image/png"
		return v, e
	}
}
func TestEvidenceModePreservesPackageDefaults(t *testing.T) {
	cfg, data := fixture()
	path := filepath.Join(t.TempDir(), "cache")
	c := open(t, path, cfg)
	if _, _, err := c.Fetch(context.Background(), digest(data), int64(len(data)), mediaGetter(data, nil)); err == nil {
		t.Fatal("package fetch accepted image")
	}
	if _, _, err := c.FetchEvidence(context.Background(), digest(data), int64(len(data)), "image/png", mediaGetter(data, nil)); err == nil {
		t.Fatal("package cache enabled media implicitly")
	}
	c.Close()
	cfg.Evidence = true
	if c, e := Open(path, cfg); e == nil {
		c.Close()
		t.Fatal("cache mode changed")
	}
	c = open(t, filepath.Join(t.TempDir(), "media"), cfg)
	if _, _, err := c.FetchEvidence(context.Background(), digest(data), int64(len(data))+1, "image/png", mediaGetter(data, nil)); err == nil {
		t.Fatal("wrong exact size accepted")
	}
	v, _ := c.Inventory()
	if len(v.Entries) != 0 {
		t.Fatal("wrong reference allocated checkpoint")
	}
	if _, _, err := c.FetchEvidence(context.Background(), digest(data), int64(len(data)), "audio/wav", mediaGetter(data, nil)); err == nil {
		t.Fatal("wrong MIME accepted")
	}
	if got, _, err := c.FetchEvidence(context.Background(), digest(data), int64(len(data)), "image/png", mediaGetter(data, nil)); err != nil || !bytes.Equal(got, data) {
		t.Fatal(err)
	}
}
func TestEvidenceCrashRecovery(t *testing.T) {
	for _, test := range []struct {
		stage  string
		count  int
		offset int64
	}{{"after-data-sync", 1, 0}, {"after-data-sync", 2, evidence.MaxChunk}, {"after-checkpoint-rename", 2, 2 * evidence.MaxChunk}, {"after-checkpoint-sync", 2, 2 * evidence.MaxChunk}} {
		t.Run(test.stage+strconv.Itoa(test.count), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "media")
			cmd := exec.Command(os.Args[0], "-test.run=^TestEvidenceCrashHelper$")
			cmd.Env = append(os.Environ(), "WORLD_EVIDENCE_CRASH_PATH="+path, "WORLD_EVIDENCE_CRASH_STAGE="+test.stage, "WORLD_EVIDENCE_CRASH_COUNT="+strconv.Itoa(test.count))
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || !exit.Sys().(syscall.WaitStatus).Signaled() {
				t.Fatal("child did not die at persistence boundary", string(output), err)
			}
			cfg, data := fixture()
			cfg.Evidence = true
			c := open(t, path, cfg)
			var offsets []int64
			get := mediaGetter(data, &offsets)
			if e, ok := c.entries[digest(data)]; ok {
				base := get
				get = func(ctx context.Context, r evidence.GetRequest) (evidence.GetResponse, error) {
					v, err := base(ctx, r)
					v.RetainUntilUnixMS = e.RetainUntilUnixMS
					return v, err
				}
			}
			got, progress, err := c.FetchEvidence(context.Background(), digest(data), int64(len(data)), "image/png", get)
			if err != nil || !bytes.Equal(got, data) || offsets[0] != test.offset || progress.ResumedBytes != test.offset {
				t.Fatal(progress, offsets, err)
			}
		})
	}
}
func TestEvidenceCrashHelper(t *testing.T) {
	path := os.Getenv("WORLD_EVIDENCE_CRASH_PATH")
	if path == "" {
		t.Skip("child helper")
	}
	cfg, data := fixture()
	cfg.Evidence = true
	c, err := Open(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	limit, _ := strconv.Atoi(os.Getenv("WORLD_EVIDENCE_CRASH_COUNT"))
	count := 0
	c.afterWrite = func(stage string) error {
		if stage == os.Getenv("WORLD_EVIDENCE_CRASH_STAGE") {
			count++
			if count == limit {
				syscall.Kill(os.Getpid(), syscall.SIGKILL)
			}
		}
		return nil
	}
	c.FetchEvidence(context.Background(), digest(data), int64(len(data)), "image/png", mediaGetter(data, nil))
	t.Fatal("did not crash")
}
