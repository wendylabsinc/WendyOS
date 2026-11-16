//go:build darwin || linux || windows

package commands

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestOSInstallESP32RefusesBeforeAnyManifestFetch(t *testing.T) {
	stubNonInteractive(t)
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_ANALYTICS", "false")
	previousFetch, previousTransport := fetchInstallMainManifest, http.DefaultTransport
	t.Cleanup(func() {
		fetchInstallMainManifest, http.DefaultTransport = previousFetch, previousTransport
	})
	for _, target := range []string{"esp32-c5", "esp32-c6", "esp32-c61", "esp32-p4", "esp32-s3"} {
		for _, mode := range [][]string{nil, {"--nightly"}, {"--pr", "2181"}} {
			t.Run(target+strings.Join(mode, ""), func(t *testing.T) {
				fetches, requests := 0, 0
				fetchInstallMainManifest = func(context.Context, int) (*mainManifest, error) {
					fetches++
					return nil, errors.New("manifest must not be consulted")
				}
				http.DefaultTransport = driverManifestTransport(func(*http.Request) (*http.Response, error) {
					requests++
					return nil, errors.New("network must not be contacted")
				})
				cmd := newOSInstallCmd()
				cmd.SetOut(io.Discard)
				cmd.SetErr(io.Discard)
				cmd.SetArgs(append([]string{"--device-type", target, "--no-wifi"}, mode...))
				err := cmd.Execute()
				if !errors.Is(err, errESP32DeviceType) {
					t.Fatalf("expected ESP32 refusal, got %v", err)
				}
				if fetches != 0 || requests != 0 {
					t.Fatalf("unsupported target consulted manifests: %d fetches, %d HTTP requests", fetches, requests)
				}
			})
		}
	}
}
