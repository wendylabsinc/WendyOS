package packagecache

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestResumeHardlink(t *testing.T) {
	cfg, data := fixture()
	path := filepath.Join(t.TempDir(), "cache")
	c := open(t, path, cfg)
	base := getter(data, nil)
	hash := digest(data)
	_, _, err := c.Fetch(context.Background(), hash, int64(len(data)), func(ctx context.Context, r evidence.GetRequest) (evidence.GetResponse, error) {
		if r.Offset > 0 {
			return evidence.GetResponse{}, errors.New("link lost")
		}
		return base(ctx, r)
	})
	if err == nil {
		t.Fatal("expected interrupted prefix")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	linked := false
	_, _, err = c.Fetch(context.Background(), hash, int64(len(data)), func(ctx context.Context, r evidence.GetRequest) (evidence.GetResponse, error) {
		if !linked {
			if err := os.Link(filepath.Join(path, hash+".data"), outside); err != nil {
				t.Fatal(err)
			}
			linked = true
		}
		return base(ctx, r)
	})
	stat, e := os.Stat(outside)
	if e != nil {
		t.Fatal(e)
	}
	if err == nil {
		t.Fatal("unsafe descriptor accepted")
	}
	if stat.Size() != evidence.MaxChunk {
		t.Fatalf("external hardlink changed from committed %d bytes to %d bytes; fetch err=%v", evidence.MaxChunk, stat.Size(), err)
	}
}

func TestUnsafeWritableFiles(t *testing.T) {
	for _, kind := range []string{"hardlink", "public", "symlink", "fifo", "directory"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "data")
			if kind == "fifo" {
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			} else if kind == "directory" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if kind == "symlink" {
				if err := os.Symlink(filepath.Join(dir, "outside"), path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, []byte("unchanged"), 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "hardlink" {
					if err := os.Link(path, filepath.Join(dir, "outside")); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "public" {
					if err := os.Chmod(path, 0644); err != nil {
						t.Fatal(err)
					}
				}
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			f, err := openWritable(root, "data", os.O_WRONLY, 0)
			if err == nil {
				f.Close()
				t.Fatal("unsafe writable descriptor accepted")
			}
			if kind == "hardlink" || kind == "public" {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "unchanged" {
					t.Fatal("unsafe file mutated", err)
				}
			}
		})
	}
}
