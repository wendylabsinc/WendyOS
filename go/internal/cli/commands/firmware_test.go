//go:build darwin || linux || windows

package commands

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestFirmwareCachedPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	asset := &firmwareAsset{Name: "wendy_mcu_esp32c6_native.bin", Version: "0.2.0"}
	got, err := firmwareCachedPath(asset)
	if err != nil {
		t.Fatalf("firmwareCachedPath: %v", err)
	}
	if filepath.Base(got) != "0.2.0-wendy_mcu_esp32c6_native.bin" {
		t.Errorf("cache file name = %q; want %q", filepath.Base(got), "0.2.0-wendy_mcu_esp32c6_native.bin")
	}
	if filepath.Base(filepath.Dir(got)) != "wendy-lite-firmware" {
		t.Errorf("cache dir = %q; want wendy-lite-firmware", filepath.Dir(got))
	}
}

func TestFirmwareCachedPath_RejectsPathTraversal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	tests := []*firmwareAsset{
		{Name: "../../etc/passwd", Version: "0.2.0"},
		{Name: "fw.bin", Version: "../../0.2.0"},
	}
	for _, asset := range tests {
		if _, err := firmwareCachedPath(asset); err == nil {
			t.Errorf("firmwareCachedPath(%+v) should have rejected path traversal", asset)
		}
	}
}

func TestResolveFirmware_CacheHit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	asset := &firmwareAsset{
		Name:    "wendy_mcu_esp32c6_native.bin",
		Version: "0.2.0",
		// A cache hit must never touch the network, so point at a URL that
		// would fail if dialed.
		DownloadURL: "http://127.0.0.1:1/unreachable",
	}
	cached, err := firmwareCachedPath(asset)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cached, []byte("cached firmware bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveFirmware(context.Background(), asset)
	if err != nil {
		t.Fatalf("resolveFirmware: %v", err)
	}
	if got != cached {
		t.Errorf("got %q; want %q", got, cached)
	}
}

// downloadFirmwareInto is the synchronous, TUI-free half of the download
// path that resolveFirmware's cache-miss branch relies on — exercised
// directly here since driving the TUI-owning downloadFirmware requires a
// real TTY (the same reason TestResolveOSImage_* only cover cache hits).
func TestDownloadFirmwareInto(t *testing.T) {
	const body = "fresh firmware bytes"
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write([]byte(body))
	}))
	defer srv.Close()

	asset := &firmwareAsset{
		Name:        "wendy_mcu_esp32c6_native.bin",
		Version:     "0.2.0",
		DownloadURL: srv.URL,
	}

	dir := t.TempDir()
	var lastDownloaded, lastTotal int64
	path, err := downloadFirmwareInto(context.Background(), asset, dir, func(downloaded, total int64) {
		lastDownloaded, lastTotal = downloaded, total
	})
	if err != nil {
		t.Fatalf("downloadFirmwareInto: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("downloaded file %q not written inside dir %q", path, dir)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != body {
		t.Errorf("downloaded content = %q; want %q", data, body)
	}
	if hits != 1 {
		t.Fatalf("hits = %d; want 1", hits)
	}
	if lastDownloaded != int64(len(body)) || lastTotal != int64(len(body)) {
		t.Errorf("final progress = %d/%d; want %d/%d", lastDownloaded, lastTotal, len(body), len(body))
	}
}
