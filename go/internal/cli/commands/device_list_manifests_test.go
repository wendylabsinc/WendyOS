//go:build darwin || linux || windows

package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

const masterManifestPath = "manifests/master.json"

// listedMaster is a master.json with boards of every kind a device list has to
// handle: disk-image boards, an Orin, a board flashed from a bundle, a board
// whose manifest alone says it cannot be written here (acme-board), an alias,
// an unpublished entry, and one Wendy Lite firmware.
const listedMaster = `{"devices":{
"raspberry-pi-5":{"latest":"1.0.0","latest_nightly":"nightly-2","manifest_path":"manifests/pi5.json","stability":"stable"},
"jetson-orin-nano":{"latest":"1.0.0","manifest_path":"manifests/nano.json","stability":"stable"},
"generic-x86-64":{"latest":"1.0.0","manifest_path":"manifests/x86.json","stability":"experimental"},
"acme-board":{"latest":"1.0.0","manifest_path":"manifests/acme.json","stability":"experimental"},
"dragonwing-iq-9075":{"latest":"1.0.0","manifest_path":"manifests/iq9.json","stability":"stable"},
"jetson-agx-orin-emmc":{"latest":"1.0.0","manifest_path":"manifests/agx-emmc.json","alias_of":"jetson-agx-orin","alias_storage":"emmc"},
"test-device":{"latest":"","manifest_path":"","stability":"experimental"}},
"firmware":{"esp32c6_net":{"latest":"0.15.0","manifest_path":"manifests/esp32c6_net.json"}}}`

func listedManifests() map[string]string {
	return map[string]string{
		masterManifestPath:    listedMaster,
		"manifests/pi5.json":  `{"versions":{"1.0.0":{"path":"pi5/1.0.0.img.zip","ota_update_path":"pi5/1.0.0.wendy"}}}`,
		"manifests/nano.json": `{"versions":{"1.0.0":{"install_mode":"recovery","ota_update_path":"nano/1.0.0.wendy"}}}`,
		"manifests/x86.json":  `{"versions":{"1.0.0":{"path":"x86/1.0.0.img.zip","ota_update_path":"x86/1.0.0.wendy"}}}`,
		"manifests/acme.json": `{"versions":{"1.0.0":{"install_mode":"bundle","path":"acme/1.0.0.tar.gz"}}}`,
		"manifests/iq9.json":  `{"versions":{"1.0.0":{"install_mode":"edl","qcomflash_path":"iq9/bundle.tar.gz"}}}`,
	}
}

// manifestServer stands in for the public manifest bucket: it answers from
// files keyed by object path, counts every request, and answers 503 for a path
// marked as failing.
type manifestServer struct {
	mu       sync.Mutex
	requests map[string]int
	failing  map[string]bool
}

func serveCountedManifests(t *testing.T, files map[string]string) *manifestServer {
	t.Helper()
	previousTransport, previousFetch := http.DefaultTransport, fetchInstallMainManifest
	t.Cleanup(func() { http.DefaultTransport, fetchInstallMainManifest = previousTransport, previousFetch })
	s := &manifestServer{requests: map[string]int{}, failing: map[string]bool{}}
	http.DefaultTransport = driverManifestTransport(func(req *http.Request) (*http.Response, error) {
		path := strings.TrimPrefix(req.URL.Path, "/wendyos-images-public/")
		s.mu.Lock()
		s.requests[path]++
		failing := s.failing[path]
		s.mu.Unlock()
		body, ok := files[path]
		status := http.StatusOK
		switch {
		case failing:
			status, body = http.StatusServiceUnavailable, ""
		case !ok:
			status = http.StatusNotFound
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	// As in a real run, alias lookups read the master manifest from the bucket.
	fetchInstallMainManifest = func(ctx context.Context, pr int) (*mainManifest, error) {
		if pr > 0 {
			return fetchPRMainManifestContext(ctx, pr)
		}
		return fetchMainManifestContext(ctx)
	}
	return s
}

func (s *manifestServer) fail(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing[path] = true
}

func (s *manifestServer) recover(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.failing, path)
}

// wantRequests fails the test unless exactly these requests were made so far.
func (s *manifestServer) wantRequests(t *testing.T, want map[string]int) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !reflect.DeepEqual(s.requests, want) {
		t.Errorf("manifest requests so far = %s, want %s", requestList(s.requests), requestList(want))
	}
}

func requestList(requests map[string]int) string {
	var parts []string
	for path, n := range requests {
		parts = append(parts, fmt.Sprintf("%s x%d", path, n))
	}
	sort.Strings(parts)
	return "[" + strings.Join(parts, ", ") + "]"
}

func runInstallCommand(args ...string) error {
	cmd := newOSInstallCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(append([]string{}, args...))
	return cmd.Execute()
}

func stubInstallPicker(t *testing.T, pick func(call int, rows []tui.PickerItem) (string, error)) *int {
	t.Helper()
	previous := pickInstallDeviceFn
	t.Cleanup(func() { pickInstallDeviceFn = previous })
	calls := new(int)
	pickInstallDeviceFn = func(rows []tui.PickerItem) (string, error) {
		*calls++
		return pick(*calls, rows)
	}
	return calls
}

func stubListedDevicePicker(t *testing.T, pick func(call int, rows []tui.PickerItem) (string, error)) *int {
	t.Helper()
	previous := pickListedDeviceFn
	t.Cleanup(func() { pickListedDeviceFn = previous })
	calls := new(int)
	pickListedDeviceFn = func(_ string, rows []tui.PickerItem) (string, error) {
		*calls++
		return pick(*calls, rows)
	}
	return calls
}

// rowDescriptions maps each row's value to its description.
func rowDescriptions(rows []tui.PickerItem) map[string]string {
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.Value.(string)] = row.Description
	}
	return out
}

func wantRows(t *testing.T, rows []tui.PickerItem, want map[string]string) {
	t.Helper()
	if got := rowDescriptions(rows); !reflect.DeepEqual(got, want) {
		t.Fatalf("picker rows = %v, want %v", got, want)
	}
}

// Without a terminal and without --device-type the command names the boards it
// would have listed. Building that list takes the master manifest and nothing
// else: no board's own manifest, and no second copy of the master.
func TestInstallListsBoardsAfterOneMasterManifestRequest(t *testing.T) {
	stubSessionTerminals(t, false, false, nil)
	analytics.Disable()
	server := serveCountedManifests(t, listedManifests())

	err := runInstallCommand()
	if err == nil || !IsUsageError(err) {
		t.Fatalf("got %v; want the usage error that lists --device-type values", err)
	}
	for _, board := range []string{"raspberry-pi-5", "jetson-orin-nano", "generic-x86-64", "acme-board", "dragonwing-iq-9075", linuxDesktopValue} {
		if !strings.Contains(err.Error(), board) {
			t.Errorf("error %q does not offer %s", err, board)
		}
	}
	for _, hidden := range []string{"jetson-agx-orin-emmc", "test-device"} {
		if strings.Contains(err.Error(), hidden) {
			t.Errorf("error %q offers %s", err, hidden)
		}
	}
	server.wantRequests(t, map[string]int{masterManifestPath: 1})
}

func TestInstallPickerOpensAfterOneMasterManifestRequest(t *testing.T) {
	stubSessionTerminals(t, true, true, nil)
	analytics.Disable()
	server := serveCountedManifests(t, listedManifests())
	calls := stubInstallPicker(t, func(_ int, rows []tui.PickerItem) (string, error) {
		server.wantRequests(t, map[string]int{masterManifestPath: 1})
		// The rows are the ones the picker showed when every manifest was read first.
		wantRows(t, rows, map[string]string{
			"raspberry-pi-5":       "(1.0.0)",
			"jetson-orin-nano":     "(1.0.0)",
			"generic-x86-64":       "(1.0.0)",
			"acme-board":           "(1.0.0)",
			"dragonwing-iq-9075":   "(1.0.0)",
			"wlite_target_esp32c6": "",
			linuxDesktopValue:      "Install wendy-agent on an existing Linux machine",
			headlessMacValue:       "Install wendy-agent on an existing Mac (Apple Silicon)",
		})
		return "", ErrUserCancelled
	})

	if err := runInstallCommand(); !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("got %v; want the picker's cancellation", err)
	}
	if *calls != 1 {
		t.Fatalf("picker opened %d times, want 1", *calls)
	}
	server.wantRequests(t, map[string]int{masterManifestPath: 1})
}

// --device-type reads the master manifest once (alias lookup and device list
// share it) and only the named board's own manifest.
func TestInstallWithDeviceTypeReadsOnlyThatBoardsManifest(t *testing.T) {
	stubSessionTerminals(t, false, false, nil)
	analytics.Disable()

	t.Run("release", func(t *testing.T) {
		server := serveCountedManifests(t, listedManifests())
		// The run stops at the first thing read from the board's manifest.
		err := runInstallCommand("--device-type", "raspberry-pi-5", "--version", "9.9.9", "--no-wifi")
		if err == nil || !strings.Contains(err.Error(), `version "9.9.9" not found for Raspberry Pi 5`) {
			t.Fatalf("got %v; want the version looked up in the board's manifest", err)
		}
		server.wantRequests(t, map[string]int{masterManifestPath: 1, "manifests/pi5.json": 1})
	})

	// --driver looks the board's add-ons up before the install starts; that
	// lookup shares the master manifest as well.
	t.Run("driver", func(t *testing.T) {
		cache := t.TempDir()
		t.Setenv("HOME", cache)
		t.Setenv("XDG_CACHE_HOME", cache)
		t.Setenv("LocalAppData", cache)
		t.Setenv("WENDY_AGENT_SOCKET", "")
		server := serveCountedManifests(t, listedManifests())
		// The manifest publishes no such driver, so the run stops before installing.
		err := runInstallCommand("--device-type", "raspberry-pi-5", "--driver", "intel-be202", "--device-name", "lab-pi", "--no-wifi")
		if err == nil {
			t.Fatal("accepted a driver the board's manifest does not publish")
		}
		server.wantRequests(t, map[string]int{masterManifestPath: 1, "manifests/pi5.json": 1})
	})

	// A PR lists from its own master manifest; the release one, which only the
	// Wendy Lite rows need, is not read at all.
	t.Run("pr", func(t *testing.T) {
		server := serveCountedManifests(t, map[string]string{
			"pr/77/manifests/master.json": `{"devices":{
"raspberry-pi-5":{"latest_nightly":"pr-77","manifest_path":"pr/77/manifests/pi5.json"},
"jetson-orin-nano":{"latest_nightly":"pr-77","manifest_path":"pr/77/manifests/nano.json"}}}`,
			"pr/77/manifests/pi5.json":  `{"versions":{"pr-77":{"install_mode":"bundle"}}}`,
			"pr/77/manifests/nano.json": `{"versions":{"pr-77":{"install_mode":"recovery"}}}`,
		})
		err := runInstallCommand("--pr", "77", "--device-type", "raspberry-pi-5", "--no-wifi")
		if err == nil || !strings.Contains(err.Error(), `version pr-77 installs in "bundle" mode`) {
			t.Fatalf("got %v; want the install mode read from the board's PR manifest", err)
		}
		server.wantRequests(t, map[string]int{"pr/77/manifests/master.json": 1, "pr/77/manifests/pi5.json": 1})
	})
}

// A board whose manifest does not load is reported with the reason, not as a
// board the manifest does not know.
func TestInstallWithDeviceTypeReportsAManifestThatDoesNotLoad(t *testing.T) {
	stubSessionTerminals(t, false, false, nil)
	analytics.Disable()
	server := serveCountedManifests(t, listedManifests())
	server.fail("manifests/pi5.json")

	err := runInstallCommand("--device-type", "raspberry-pi-5", "--no-wifi")
	if err == nil {
		t.Fatal("install went on without the board's manifest")
	}
	if strings.Contains(err.Error(), "not found in manifest") {
		t.Fatalf("error %q says the board does not exist; its manifest failed to load", err)
	}
	if !errors.Is(err, ErrManifestUnreachable) || !strings.Contains(err.Error(), "Raspberry Pi 5") || !strings.Contains(err.Error(), "503") {
		t.Fatalf("error %q does not name the board and the reason its manifest failed to load", err)
	}
	server.wantRequests(t, map[string]int{masterManifestPath: 1, "manifests/pi5.json": 1})
}

func TestInstallPickerStaysOpenWhenABoardManifestDoesNotLoad(t *testing.T) {
	stubSessionTerminals(t, true, true, nil)
	analytics.Disable()
	server := serveCountedManifests(t, listedManifests())
	server.fail("manifests/pi5.json")
	calls := stubInstallPicker(t, func(call int, rows []tui.PickerItem) (string, error) {
		if _, listed := rowDescriptions(rows)["raspberry-pi-5"]; !listed {
			t.Fatalf("pick %d: raspberry-pi-5, whose manifest does not load, is missing from the rows %v", call, rowDescriptions(rows))
		}
		if call == 1 {
			return "raspberry-pi-5", nil
		}
		// The second choice loads; the run stops at what its manifest says.
		return "acme-board", nil
	})

	var err error
	warning := captureStderr(t, func() { err = runInstallCommand() })
	if *calls != 2 {
		t.Fatalf("picker opened %d times, want 2: once more after the failed choice", *calls)
	}
	if !strings.Contains(warning, "Raspberry Pi 5") || !strings.Contains(warning, "503") {
		t.Errorf("warning %q does not name the board and the reason its manifest failed to load", warning)
	}
	if err == nil || !strings.Contains(err.Error(), `version 1.0.0 installs in "bundle" mode`) {
		t.Fatalf("got %v; want the second choice's manifest to be read", err)
	}
	server.wantRequests(t, map[string]int{masterManifestPath: 1, "manifests/pi5.json": 1, "manifests/acme.json": 1})
}

// `wendy os download` lists boards the same way: from the master manifest,
// reading the chosen board's manifest afterwards.
func TestOSDownloadPickerReadsOnlyTheChosenBoardsManifest(t *testing.T) {
	server := serveCountedManifests(t, listedManifests())
	server.fail("manifests/pi5.json")
	calls := stubListedDevicePicker(t, func(call int, rows []tui.PickerItem) (string, error) {
		// The Dragonwing installs from a flash bundle and is never offered here.
		wantRows(t, rows, map[string]string{
			"raspberry-pi-5":   "(latest: 1.0.0)",
			"jetson-orin-nano": "(latest: 1.0.0)",
			"generic-x86-64":   "(latest: 1.0.0)",
			"acme-board":       "(latest: 1.0.0)",
		})
		if call == 1 {
			server.wantRequests(t, map[string]int{masterManifestPath: 1})
			return "raspberry-pi-5", nil
		}
		return "generic-x86-64", nil
	})

	var key string
	var dev deviceInfo
	var err error
	warning := captureStderr(t, func() { key, dev, err = pickLinuxDevice() })
	if err != nil || key != "generic-x86-64" || dev.Manifest == nil || dev.Manifest.Versions["1.0.0"].Path != "x86/1.0.0.img.zip" {
		t.Fatalf("got %q, %+v, %v; want generic-x86-64 with its manifest", key, dev, err)
	}
	if *calls != 2 {
		t.Fatalf("picker opened %d times, want 2: once more after the failed choice", *calls)
	}
	if !strings.Contains(warning, "Raspberry Pi 5") || !strings.Contains(warning, "503") {
		t.Errorf("warning %q does not name the board and the reason its manifest failed to load", warning)
	}
	server.wantRequests(t, map[string]int{masterManifestPath: 1, "manifests/pi5.json": 1, "manifests/x86.json": 1})
}

// A board outside the known flash-bundle types is listed, because only its own
// manifest says that it installs from a bundle. Choosing it is refused with the
// reason, and the list opens again.
func TestOSDownloadPickerRefusesAFlashBundleBoardOnceItsManifestSaysSo(t *testing.T) {
	server := serveCountedManifests(t, listedManifests())
	calls := stubListedDevicePicker(t, func(call int, _ []tui.PickerItem) (string, error) {
		if call == 1 {
			return "acme-board", nil
		}
		return "", ErrUserCancelled
	})

	var err error
	warning := captureStderr(t, func() { _, _, err = pickLinuxDevice() })
	if !errors.Is(err, ErrUserCancelled) || *calls != 2 {
		t.Fatalf("got %v after %d picks; want the list opened again and then cancelled", err, *calls)
	}
	if !strings.Contains(warning, "Acme Board") || !strings.Contains(warning, "flash bundle") {
		t.Errorf("warning %q does not say why Acme Board cannot be downloaded", warning)
	}
	server.wantRequests(t, map[string]int{masterManifestPath: 1, "manifests/acme.json": 1})
}

// The device-type picker of `wendy os update` too; the list and the chosen
// board's update share one master manifest.
func TestOSUpdateDeviceTypePickerReadsOnlyTheChosenBoardsManifest(t *testing.T) {
	server := serveCountedManifests(t, listedManifests())
	server.fail("manifests/nano.json")
	calls := stubListedDevicePicker(t, func(call int, rows []tui.PickerItem) (string, error) {
		wantRows(t, rows, map[string]string{
			"raspberry-pi-5":     "(latest: 1.0.0)",
			"jetson-orin-nano":   "(latest: 1.0.0)",
			"generic-x86-64":     "(latest: 1.0.0)",
			"acme-board":         "(latest: 1.0.0)",
			"dragonwing-iq-9075": "(latest: 1.0.0)",
		})
		if call == 1 {
			server.wantRequests(t, map[string]int{masterManifestPath: 1})
			return "jetson-orin-nano", nil
		}
		return "raspberry-pi-5", nil
	})

	var url string
	var err error
	warning := captureStderr(t, func() { url, err = pickDeviceTypeAndGetLatestOTA("", false) })
	if err != nil || url != gcsBaseURL+"/pi5/1.0.0.wendy" {
		t.Fatalf("got %q, %v; want the Raspberry Pi 5 update", url, err)
	}
	if *calls != 2 {
		t.Fatalf("picker opened %d times, want 2: once more after the failed choice", *calls)
	}
	if !strings.Contains(warning, "Jetson Orin Nano") || !strings.Contains(warning, "503") {
		t.Errorf("warning %q does not name the board and the reason its update could not be read", warning)
	}
	server.wantRequests(t, map[string]int{masterManifestPath: 1, "manifests/nano.json": 1, "manifests/pi5.json": 1})
}

// Steps of one command that share a context from withMainManifestMemo share
// one download of each master manifest. A failed download is not remembered.
func TestMainManifestIsDownloadedOncePerCommand(t *testing.T) {
	server := serveCountedManifests(t, map[string]string{
		masterManifestPath:            listedMaster,
		"pr/77/manifests/master.json": `{"devices":{"raspberry-pi-5":{"latest_nightly":"pr-77","manifest_path":"pr/77/manifests/pi5.json"}}}`,
	})

	ctx := withMainManifestMemo(context.Background())
	if withMainManifestMemo(ctx) != ctx {
		t.Fatal("a second memo replaced the command's first one")
	}
	for range 2 {
		main, err := fetchMainManifestContext(ctx)
		if err != nil || main.Devices["raspberry-pi-5"].Latest != "1.0.0" {
			t.Fatalf("release master manifest: %+v, %v", main, err)
		}
		pr, err := fetchPRMainManifestContext(ctx, 77)
		if err != nil || pr.Devices["raspberry-pi-5"].LatestNightly != "pr-77" {
			t.Fatalf("PR master manifest: %+v, %v", pr, err)
		}
	}
	server.wantRequests(t, map[string]int{masterManifestPath: 1, "pr/77/manifests/master.json": 1})

	// Outside a command's context every call downloads, as before.
	for range 2 {
		if _, err := fetchMainManifestContext(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	server.wantRequests(t, map[string]int{masterManifestPath: 3, "pr/77/manifests/master.json": 1})

	retry := withMainManifestMemo(context.Background())
	server.fail(masterManifestPath)
	if _, err := fetchMainManifestContext(retry); !errors.Is(err, ErrManifestUnreachable) {
		t.Fatalf("got %v; want the failed download reported", err)
	}
	server.recover(masterManifestPath)
	if _, err := fetchMainManifestContext(retry); err != nil {
		t.Fatalf("the failed download was remembered: %v", err)
	}
	server.wantRequests(t, map[string]int{masterManifestPath: 5, "pr/77/manifests/master.json": 1})
}

// The tour still reads every board's manifest behind its loading screen: it
// hides boards that install from a bundle and picks the Orin flash mode from
// them. A board whose manifest does not load stays listed; its installer
// reports why.
func TestTourDeviceListKeepsABoardWhoseManifestDoesNotLoad(t *testing.T) {
	server := serveCountedManifests(t, listedManifests())
	server.fail("manifests/pi5.json")

	msg, ok := loadDevicesCmd()().(tourDevicesLoadedMsg)
	if !ok || msg.err != nil {
		t.Fatalf("got %+v; want the loaded device list", msg)
	}
	byKey := make(map[string]deviceInfo, len(msg.devices))
	var keys []string
	for _, d := range msg.devices {
		byKey[d.Key] = d
		keys = append(keys, d.Key)
	}
	sort.Strings(keys)
	// acme-board's manifest says it installs from a bundle; the Dragonwing's type does.
	if want := []string{"generic-x86-64", "jetson-orin-nano", "raspberry-pi-5"}; !slices.Equal(keys, want) {
		t.Fatalf("tour lists %v, want %v", keys, want)
	}
	if byKey["raspberry-pi-5"].Manifest != nil {
		t.Fatal("raspberry-pi-5 has a manifest although its download failed")
	}
	nano := byKey["jetson-orin-nano"]
	if args := tourNativeInstallArgs(&nano); !slices.Contains(args, "--rootfs-only=false") {
		t.Fatalf("Orin Nano install args %v do not select full recovery from its manifest", args)
	}
	server.wantRequests(t, map[string]int{
		masterManifestPath: 1, "manifests/pi5.json": 1, "manifests/nano.json": 1,
		"manifests/x86.json": 1, "manifests/acme.json": 1, "manifests/iq9.json": 1,
	})
}
