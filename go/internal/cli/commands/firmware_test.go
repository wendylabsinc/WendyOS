//go:build darwin || linux || windows

package commands

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestFetchFirmwareFromManifestUsesRequestedVersion(t *testing.T) {
	const main = `{"firmware":{"p4":{"latest":"2.0.0","latest_nightly":"2.1.0","manifest_path":"manifests/p4.json"}}}`
	checksum := strings.Repeat("a", 64)
	chip := fmt.Sprintf(`{"chip_id":"p4","versions":{
		"1.0.0":{"path":"firmware/p4/1.0.0/host.bin"},
		"2.0.0":{"path":"firmware/p4/2.0.0/digest/host.bin","checksum":%q,"size_bytes":1,
			"companion":{"path":"firmware/p4/2.0.0/digest/radio.bin","checksum":%q,"size_bytes":1,"build_id":%q}},
		"2.1.0":{"path":"firmware/p4/2.1.0/host.bin"}
	}}`, checksum, checksum, testedCompanionBuildID)
	transport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = transport })
	http.DefaultTransport = vmImageTestTransport(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.URL.Path {
		case "/wendyos-images-public/manifests/master.json":
			body = main
		case "/wendyos-images-public/manifests/p4.json":
			body = chip
		default:
			return nil, fmt.Errorf("unexpected request for %s", req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})

	legacy, err := fetchFirmwareFromManifest(t.Context(), "p4", "1.0.0", false)
	if err != nil || legacy.Version != "1.0.0" || legacy.Companion != nil {
		t.Fatalf("requested legacy version = %+v, %v", legacy, err)
	}
	latest, err := fetchFirmwareFromManifest(t.Context(), "p4", "", false)
	if err != nil || latest.Version != "2.0.0" || latest.Companion == nil || latest.Checksum != checksum {
		t.Fatalf("latest paired version = %+v, %v", latest, err)
	}
	nightly, err := fetchFirmwareFromManifest(t.Context(), "p4", "", true)
	if err != nil || nightly.Version != "2.1.0" || nightly.Companion != nil {
		t.Fatalf("latest nightly = %+v, %v", nightly, err)
	}
	if _, err := fetchFirmwareFromManifest(t.Context(), "p4", "missing", false); err == nil {
		t.Fatal("accepted a missing explicit version")
	}
	chip = strings.Replace(chip, `"chip_id":"p4"`, `"chip_id":"other"`, 1)
	if _, err := fetchFirmwareFromManifest(t.Context(), "p4", "1.0.0", false); err == nil {
		t.Fatal("accepted a manifest for the wrong chip")
	}
}

func TestDownloadFirmwareIntoValidatesPairedHost(t *testing.T) {
	const expected = "host image"
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(expected)))
	for _, body := range []string{expected, "bad  image", "short", "much longer than the expected image"} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			asset := &firmwareAsset{DownloadURL: server.URL, Size: int64(len(expected)), Checksum: sum}
			dir := t.TempDir()
			filename, err := downloadFirmwareInto(t.Context(), asset, dir, nil)
			if body == expected {
				if err != nil {
					t.Fatal(err)
				}
				if err := asset.verifyFile(filename, asset.Size); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted invalid paired host bytes %q", body)
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) > 0 {
				t.Fatalf("invalid download left files behind: %v, %v", files, err)
			}
		})
	}
}
