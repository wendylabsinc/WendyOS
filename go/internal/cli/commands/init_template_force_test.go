package commands

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type initTemplateFixtureTransport func(*http.Request) (*http.Response, error)

func (f initTemplateFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// Keep the command's real download, archive extraction, rendering and writes;
// redirect only its archive HTTP request to a controlled local server.
func installInitTemplateArchiveFixture(t *testing.T) {
	t.Helper()
	archive := buildTestTarball(t, "templates-fixture", "swift", "simple-api", map[string]string{
		"template.json":                `{"name":"simple-api"}`,
		"Package.swift":                "// template package for {{.APP_ID}}\n",
		"wendy.json":                   `{"appId":"{{.APP_ID}}","platform":"linux"}`,
		"Sources/simple-api/App.swift": "print(\"{{.APP_ID}}\")\n",
		"asset.bin":                    string([]byte{0, 255, 42}),
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/wendylabsinc/templates/tar.gz/refs/heads/main" {
			t.Errorf("unexpected archive path: %s", req.URL.Path)
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)
	fixtureURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = initTemplateFixtureTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "codeload.github.com" {
			return nil, fmt.Errorf("unexpected network request: %s", req.URL)
		}
		local := req.Clone(req.Context())
		local.URL.Scheme = fixtureURL.Scheme
		local.URL.Host = fixtureURL.Host
		return previous.RoundTrip(local)
	})
	stubFetchRepoMeta(t, &repoMeta{
		Templates: []repoMetaTemplate{{Name: "simple-api", Languages: []string{"swift"}}},
		Languages: []repoMetaLanguage{{Key: "swift", Name: "Swift"}},
	}, nil)
	stubNonInteractive(t)
}

func TestInitTemplate_ForceOverlayAndDefaultEmptyDestinations(t *testing.T) {
	for _, mode := range []string{"new", "empty", "here-empty", "force", "here-force"} {
		t.Run(mode, func(t *testing.T) {
			cwd := chdirTemp(t)
			installInitTemplateArchiveFixture(t)
			here := strings.HasPrefix(mode, "here-")
			force := strings.Contains(mode, "force")
			dest := filepath.Join(cwd, "sapi")
			if here {
				dest = cwd
			}
			if mode != "new" {
				if err := os.MkdirAll(dest, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			precious := []byte("keep unrelated bytes\x00\xff\n")
			if force {
				for name, content := range map[string][]byte{
					"Package.swift": []byte("old package\n"),
					"wendy.json":    []byte(`{"appId":"old-project"}`),
					"asset.bin":     []byte("old asset"),
					"IMPORTANT.txt": precious,
				} {
					if err := os.WriteFile(filepath.Join(dest, name), content, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			cmd := newInitCmd()
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			args := []string{"--app-id", "sapi", "--target", "wendyos", "--language", "swift", "--template", "simple-api", "--assistant", "skip", "--git-init", "no"}
			if here {
				args = append(args, "--here")
			}
			if force {
				args = append(args, "--force")
			}
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("initializing %s: %v", mode, err)
			}
			for name, want := range map[string][]byte{
				"Package.swift":          []byte("// template package for sapi\n"),
				"wendy.json":             []byte(`{"appId":"sapi","platform":"linux"}`),
				"Sources/sapi/App.swift": []byte("print(\"sapi\")\n"),
				"asset.bin":              {0, 255, 42},
			} {
				got, err := os.ReadFile(filepath.Join(dest, name))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("%s: got %q, want %q", name, got, want)
				}
			}
			if force {
				got, err := os.ReadFile(filepath.Join(dest, "IMPORTANT.txt"))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, precious) {
					t.Fatalf("unrelated file changed: %q", got)
				}
			}
		})
	}
}

func TestInitTemplate_ForceWriteErrorFailsLoudly(t *testing.T) {
	cwd := chdirTemp(t)
	installInitTemplateArchiveFixture(t)
	blocked := filepath.Join(cwd, "sapi", "Package.swift")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte("keep directory contents\n")
	path := filepath.Join(blocked, "IMPORTANT.txt")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newInitCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"--app-id", "sapi", "--target", "wendyos", "--language", "swift", "--template", "simple-api", "--force", "--assistant", "skip", "--git-init", "no"})
	var runErr error
	stderr := captureStderr(t, func() { runErr = cmd.Execute() })
	if runErr == nil || !strings.Contains(runErr.Error(), "Package.swift") {
		t.Fatalf("want a path-scoped write error, got %v", runErr)
	}
	if strings.Contains(stderr, "Your project is ready!") {
		t.Fatalf("write failure reported success: %s", stderr)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("directory contents changed: %q", got)
	}
}

func TestInitTemplate_ForceRequiresTemplate(t *testing.T) {
	cwd := chdirTemp(t)
	cmd := newInitCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"--app-id", "sapi", "--target", "wendyos", "--language", "swift", "--force", "--assistant", "skip", "--git-init", "no"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--force requires --template") {
		t.Fatalf("want an actionable template-only flag error, got %v", err)
	}
	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("invalid flag combination changed the directory: %v", entries)
	}
}
