//go:build darwin || linux || windows

package commands

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wendylabsinc/wendy/internal/shared/version"
)

func TestNewOSInstallCmd_Flags(t *testing.T) {
	cmd := newOSInstallCmd()
	if cmd.Use != "install [image] [drive]" {
		t.Errorf("Use = %q; want %q", cmd.Use, "install [image] [drive]")
	}

	expectedFlags := []string{"nightly", "force", "yes-overwrite-internal", "device-type", "version", "drive", "wifi-ssid", "wifi-password", "wifi", "no-wifi", "device-name"}
	for _, name := range expectedFlags {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("missing flag %q", name)
		}
	}
}

func TestNewOSInstallCmd_NightlyVersionMutualExclusion(t *testing.T) {
	cmd := newOSInstallCmd()
	cmd.SetArgs([]string{"--nightly", "--version", "0.10.0"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --nightly and --version are both set")
	}
	if got := err.Error(); got != "--nightly and --version are mutually exclusive" {
		t.Errorf("unexpected error: %q", got)
	}
}

func TestNewOSInstallCmd_PositionalArgsIncompatibleWithFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"positional with --device-type", []string{"image.img", "/dev/disk4", "--device-type", "raspberry-pi-5", "--force"}},
		{"positional with --version", []string{"image.img", "/dev/disk4", "--version", "0.10.0", "--force"}},
		{"positional with --drive", []string{"image.img", "/dev/disk4", "--drive", "/dev/disk5", "--force"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newOSInstallCmd()
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected error when positional args are combined with manifest flags")
			}
			expected := "positional [image] [drive] arguments cannot be combined with --device-type, --version, --drive, --wifi-ssid, --wifi-password, --wifi, --no-wifi, or --device-name"
			if got := err.Error(); got != expected {
				t.Errorf("unexpected error: %q; want %q", got, expected)
			}
		})
	}
}

func TestNewOSInstallCmd_SinglePositionalArgRejected(t *testing.T) {
	cmd := newOSInstallCmd()
	cmd.SetArgs([]string{"image.img"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when exactly 1 positional arg is provided")
	}
	expected := "positional arguments must be provided as [image] [drive]; got 1 argument"
	if got := err.Error(); got != expected {
		t.Errorf("unexpected error: %q; want %q", got, expected)
	}
}

func TestNewOSInstallCmd_ESP32DeviceTypeRejected(t *testing.T) {
	for _, dt := range []string{"esp32-c6", "esp32-c5"} {
		t.Run(dt, func(t *testing.T) {
			cmd := newOSInstallCmd()
			cmd.SetArgs([]string{"--device-type", dt})
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected error for ESP32 --device-type")
			}
			if !strings.Contains(err.Error(), "does not support ESP32") {
				t.Errorf("unexpected error: %q", err.Error())
			}
		})
	}
}

func TestPickManifestVersion_SemverOrdering(t *testing.T) {
	// Verify that version keys are sorted semantically, not lexicographically.
	// "0.10.0" should come after "0.9.0" semantically but before it lexicographically.
	versions := []string{"0.2.0", "0.10.0", "0.9.0", "0.1.0", "0.10.1"}

	// Use the same sorting logic as pickManifestVersion.
	sorted := make([]string, len(versions))
	copy(sorted, versions)
	sortFunc := func(i, j int) bool {
		return version.CompareVersions(sorted[i], sorted[j]) > 0
	}

	// Simple bubble sort for testing.
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			if !sortFunc(i, j) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	expected := []string{"0.10.1", "0.10.0", "0.9.0", "0.2.0", "0.1.0"}
	for i, v := range sorted {
		if v != expected[i] {
			t.Errorf("sorted[%d] = %q; want %q (full: %v)", i, v, expected[i], sorted)
			break
		}
	}
}

func TestOsCachedImagePath_Sanitization(t *testing.T) {
	// Valid inputs should produce a valid path.
	path, err := osCachedImagePath("raspberry-pi-5", "0.10.4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path == "" {
		t.Fatal("expected non-empty path")
	}

	// Path traversal in version should be rejected.
	_, err = osCachedImagePath("raspberry-pi-5", "../../../etc/passwd")
	if err == nil {
		t.Fatal("expected error for path traversal in version")
	}

	// Path traversal in device key should be rejected.
	_, err = osCachedImagePath("../evil", "0.10.4")
	if err == nil {
		t.Fatal("expected error for path traversal in device key")
	}
}

func TestOsCachedZipPath_Sanitization(t *testing.T) {
	path, err := osCachedZipPath("raspberry-pi-5", "0.10.4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(path, ".zip") {
		t.Fatalf("expected .zip suffix, got %q", path)
	}

	_, err = osCachedZipPath("raspberry-pi-5", "../../../etc/passwd")
	if err == nil {
		t.Fatal("expected error for path traversal in version")
	}

	_, err = osCachedZipPath("../evil", "0.10.4")
	if err == nil {
		t.Fatal("expected error for path traversal in device key")
	}
}

func makeTestZip(t *testing.T, entryName string, content []byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test-*.zip")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	w := zip.NewWriter(f)
	fw, err := w.Create(entryName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func TestStreamZipImageEntry(t *testing.T) {
	content := []byte("fake image data 12345")

	t.Run("reads img entry", func(t *testing.T) {
		zipPath := makeTestZip(t, "wendyos.img", content)
		r, size, err := streamZipImageEntry(zipPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer r.Close()
		if size != int64(len(content)) {
			t.Errorf("size = %d; want %d", size, len(content))
		}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("reading: %v", err)
		}
		if !bytes.Equal(got, content) {
			t.Errorf("content mismatch")
		}
	})

	t.Run("reads raw entry", func(t *testing.T) {
		zipPath := makeTestZip(t, "wendyos.raw", content)
		r, _, err := streamZipImageEntry(zipPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		r.Close()
	})

	t.Run("reads wic entry", func(t *testing.T) {
		zipPath := makeTestZip(t, "wendyos.wic", content)
		r, _, err := streamZipImageEntry(zipPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		r.Close()
	})

	t.Run("no image entry returns error", func(t *testing.T) {
		zipPath := makeTestZip(t, "readme.txt", content)
		_, _, err := streamZipImageEntry(zipPath)
		if err == nil {
			t.Fatal("expected error for zip with no image entry")
		}
	})

	t.Run("nonexistent file returns error", func(t *testing.T) {
		_, _, err := streamZipImageEntry("/nonexistent/path/image.zip")
		if err == nil {
			t.Fatal("expected error for nonexistent file")
		}
	})
}

func TestParseWiFiEntry(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantSSID string
		wantPW   string
		wantPri  int32
		wantHid  bool
		wantSec  string
		wantErr  bool
	}{
		{"ssid only", "ssid=Home", "Home", "", 0, false, "", false},
		{"all fields", "ssid=Home,password=p,priority=10,hidden=true,security=wpa3", "Home", "p", 10, true, "wpa3", false},
		{"escaped comma", `ssid=My\,Net,password=x`, "My,Net", "x", 0, false, "", false},
		{"missing ssid", "password=p", "", "", 0, false, "", true},
		{"bad priority", "ssid=A,priority=nope", "", "", 0, false, "", true},
		{"unknown key", "ssid=A,foo=bar", "", "", 0, false, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := parseWiFiEntry(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", c)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.SSID != tc.wantSSID || c.Password != tc.wantPW || c.Priority != tc.wantPri || c.Hidden != tc.wantHid || c.Security != tc.wantSec {
				t.Errorf("got %+v; want ssid=%q pw=%q pri=%d hidden=%v sec=%q",
					c, tc.wantSSID, tc.wantPW, tc.wantPri, tc.wantHid, tc.wantSec)
			}
		})
	}
}

func TestResolveWiFiCredentialsListFlags(t *testing.T) {
	// --wifi-ssid + --wifi-password shortcut (non-TTY path: isInteractiveTerminal returns false in tests).
	creds, err := resolveWiFiCredentialsList(wifiCLIOptions{SSID: "Home", Password: "pw"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(creds) != 1 || creds[0].SSID != "Home" || creds[0].Password != "pw" {
		t.Errorf("shortcut produced %+v", creds)
	}

	// Repeatable --wifi: order preserved, priorities honoured.
	creds, err = resolveWiFiCredentialsList(wifiCLIOptions{Entries: []string{
		"ssid=First,password=a,priority=100",
		"ssid=Second,priority=50",
		"ssid=Hidden,hidden=true",
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(creds) != 3 {
		t.Fatalf("got %d creds; want 3", len(creds))
	}
	if creds[0].SSID != "First" || creds[0].Priority != 100 {
		t.Errorf("creds[0] = %+v", creds[0])
	}
	if creds[2].SSID != "Hidden" || !creds[2].Hidden {
		t.Errorf("creds[2] = %+v", creds[2])
	}

	// --no-wifi short-circuits even when other flags are empty.
	creds, err = resolveWiFiCredentialsList(wifiCLIOptions{NoWifi: true})
	if err != nil || creds != nil {
		t.Errorf("no-wifi: got %v, %+v", err, creds)
	}

	// --no-wifi combined with --wifi-ssid should error.
	if _, err := resolveWiFiCredentialsList(wifiCLIOptions{NoWifi: true, SSID: "Home"}); err == nil {
		t.Error("expected error when --no-wifi is combined with --wifi-ssid")
	}

	// --wifi-password without --wifi-ssid should error.
	if _, err := resolveWiFiCredentialsList(wifiCLIOptions{Password: "pw"}); err == nil {
		t.Error("expected error when --wifi-password is passed alone")
	}
}

func TestResolveOSImage_ZipCacheHit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	content := []byte("fake image bytes")
	zipPath, err := osCachedZipPath("test-device", "9.9.9")
	if err != nil {
		t.Fatal(err)
	}

	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	fw, err := w.Create("image.img")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	img := &imageInfo{Version: "9.9.9", DownloadURL: "https://example.com/image.zip"}
	got, err := resolveOSImage("test-device", img)
	if err != nil {
		t.Fatalf("resolveOSImage: %v", err)
	}
	if got != zipPath {
		t.Errorf("got %q; want %q", got, zipPath)
	}
}

func TestResolveOSImage_LegacyImgCacheHit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	imgPath, err := osCachedImagePath("test-device", "8.8.8")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(imgPath, []byte("legacydata"), 0o644); err != nil {
		t.Fatal(err)
	}

	img := &imageInfo{Version: "8.8.8", DownloadURL: "https://example.com/image.zip"}
	got, err := resolveOSImage("test-device", img)
	if err != nil {
		t.Fatalf("resolveOSImage: %v", err)
	}
	if got != imgPath {
		t.Errorf("got %q; want %q (legacy img cache)", got, imgPath)
	}
}

func TestOpenOSImageStream_ZipCacheHit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	content := []byte("stream me please")
	zipPath, err := osCachedZipPath("stream-device", "7.7.7")
	if err != nil {
		t.Fatal(err)
	}

	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	fw, err := w.Create("wendyos.img")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	img := &imageInfo{Version: "7.7.7", DownloadURL: "https://example.com/image.zip"}
	r, size, err := openOSImageStream("stream-device", img)
	if err != nil {
		t.Fatalf("openOSImageStream: %v", err)
	}
	defer r.Close()

	if size != int64(len(content)) {
		t.Errorf("size = %d; want %d", size, len(content))
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Error("content mismatch")
	}
}

func TestOpenOSImageStream_LegacyImgCacheHit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	content := []byte("old img cache data")
	imgPath, err := osCachedImagePath("legacy-device", "6.6.6")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(imgPath, content, 0o644); err != nil {
		t.Fatal(err)
	}

	img := &imageInfo{Version: "6.6.6", DownloadURL: "https://example.com/image.zip"}
	r, size, err := openOSImageStream("legacy-device", img)
	if err != nil {
		t.Fatalf("openOSImageStream: %v", err)
	}
	defer r.Close()

	if size != int64(len(content)) {
		t.Errorf("size = %d; want %d", size, len(content))
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Error("content mismatch")
	}
}

func TestExternalDrivePickerItems(t *testing.T) {
	drives := []drive{
		{Name: "Sandisk USB", DevicePath: "/dev/disk4", Size: "32 GB", IsRemovable: true},
		{Name: "USB SSD", DevicePath: "/dev/disk5", IsRemovable: true},
	}

	items := externalDrivePickerItems(drives)
	if got := len(items); got != 2 {
		t.Fatalf("items = %d, want 2", got)
	}
	if items[0].Name != "Sandisk USB" {
		t.Errorf("Name = %q, want Sandisk USB", items[0].Name)
	}
	if items[0].Description != "/dev/disk4  32 GB" {
		t.Errorf("Description = %q, want device path and size", items[0].Description)
	}
	if items[0].DedupKey != "/dev/disk4" {
		t.Errorf("DedupKey = %q, want /dev/disk4", items[0].DedupKey)
	}
	if items[1].Description != "/dev/disk5" {
		t.Errorf("Description without size = %q, want /dev/disk5", items[1].Description)
	}

	selected, ok := items[0].Value.(drive)
	if !ok {
		t.Fatalf("Value has type %T, want drive", items[0].Value)
	}
	if selected.DevicePath != drives[0].DevicePath {
		t.Errorf("selected drive path = %q, want %q", selected.DevicePath, drives[0].DevicePath)
	}
}

func TestConfirmOverwriteInternalDrive(t *testing.T) {
	removable := drive{Name: "Sandisk USB", DevicePath: "/dev/disk4", IsRemovable: true}
	internal := drive{Name: "Internal SSD", DevicePath: "/dev/disk1", IsRemovable: false}

	t.Run("removable + force is fine", func(t *testing.T) {
		if err := confirmOverwriteInternalDrive(removable, true, false); err != nil {
			t.Errorf("removable drive should always pass: %v", err)
		}
	})

	t.Run("removable interactive is fine", func(t *testing.T) {
		if err := confirmOverwriteInternalDrive(removable, false, false); err != nil {
			t.Errorf("removable drive should always pass: %v", err)
		}
	})

	t.Run("internal + force without override errors out", func(t *testing.T) {
		err := confirmOverwriteInternalDrive(internal, true, false)
		if err == nil {
			t.Fatal("internal drive with --force and no --yes-overwrite-internal must be rejected")
		}
		if !strings.Contains(err.Error(), "yes-overwrite-internal") {
			t.Errorf("error should mention --yes-overwrite-internal: %v", err)
		}
		if !strings.Contains(err.Error(), internal.DevicePath) {
			t.Errorf("error should name the drive: %v", err)
		}
	})

	t.Run("internal + force + override is allowed", func(t *testing.T) {
		if err := confirmOverwriteInternalDrive(internal, true, true); err != nil {
			t.Errorf("override flag should permit overwrite: %v", err)
		}
	})

	t.Run("internal interactive + override skips typed prompt", func(t *testing.T) {
		// yesOverwriteInternal = true means we never reach the stdin read.
		if err := confirmOverwriteInternalDrive(internal, false, true); err != nil {
			t.Errorf("override flag should bypass typed prompt: %v", err)
		}
	})
}

func TestProbeRangeSupport(t *testing.T) {
	t.Run("returns content length when server supports ranges", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodHead {
				t.Errorf("expected HEAD, got %s", r.Method)
			}
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", "8192")
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		img := &imageInfo{DownloadURL: srv.URL + "/image.img"}
		cl, ok := probeRangeSupport(&http.Client{}, img)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if cl != 8192 {
			t.Fatalf("expected contentLength=8192, got %d", cl)
		}
	})

	t.Run("returns false when Accept-Ranges header is absent", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "8192")
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		img := &imageInfo{DownloadURL: srv.URL + "/image.img"}
		_, ok := probeRangeSupport(&http.Client{}, img)
		if ok {
			t.Fatal("expected ok=false when no Accept-Ranges header")
		}
	})

	t.Run("falls back to img.ImageSize when Content-Length is absent", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Accept-Ranges", "bytes")
			// No Content-Length header.
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		img := &imageInfo{DownloadURL: srv.URL + "/image.img", ImageSize: 4096}
		cl, ok := probeRangeSupport(&http.Client{}, img)
		if !ok {
			t.Fatal("expected ok=true with ImageSize fallback")
		}
		if cl != 4096 {
			t.Fatalf("expected contentLength=4096 from ImageSize, got %d", cl)
		}
	})

	t.Run("returns false when server returns non-200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", "8192")
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		img := &imageInfo{DownloadURL: srv.URL + "/image.img"}
		_, ok := probeRangeSupport(&http.Client{}, img)
		if ok {
			t.Fatal("expected ok=false when server returns non-200")
		}
	})
}

func TestDownloadParallel(t *testing.T) {
	// 8 KiB fixture — with 8 workers each gets a 1 KiB chunk.
	fixture := make([]byte, 8*1024)
	for i := range fixture {
		fixture[i] = byte(i % 251) // prime modulus gives a non-trivial pattern
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			http.Error(w, "range required", http.StatusBadRequest)
			return
		}
		var start, end int64
		if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range header", http.StatusBadRequest)
			return
		}
		if end >= int64(len(fixture)) {
			end = int64(len(fixture)) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(fixture)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(fixture[start : end+1]) //nolint:errcheck
	}))
	defer srv.Close()

	dir := t.TempDir()
	f, err := os.CreateTemp(dir, "wendy-test-*.img")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	contentLength := int64(len(fixture))
	if err := f.Truncate(contentLength); err != nil {
		t.Fatal(err)
	}

	var progressCalled atomic.Bool
	err = downloadParallel(&http.Client{}, srv.URL+"/image.img", contentLength, f, func(downloaded, total int64) {
		progressCalled.Store(true)
	})
	if err != nil {
		t.Fatalf("downloadParallel: %v", err)
	}
	if !progressCalled.Load() {
		t.Error("progress callback was never called")
	}

	f.Close()

	got, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fixture) {
		t.Errorf("content mismatch: got %d bytes, want %d bytes", len(got), len(fixture))
		for i := range fixture {
			if i >= len(got) || got[i] != fixture[i] {
				t.Errorf("first diff at byte %d: got %d, want %d", i, got[i], fixture[i])
				break
			}
		}
	}
}
