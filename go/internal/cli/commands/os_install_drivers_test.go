//go:build darwin || linux || windows

package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestOSInstallDriversPreparePinsImageKernel(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("HOME", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("LocalAppData", cache)
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	digest := sha256.Sum256([]byte("driver payload"))
	downloads := 0
	http.DefaultTransport = driverManifestTransport(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.URL.Path {
		case "/wendyos-images-public/manifests/master.json":
			body = `{"devices":{"dragonwing-iq-9075":{"latest_nightly":"nightly-1","manifest_path":"manifests/iq9.json"}}}`
		case "/wendyos-images-public/manifests/iq9.json":
			body = fmt.Sprintf(`{"versions":{"nightly-1":{"devkit":{"kernel_version":"new-kernel"},"extensions":[
{"name":"intel-be202","kernel_version":"old-kernel","path":"wrong.raw"},
{"name":"intel-be202","kernel_version":"new-kernel","path":"intel.raw","sha256":%q,"signature":"AQID","modules_load":["iwlmld"]}
]}}}`, hex.EncodeToString(digest[:]))
		case "/wendyos-images-public/intel.raw":
			downloads++
			body = "driver payload"
		default:
			t.Fatalf("unexpected request %s", req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	p, err := prepareOSInstallDrivers(context.Background(), "dragonwing-iq-9075", "", true, 0, []string{"intel-be202", "intel-be202"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	if p.version != "nightly-1" || p.kernel != "new-kernel" || len(p.drivers) != 1 || downloads != 1 {
		t.Fatalf("incorrect pinned selection: %+v, downloads=%d", p, downloads)
	}
	spec := p.drivers[0].spec
	if spec.GetArtifactUrl() != "" || spec.GetSha256() != hex.EncodeToString(digest[:]) || len(spec.GetSignature()) != 3 || spec.GetModulesLoad()[0] != "iwlmld" {
		t.Fatalf("published metadata not preserved: %v", spec)
	}
	// A sudo re-exec or UAC relaunch skips close; the elevated run reuses the
	// download and its close removes it.
	relaunched, err := prepareOSInstallDrivers(context.Background(), "dragonwing-iq-9075", "", true, 0, []string{"intel-be202"})
	if err != nil || downloads != 1 || relaunched.drivers[0].path != p.drivers[0].path {
		t.Fatalf("relaunch did not reuse the download: downloads=%d err=%v", downloads, err)
	}
	relaunched.close()
	// A missing second driver must clean up the first download before flashing.
	if _, err := prepareOSInstallDrivers(context.Background(), "dragonwing-iq-9075", "", true, 0, []string{"intel-be202", "missing"}); err == nil {
		t.Fatal("accepted missing driver")
	}
	dir, err := osCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("downloads left behind: %v, %v", entries, err)
	}
}

func TestOSInstallDriversAfterBootTargetsOnlyNamedDevice(t *testing.T) {
	path := t.TempDir() + "/driver.raw"
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mismatch := range []string{"", "name", "bare-name", "board", "kernel"} {
		t.Run(mismatch, func(t *testing.T) {
			var installs []*fakeInstallStream
			deviceType := "dragonwing-iq-9075"
			hostname := "wendyos-iq9-mesh"
			kernel := "expected-kernel"
			switch mismatch {
			case "name":
				hostname = "wendyos-another-device"
			case "bare-name":
				// An explicit rename to the bare name is a different hostname.
				hostname = "iq9-mesh"
			case "board":
				deviceType = "raspberry-pi-5"
			case "kernel":
				kernel = "old-kernel"
			}
			conn := &grpcclient.AgentConnection{
				AgentService:  &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{DeviceType: &deviceType, Hostname: hostname}},
				DriverService: fakeDriverClient{installs: &installs, list: &agentpbv2.ListDriversResponse{KernelVersion: kernel}},
			}
			previous := dialInstallDriverDevice
			t.Cleanup(func() { dialInstallDriverDevice = previous })
			dialInstallDriverDevice = func(_ context.Context, address string) (*grpcclient.AgentConnection, error) {
				if address != "wendyos-iq9-mesh.local:50051" {
					t.Fatalf("unexpected device %s", address)
				}
				return conn, nil
			}
			p := &osInstallDrivers{deviceType: "dragonwing-iq-9075", kernel: "expected-kernel", drivers: []preparedInstallDriver{{spec: &agentpbv2.DriverSpec{Name: "intel-be202"}, path: path}}}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err := p.installAfterBoot(ctx, "iq9-mesh")
			if mismatch != "" {
				if err == nil || len(installs) != 0 {
					t.Fatalf("mismatched device was modified: installs=%v err=%v", installs, err)
				}
			} else if err != nil || len(installs) != 1 || string(installs[0].data) != "payload" {
				t.Fatalf("driver upload failed: installs=%v err=%v", installs, err)
			}
		})
	}
}

func TestOSInstallDriversAfterBootFallsBackToUSB(t *testing.T) {
	path := t.TempDir() + "/driver.raw"
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	previousDial := dialInstallDriverDevice
	t.Cleanup(func() { dialInstallDriverDevice = previousDial })
	dialInstallDriverDevice = func(context.Context, string) (*grpcclient.AgentConnection, error) {
		return nil, errors.New("no mDNS on this host")
	}
	withUSBDirectStubs(t, []discovery.USBDirectCandidate{{Interface: "enxa", Zone: "enxa"}}, getAgentVersionAtAddress)
	var installs []*fakeInstallStream
	deviceType, hostname := "dragonwing-iq-9075", "wendyos-iq9-mesh"
	previousUSB := usbDirectConnectFn
	t.Cleanup(func() { usbDirectConnectFn = previousUSB })
	usbDirectConnectFn = func(context.Context, string) (*grpcclient.AgentConnection, error) {
		return &grpcclient.AgentConnection{
			AgentService:  &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{DeviceType: &deviceType, Hostname: hostname}},
			DriverService: fakeDriverClient{installs: &installs, list: &agentpbv2.ListDriversResponse{KernelVersion: "expected-kernel"}},
		}, nil
	}
	p := &osInstallDrivers{deviceType: deviceType, kernel: "expected-kernel", drivers: []preparedInstallDriver{{spec: &agentpbv2.DriverSpec{Name: "intel-be202"}, path: path}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.installAfterBoot(ctx, "iq9-mesh"); err != nil || len(installs) != 1 {
		t.Fatalf("USB-connected device not installed: installs=%v err=%v", installs, err)
	}
}

func TestOSInstallDriversRequiresExplicitTarget(t *testing.T) {
	for _, args := range [][]string{
		{"--driver", "intel-be202"},
		{"--driver", "intel-be202", "--device-type", "dragonwing-iq-9075"},
		{"--driver", "intel-be202", "image.img", "/dev/disk99"},
	} {
		cmd := newOSInstallCmd()
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
