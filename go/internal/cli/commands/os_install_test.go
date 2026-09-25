//go:build darwin || linux || windows

package commands

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

func TestNewOSInstallCmd_Flags(t *testing.T) {
	cmd := newOSInstallCmd()
	if cmd.Use != "install [image] [drive]" {
		t.Errorf("Use = %q; want %q", cmd.Use, "install [image] [drive]")
	}

	expectedFlags := []string{"nightly", "force", "yes-overwrite-internal", "device-type", "version", "drive", "wifi-ssid", "wifi-password", "wifi", "no-wifi", "device-name", "storage", "no-bmap", "pr"}
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

func TestOSInstallPRMutualExclusion(t *testing.T) {
	const mutexErr = "--pr cannot be combined with --nightly, --version, or positional image/drive arguments"
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"pr with nightly", []string{"--pr", "123", "--nightly"}, mutexErr},
		{"pr with version", []string{"--pr", "123", "--version", "0.10.0"}, mutexErr},
		{"pr with positional args", []string{"--pr", "123", "image.img", "/dev/disk4"}, mutexErr},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newOSInstallCmd()
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("expected error for args %v", tc.args)
			}
			if got := err.Error(); got != tc.wantErr {
				t.Errorf("unexpected error: %q; want %q", got, tc.wantErr)
			}
		})
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
			expected := "positional [image] [drive] arguments cannot be combined with --device-type, --version, --drive, --wifi-ssid, --wifi-password, --wifi, --no-wifi, --device-name, or --cloud-grpc"
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
	for _, dt := range []string{"esp32-c5", "esp32-c6", "esp32-c61", "esp32-p4", "esp32-s3"} {
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
	path, err := osCachedImagePath("raspberry-pi-5", "0.10.4", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path == "" {
		t.Fatal("expected non-empty path")
	}

	// Path traversal in version should be rejected.
	_, err = osCachedImagePath("raspberry-pi-5", "../../../etc/passwd", "")
	if err == nil {
		t.Fatal("expected error for path traversal in version")
	}

	// Path traversal in device key should be rejected.
	_, err = osCachedImagePath("../evil", "0.10.4", "")
	if err == nil {
		t.Fatal("expected error for path traversal in device key")
	}

	// Path traversal in storage key should be rejected.
	_, err = osCachedImagePath("raspberry-pi-5", "0.10.4", "../evil")
	if err == nil {
		t.Fatal("expected error for path traversal in storage key")
	}
}

func TestOsCachedZipPath_Sanitization(t *testing.T) {
	path, err := osCachedZipPath("raspberry-pi-5", "0.10.4", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(path, ".zip") {
		t.Fatalf("expected .zip suffix, got %q", path)
	}

	_, err = osCachedZipPath("raspberry-pi-5", "../../../etc/passwd", "")
	if err == nil {
		t.Fatal("expected error for path traversal in version")
	}

	_, err = osCachedZipPath("../evil", "0.10.4", "")
	if err == nil {
		t.Fatal("expected error for path traversal in device key")
	}
}

// The storage key, when set, becomes part of the cache filename so an SD image
// and an NVMe image of the same device+version never collide on one file.
func TestOsCachedPath_StorageKeyed(t *testing.T) {
	sd, err := osCachedZipPath("raspberry-pi-5", "0.16.0", "sd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	nvme, err := osCachedZipPath("raspberry-pi-5", "0.16.0", "nvme")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sd == nvme {
		t.Fatalf("sd and nvme cache paths must differ, both = %q", sd)
	}
	if !strings.HasSuffix(sd, "raspberry-pi-5-0.16.0-sd.zip") {
		t.Errorf("unexpected sd cache path: %q", sd)
	}
	if !strings.HasSuffix(nvme, "raspberry-pi-5-0.16.0-nvme.zip") {
		t.Errorf("unexpected nvme cache path: %q", nvme)
	}

	// Empty storage keeps the legacy (unsuffixed) name for backward compat.
	legacy, err := osCachedZipPath("raspberry-pi-5", "0.16.0", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(legacy, "raspberry-pi-5-0.16.0.zip") {
		t.Errorf("unexpected legacy cache path: %q", legacy)
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
		stream, err := streamZipImageEntry(zipPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer stream.Close()
		if stream.uncompressedSize != int64(len(content)) {
			t.Errorf("uncompressedSize = %d; want %d", stream.uncompressedSize, len(content))
		}
		got, err := io.ReadAll(stream)
		if err != nil {
			t.Fatalf("reading: %v", err)
		}
		if !bytes.Equal(got, content) {
			t.Errorf("content mismatch")
		}
	})

	t.Run("reads raw entry", func(t *testing.T) {
		zipPath := makeTestZip(t, "wendyos.raw", content)
		stream, err := streamZipImageEntry(zipPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		stream.Close()
	})

	t.Run("reads wic entry", func(t *testing.T) {
		zipPath := makeTestZip(t, "wendyos.wic", content)
		stream, err := streamZipImageEntry(zipPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		stream.Close()
	})

	t.Run("reads sdimg entry", func(t *testing.T) {
		zipPath := makeTestZip(t, "wendyos.sdimg", content)
		stream, err := streamZipImageEntry(zipPath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		stream.Close()
	})

	t.Run("no image entry returns error", func(t *testing.T) {
		zipPath := makeTestZip(t, "readme.txt", content)
		_, err := streamZipImageEntry(zipPath)
		if err == nil {
			t.Fatal("expected error for zip with no image entry")
		}
	})

	t.Run("nonexistent file returns error", func(t *testing.T) {
		_, err := streamZipImageEntry("/nonexistent/path/image.zip")
		if err == nil {
			t.Fatal("expected error for nonexistent file")
		}
	})
}

func makeTestGzip(t *testing.T, namePattern string, content []byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), namePattern)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	if _, err := gw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func makeTestPlainFile(t *testing.T, namePattern string, content []byte) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), namePattern)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(content); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func TestIsGzipFile(t *testing.T) {
	content := []byte("fake image data 12345")

	t.Run("detects gzip regardless of extension", func(t *testing.T) {
		// gzip content stored under a non-.gz name — the cache path may not
		// retain the original .gz extension.
		path := makeTestGzip(t, "image-*.img", content)
		if !isGzipFile(path) {
			t.Error("isGzipFile() = false; want true for gzip content under a .img name")
		}
	})

	t.Run("returns false for non-gzip content", func(t *testing.T) {
		path := makeTestPlainFile(t, "image-*.img.gz", content)
		if isGzipFile(path) {
			t.Error("isGzipFile() = true; want false for plain (non-gzip) content")
		}
	})

	t.Run("returns false for nonexistent file", func(t *testing.T) {
		if isGzipFile("/nonexistent/path/image.img.gz") {
			t.Error("isGzipFile() = true; want false for nonexistent file")
		}
	})
}

func TestStreamGzipImage(t *testing.T) {
	content := []byte("fake decompressed wendyos image payload")

	t.Run("decompresses content and reports compressed progress", func(t *testing.T) {
		path := makeTestGzip(t, "image-*.img.gz", content)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}

		stream, err := streamGzipImage(path)
		if err != nil {
			t.Fatalf("streamGzipImage() error = %v", err)
		}
		defer stream.Close()

		// gzip's ISIZE trailer stores the uncompressed size mod 2^32, so it
		// must never be trusted: a 19.2 GiB image reports 3.2 GiB and the
		// progress bar overshoots its total (WDY: writing 17.3 GiB / 3.2 GiB).
		if stream.uncompressedSize != 0 {
			t.Errorf("uncompressedSize = %d; want 0 (unknown before measuring)", stream.uncompressedSize)
		}
		if stream.compressedSize != info.Size() {
			t.Errorf("compressedSize = %d; want %d (gzip file size)", stream.compressedSize, info.Size())
		}
		if stream.compressedRead == nil {
			t.Fatal("compressedRead = nil; want counter over the compressed source")
		}
		if stream.sourcePath != path {
			t.Errorf("sourcePath = %q; want %q (needed for measuring)", stream.sourcePath, path)
		}

		got, err := io.ReadAll(stream)
		if err != nil {
			t.Fatalf("reading decompressed stream: %v", err)
		}
		if !bytes.Equal(got, content) {
			t.Errorf("decompressed content = %q; want %q", got, content)
		}
		if read := stream.compressedRead(); read != info.Size() {
			t.Errorf("compressedRead() after full read = %d; want %d", read, info.Size())
		}
	})

	t.Run("nonexistent file returns error", func(t *testing.T) {
		if _, err := streamGzipImage("/nonexistent/path/image.img.gz"); err == nil {
			t.Fatal("expected error for nonexistent file")
		}
	})
}

func TestMeasureGzipImage(t *testing.T) {
	// Zero-heavy payload mimicking a sparse disk image: the decompressed size
	// vastly exceeds the compressed size, the case where the ISIZE trailer
	// and compressed-progress heuristics both mislead.
	content := make([]byte, 4<<20)
	copy(content, []byte("partition data at the front"))
	path := makeTestGzip(t, "image-*.img.gz", content)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	var lastRead, lastTotal int64
	size, err := measureGzipImage(path, func(read, total int64) { lastRead, lastTotal = read, total })
	if err != nil {
		t.Fatalf("measureGzipImage() error = %v", err)
	}
	if size != int64(len(content)) {
		t.Errorf("size = %d; want %d (exact decompressed size)", size, len(content))
	}
	if lastTotal != info.Size() {
		t.Errorf("progress total = %d; want %d (compressed file size)", lastTotal, info.Size())
	}
	if lastRead != info.Size() {
		t.Errorf("final progress read = %d; want %d (whole compressed file)", lastRead, info.Size())
	}

	t.Run("nil progress is allowed", func(t *testing.T) {
		if _, err := measureGzipImage(path, nil); err != nil {
			t.Fatalf("measureGzipImage() error = %v", err)
		}
	})

	t.Run("corrupt file returns error", func(t *testing.T) {
		bad := makeTestPlainFile(t, "image-*.img.gz", []byte("not gzip at all"))
		if _, err := measureGzipImage(bad, nil); err == nil {
			t.Fatal("expected error for non-gzip file")
		}
	})
}

func TestImageStreamMeasureUncompressedSize(t *testing.T) {
	content := bytes.Repeat([]byte("wendyos"), 1<<16)

	t.Run("measures once and caches in a sidecar", func(t *testing.T) {
		path := makeTestGzip(t, "image-*.img.gz", content)

		stream, err := streamGzipImage(path)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		if stream.uncompressedSize != 0 {
			t.Fatalf("uncompressedSize = %d before measuring; want 0", stream.uncompressedSize)
		}

		if err := stream.measureUncompressedSize(nil); err != nil {
			t.Fatalf("measureUncompressedSize() error = %v", err)
		}
		if stream.uncompressedSize != int64(len(content)) {
			t.Errorf("uncompressedSize = %d; want %d", stream.uncompressedSize, len(content))
		}

		// A fresh stream over the same file must pick the size up from the
		// sidecar without another measuring pass.
		again, err := streamGzipImage(path)
		if err != nil {
			t.Fatal(err)
		}
		defer again.Close()
		if again.uncompressedSize != int64(len(content)) {
			t.Errorf("uncompressedSize from sidecar = %d; want %d", again.uncompressedSize, len(content))
		}
	})

	t.Run("stale sidecar is ignored", func(t *testing.T) {
		path := makeTestGzip(t, "image-*.img.gz", content)
		// Sidecar recorded against a different compressed size — e.g. the
		// cached image was replaced by a new version under the same name.
		writeImageSizeSidecar(path, 12345, 99999)

		stream, err := streamGzipImage(path)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		if stream.uncompressedSize != 0 {
			t.Errorf("uncompressedSize = %d; want 0 (stale sidecar must be ignored)", stream.uncompressedSize)
		}
	})

	t.Run("no-op when size already known", func(t *testing.T) {
		s := &imageStream{uncompressedSize: 42}
		if err := s.measureUncompressedSize(nil); err != nil {
			t.Fatalf("measureUncompressedSize() error = %v", err)
		}
		if s.uncompressedSize != 42 {
			t.Errorf("uncompressedSize = %d; want 42 (unchanged)", s.uncompressedSize)
		}
	})

	t.Run("no-op without a source path", func(t *testing.T) {
		s := &imageStream{}
		if err := s.measureUncompressedSize(nil); err != nil {
			t.Fatalf("measureUncompressedSize() error = %v", err)
		}
		if s.uncompressedSize != 0 {
			t.Errorf("uncompressedSize = %d; want 0", s.uncompressedSize)
		}
	})
}

func TestImageStreamWriteProgressMsg(t *testing.T) {
	t.Run("exact uncompressed size drives percent and total", func(t *testing.T) {
		s := &imageStream{uncompressedSize: 200}
		msg, ok := s.writeProgressMsg(50)
		if !ok {
			t.Fatal("writeProgressMsg() ok = false; want true")
		}
		if msg.Percent != 0.25 {
			t.Errorf("Percent = %v; want 0.25", msg.Percent)
		}
		if msg.Written != 50 || msg.Total != 200 {
			t.Errorf("Written/Total = %d/%d; want 50/200", msg.Written, msg.Total)
		}
	})

	t.Run("unknown size falls back to compressed progress without a total", func(t *testing.T) {
		s := &imageStream{
			compressedRead: func() int64 { return 75 },
			compressedSize: 100,
		}
		const written = int64(18_575_000_000) // ~17.3 GiB, far past any bogus total
		msg, ok := s.writeProgressMsg(written)
		if !ok {
			t.Fatal("writeProgressMsg() ok = false; want true")
		}
		if msg.Percent != 0.75 {
			t.Errorf("Percent = %v; want 0.75 (compressed bytes consumed)", msg.Percent)
		}
		if msg.Written != written {
			t.Errorf("Written = %d; want %d", msg.Written, written)
		}
		if msg.Total != 0 {
			t.Errorf("Total = %d; want 0 (unknown, must not render a bogus total)", msg.Total)
		}
	})

	t.Run("no size information yields no update", func(t *testing.T) {
		s := &imageStream{}
		if _, ok := s.writeProgressMsg(50); ok {
			t.Error("writeProgressMsg() ok = true; want false with no size info")
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

func TestResolveDeviceNameFlag(t *testing.T) {
	got, err := resolveDeviceName("wendy-pi-5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "wendy-pi-5" {
		t.Fatalf("got %q; want %q", got, "wendy-pi-5")
	}
}

func TestResolveDeviceNameNoFlagNonInteractive(t *testing.T) {
	origInteractive := isInteractiveTerminalFn
	isInteractiveTerminalFn = func() bool { return false }
	t.Cleanup(func() { isInteractiveTerminalFn = origInteractive })

	got, err := resolveDeviceName("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q; want empty device name", got)
	}
}

func TestResolveDeviceNameInteractiveCancelled(t *testing.T) {
	origInteractive := isInteractiveTerminalFn
	origPrompt := promptDeviceName
	isInteractiveTerminalFn = func() bool { return true }
	promptDeviceName = func(_, _ string, _ tui.ValidateFunc) (string, error) {
		return "", tui.ErrCancelled
	}
	t.Cleanup(func() {
		isInteractiveTerminalFn = origInteractive
		promptDeviceName = origPrompt
	})

	_, err := resolveDeviceName("")
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("expected ErrUserCancelled, got %v", err)
	}
}

func TestResolveDeviceNameInteractiveBlankReturnsEmpty(t *testing.T) {
	origInteractive := isInteractiveTerminalFn
	origPrompt := promptDeviceName
	isInteractiveTerminalFn = func() bool { return true }
	promptDeviceName = func(_, _ string, _ tui.ValidateFunc) (string, error) {
		return "   ", nil
	}
	t.Cleanup(func() {
		isInteractiveTerminalFn = origInteractive
		promptDeviceName = origPrompt
	})

	got, err := resolveDeviceName("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("got %q; want empty device name", got)
	}
}

// stubWifiPrompts replaces all interactive prompt hooks used by
// promptAddOneCredential and restores them on test cleanup.
func stubWifiPrompts(t *testing.T) {
	t.Helper()
	origSelect := selectWifiNetworkFromScan
	origManual := confirmManualWifiEntry
	origKeychain := confirmKeychainLookup
	origSSID := promptWifiSSID
	origPassword := promptWifiPassword
	t.Cleanup(func() {
		selectWifiNetworkFromScan = origSelect
		confirmManualWifiEntry = origManual
		confirmKeychainLookup = origKeychain
		promptWifiSSID = origSSID
		promptWifiPassword = origPassword
	})
}

func TestPromptAddOneCredentialSkipsWhenManualEntryDeclined(t *testing.T) {
	stubWifiPrompts(t)
	selectWifiNetworkFromScan = func() (wifiScanSelection, error) {
		return wifiScanSelection{ScanErr: errNoWifiAdapter}, nil
	}
	confirmManualWifiEntry = func() (bool, error) { return false, nil }
	promptWifiSSID = func() (string, error) {
		t.Fatal("SSID prompt must not be shown after declining manual entry")
		return "", nil
	}

	_, added, err := promptAddOneCredential(0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if added {
		t.Fatal("expected added=false when the user skips WiFi setup")
	}
}

func TestPromptAddOneCredentialManualEntryAfterScanFailure(t *testing.T) {
	stubWifiPrompts(t)
	selectWifiNetworkFromScan = func() (wifiScanSelection, error) {
		return wifiScanSelection{ScanErr: errors.New("exit status 1")}, nil
	}
	confirmManualWifiEntry = func() (bool, error) { return true, nil }
	confirmKeychainLookup = func(string) (bool, error) { return false, nil }
	promptWifiSSID = func() (string, error) { return "homenet", nil }
	promptWifiPassword = func(string) (string, error) { return "hunter2", nil }

	c, added, err := promptAddOneCredential(0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !added {
		t.Fatal("expected added=true after manual entry")
	}
	if c.SSID != "homenet" || c.Password != "hunter2" {
		t.Fatalf("got SSID=%q password=%q; want homenet/hunter2", c.SSID, c.Password)
	}
	if c.Priority != 100 {
		t.Fatalf("got priority %d; want 100 for first network", c.Priority)
	}
}

func TestPromptAddOneCredentialEmptyScanOffersSkip(t *testing.T) {
	stubWifiPrompts(t)
	selectWifiNetworkFromScan = func() (wifiScanSelection, error) {
		return wifiScanSelection{}, nil
	}
	confirmManualWifiEntry = func() (bool, error) { return false, nil }

	_, added, err := promptAddOneCredential(0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if added {
		t.Fatal("expected added=false when scan finds nothing and user declines manual entry")
	}
}

func TestPromptAddOneCredentialScanCancelled(t *testing.T) {
	stubWifiPrompts(t)
	selectWifiNetworkFromScan = func() (wifiScanSelection, error) {
		return wifiScanSelection{}, ErrUserCancelled
	}

	_, _, err := promptAddOneCredential(0)
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("expected ErrUserCancelled, got %v", err)
	}
}

func TestPromptAddOneCredentialUsesPickedNetwork(t *testing.T) {
	stubWifiPrompts(t)
	selectWifiNetworkFromScan = func() (wifiScanSelection, error) {
		return wifiScanSelection{SSID: "cafe", HadNetworks: true}, nil
	}
	confirmManualWifiEntry = func() (bool, error) {
		t.Fatal("skip confirm must not be shown after a successful pick")
		return false, nil
	}
	confirmKeychainLookup = func(string) (bool, error) { return false, nil }
	promptWifiPassword = func(string) (string, error) { return "hunter2", nil }

	c, added, err := promptAddOneCredential(0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !added || c.SSID != "cafe" {
		t.Fatalf("got added=%v SSID=%q; want picked network", added, c.SSID)
	}
}

func TestPromptAddOneCredentialEscWithNetworksGoesManual(t *testing.T) {
	stubWifiPrompts(t)
	// Networks were listed but the user pressed esc to type manually, as
	// the picker title advertises: no skip confirm, straight to the prompt.
	selectWifiNetworkFromScan = func() (wifiScanSelection, error) {
		return wifiScanSelection{HadNetworks: true}, nil
	}
	confirmManualWifiEntry = func() (bool, error) {
		t.Fatal("skip confirm must not be shown when the user chose manual entry")
		return false, nil
	}
	confirmKeychainLookup = func(string) (bool, error) { return false, nil }
	promptWifiSSID = func() (string, error) { return "homenet", nil }
	promptWifiPassword = func(string) (string, error) { return "hunter2", nil }

	c, added, err := promptAddOneCredential(0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !added || c.SSID != "homenet" {
		t.Fatalf("got added=%v SSID=%q; want manual entry", added, c.SSID)
	}
}

func TestResolveDeviceNameFlagValidation(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr string
	}{
		{"too short", "ab", "3–55 characters"},
		{"too long", strings.Repeat("a", 56), "3–55 characters"},
		{"starts with number", "1device", "start with a lowercase letter"},
		{"uppercase", "Wendy", "lowercase letters, digits, and hyphens"},
		{"underscore", "wendy_pi", "lowercase letters, digits, and hyphens"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveDeviceName(tc.in)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), "--device-name") {
				t.Fatalf("error should mention --device-name, got %q", err.Error())
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q should contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestValidateDeviceNameLengthBoundary pins the device-name length cap to the
// value that keeps the agent-derived "wendyos-<name>" hostname within the
// 63-octet RFC 1035 label limit (WDY-1518).
func TestValidateDeviceNameLengthBoundary(t *testing.T) {
	if maxDeviceNameLen != 55 {
		t.Fatalf("maxDeviceNameLen = %d; want 55 so wendyos-<name> stays a valid DNS label", maxDeviceNameLen)
	}

	maxName := strings.Repeat("a", maxDeviceNameLen)
	if err := validateDeviceName(maxName); err != nil {
		t.Fatalf("name of max length %d should be valid: %v", maxDeviceNameLen, err)
	}
	if got := len("wendyos-" + maxName); got > 63 {
		t.Fatalf("derived hostname label is %d octets; exceeds the RFC 1035 limit of 63", got)
	}

	if err := validateDeviceName(strings.Repeat("a", maxDeviceNameLen+1)); err == nil {
		t.Fatalf("name longer than %d should be rejected", maxDeviceNameLen)
	}
}

func TestOptionalDeviceNameValidatorAllowsAutoGenerate(t *testing.T) {
	if err := optionalDeviceNameValidator(""); err != nil {
		t.Fatalf("empty device name should allow auto-generation: %v", err)
	}
	if err := optionalDeviceNameValidator("   "); err != nil {
		t.Fatalf("blank device name should allow auto-generation: %v", err)
	}
	if err := optionalDeviceNameValidator("wendy-pi"); err != nil {
		t.Fatalf("valid device name should pass: %v", err)
	}
	if err := optionalDeviceNameValidator("pi"); err == nil {
		t.Fatal("invalid non-empty device name should fail")
	}
}

func TestResolveOSImage_ZipCacheHit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	content := []byte("fake image bytes")
	zipPath, err := osCachedZipPath("test-device", "9.9.9", "")
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

	imgPath, err := osCachedImagePath("test-device", "8.8.8", "")
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
	zipPath, err := osCachedZipPath("stream-device", "7.7.7", "")
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
	stream, err := openOSImageStream("stream-device", img)
	if err != nil {
		t.Fatalf("openOSImageStream: %v", err)
	}
	defer stream.Close()

	if stream.uncompressedSize != int64(len(content)) {
		t.Errorf("uncompressedSize = %d; want %d", stream.uncompressedSize, len(content))
	}
	got, err := io.ReadAll(stream)
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
	imgPath, err := osCachedImagePath("legacy-device", "6.6.6", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(imgPath, content, 0o644); err != nil {
		t.Fatal(err)
	}

	img := &imageInfo{Version: "6.6.6", DownloadURL: "https://example.com/image.zip"}
	stream, err := openOSImageStream("legacy-device", img)
	if err != nil {
		t.Fatalf("openOSImageStream: %v", err)
	}
	defer stream.Close()

	if stream.uncompressedSize != int64(len(content)) {
		t.Errorf("uncompressedSize = %d; want %d", stream.uncompressedSize, len(content))
	}
	got, err := io.ReadAll(stream)
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

func TestResolveDeviceNamePromptStatesConstraints(t *testing.T) {
	origInteractive := isInteractiveTerminalFn
	origPrompt := promptDeviceName
	isInteractiveTerminalFn = func() bool { return true }

	var gotHint string
	var gotValidate tui.ValidateFunc
	promptDeviceName = func(_, hint string, validate tui.ValidateFunc) (string, error) {
		gotHint = hint
		gotValidate = validate
		return "brave-dolphin", nil
	}
	t.Cleanup(func() {
		isInteractiveTerminalFn = origInteractive
		promptDeviceName = origPrompt
	})

	if _, err := resolveDeviceName(""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The hint must surface the naming constraints inline (WDY-1475).
	for _, want := range []string{"a-z", "3–55", "auto-generate"} {
		if !strings.Contains(gotHint, want) {
			t.Errorf("hint %q should mention %q", gotHint, want)
		}
	}

	// The prompt must be wired to the real validator so invalid input
	// re-prompts with the specific violation instead of terminating.
	if gotValidate == nil {
		t.Fatal("prompt must receive a validator")
	}
	if err := gotValidate("MyDevice"); err == nil || !strings.Contains(err.Error(), "lowercase") {
		t.Fatalf("validator should reject uppercase with a specific message, got %v", err)
	}
	if err := gotValidate(""); err != nil {
		t.Fatalf("validator should accept empty (auto-generate), got %v", err)
	}
}

func TestShouldPromptFlashMode(t *testing.T) {
	tests := []struct {
		name               string
		deviceType         string
		installMode        string
		rootfsOnlyExplicit bool
		storageOverride    string
		interactive        bool
		want               bool
	}{
		{"AGX Orin recovery interactive prompts", orinDeviceType, "recovery", false, "", true, true},
		{"Orin Nano recovery interactive prompts", orinNanoDeviceType, "recovery", false, "", true, true},
		{"non-interactive no prompt", orinDeviceType, "recovery", false, "", false, false},
		{"explicit --rootfs-only no prompt", orinDeviceType, "recovery", true, "", true, false},
		{"storage emmc no prompt", orinDeviceType, "recovery", false, "emmc", true, false},
		{"legacy install mode no prompt", orinDeviceType, "image", false, "", true, false},
		{"thor no prompt", thorDeviceType, "recovery", false, "", true, false},
		{"raspberry pi no prompt", "raspberry-pi-5", "recovery", false, "", true, false},
	}
	for _, tc := range tests {
		if got := shouldPromptFlashMode(tc.deviceType, tc.installMode, tc.rootfsOnlyExplicit, tc.storageOverride, tc.interactive); got != tc.want {
			t.Errorf("%s: shouldPromptFlashMode = %v; want %v", tc.name, got, tc.want)
		}
	}
}

// TestFlashRetryAction covers installESP32Firmware's post-flash decision
// rules. The load-bearing case is cancellation: tui.ProgressModel reports q,
// Ctrl+C and SIGINT as context.Canceled, and the retry prompt defaults to Yes,
// so a cancelled flash must never come back as retryable.
func TestFlashRetryAction(t *testing.T) {
	genericErr := errors.New("sync: failed to sync with ESP32 bootloader")
	busyErr := fmt.Errorf("%w: opening USB device /dev/fake", errPortBusy)

	tests := []struct {
		name        string
		flashErr    error
		interactive bool
		wantRetry   bool
		wantFatal   error // errors.Is target; nil means "fatal must be nil"
	}{
		{"success interactive", nil, true, false, nil},
		{"success non-interactive", nil, false, false, nil},

		// Cancellation is fatal in both modes, and is returned unwrapped so
		// callers up the stack can still recognise it.
		{"cancelled interactive", context.Canceled, true, false, context.Canceled},
		{"cancelled non-interactive", context.Canceled, false, false, context.Canceled},
		{"cancelled wrapped", fmt.Errorf("flash: %w", context.Canceled), true, false, context.Canceled},

		{"busy interactive", busyErr, true, true, nil},
		{"busy non-interactive", busyErr, false, false, errPortBusy},

		{"generic interactive", genericErr, true, true, nil},
		{"generic non-interactive", genericErr, false, false, genericErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			retry, fatal := flashRetryAction(tt.flashErr, tt.interactive)

			if retry != tt.wantRetry {
				t.Errorf("retry = %v, want %v", retry, tt.wantRetry)
			}
			if tt.wantFatal == nil {
				if fatal != nil {
					t.Errorf("fatal = %v, want nil", fatal)
				}
			} else if !errors.Is(fatal, tt.wantFatal) {
				t.Errorf("fatal = %v, want one matching %v", fatal, tt.wantFatal)
			}
			if retry && fatal != nil {
				t.Errorf("retry and fatal are mutually exclusive, got retry=%v fatal=%v", retry, fatal)
			}
		})
	}
}

// A cancelled flash must be fatal no matter what the interactive flag says —
// the single most important property of flashRetryAction.
func TestFlashRetryAction_CancellationNeverRetries(t *testing.T) {
	for _, interactive := range []bool{true, false} {
		retry, fatal := flashRetryAction(context.Canceled, interactive)
		if retry {
			t.Errorf("interactive=%v: retry = true, want false on cancellation", interactive)
		}
		if !errors.Is(fatal, context.Canceled) {
			t.Errorf("interactive=%v: fatal = %v, want context.Canceled", interactive, fatal)
		}
	}
}

func TestSupportedInstallMode(t *testing.T) {
	for _, mode := range []string{"", "recovery"} {
		if !supportedInstallMode(mode) {
			t.Errorf("supportedInstallMode(%q) = false, want true", mode)
		}
	}
	for _, mode := range []string{"edl", "something-new"} {
		if supportedInstallMode(mode) {
			t.Errorf("supportedInstallMode(%q) = true, want false", mode)
		}
	}
}

func TestCheckImageFitsDrive(t *testing.T) {
	const gb = int64(1_000_000_000)
	sd8 := drive{Name: "SD Card", DevicePath: "/dev/sdb", SizeBytes: 7_948_206_080}
	tests := []struct {
		name      string
		imageSize int64
		d         drive
		wantErr   bool
	}{
		{"image larger than drive", 18_165_548_032, sd8, true},
		{"image one byte too large", sd8.SizeBytes + 1, sd8, true},
		{"image exactly drive size", sd8.SizeBytes, sd8, false},
		{"image smaller than drive", 4 * gb, sd8, false},
		{"unknown image size", 0, sd8, false},
		{"unknown drive size", 18 * gb, drive{DevicePath: "/dev/sdb"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkImageFitsDrive(tt.imageSize, tt.d)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkImageFitsDrive(%d, %d) = %v, wantErr %v", tt.imageSize, tt.d.SizeBytes, err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "/dev/sdb") {
				t.Errorf("error %q does not name the drive", err)
			}
		})
	}
}

func TestPreflightImageFits(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	const imageSize = 18_165_548_032
	bmapXML := fmt.Sprintf(`<bmap version="2.0"><ImageSize>%d</ImageSize><BlockSize>4096</BlockSize>`+
		`<ChecksumType>sha256</ChecksumType><BlockMap><Range chksum="ab">0</Range></BlockMap></bmap>`, imageSize)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/image.bmap" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, bmapXML)
	}))
	defer srv.Close()

	small := drive{Name: "SD Card", DevicePath: "/dev/sdb", SizeBytes: 7_948_206_080}
	large := drive{Name: "SD Card", DevicePath: "/dev/sdb", SizeBytes: 31_914_983_424}
	tests := []struct {
		name    string
		bmapURL string
		d       drive
		noBmap  bool
		wantErr bool
	}{
		{"drive too small", srv.URL + "/image.bmap", small, false, true},
		{"drive large enough", srv.URL + "/image.bmap", large, false, false},
		{"no-bmap skips the check", srv.URL + "/image.bmap", small, true, false},
		{"no bmap published", "", small, false, false},
		{"bmap fetch fails", srv.URL + "/missing.bmap", small, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			img := &imageInfo{BmapURL: tt.bmapURL}
			err := preflightImageFits("test-device", "9.9.9", "sd", img, tt.d, tt.noBmap)
			if (err != nil) != tt.wantErr {
				t.Fatalf("preflightImageFits() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
