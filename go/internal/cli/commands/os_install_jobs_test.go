//go:build darwin || linux || windows

package commands

import (
	"bytes"
	"compress/gzip"
	"context"
	"github.com/klauspost/compress/zstd"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/onboarding"
)

func TestInstallJobCompressedImageMeasurement(t *testing.T) {
	payload := bytes.Repeat([]byte("wendyos image bytes"), 4096)
	var gz bytes.Buffer
	writer := gzip.NewWriter(&gz)
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	z, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressedZstd := z.EncodeAll(payload, nil)
	z.Close()
	for name, compressed := range map[string][]byte{"gzip": gz.Bytes(), "ordinary-zstd": compressedZstd} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "download.img")
			if err := os.WriteFile(path, compressed, 0600); err != nil {
				t.Fatal(err)
			}
			s, err := openMeasuredInstallJobImage(context.Background(), path, int64(len(payload)))
			if err != nil {
				t.Fatal(err)
			}
			if s.uncompressedSize != int64(len(payload)) {
				t.Fatalf("size=%d", s.uncompressedSize)
			}
			data, err := io.ReadAll(s)
			s.Close()
			if err != nil || !bytes.Equal(data, payload) {
				t.Fatalf("stream was not reopened at byte zero: %v", err)
			}
			if s, err := openMeasuredInstallJobImage(context.Background(), path, int64(len(payload)-1)); err == nil {
				s.Close()
				t.Fatal("accepted decompressed image exceeding capacity")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if s, err := openMeasuredInstallJobImage(ctx, path, int64(len(payload))); err == nil {
				s.Close()
				t.Fatal("measurement ignored cancellation")
			}
		})
	}
}

func TestInstallJobFingerprintBindsObservedIdentity(t *testing.T) {
	base := onboarding.Target{ID: "/dev/test", Name: "Example SSD", Capacity: 64000000000, Removable: true, Identity: "serial-123"}
	fingerprintInstallTarget(&base)
	if len(base.Fingerprint) != 64 {
		t.Fatal("missing confirmation fingerprint")
	}
	for _, change := range []func(*onboarding.Target){
		func(t *onboarding.Target) { t.Identity = "serial-456" },
		func(t *onboarding.Target) { t.ID = "/dev/other" },
		func(t *onboarding.Target) { t.Capacity++ },
		func(t *onboarding.Target) { t.Removable = false },
		func(t *onboarding.Target) { t.USBECID = "another-chip" },
	} {
		changed := base
		change(&changed)
		fingerprintInstallTarget(&changed)
		if changed.Fingerprint == base.Fingerprint {
			t.Fatal("changed target retained authorization token")
		}
	}
	again := base
	fingerprintInstallTarget(&again)
	if again.Fingerprint != base.Fingerprint {
		t.Fatal("fingerprint is not stable")
	}
}

func TestInstallJobCommandsHaveNoInteractiveFallback(t *testing.T) {
	cmd := newOSInstallCmd()
	for _, sub := range []string{"start", "status", "resume", "__worker"} {
		found, _, err := cmd.Find([]string{"jobs", sub})
		if err != nil || found.Name() != sub {
			t.Fatalf("missing jobs %s", sub)
		}
		if found.RunE == nil {
			t.Fatalf("jobs %s has no execution handler", sub)
		}
	}
	backend := installationJobBackend()
	if backend.Plan == nil || backend.Drives == nil || backend.Start == nil || backend.Status == nil || backend.Resume == nil {
		t.Fatal("incomplete job backend")
	}
	status := newOSInstallJobsCmd()
	status.SetArgs([]string{"status", "--job-root", t.TempDir(), "--job-id", "../../outside"})
	if err := status.Execute(); err == nil || !strings.Contains(err.Error(), "invalid installation job ID") {
		t.Fatalf("status accepted path as job ID: %v", err)
	}
}
