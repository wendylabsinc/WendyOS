package go2

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func TestMaterializeExampleStandaloneBuildInputs(t *testing.T) {
	for _, kind := range []string{"patrol", "roam", "teleop", "sensors"} {
		t.Run(kind, func(t *testing.T) {
			path, err := MaterializeExample(kind, filepath.Join(t.TempDir(), kind))
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := appconfig.LoadFromFile(filepath.Join(path, "wendy.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			if cfg.AppID != "sh.wendy.simulator.go2.examples."+kind {
				t.Fatalf("unexpected example app ID %q", cfg.AppID)
			}
			tree, _ := fs.Sub(exampleSources, "examples/"+kind)
			files, err := treeFiles(tree)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range files {
				if strings.HasPrefix(filepath.Base(name), "test_") || strings.HasSuffix(name, ".pyc") || strings.Contains(name, "__pycache__") {
					t.Fatalf("generated or test file bundled: %s", name)
				}
				want, err := os.ReadFile(filepath.Join("examples", kind, filepath.FromSlash(name)))
				if err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(filepath.Join(path, filepath.FromSlash(name)))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("source %s was not copied intact: %v", name, err)
				}
			}
			// Every COPY input in the shipped Dockerfile must resolve in the
			// standalone directory, including vendored Unitree message packages.
			dockerfile, err := os.ReadFile(filepath.Join(path, "Dockerfile"))
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(dockerfile), "\n") {
				fields := strings.Fields(line)
				if len(fields) < 3 || fields[0] != "COPY" {
					continue
				}
				for _, input := range fields[1 : len(fields)-1] {
					if _, err := os.Stat(filepath.Join(path, input)); err != nil {
						t.Fatalf("missing Docker COPY input %q: %v", input, err)
					}
				}
			}
			if kind != "sensors" {
				for _, name := range []string{"LICENSE", "UPSTREAM.md", "CMakeLists.txt", "package.xml", "msg/Request.msg", "msg/RequestHeader.msg", "msg/RequestIdentity.msg", "msg/RequestLease.msg", "msg/RequestPolicy.msg"} {
					if _, err := os.Stat(filepath.Join(path, "unitree_api", name)); err != nil {
						t.Fatalf("missing Unitree build/legal input %s: %v", name, err)
					}
				}
			}
			if err := os.WriteFile(filepath.Join(path, "go2_io.py"), []byte("# user edit\n"), 0o644); err != nil {
				t.Fatalf("editable source is not writable: %v", err)
			}
		})
	}
}

func TestMaterializeExampleDoesNotClobber(t *testing.T) {
	root := t.TempDir()
	path, err := MaterializeExample("patrol", filepath.Join(root, "patrol"))
	if err != nil {
		t.Fatal(err)
	}
	edited := filepath.Join(path, "patrol.py")
	if err := os.WriteFile(edited, []byte("user edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeExample("patrol", path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("err = %v, want existing directory error", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeExample("patrol", link); !errors.Is(err, os.ErrExist) {
		t.Fatalf("err = %v, want existing symlink error", err)
	}
	got, _ := os.ReadFile(edited)
	if string(got) != "user edit" {
		t.Fatal("existing edits were overwritten")
	}
}

func TestMaterializeExampleRejectsUnknownKindsBeforeWriting(t *testing.T) {
	for _, kind := range []string{"", "hallway", "../patrol", "patrol/../../other"} {
		path := filepath.Join(t.TempDir(), "app")
		if _, err := MaterializeExample(kind, path); err == nil {
			t.Fatalf("accepted example %q", kind)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid kind %q created a directory", kind)
		}
	}
}
