//go:build darwin || linux || windows

package commands

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/onboarding"
)

// masterWithAlias is master.json as the publisher writes it for the AGX eMMC alias.
const masterWithAlias = `{"devices":{
"jetson-agx-orin":{"latest":"1.0.0","latest_nightly":"nightly-1","manifest_path":"manifests/agx.json"},
"jetson-agx-orin-emmc":{"latest":"1.0.0","latest_nightly":"nightly-1","manifest_path":"manifests/agx-emmc.json",
 "alias_of":"jetson-agx-orin","alias_storage":"emmc"}}}`

func decodeMasterWithAlias(t *testing.T) *mainManifest {
	t.Helper()
	var main mainManifest
	if err := json.Unmarshal([]byte(masterWithAlias), &main); err != nil {
		t.Fatal(err)
	}
	return &main
}

// serveManifests answers GCS requests from files keyed by object path, and
// lets alias lookups read the same master manifest.
func serveManifests(t *testing.T, files map[string]string) {
	t.Helper()
	previousTransport, previousFetch := http.DefaultTransport, fetchInstallMainManifest
	t.Cleanup(func() { http.DefaultTransport, fetchInstallMainManifest = previousTransport, previousFetch })
	http.DefaultTransport = driverManifestTransport(func(req *http.Request) (*http.Response, error) {
		body, ok := files[strings.TrimPrefix(req.URL.Path, "/wendyos-images-public/")]
		status := http.StatusOK
		if !ok {
			t.Errorf("unexpected request %s", req.URL)
			status = http.StatusNotFound
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	fetchInstallMainManifest = func(ctx context.Context, _ int) (*mainManifest, error) {
		return fetchMainManifestContext(ctx)
	}
}

func TestApplyDeviceTypeAlias(t *testing.T) {
	main := decodeMasterWithAlias(t)
	for _, tc := range []struct {
		name, device, storage            string
		wantDevice, wantStorage, wantErr string
	}{
		{name: "alias maps to its storage", device: "jetson-agx-orin-emmc", wantDevice: orinDeviceType, wantStorage: "emmc"},
		{name: "matching storage is accepted", device: "jetson-agx-orin-emmc", storage: "emmc", wantDevice: orinDeviceType, wantStorage: "emmc"},
		{name: "conflicting storage is refused", device: "jetson-agx-orin-emmc", storage: "nvme", wantErr: "--device-type jetson-agx-orin with --storage"},
		{name: "storage the device lacks is refused", device: "jetson-agx-orin-emmc", storage: "sd", wantErr: "--device-type jetson-agx-orin with --storage"},
		{name: "canonical device passes through", device: orinDeviceType, storage: "nvme", wantDevice: orinDeviceType, wantStorage: "nvme"},
		{name: "unknown device passes through", device: "raspberry-pi-5", storage: "sd", wantDevice: "raspberry-pi-5", wantStorage: "sd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device, storage, err := applyDeviceTypeAlias(main, tc.device, tc.storage)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				// Echoing the requested storage would suggest a combination that may not exist.
				if strings.Contains(err.Error(), "--storage "+tc.storage) {
					t.Fatalf("error %q suggests --storage %s", err, tc.storage)
				}
				return
			}
			if err != nil || device != tc.wantDevice || storage != tc.wantStorage {
				t.Fatalf("got %q/%q/%v, want %q/%q", device, storage, err, tc.wantDevice, tc.wantStorage)
			}
		})
	}
}

func TestDeviceInfosHideAliases(t *testing.T) {
	var mu sync.Mutex
	var fetched []string
	devices := fetchDeviceInfos(decodeMasterWithAlias(t), func(dev manifestDevice) string { return dev.Latest },
		func(path string) (*deviceManifest, error) {
			mu.Lock()
			defer mu.Unlock()
			fetched = append(fetched, path)
			return &deviceManifest{}, nil
		})
	if len(devices) != 1 || devices[0].Key != orinDeviceType || len(fetched) != 1 {
		t.Fatalf("devices = %+v, fetched %v; want only %s", devices, fetched, orinDeviceType)
	}
}

func TestInstallPlanResolvesAlias(t *testing.T) {
	recovery := `{"versions":{"1.0.0":{"install_mode":"recovery","emmc_flashpack_path":"emmc.tar.zst","emmc_flashpack_checksum":"sha","emmc_flashpack_size_bytes":100}}}`
	serveManifests(t, map[string]string{"manifests/master.json": masterWithAlias, "manifests/agx.json": recovery})
	opts := onboarding.Options{DeviceType: "jetson-agx-orin-emmc", Carrier: "developer-kit"}
	p, err := planOSInstall(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	command := strings.Join(p.Command, " ")
	if p.DeviceType != orinDeviceType || p.Storage != "emmc" || p.Method != "usb-recovery" ||
		!strings.Contains(command, "--device-type "+orinDeviceType) || !strings.Contains(command, "--storage emmc") {
		t.Fatalf("plan %+v", p)
	}

	for _, storage := range []string{"nvme", "bogus"} {
		opts.Storage = storage
		_, err := planOSInstall(context.Background(), opts)
		if err == nil || strings.Contains(err.Error(), storage) {
			t.Fatalf("storage %q: error = %v, want a refusal that does not suggest it", storage, err)
		}
	}
}

// Drivers are pinned before flashing, so the alias must already name the device whose image is written.
func TestOSInstallDriversResolveAliasFirst(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("LocalAppData", cache)
	t.Setenv("WENDY_AGENT_SOCKET", "")
	// Neither manifest publishes the driver, so the run stops before any flashing step.
	noDriver := `{"versions":{"nightly-1":{"devkit":{"kernel_version":"k"}}}}`
	serveManifests(t, map[string]string{"manifests/master.json": masterWithAlias, "manifests/agx.json": noDriver, "manifests/agx-emmc.json": noDriver})

	cmd := newOSInstallCmd()
	cmd.SetArgs([]string{"--nightly", "--driver", "intel-be202", "--device-type", "jetson-agx-orin-emmc", "--device-name", "agx-test"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "on "+orinDeviceType+",") {
		t.Fatalf("error = %v, want the driver lookup against %s", err, orinDeviceType)
	}
}
