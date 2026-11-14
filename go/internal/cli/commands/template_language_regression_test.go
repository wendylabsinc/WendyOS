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
	"sync/atomic"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func installLanguageArchive(t *testing.T, language, name, target string, files map[string]string, alternatives []string) *atomic.Int32 {
	t.Helper()
	archive := buildTestTarball(t, "templates-language-fixture", language, name, files)
	requests := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/wendylabsinc/templates/tar.gz/refs/heads/main" {
			t.Errorf("unexpected archive request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)
	localURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = initTemplateFixtureTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "codeload.github.com" {
			return nil, fmt.Errorf("unexpected external request: %s", r.URL)
		}
		local := r.Clone(r.Context())
		local.URL.Scheme, local.URL.Host = localURL.Scheme, localURL.Host
		return previous.RoundTrip(local)
	})
	languages := []string{language}
	languages = append(languages, alternatives...)
	metaLanguages := []repoMetaLanguage{{Key: language, Name: language}, {Key: "swift", Name: "Swift"}}
	stubFetchRepoMeta(t, &repoMeta{
		Templates: []repoMetaTemplate{{Name: name, Targets: []string{target}, Languages: languages}},
		Languages: metaLanguages,
	}, nil)
	stubNonInteractive(t)
	return requests
}

func TestTemplateLanguageMojoRendersDownloadedSources(t *testing.T) {
	for _, variant := range []string{"default", "override", "undeclared-token"} {
		t.Run(variant, func(t *testing.T) {
			cwd := chdirTemp(t)
			const templateName = "mojo-api"
			const appID = "mojo-sentinel-project"
			source := "fn main():\n    print(\"{{.APP_ID}}\")\n    print({{.PORT}})\n"
			if variant == "undeclared-token" {
				source += "    print({{.UNDECLARED}})\n"
			}
			binary := "\x00\xff{{.PORT}}\x00"
			requests := installLanguageArchive(t, "mojo", templateName, "wendyos", map[string]string{
				"template.json":          `{"name":"mojo-api","variables":[{"name":"PORT","type":"integer","default":8080}]}`,
				"wendy.json":             `{"appId":"{{.APP_ID}}","platform":"linux","language":"mojo"}`,
				"src/mojo-api/main.mojo": source,
				"asset.bin":              binary,
			}, nil)
			args := []string{"--app-id", appID, "--target", "wendyos", "--template", templateName, "--assistant", "skip", "--git-init", "no"}
			port := "8080"
			if variant == "override" {
				args = append(args, "--language", "mojo", "--var", "PORT=4177")
				port = "4177"
			}
			command := newInitCmd()
			command.SetArgs(args)
			err := command.Execute()
			if requests.Load() != 1 {
				t.Fatalf("want one real archive download, got %d", requests.Load())
			}
			path := filepath.Join(cwd, appID, "src", appID, "main.mojo")
			if variant == "undeclared-token" {
				if err == nil || !strings.Contains(err.Error(), "UNDECLARED") || !strings.Contains(err.Error(), "main.mojo") {
					t.Fatalf("bad Mojo template did not fail at its source path: %v", err)
				}
				if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
					t.Fatal("invalid Mojo source was published")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			want := "fn main():\n    print(\"" + appID + "\")\n    print(" + port + ")\n"
			if err != nil || string(got) != want {
				t.Fatalf("Mojo source still contains unresolved/wrong values: %q (%v)", got, err)
			}
			if got, err := os.ReadFile(filepath.Join(cwd, appID, "asset.bin")); err != nil || !bytes.Equal(got, []byte(binary)) {
				t.Fatalf("binary template asset changed: %q (%v)", got, err)
			}
		})
	}
}

func TestTemplateLanguageLiteNativeProjectsRetainTargetAndEntitlements(t *testing.T) {
	for _, language := range []string{"c", "cpp"} {
		for _, explicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-explicit-%t", language, explicit), func(t *testing.T) {
				cwd := chdirTemp(t)
				appID := "lite-sentinel-" + language
				var alternatives []string
				if explicit {
					alternatives = []string{"swift"}
				}
				requests := installLanguageArchive(t, language, "blink-rgb-led", "wendy-lite", map[string]string{
					"template.json":         "{\"name\":\"blink-rgb-led\"}",
					"wendy.json":            `{"appId":"{{.APP_ID}}","platform":"wendy-lite","language":"` + language + `","entitlements":[]}`,
					"CMakeLists.txt":        "cmake_minimum_required(VERSION 3.16)\ninclude($ENV{IDF_PATH}/tools/cmake/project.cmake)\nproject({{.APP_ID}})\n",
					"main/main." + language: "// {{.APP_ID}}\nvoid app_main(void) {}\n",
				}, alternatives)
				args := []string{"--app-id", appID, "--target", "wendy-lite", "--template", "blink-rgb-led", "--assistant", "skip", "--git-init", "no"}
				if explicit {
					args = append(args, "--language", language)
				}
				command := newInitCmd()
				command.SetArgs(args)
				if err := command.Execute(); err != nil {
					t.Fatalf("declared non-Swift template rejected: %v", err)
				}
				if requests.Load() != 1 {
					t.Fatalf("want one archive request, got %d", requests.Load())
				}
				dir := filepath.Join(cwd, appID)
				configPath := filepath.Join(dir, "wendy.json")
				cfg, err := appconfig.LoadFromFile(configPath)
				if err != nil || cfg.AppID != appID || cfg.Language != language || cfg.Platform != appconfig.PlatformWendyLite || len(cfg.Entitlements) != 0 {
					t.Fatalf("bad generated native configuration: %+v %v", cfg, err)
				}
				if err := cfg.Validate(); err != nil {
					t.Fatalf("generated native configuration is invalid: %v", err)
				}
				if got, err := os.ReadFile(filepath.Join(dir, "main/main."+language)); err != nil || !strings.Contains(string(got), appID) || strings.Contains(string(got), "{{") {
					t.Fatalf("native source was not rendered: %q %v", got, err)
				}
				if detected, err := detectProjectType(dir); err != nil || detected != "esp-idf" {
					t.Fatalf("generated native project is not discoverable: %q %v", detected, err)
				}
				// Exercise the build-time config creation path as well: it must
				// infer bare-metal defaults from files, without a language hint.
				if err := os.Remove(configPath); err != nil {
					t.Fatal(err)
				}
				if _, err := ensureAppConfig(configPath, true); err != nil {
					t.Fatal(err)
				}
				cfg, err = appconfig.LoadFromFile(configPath)
				if err != nil || cfg.AppID != appID || cfg.Platform != appconfig.PlatformWendyLite || len(cfg.Entitlements) != 0 {
					t.Fatalf("ESP-IDF auto config acquired host capabilities: %+v %v", cfg, err)
				}
				if err := cfg.Validate(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestTemplateLanguageLiteRejectsUnavailableLanguageBeforeWrites(t *testing.T) {
	cwd := chdirTemp(t)
	requests := installLanguageArchive(t, "c", "blink-rgb-led", "wendy-lite", map[string]string{"template.json": `{"name":"blink-rgb-led"}`}, nil)
	command := newInitCmd()
	command.SetArgs([]string{"--app-id", "unavailable-language", "--target", "wendy-lite", "--template", "blink-rgb-led", "--language", "swift", "--assistant", "skip", "--git-init", "no"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "not available for language") {
		t.Fatalf("undeclared language silently accepted: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("unavailable language reached archive download")
	}
	if _, err := os.Stat(filepath.Join(cwd, "unavailable-language")); !os.IsNotExist(err) {
		t.Fatal("invalid selection created project files")
	}
}

func TestTemplateLanguageAutoConfigKeepsHostDefaultsAndExistingConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "wendy.json")
	cfg, err := ensureAppConfig(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Language != "python" || cfg.Platform == appconfig.PlatformWendyLite || len(cfg.Entitlements) != 2 || cfg.Entitlements[0].Type != appconfig.EntitlementNetwork || cfg.Entitlements[1].Type != appconfig.EntitlementGPU {
		t.Fatalf("bare-metal defaults leaked into a host project: %+v", cfg)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ensureAppConfig(path, true); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
		t.Fatal("existing configuration was rewritten")
	}
}
