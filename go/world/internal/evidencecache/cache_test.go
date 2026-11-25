package evidencecache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/packagecache"
)

func testConfig() Config {
	return Config{Sources: []Source{{ID: "a", Scope: packagecache.Scope{Tenant: "t", Site: "s", Domain: "simulation", Audience: "a", Endpoint: "http://localhost:1"}}, {ID: "b", Scope: packagecache.Scope{Tenant: "t", Site: "s", Domain: "simulation", Audience: "b", Endpoint: "http://localhost:2"}}}, MaxBytes: 100}
}
func TestBoundedCatalogAndAnchoredSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache")
	cfg := testConfig()
	c, err := Open(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(path, cfg)
	if err == nil {
		other.Close()
		t.Fatal("concurrent cache opened")
	}
	source, err := c.Source("a")
	if err != nil {
		t.Fatal(err)
	}
	inv, _ := source.Inventory()
	source.Close()
	if inv.MaxBytes != 50 || inv.MaxEntries != 128 {
		t.Fatal(inv)
	}
	c.Close()
	invAll, err := Inspect(path)
	if err != nil || len(invAll.Sources) != 2 || invAll.MaxBytes != 100 {
		t.Fatal(invAll, err)
	}
	cfg.Sources[0].Scope.Endpoint = "http://localhost:3"
	if c, err = Open(path, cfg); err == nil {
		c.Close()
		t.Fatal("changed endpoint reused cache")
	}
	cfg = testConfig()
	cfg.Sources[1].Scope.Site = "foreign"
	if c, err = Open(filepath.Join(t.TempDir(), "foreign"), cfg); err == nil {
		c.Close()
		t.Fatal("cross-site cache created")
	}
	cfg = testConfig()
	cfg.Sources = append(cfg.Sources, cfg.Sources[0])
	if c, err = Open(filepath.Join(t.TempDir(), "duplicate"), cfg); err == nil {
		c.Close()
		t.Fatal("duplicate sources admitted")
	}
	c, err = Open(path, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	foreign := t.TempDir()
	os.Chmod(foreign, 0700)
	if err = os.Symlink(foreign, filepath.Join(path, "b")); err != nil {
		t.Fatal(err)
	}
	if source, err = c.Source("b"); err == nil {
		source.Close()
		t.Fatal("source symlink followed")
	}
	if _, err = os.Stat(filepath.Join(foreign, "scope.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("outside path modified")
	}
}
func TestExportAuthoritySafetyAndNoReplacement(t *testing.T) {
	for _, variant := range []string{"ok", "denied", "expired", "occupied", "symlink", "hardlink", "fifo", "changed-bytes", "public-staging", "late-destination"} {
		t.Run(variant, func(t *testing.T) {
			dir := t.TempDir()
			os.Chmod(dir, 0700)
			target := filepath.Join(dir, "image.png")
			data := []byte("verified synthetic bytes")
			until := time.Now().Add(time.Hour).UnixMilli()
			if variant == "occupied" {
				os.WriteFile(target, []byte("keep"), 0600)
			}
			outside := filepath.Join(dir, "outside")
			os.WriteFile(outside, []byte("keep"), 0600)
			if variant == "symlink" {
				os.Symlink(outside, target)
			}
			authorize := func() error {
				switch variant {
				case "denied":
					return errors.New("revoked")
				case "expired":
					until = 0
					return context.DeadlineExceeded
				case "late-destination":
					return os.WriteFile(target, []byte("keep"), 0600)
				}
				entries, _ := os.ReadDir(dir)
				var stage string
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), ".evidence-export-") {
						stage = filepath.Join(dir, e.Name())
					}
				}
				switch variant {
				case "hardlink":
					return os.Link(stage, filepath.Join(dir, "linked"))
				case "fifo":
					os.Remove(stage)
					return syscall.Mkfifo(stage, 0600)
				case "changed-bytes":
					return os.WriteFile(stage, []byte("tampered"), 0600)
				case "public-staging":
					return os.Chmod(stage, 0644)
				}
				return nil
			}
			err := Export(target, data, until, authorize)
			if variant == "ok" {
				if err != nil {
					t.Fatal(err)
				}
				got, _ := os.ReadFile(target)
				if string(got) != string(data) {
					t.Fatal("wrong export")
				}
			} else {
				if err == nil {
					t.Fatal("unsafe export succeeded")
				}
				if variant != "occupied" && variant != "symlink" && variant != "late-destination" {
					if _, err = os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("failed export destination exists")
					}
				}
			}
			got, _ := os.ReadFile(outside)
			if string(got) != "keep" {
				t.Fatal("outside modified")
			}
		})
	}
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	if err := Export(filepath.Join(dir, "expired"), []byte("x"), 1, func() error { return nil }); err == nil {
		t.Fatal("expired export succeeded")
	}
}
