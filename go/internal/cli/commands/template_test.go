package commands

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// buildTestTarball constructs a minimal gzipped tarball that mirrors the
// layout of wendylabsinc/templates. The top-level directory name is arbitrary
// because extractTemplateArchive strips it; "templates-main/" matches what
// GitHub's tarball endpoint emits for the main branch.
func buildTestTarball(t *testing.T, topDir, language, templateName string, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for relPath, content := range files {
		name := topDir + "/" + language + "/" + templateName + "/" + relPath
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("tw.Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz.Close: %v", err)
	}
	return buf.Bytes()
}

func TestExtractTemplateArchive_ReturnsFilesAndManifest(t *testing.T) {
	manifest := `{"name":"simple-api","description":"Test template","variables":[{"name":"PORT","type":"integer","default":8080}]}`
	files := map[string]string{
		"template.json": manifest,
		"main.go":       "package main\n",
		"README.md":     "# {{.APP_ID}}\n",
	}

	archive := buildTestTarball(t, "templates-main", "rust", "simple-api", files)

	got, gotManifest, err := extractTemplateArchive(bytes.NewReader(archive), "rust", "simple-api")
	if err != nil {
		t.Fatalf("extractTemplateArchive: %v", err)
	}

	if gotManifest == nil {
		t.Fatal("expected manifest to be returned")
	}
	if gotManifest.Name != "simple-api" {
		t.Errorf("manifest.Name = %q, want %q", gotManifest.Name, "simple-api")
	}
	if len(gotManifest.Variables) != 1 || gotManifest.Variables[0].Name != "PORT" {
		t.Errorf("manifest.Variables = %+v, want one PORT variable", gotManifest.Variables)
	}

	if _, ok := got["template.json"]; ok {
		t.Error("template.json should be stripped from the returned files map")
	}
	if string(got["main.go"]) != "package main\n" {
		t.Errorf("main.go contents = %q", got["main.go"])
	}
	if string(got["README.md"]) != "# {{.APP_ID}}\n" {
		t.Errorf("README.md contents = %q", got["README.md"])
	}
}

func TestExtractTemplateArchive_MissingManifestErrors(t *testing.T) {
	archive := buildTestTarball(t, "templates-main", "rust", "simple-api", map[string]string{
		"main.go": "package main\n",
	})

	_, _, err := extractTemplateArchive(bytes.NewReader(archive), "rust", "simple-api")
	if err == nil {
		t.Fatal("expected error for tarball missing template.json")
	}
	if !strings.Contains(err.Error(), "no template.json") {
		t.Errorf("error = %q, want error mentioning missing template.json", err.Error())
	}
}

func TestExtractTemplateArchive_IgnoresOtherLanguages(t *testing.T) {
	// Build a tarball containing two templates — rust/simple-api and
	// python/other — and ensure only the rust files come back.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	write := func(name, content string) {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	write("templates-main/rust/simple-api/template.json", `{"name":"simple-api"}`)
	write("templates-main/rust/simple-api/main.rs", "fn main() {}\n")
	write("templates-main/python/other/template.json", `{"name":"other"}`)
	write("templates-main/python/other/app.py", "print('hi')\n")

	if err := tw.Close(); err != nil {
		t.Fatalf("tw.Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz.Close: %v", err)
	}

	files, manifest, err := extractTemplateArchive(bytes.NewReader(buf.Bytes()), "rust", "simple-api")
	if err != nil {
		t.Fatalf("extractTemplateArchive: %v", err)
	}
	if manifest == nil || manifest.Name != "simple-api" {
		t.Errorf("got manifest %+v, want simple-api", manifest)
	}
	if _, ok := files["main.rs"]; !ok {
		t.Errorf("expected main.rs in files map, got keys %v", keys(files))
	}
	if _, ok := files["app.py"]; ok {
		t.Error("python template should not leak into rust files map")
	}
}

func TestDownloadTemplateArchiveFromURL_InvokesProgressCallback(t *testing.T) {
	archive := buildTestTarball(t, "templates-main", "rust", "simple-api", map[string]string{
		"template.json": `{"name":"simple-api"}`,
		"main.rs":       strings.Repeat("fn main() {}\n", 500),
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)

	var (
		mu       sync.Mutex
		calls    int
		lastSize int64
		lastTot  int64
	)
	cb := func(written, total int64) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		lastSize = written
		lastTot = total
	}

	files, manifest, err := downloadTemplateArchiveFromURL(context.Background(), srv.URL, "main", "rust", "simple-api", cb)
	if err != nil {
		t.Fatalf("downloadTemplateArchiveFromURL: %v", err)
	}
	if manifest == nil || manifest.Name != "simple-api" {
		t.Errorf("manifest = %+v", manifest)
	}
	if _, ok := files["main.rs"]; !ok {
		t.Error("main.rs missing from files map")
	}

	mu.Lock()
	defer mu.Unlock()
	if calls == 0 {
		t.Error("progress callback was never invoked")
	}
	if lastTot != int64(len(archive)) {
		t.Errorf("final total = %d, want %d (Content-Length)", lastTot, len(archive))
	}
	if lastSize != int64(len(archive)) {
		t.Errorf("final written = %d, want %d", lastSize, len(archive))
	}
}

// TestDownloadTemplateArchiveFromURL_UnknownContentLengthNormalized verifies
// that when the server doesn't send Content-Length (chunked/unknown length),
// the progress callback receives total=0 rather than -1. This is the
// progressCallback doc contract.
func TestDownloadTemplateArchiveFromURL_UnknownContentLengthNormalized(t *testing.T) {
	archive := buildTestTarball(t, "templates-main", "rust", "simple-api", map[string]string{
		"template.json": `{"name":"simple-api"}`,
		"main.rs":       "fn main() {}\n",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Force chunked transfer encoding so ContentLength == -1 on the
		// client side.
		w.Header().Set("Transfer-Encoding", "chunked")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		half := len(archive) / 2
		_, _ = w.Write(archive[:half])
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write(archive[half:])
	}))
	t.Cleanup(srv.Close)

	var (
		mu       sync.Mutex
		sawTotal int64 = -99 // sentinel so we can tell if callback ran
	)
	cb := func(written, total int64) {
		mu.Lock()
		defer mu.Unlock()
		sawTotal = total
	}

	_, _, err := downloadTemplateArchiveFromURL(context.Background(), srv.URL, "main", "rust", "simple-api", cb)
	if err != nil {
		t.Fatalf("downloadTemplateArchiveFromURL: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if sawTotal < 0 {
		t.Errorf("progress callback received total = %d, want 0 (normalized from unknown)", sawTotal)
	}
}

func TestDownloadTemplateArchiveFromURL_NilCallbackIsSafe(t *testing.T) {
	archive := buildTestTarball(t, "templates-main", "rust", "simple-api", map[string]string{
		"template.json": `{"name":"simple-api"}`,
		"main.rs":       "fn main() {}\n",
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)

	files, manifest, err := downloadTemplateArchiveFromURL(context.Background(), srv.URL, "main", "rust", "simple-api", nil)
	if err != nil {
		t.Fatalf("downloadTemplateArchiveFromURL with nil callback: %v", err)
	}
	if manifest == nil {
		t.Fatal("expected manifest")
	}
	if _, ok := files["main.rs"]; !ok {
		t.Error("main.rs missing from files map")
	}
}

// TestDownloadTemplateArchiveFromURL_CancelledContext verifies that a
// cancelled context aborts the HTTP request rather than blocking forever.
func TestDownloadTemplateArchiveFromURL_CancelledContext(t *testing.T) {
	// Server that hangs until the client disconnects. It never sends a
	// response body, so only a cancelled context can unblock the client.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel before the call so it aborts immediately

	_, _, err := downloadTemplateArchiveFromURL(ctx, srv.URL, "main", "rust", "simple-api", nil)
	if err == nil {
		t.Fatal("expected cancelled context to produce an error")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("error = %q, want it to mention cancellation", err.Error())
	}
}

func TestDownloadTemplateArchiveFromURL_404MentionsBranch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	_, _, err := downloadTemplateArchiveFromURL(context.Background(), srv.URL, "nonexistent-branch", "rust", "simple-api", nil)
	if err == nil {
		t.Fatal("expected 404 to return an error")
	}
	if !strings.Contains(err.Error(), "nonexistent-branch") {
		t.Errorf("error = %q, want it to mention the branch name", err.Error())
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to mention 'not found'", err.Error())
	}
}

func TestDownloadTemplateArchiveFromURL_RetriesTransientTimeout(t *testing.T) {
	archive := buildTestTarball(t, "templates-main", "python", "simple-api", map[string]string{
		"template.json": `{"name":"simple-api"}`,
		"app.py":        "print('hi')\n",
	})

	origTimeout := templateArchiveAttemptTimeout
	origAttempts := templateArchiveMaxAttempts
	origDelay := templateArchiveRetryDelay
	templateArchiveAttemptTimeout = 50 * time.Millisecond
	templateArchiveMaxAttempts = 2
	templateArchiveRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() {
		templateArchiveAttemptTimeout = origTimeout
		templateArchiveMaxAttempts = origAttempts
		templateArchiveRetryDelay = origDelay
	})

	var (
		mu       sync.Mutex
		attempts int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		attempt := attempts
		mu.Unlock()

		if attempt == 1 {
			flusher, _ := w.(http.Flusher)
			w.Header().Set("Content-Type", "application/gzip")
			w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
			_, _ = w.Write(archive[:len(archive)/2])
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(2 * templateArchiveAttemptTimeout)
			_, _ = w.Write(archive[len(archive)/2:])
			return
		}

		_, _ = w.Write(archive)
	}))
	t.Cleanup(srv.Close)

	files, manifest, err := downloadTemplateArchiveFromURL(context.Background(), srv.URL, "main", "python", "simple-api", nil)
	if err != nil {
		t.Fatalf("downloadTemplateArchiveFromURL after retry: %v", err)
	}
	if manifest == nil || manifest.Name != "simple-api" {
		t.Fatalf("manifest = %+v, want simple-api", manifest)
	}
	if string(files["app.py"]) != "print('hi')\n" {
		t.Fatalf("app.py = %q", files["app.py"])
	}

	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestDownloadTemplateArchiveFromURL_CancelledDuringRetryWaitReturnsContextError(t *testing.T) {
	origTimeout := templateArchiveAttemptTimeout
	origAttempts := templateArchiveMaxAttempts
	origDelay := templateArchiveRetryDelay
	templateArchiveAttemptTimeout = 20 * time.Millisecond
	templateArchiveMaxAttempts = 3
	templateArchiveRetryDelay = 200 * time.Millisecond
	t.Cleanup(func() {
		templateArchiveAttemptTimeout = origTimeout
		templateArchiveMaxAttempts = origAttempts
		templateArchiveRetryDelay = origDelay
	})

	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		time.Sleep(2 * templateArchiveAttemptTimeout)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, _, err := downloadTemplateArchiveFromURL(ctx, srv.URL, "main", "python", "simple-api", nil)
		errCh <- err
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	err := <-errCh
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

func TestProgressReader_TracksBytes(t *testing.T) {
	data := []byte("hello world from the progress reader")
	var calls []int64

	pr := &progressReader{
		r:     bytes.NewReader(data),
		total: int64(len(data)),
		onProgress: func(written, total int64) {
			calls = append(calls, written)
		},
	}

	buf := make([]byte, 8)
	var total int
	for {
		n, err := pr.Read(buf)
		total += n
		if err != nil {
			break
		}
	}

	if total != len(data) {
		t.Errorf("read %d bytes, want %d", total, len(data))
	}
	if len(calls) == 0 {
		t.Fatal("progress callback never invoked")
	}
	if calls[len(calls)-1] != int64(len(data)) {
		t.Errorf("final written = %d, want %d", calls[len(calls)-1], len(data))
	}
}

// keys returns the sorted keys of a map[string][]byte — used in test failure
// messages so the output is deterministic.
func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
