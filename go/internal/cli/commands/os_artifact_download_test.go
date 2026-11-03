package commands

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDownloadArtifactToTempNoninteractive(t *testing.T) {
	for _, tc := range []struct {
		name        string
		interactive bool
		json        bool
		chunked     bool
	}{
		{name: "no terminal"},
		{name: "unknown content length", chunked: true},
		{name: "JSON on terminal", interactive: true, json: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateArtifactDownload(t, tc.interactive, tc.json)
			const artifact = "test artifact bytes"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.chunked {
					w.(http.Flusher).Flush()
				}
				fmt.Fprint(w, artifact)
			}))
			defer server.Close()

			path, err := downloadArtifactToTemp(server.URL + "/image.wendy?token=test")
			if err != nil {
				t.Fatal(err)
			}
			defer os.Remove(path)
			if filepath.Ext(path) != ".wendy" {
				t.Fatalf("artifact suffix lost: %s", path)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != artifact {
				t.Fatalf("artifact = %q, err = %v", data, err)
			}
		})
	}
}

func TestDownloadArtifactToTempFailureCleanup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		short  bool
		want   string
	}{
		{name: "HTTP failure", status: http.StatusNotFound, want: "status 404"},
		{name: "truncated download", status: http.StatusOK, short: true, want: "unexpected EOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateArtifactDownload(t, false, false)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.short {
					w.Header().Set("Content-Length", "100")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, "short")
			}))
			defer server.Close()

			path, err := downloadArtifactToTemp(server.URL + "/image.wendy")
			if err == nil || !strings.Contains(err.Error(), tc.want) || path != "" {
				t.Fatalf("path = %q, err = %v; want %q", path, err, tc.want)
			}
			cacheDir, err := osCacheDir()
			if err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(cacheDir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("partial artifacts left: %v, err = %v", entries, err)
			}
		})
	}
}

// The OTA artifact shares the image downloader: a connection dropped mid-body
// resumes from the last byte instead of failing the update.
func TestDownloadArtifactToTempResumesAfterDrop(t *testing.T) {
	isolateArtifactDownload(t, false, false)
	s := &flakyImageServer{payload: testImagePayload(8 << 10), ranges: true, headStatus: http.StatusOK, failAt: 5000}
	server := httptest.NewServer(s)
	defer server.Close()

	path, err := downloadArtifactToTemp(server.URL + "/image.wendy")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, s.payload) {
		t.Fatalf("artifact differs from the served bytes (%d of %d), err = %v", len(data), len(s.payload), err)
	}
	if !s.sawRange("bytes=5000-") {
		t.Fatalf("artifact download restarted instead of resuming; saw %q", s.seen)
	}
}

func isolateArtifactDownload(t *testing.T, interactive, json bool) {
	t.Helper()
	// Isolate UserCacheDir on macOS, Linux and Windows. Never use the
	// developer's cache for either success or failure-cleanup checks.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("LocalAppData", t.TempDir())
	useTestDownloadPolicy(t, 200*time.Millisecond)
	originalTerminal, originalJSON := isInteractiveTerminalFn, jsonOutput
	t.Cleanup(func() {
		isInteractiveTerminalFn, jsonOutput = originalTerminal, originalJSON
	})
	isInteractiveTerminalFn = func() bool { return interactive }
	jsonOutput = json
}
