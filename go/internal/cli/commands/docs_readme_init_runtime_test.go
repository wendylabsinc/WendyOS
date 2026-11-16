//go:build regression_docs_fixture

package commands

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/shlex"
)

// This fixture executes the README handler with actual pinned repository bytes.
// Only outbound template HTTP is translated to the owned loopback server.
// The caller owns the output directory so it can compile and run the generated
// app after this Go process exits. Device discovery/deployment is never run.
func TestREADMEInitPublishedTemplateRuntime(t *testing.T) {
	checkout := os.Getenv("WENDY_DOCS_TEMPLATE_CHECKOUT")
	archive := os.Getenv("WENDY_DOCS_TEMPLATE_ARCHIVE")
	work := os.Getenv("WENDY_DOCS_TEMPLATE_WORK")
	if checkout == "" || archive == "" || work == "" {
		t.Fatal("explicit pinned template fixture paths are required")
	}
	oldDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile(filepath.Join(oldDirectory, "../../../../README.md"))
	if err != nil {
		t.Fatal(err)
	}
	var line string
	for _, candidate := range strings.Split(string(readme), "\n") {
		if strings.HasPrefix(candidate, "wendy init hello-wendy ") {
			if line != "" {
				t.Fatal("ambiguous README init command")
			}
			line = candidate
		}
	}
	if line == "" {
		t.Fatal("README init command missing")
	}
	args, err := shlex.Split(line)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(work)
	if err != nil || len(entries) != 0 {
		t.Fatalf("owned work directory must be empty: %v", err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldDirectory); err != nil {
			t.Error(err)
		}
	})
	for key, value := range map[string]string{
		"HOME": t.TempDir(), "WENDY_CONFIG_DIR": t.TempDir(), "WENDY_SECRET_STORE": "file",
		"WENDY_ANALYTICS": "false", "WENDY_NO_UPDATE_CHECK": "1", "CI": "1",
	} {
		t.Setenv(key, value)
	}
	paths := map[string]string{
		"raw.githubusercontent.com/wendylabsinc/templates/main/meta.json":                     filepath.Join(checkout, "meta.json"),
		"raw.githubusercontent.com/wendylabsinc/templates/main/node/simple-api/template.json": filepath.Join(checkout, "node/simple-api/template.json"),
		"codeload.github.com/wendylabsinc/templates/tar.gz/refs/heads/main":                   archive,
	}
	seen := make(map[string]int)
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		key := strings.TrimPrefix(request.URL.Path, "/")
		path, ok := paths[key]
		if !ok {
			http.NotFound(w, request)
			return
		}
		mu.Lock()
		seen[key]++
		mu.Unlock()
		http.ServeFile(w, request, path)
	}))
	t.Cleanup(server.Close)
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = initTemplateFixtureTransport(func(request *http.Request) (*http.Response, error) {
		key := request.URL.Host + request.URL.Path
		if request.Method != http.MethodGet || request.URL.Scheme != "https" || request.URL.RawQuery != "" {
			return nil, fmt.Errorf("unapproved template request: %s %s", request.Method, request.URL)
		}
		if _, ok := paths[key]; !ok {
			return nil, fmt.Errorf("unapproved template URL: %s", request.URL)
		}
		local := request.Clone(request.Context())
		copyURL := *request.URL
		copyURL.Scheme, copyURL.Host, copyURL.Path = base.Scheme, base.Host, "/"+key
		local.URL = &copyURL
		return transport.RoundTrip(local)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	root := NewRootCmd()
	root.SetArgs(args[1:])
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("README_INIT_EXECUTION: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, key := range []string{
		"raw.githubusercontent.com/wendylabsinc/templates/main/meta.json",
		"codeload.github.com/wendylabsinc/templates/tar.gz/refs/heads/main",
	} {
		if seen[key] == 0 {
			t.Fatalf("actual template request was not observed: %s", key)
		}
	}
	for _, name := range []string{"package.json", "src/index.ts", "tsconfig.json", "wendy.json", "Dockerfile"} {
		data, err := os.ReadFile(filepath.Join(work, "hello-wendy", name))
		if err != nil || len(data) == 0 || strings.Contains(string(data), "{{.") {
			t.Fatalf("README_INIT_RENDER: %s remains missing/unrendered: %v", name, err)
		}
	}
	t.Log("README_INIT_GENERATED_VERIFIED: actual documented argv and published template HTTP/archive/render path")
}
