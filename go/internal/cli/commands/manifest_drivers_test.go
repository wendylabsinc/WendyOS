package commands

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type driverManifestTransport func(*http.Request) (*http.Response, error)

func (f driverManifestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestDriverExtensionsForChannel(t *testing.T) {
	const device = "raspberry-pi-5"
	const kernel = "6.18.33-v8-16k"
	const mainPath = "/manifests/master.json"
	const devicePath = "/manifests/raspberry-pi-5.json"
	const prMainPath = "/pr/123/manifests/master.json"
	const prDevicePath = "/pr/123/manifests/raspberry-pi-5.json"

	tests := []struct {
		name          string
		osVersion     string
		nightly       bool
		pr            int
		legacy        bool
		latestNightly string
		wantPath      string
		wantErr       string
	}{
		{
			name:      "release uses the installed OS version",
			osVersion: "0.16.0", latestNightly: "0.18.0-nightly.2",
			wantPath: "images/release/driver.raw",
		},
		{
			name:      "installed nightly stays pinned without the flag",
			osVersion: "0.18.0-nightly.1", latestNightly: "0.18.0-nightly.2",
			wantPath: "images/older-nightly/driver.raw",
		},
		{
			name:      "nightly selects the latest nightly",
			osVersion: "0.16.0", nightly: true, latestNightly: "0.18.0-nightly.2",
			wantPath: "images/latest-nightly/driver.raw",
		},
		{
			name:      "PR selects its own nightly version",
			osVersion: "0.16.0", pr: 123,
			wantPath: "pr/123/images/driver.raw",
		},
		{
			name:      "OTA resolver retains exact version behavior",
			osVersion: "0.16.0", legacy: true, latestNightly: "0.18.0-nightly.2",
			wantPath: "images/release/driver.raw",
		},
		{
			name:      "OTA resolver retains PR resolution",
			osVersion: "0.16.0", legacy: true, pr: 123,
			wantPath: "pr/123/images/driver.raw",
		},
		{
			name:      "missing nightly does not fall back to the release",
			osVersion: "0.16.0", nightly: true,
			wantErr: "no nightly version available for raspberry-pi-5",
		},
		{
			name:      "missing nightly manifest entry does not fall back to the release",
			osVersion: "0.16.0", nightly: true, latestNightly: "0.18.0-nightly.missing",
			wantErr: "no manifest entry for version 0.18.0-nightly.missing on raspberry-pi-5",
		},
		{
			name:      "conflicting channels are rejected before fetching",
			osVersion: "0.16.0", nightly: true, pr: 123,
			wantErr: "--nightly and --pr cannot be combined",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requested []string
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			http.DefaultTransport = driverManifestTransport(func(req *http.Request) (*http.Response, error) {
				path := strings.TrimPrefix(req.URL.String(), gcsBaseURL)
				requested = append(requested, path)
				var body string
				switch path {
				case mainPath:
					body = fmt.Sprintf(`{"devices":{"raspberry-pi-5":{"latest":"0.17.0","latest_nightly":%q,"manifest_path":"manifests/raspberry-pi-5.json"}}}`, tt.latestNightly)
				case devicePath:
					body = `{"versions":{
						"0.16.0":{"extensions":[{"name":"driver","kernel_version":"6.18.33-v8-16k","path":"images/release/driver.raw"}]},
						"0.17.0":{"extensions":[{"name":"driver","kernel_version":"6.18.33-v8-16k","path":"images/latest-release/driver.raw"}]},
						"0.18.0-nightly.1":{"extensions":[{"name":"driver","kernel_version":"6.18.33-v8-16k","path":"images/older-nightly/driver.raw"}]},
						"0.18.0-nightly.2":{"extensions":[
							{"name":"driver","kernel_version":"different-kernel","path":"images/incompatible/driver.raw"},
							{"name":"driver","kernel_version":"6.18.33-v8-16k","path":"images/latest-nightly/driver.raw"}
						]}
					}}`
				case prMainPath:
					body = `{"devices":{"raspberry-pi-5":{"latest_nightly":"pr-123","manifest_path":"pr/123/manifests/raspberry-pi-5.json"}}}`
				case prDevicePath:
					body = `{"versions":{"pr-123":{"extensions":[{"name":"driver","kernel_version":"6.18.33-v8-16k","path":"pr/123/images/driver.raw"}]}}}`
				default:
					return nil, fmt.Errorf("unexpected manifest URL %s", req.URL)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})

			var exts []extensionEntry
			var err error
			if tt.legacy {
				exts, err = driverExtensionsFor(device, tt.osVersion, tt.pr)
			} else {
				exts, err = driverExtensionsForChannel(context.Background(), device, tt.osVersion, tt.nightly, tt.pr)
			}
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("driver extensions error = %v, want %q", err, tt.wantErr)
				}
				if tt.nightly && tt.pr > 0 && len(requested) != 0 {
					t.Fatalf("conflicting channels fetched manifests: %v", requested)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			e, ok := selectExtension(exts, "driver", kernel)
			if !ok || e.Path != tt.wantPath {
				t.Fatalf("selected driver = %+v, found=%v, want path %q", e, ok, tt.wantPath)
			}
			if _, ok := selectExtension(exts, "driver", "unsupported-kernel"); ok {
				t.Fatal("selected a driver for an unsupported kernel")
			}
			wantRequests := []string{mainPath, devicePath}
			if tt.pr > 0 {
				wantRequests = []string{prMainPath, prDevicePath}
			}
			if strings.Join(requested, ",") != strings.Join(wantRequests, ",") {
				t.Fatalf("requested manifests = %v, want %v", requested, wantRequests)
			}
		})
	}
}
