package arduinotoolchain

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

func withCacheDir(t *testing.T, dir string) {
	t.Helper()
	orig := cacheDir
	t.Cleanup(func() { cacheDir = orig })
	cacheDir = func() (string, error) { return dir, nil }
}

func withHost(t *testing.T, host string) {
	t.Helper()
	orig := hostPlatform
	t.Cleanup(func() { hostPlatform = orig })
	hostPlatform = func() string { return host }
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// serve answers every request with body and counts the requests.
func serve(t *testing.T, body []byte) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	orig := httpClient
	t.Cleanup(func() { httpClient = orig })
	httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}
	return &n
}

func withArchive(t *testing.T, host string, a archive) {
	t.Helper()
	orig := archives
	t.Cleanup(func() { archives = orig })
	archives = map[string]archive{host: a}
}

func tarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		data []byte
	}{{"LICENSE.txt", []byte("license")}, {name, content}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.data))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(f.data)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func zipArchive(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(content)
	_ = zw.Close()
	return buf.Bytes()
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestEnsureCLIRejectsBadChecksum(t *testing.T) {
	root := t.TempDir()
	withCacheDir(t, root)
	withHost(t, "linux/amd64")
	withArchive(t, "linux/amd64", archive{File: "a.tar.gz", SHA256: sum([]byte("expected")), Binary: "arduino-cli"})
	serve(t, tarGz(t, "arduino-cli", []byte("#!/bin/sh\n")))

	_, err := EnsureCLI(context.Background(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("EnsureCLI() = %v, want a SHA-256 mismatch", err)
	}
	if _, err := os.Stat(filepath.Join(root, "toolchains", "arduino-cli", CLIVersion, "arduino-cli")); err == nil {
		t.Fatal("binary installed despite the checksum mismatch")
	}
}

func TestEnsureCLIReusesCache(t *testing.T) {
	withCacheDir(t, t.TempDir())
	withHost(t, "linux/amd64")
	body := tarGz(t, "arduino-cli", []byte("#!/bin/sh\necho cli\n"))
	withArchive(t, "linux/amd64", archive{File: "a.tar.gz", SHA256: sum(body), Binary: "arduino-cli"})
	requests := serve(t, body)

	first, err := EnsureCLI(context.Background(), io.Discard)
	if err != nil {
		t.Fatalf("first EnsureCLI: %v", err)
	}
	second, err := EnsureCLI(context.Background(), io.Discard)
	if err != nil {
		t.Fatalf("second EnsureCLI: %v", err)
	}
	if first != second || requests.Load() != 1 {
		t.Fatalf("paths %q/%q, %d requests; want one path and 1 request", first, second, requests.Load())
	}
	data, err := os.ReadFile(first)
	if err != nil || !strings.Contains(string(data), "echo cli") {
		t.Fatalf("installed binary = %q, %v", data, err)
	}
}

func TestEnsureCLIWritesIsolatedConfig(t *testing.T) {
	root := t.TempDir()
	withCacheDir(t, root)
	withHost(t, "linux/amd64")
	body := tarGz(t, "arduino-cli", []byte("x"))
	withArchive(t, "linux/amd64", archive{File: "a.tar.gz", SHA256: sum(body), Binary: "arduino-cli"})
	serve(t, body)
	if _, err := EnsureCLI(context.Background(), io.Discard); err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(filepath.Join(root, "arduino", "arduino-cli.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"data", "downloads", "user"} {
		if !strings.Contains(string(cfg), filepath.Join(root, "arduino", d)) {
			t.Errorf("config %q lacks the %s directory under the cache", cfg, d)
		}
	}
}

func TestEnsureCLIExtractsZipOnWindows(t *testing.T) {
	withCacheDir(t, t.TempDir())
	withHost(t, "windows/amd64")
	body := zipArchive(t, "arduino-cli.exe", []byte("MZ"))
	withArchive(t, "windows/amd64", archive{File: "a.zip", SHA256: sum(body), Binary: "arduino-cli.exe"})
	serve(t, body)
	got, err := EnsureCLI(context.Background(), io.Discard)
	if err != nil {
		t.Fatalf("EnsureCLI: %v", err)
	}
	if !strings.HasSuffix(got, "arduino-cli.exe") {
		t.Fatalf("EnsureCLI = %q, want arduino-cli.exe", got)
	}
}

func TestEnsureCLIUnsupportedHost(t *testing.T) {
	withCacheDir(t, t.TempDir())
	withHost(t, "plan9/386")
	if _, err := EnsureCLI(context.Background(), io.Discard); err == nil || !strings.Contains(err.Error(), "plan9/386") {
		t.Fatalf("EnsureCLI() = %v, want an error naming the host", err)
	}
}

func TestArchivesCoverSupportedHosts(t *testing.T) {
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for _, host := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64"} {
		a, ok := archives[host]
		if !ok {
			t.Errorf("no arduino-cli archive for %s", host)
			continue
		}
		if !hex64.MatchString(a.SHA256) || !strings.Contains(a.File, CLIVersion) {
			t.Errorf("%s archive %+v: want a 64-hex SHA-256 and the pinned version", host, a)
		}
	}
}
