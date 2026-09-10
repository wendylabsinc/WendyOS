package providers

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/espidftoolchain"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/swifttoolchain"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

const (
	// microWendyServiceType is the mDNS service type advertised by ESP32 Wendy devices.
	microWendyServiceType = "_wendy-lite._tcp"

	// Currently supported SDK for WASI on Wendy Lite.
	// This also works for older projects that expected to build for wasm32-unknown-none-wasm
	microWendyEmbeddedSDK = "-embedded"
	microWendySwiftTarget = "wasm32-unknown-wasip1"
)

// microWendyBuildContext is stored in BuiltApp.Context.
type microWendyBuildContext struct {
	AppPath string
	AppType liteclient.AppType
}

// MicroWendyProvider builds Swift packages to WASM and serves them to ESP32 devices.
type MicroWendyProvider struct{}

func (p *MicroWendyProvider) Key() string         { return "wendy-lite" }
func (p *MicroWendyProvider) DisplayName() string { return "Wendy Lite" }

func (p *MicroWendyProvider) IsAvailable(ctx context.Context) bool {
	return true
}

func (p *MicroWendyProvider) CheckRequirements(ctx context.Context) error {
	return nil
}

// DiscoverDevices finds Wendy Lite boards plugged in over USB serial and
// reachable on the LAN via mDNS.
//
// Bluetooth boards cannot be discovered through this method — use
// DiscoverDevicesContinuous instead. A BLE scan has no end of its own (see
// DeviceProvider.DiscoverDevices), and every candidate then needs a
// connect-and-read of its info service on a radio that can take seconds just
// to come up (see startBLELiteSource), so folding it in would stretch this
// one-shot call far past the wait a caller expects here.
func (p *MicroWendyProvider) DiscoverDevices(ctx context.Context) ([]models.ExternalDevice, error) {
	sd := discovery.GetSerialDiscovery()
	sd.StartScan(0)

	services, err := discovery.BrowseMDNSServices(ctx, microWendyServiceType, 3*time.Second)
	if err != nil {
		return nil, err
	}

	// WaitForIdle, not Devices: StartScan returns immediately and probes in
	// the background, so reading Devices() right after the mDNS browse above
	// races that pass's own per-port handshake budget against this unrelated
	// fixed window — on a cold first-plug-in the probe can still be running
	// when this reads, silently dropping a genuinely connected, responsive
	// board (WDY-2319). Bounded independently of ctx so a caller with no
	// deadline can't block here forever on a wedged serial port.
	serialCtx, cancel := context.WithTimeout(ctx, serialIdleTimeout)
	defer cancel()

	var devices []models.ExternalDevice
	for _, svc := range services {
		if !connectableLiteMDNSService(svc) {
			continue
		}
		devices = append(devices, p.mdnsExternalDevice(svc))
	}
	for _, dev := range sd.WaitForIdle(serialCtx) {
		devices = append(devices, p.serialExternalDevice(dev))
	}

	// An empty result here is ambiguous: it's indistinguishable from "nothing
	// plugged in" even when a board is connected but its port is held open by
	// something else (see ContendedPorts) — the exact shape of WDY-2319.
	// Surface that distinctly instead of falling through to the generic "no
	// Wendy Lite devices found".
	if len(devices) == 0 {
		if ports := sd.ContendedPorts(); len(ports) > 0 {
			return nil, contendedPortsError(ports)
		}
	}

	return devices, nil
}

// contendedPortsError explains that ESP32 serial ports were found but every
// one of them is currently held open by something else, so no Wendy Lite
// identity handshake could even be attempted.
func contendedPortsError(ports []string) error {
	verb := "it"
	countPrefix := "an ESP32 serial port"
	if len(ports) > 1 {
		verb = "them"
		countPrefix = fmt.Sprintf("%d ESP32 serial ports", len(ports))
	}
	return fmt.Errorf(
		"found %s but couldn't open %s: %s %s in use by another process (e.g. a running `wendy device camera view` or `wendy run`) — stop it and try again",
		countPrefix, verb, strings.Join(ports, ", "), pluralIs(len(ports)),
	)
}

// pluralIs returns the correct copula for the port count in contendedPortsError.
func pluralIs(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// serialIdleTimeout bounds DiscoverDevices' wait for the serial probe pass it
// just kicked off. Generous relative to the identity handshake's own 3s
// per-port budget (see probeIdentityFn) to leave real margin for port
// enumeration and USB/serial connect overhead — the absence of that margin
// was the root cause of WDY-2319.
const serialIdleTimeout = 8 * time.Second

// mdnsExternalDevice maps a resolved _wendy-lite._tcp mDNS service to an
// ExternalDevice.
func (p *MicroWendyProvider) mdnsExternalDevice(svc discovery.MDNSService) models.ExternalDevice {
	deviceId := svc.TXTRecords["id"]
	if deviceId == "" {
		deviceId = svc.TXTRecords["name"]
	}
	if deviceId == "" {
		deviceId = svc.Hostname
	}
	displayName := svc.TXTRecords["displayname"]
	if displayName == "" {
		displayName = svc.InstanceName
	}
	if displayName == "" {
		displayName = svc.Hostname
	}
	return models.ExternalDevice{
		ID:          fmt.Sprintf("wendy-lite:%s", svc.Hostname),
		DisplayName: displayName,
		ProviderKey: p.Key(),
		ConnectionInfo: map[string]string{
			"type":     "LAN",
			"deviceId": deviceId,
			"name":     svc.TXTRecords["name"],
			"hostname": svc.Hostname,
			"ip":       svc.IPAddress,
			"port":     fmt.Sprintf("%d", svc.Port),
			"mtls":     fmt.Sprintf("%t", svc.TXTRecords["mtls"] == "true"),
		},
		IsWendyDevice: true,
	}
}

// connectableLiteMDNSService rejects incomplete/stale DNS-SD records. The
// Wendy Lite LAN connector requires both an IP and a port; presenting a row
// without them creates a blank-address "LAN (Lite)" device that can never be
// selected successfully (commonly an offline device lingering in the macOS
// mDNSResponder cache).
func connectableLiteMDNSService(svc discovery.MDNSService) bool {
	return svc.IPAddress != "" && svc.Port > 0
}

// serialExternalDevice maps a serial-port ESP32 device to an ExternalDevice.
// An unresponsive device — one that matched the ESP32 USB VID/PID but never
// completed the Wendy Lite identity handshake, because no compatible firmware
// is installed yet — is still surfaced rather than dropped, marked with
// ConnectionInfo["needsInstall"] so `wendy run` can offer to flash Wendy Lite
// onto it instead of silently ignoring the board.
func (p *MicroWendyProvider) serialExternalDevice(dev discovery.SerialDevice) models.ExternalDevice {
	if !dev.Responsive {
		return models.ExternalDevice{
			ID:          fmt.Sprintf("wendy-lite:%s", dev.Port),
			DisplayName: fmt.Sprintf("ESP32 (unflashed) — %s", dev.Port),
			ProviderKey: p.Key(),
			ConnectionInfo: map[string]string{
				"type":         "USB",
				"serialPort":   dev.Port,
				"needsInstall": "true",
			},
			IsWendyDevice: true,
		}
	}
	return models.ExternalDevice{
		ID:          fmt.Sprintf("wendy-lite:%s", dev.Port),
		DisplayName: dev.DisplayName,
		ProviderKey: p.Key(),
		ConnectionInfo: map[string]string{
			"type":       "USB",
			"deviceId":   dev.ID,
			"name":       dev.Name,
			"serialPort": dev.Port,
		},
		IsWendyDevice: true,
	}
}

// bleExternalDevice maps a Wendy Lite board found over BLE to an
// ExternalDevice. The PSM the board published travels with the row, but a 0 is
// no problem: ConnectViaBLE reads the info service itself and falls back to
// liteclient.DefaultL2CAPPSM.
func (p *MicroWendyProvider) bleExternalDevice(dev discovery.BLELiteDevice) models.ExternalDevice {
	return models.ExternalDevice{
		ID:          fmt.Sprintf("wendy-lite:%s", dev.Address),
		DisplayName: bleLiteDisplayName(dev),
		ProviderKey: p.Key(),
		ConnectionInfo: map[string]string{
			"type":     "BLE",
			"deviceId": dev.Info.DeviceID,
			"name":     dev.Info.DeviceName,
			"address":  dev.Address,
			"psm":      strconv.FormatUint(uint64(dev.Info.PSM), 10),
			"mtls":     fmt.Sprintf("%t", dev.Info.MTLSEnabled),
		},
		IsWendyDevice: true,
	}
}

// bleLiteDisplayName picks the friendliest label the board offered. The
// identity characteristics are best-effort reads (see ble.ReadLiteInfo), so
// each one can be empty; the advertised local name is the last real fallback
// before the generic label the other BLE paths already use.
func bleLiteDisplayName(dev discovery.BLELiteDevice) string {
	for _, name := range []string{dev.Info.DisplayName, dev.Info.DeviceName, dev.Name} {
		if name != "" {
			return name
		}
	}
	return "Wendy Lite"
}

// DiscoverDevicesContinuous streams wendy-lite devices as they are found:
// mDNS services via continuous browsing, serial devices via the background
// serial scanner, and Wendy Lite boards over BLE. Continuous mDNS browsing
// works on every platform — macOS via mDNSResponder, Linux via Avahi over
// D-Bus (hashicorp/mdns when the daemon is unreachable), Windows via
// hashicorp/mdns — so the polling fallback in callers is now only reached if
// the browse itself fails to start.
//
// BLE reaches a board that is neither plugged in nor on the network, which is
// the case the other two sources cannot cover at all — and the case
// DiscoverDevices cannot serve.
//
// Each emission is the whole set discovered so far across the three sources,
// per the ContinuousDiscoverer contract; see streamDevices for how they merge.
func (p *MicroWendyProvider) DiscoverDevicesContinuous(ctx context.Context) (<-chan []models.ExternalDevice, error) {
	svcCh, err := discovery.BrowseMDNSServicesContinuous(ctx, microWendyServiceType)
	if err != nil {
		return nil, err
	}

	bleCh := startBLELiteSource(ctx)

	sd := discovery.GetSerialDiscovery()
	sd.StartScan(3 * time.Second)

	// Coalesce serial snapshots into a capacity-1 channel: the listener must
	// never block (it runs under SerialDiscovery's notify lock), and only the
	// latest snapshot matters. notify() serializes listener calls, so the
	// drain-then-send below never races with itself.
	serialUpdates := make(chan []discovery.SerialDevice, 1)
	listenerID := sd.AddListener(func(devices []discovery.SerialDevice) {
		select {
		case serialUpdates <- devices:
		default:
			select {
			case <-serialUpdates:
			default:
			}
			serialUpdates <- devices
		}
	})

	ch := make(chan []models.ExternalDevice, 16)
	go func() {
		defer close(ch)
		defer sd.RemoveListener(listenerID)
		defer sd.StopScan()
		p.streamDevices(ctx, svcCh, serialUpdates, bleCh, sd.Devices(), ch)
	}()

	return ch, nil
}

// startBLELiteSource forwards the BLE discovery stream, started off the
// caller's goroutine: bringing up the radio waits several seconds for the
// adapter when Bluetooth is switched off, and the mDNS and serial rows must
// not queue behind that.
//
// BLE is best-effort — no radio, no Bluetooth permission, or no GATT client on
// this platform is ordinary — so a failure to start is not reported: the
// channel simply closes, which streamDevices reads as "no BLE source".
func startBLELiteSource(ctx context.Context) <-chan []discovery.BLELiteDevice {
	out := make(chan []discovery.BLELiteDevice)
	go func() {
		defer close(out)
		devices, err := discovery.BLELiteDeviceDiscoverContinuous(ctx)
		if err != nil {
			return
		}
		for snapshot := range devices {
			select {
			case out <- snapshot:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// streamDevices merges the three discovery sources onto out until ctx is
// done. known is the serial backlog — the devices the scanner had already
// found before the listener was registered.
//
// A source dying — the mDNS browse channel or the BLE channel closing — is
// not a reason to stop: each drops out and the merge keeps running on
// whatever's left, since neither consumer has a way to recover BLE coverage
// on its own (see startBLELiteSource and DiscoverDevicesContinuous). Only
// ctx.Done() ends the stream.
//
// Each source's latest view is kept here and every emission carries the union
// of all three, as ContinuousDiscoverer requires. The sources report shapes
// that differ, and the merge mirrors each one: serial and BLE deliver whole
// sets, so the newest set replaces the previous and a board that stopped being
// reported drops out; the mDNS browse only announces arrivals, so its rows
// accumulate (re-announcements update in place rather than duplicating) until
// the browse itself ends, after which they simply stay as last reported.
//
// Split out from DiscoverDevicesContinuous so the merge can be exercised with
// plain channels: the real sources browse the network and open serial ports.
func (p *MicroWendyProvider) streamDevices(
	ctx context.Context,
	svcCh <-chan discovery.MDNSService,
	serialUpdates <-chan []discovery.SerialDevice,
	bleCh <-chan []discovery.BLELiteDevice,
	known []discovery.SerialDevice,
	out chan<- []models.ExternalDevice,
) {
	var mdns, serial, ble []models.ExternalDevice

	upsertMDNS := func(dev models.ExternalDevice) {
		for i := range mdns {
			if mdns[i].ID == dev.ID {
				mdns[i] = dev
				return
			}
		}
		mdns = append(mdns, dev)
	}

	serialDevices := func(snap []discovery.SerialDevice) []models.ExternalDevice {
		devices := make([]models.ExternalDevice, 0, len(snap))
		for _, dev := range snap {
			devices = append(devices, p.serialExternalDevice(dev))
		}
		return devices
	}

	// emit builds a fresh slice every time rather than handing out the
	// per-source ones: the consumer keeps a snapshot (and pointers into it)
	// past the next update, so the two must not share backing arrays. An empty
	// union is not sent — there is nothing to report yet, and the seeding call
	// below would otherwise emit one for an empty backlog.
	emit := func() bool {
		if len(mdns)+len(serial)+len(ble) == 0 {
			return true
		}
		snapshot := make([]models.ExternalDevice, 0, len(mdns)+len(serial)+len(ble))
		snapshot = append(snapshot, mdns...)
		snapshot = append(snapshot, serial...)
		snapshot = append(snapshot, ble...)
		select {
		case out <- snapshot:
			return true
		case <-ctx.Done():
			return false
		}
	}

	serial = serialDevices(known)
	if !emit() {
		return
	}

	for {
		select {
		case svc, ok := <-svcCh:
			if !ok {
				// The mDNS browse ending (typically a backend error — see
				// discovery.BrowseMDNSServicesContinuous) is not a reason to
				// stop, mirroring the BLE case below: drop the source and
				// keep the other two running. The rows already found stay in
				// the snapshot; new arrivals just stop.
				svcCh = nil
				continue
			}
			if !connectableLiteMDNSService(svc) {
				continue
			}
			upsertMDNS(p.mdnsExternalDevice(svc))
		case snap := <-serialUpdates:
			serial = serialDevices(snap)
		case snap, ok := <-bleCh:
			if !ok {
				// Unlike the mDNS browse, a BLE stream that ends is not a
				// reason to stop: drop the source and keep the other two
				// running. Nothing to tear down — the scan stops with ctx.
				// The rows it already found stay in the snapshot; the source
				// going away is not evidence the boards did.
				bleCh = nil
				continue
			}
			ble = make([]models.ExternalDevice, 0, len(snap))
			for _, dev := range snap {
				if dev.Info.MTLSEnabled {
					ble = append(ble, p.bleExternalDevice(dev))
				}
			}
		case <-ctx.Done():
			return
		}

		// Reached only when a source actually changed: the continue paths
		// above (an unusable mDNS record, the mDNS browse closing, the BLE
		// source closing) skip it.
		if !emit() {
			return
		}
	}
}

func (p *MicroWendyProvider) SupportedBuildTypes() []string {
	return []string{"swift", "esp-idf"}
}

func (p *MicroWendyProvider) CanBuild(projectPath string) bool {
	_, err := os.Stat(filepath.Join(projectPath, "Package.swift"))
	return err == nil
}

func (p *MicroWendyProvider) Build(ctx context.Context, device models.ExternalDevice, projectPath, projectType, product string, debug bool) (*BuiltApp, error) {
	switch projectType {
	case "swift":
		return p.buildSwift(ctx, device, projectPath, product, debug)
	case "esp-idf":
		return p.buildEspIdf(ctx, device, projectPath, product)
	default:
		return nil, fmt.Errorf("wendy-lite provider: unsupported project type %q", projectType)
	}
}

// buildSwift compiles a Swift package to WASM for the embedded WASI target.
func (p *MicroWendyProvider) buildSwift(ctx context.Context, device models.ExternalDevice, projectPath, product string, debug bool) (*BuiltApp, error) {
	// get device info
	di, err := p.GetDeviceInfo(ctx, device)
	if err != nil {
		return nil, fmt.Errorf("getting device info: %w", err)
	}

	// check device capability
	if !di.WasmAppSupport {
		return nil, &AppRequirementsUnsupportedError{Device: device, Missing: "WASM apps"}
	}

	swiftlyTestCmd := exec.CommandContext(ctx, "swiftly", "--version")
	if swiftlyTestCmd.Run() != nil {
		return nil, fmt.Errorf("swiftly is not installed or not in PATH")
	}

	if err := swifttoolchain.EnsureSwiftVersion(ctx, os.Stdout, os.Stderr); err != nil {
		return nil, err
	}

	sdk, err := swifttoolchain.FindSwiftSDK(ctx, "wasm32", os.Stdout, os.Stderr)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(sdk, microWendyEmbeddedSDK) {
		sdk += microWendyEmbeddedSDK
	}

	args := []string{"build", "--swift-sdk", sdk, "--triple", microWendySwiftTarget}
	if !debug {
		args = append(args, "-c", "release")
	}
	cmd := swifttoolchain.SwiftCommandContext(ctx, args...)
	cmd.Dir = projectPath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("swift build (wasi): %w", err)
	}

	binArgs := []string{
		"build",
		"--swift-sdk", sdk,
		"--triple", microWendySwiftTarget,
		"--product", product,
		"--show-bin-path",
	}
	if !debug {
		binArgs = append(binArgs, "-c", "release")
	}
	binCmd := swifttoolchain.SwiftCommandContext(ctx, binArgs...)
	binCmd.Dir = projectPath
	binCmd.Stderr = os.Stderr
	out, err := binCmd.Output()
	if err != nil {
		return nil, fmt.Errorf("swift build --show-bin-path: %w", err)
	}

	binDir := strings.TrimSpace(string(out))
	if binDir == "" {
		return nil, fmt.Errorf("swift build --show-bin-path returned an empty path for %s", product)
	}

	wasmPath := filepath.Join(binDir, product+".wasm")
	if _, err := os.Stat(wasmPath); err != nil {
		return nil, fmt.Errorf("expected WASM output at %s: %w", wasmPath, err)
	}

	return &BuiltApp{
		ProviderKey: p.Key(),
		Device:      device,
		AppName:     product,
		Context:     &microWendyBuildContext{AppPath: wasmPath, AppType: liteclient.AppTypeWasm},
	}, nil
}

// boardToTarget returns the IDF target (SoC name, e.g. "esp32c6") firmware
// must be built for to run on the given board. A board and a target are
// different concepts: the device reports "esp32c6" meaning "generic esp32c6
// board", not the SoC name — they merely coincide for the boards supported
// today, so the mapping is the identity for now. A real lookup goes here once
// board names diverge from SoC names.
func boardToTarget(board string) string {
	return board
}

// espIdfBinaryPath returns the path of the firmware binary an ESP-IDF build
// produces in projectPath's build folder. The binary is named after the CMake
// project() name, falling back to product when no project() declaration is
// found. It returns an error if the binary does not exist.
func espIdfBinaryPath(projectPath, product string) (string, error) {
	binName := espidftoolchain.ProjectName(projectPath)
	if binName == "" {
		binName = product
	}
	binPath := filepath.Join(projectPath, "build", binName+".bin")
	if _, err := os.Stat(binPath); err != nil {
		return "", fmt.Errorf("expected ESP-IDF app binary at %s: %w", binPath, err)
	}
	return binPath, nil
}

// AppRequirementsUnsupportedError reports that the device firmware does not
// support what the app being deployed requires. Missing names the unsupported
// capability (e.g. "native apps", "WASM apps").
type AppRequirementsUnsupportedError struct {
	Device  models.ExternalDevice
	Missing string
}

func (e *AppRequirementsUnsupportedError) Error() string {
	return fmt.Sprintf("device %s does not support %s", e.Device.DisplayName, e.Missing)
}

// buildEspIdf builds an ESP-IDF project with idf.py (via eim) and picks up
// the firmware binary from the project's build folder. The binary is named
// after the CMake project() name, which may differ from the app ID.
func (p *MicroWendyProvider) buildEspIdf(ctx context.Context, device models.ExternalDevice, projectPath, product string) (*BuiltApp, error) {
	// get device info
	di, err := p.GetDeviceInfo(ctx, device)
	if err != nil {
		return nil, fmt.Errorf("getting device info: %w", err)
	}

	// check device capability
	if !di.NativeAppSupport {
		return nil, &AppRequirementsUnsupportedError{Device: device, Missing: "native apps"}
	}

	// ensures the ESP-IDF toolchain is available, install it if not
	if err := espidftoolchain.EnsureVersion(ctx); err != nil {
		return nil, err
	}

	// check if the project has been configured for the right target
	target := boardToTarget(di.DeviceType)
	if strings.ContainsAny(target, " \t\r\n") {
		return nil, fmt.Errorf("invalid device target %q", target)
	}
	configuredTarget := espidftoolchain.ProjectTarget(projectPath)
	if target != "" && target != configuredTarget {
		cmd := espidftoolchain.IdfCommandContext(ctx, "set-target", target)
		cmd.Dir = projectPath
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("idf.py set-target %s: %w", target, err)
		}
	}

	// verify the configuration
	sdkconfig, err := espidftoolchain.ReadSdkconfig(projectPath, []string{
		"CONFIG_WENDY_CORE",
		"CONFIG_WENDY_USJ",
		"CONFIG_ESP_CONSOLE_USB_SERIAL_JTAG_ENABLED",
	})
	if err != nil {
		return nil, fmt.Errorf("reading sdkconfig: %w", err)
	}
	if sdkconfig["CONFIG_WENDY_USJ"] == true && sdkconfig["CONFIG_ESP_CONSOLE_USB_SERIAL_JTAG_ENABLED"] == true {
		return nil, errors.New("Wendy Lite owns the USB Serial/JTAG controller, so the console must not be routed to it — set CONFIG_ESP_CONSOLE_UART_DEFAULT=y and CONFIG_ESP_CONSOLE_SECONDARY_NONE=y in sdkconfig")
	}
	if sdkconfig["CONFIG_WENDY_CORE"] != true {
		return nil, errors.New("this project does not include the wendy_core component — add it to the project's dependencies, then call wendy_core_init() as the very first statement of app_main()")
	}

	// build the project
	cmd := espidftoolchain.IdfCommandContext(ctx, "gen_project_binary") // build only the app binary, not the whole firmware image
	cmd.Dir = projectPath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("build: idf.py gen_project_binary: %w", err)
	}

	// verify the presence of the output bin file
	binPath, err := espIdfBinaryPath(projectPath, product)
	if err != nil {
		return nil, err
	}

	return &BuiltApp{
		ProviderKey: p.Key(),
		Device:      device,
		AppName:     product,
		Context:     &microWendyBuildContext{AppPath: binPath, AppType: liteclient.AppTypeNative},
	}, nil
}

func (p *MicroWendyProvider) Run(ctx context.Context, app *BuiltApp, detach bool, output chan<- RunOutput) error {
	defer close(output)

	bc, ok := app.Context.(*microWendyBuildContext)
	if !ok {
		return fmt.Errorf("wendy-lite provider: invalid build context")
	}

	client, err := p.connectClient(app.Device)
	if err != nil {
		return err
	}
	// Closure, not a bound method: the native path below replaces client with
	// a post-reboot connection that this defer must close instead.
	defer func() { client.Close() }()

	if bc.AppType == liteclient.AppTypeWasm {
		if err := client.StopApp(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: app stop: %v\n", err)
		}
	}

	if detach {
		if err := client.PushApp(bc.AppPath, bc.AppType, nil); err != nil {
			return fmt.Errorf("push app: %w", err)
		}
	} else {
		fmt.Println()
		pushProg := tui.NewProgress("Pushing app...")
		pp := tui.NewProgressProgram(pushProg)
		go func() {
			pushErr := client.PushApp(bc.AppPath, bc.AppType, func(written, total uint32) {
				var pct float64
				if total > 0 {
					pct = float64(written) / float64(total)
				}
				pp.Send(tui.ProgressUpdateMsg{
					Percent: pct,
					Written: int64(written),
					Total:   int64(total),
				})
			})
			pp.Send(tui.ProgressDoneMsg{Err: pushErr})
		}()
		finalModel, err := pp.Run()
		if err != nil {
			return fmt.Errorf("progress TUI: %w", err)
		}
		if finalModel.(tui.ProgressModel).Err() != nil {
			return fmt.Errorf("push app: %w", finalModel.(tui.ProgressModel).Err())
		}
	}

	fmt.Println()
	if bc.AppType == liteclient.AppTypeNative {
		// A native app is a full firmware image that only runs after a reboot.
		// The device drops the connection while restarting, so reconnect
		// before starting the app.
		fmt.Println("Rebooting device...")
		if err := client.ResetTargetDevice(true, 30*time.Second); err != nil {
			return fmt.Errorf("device reset: %w", err)
		}
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: close: %v\n", err)
		}

		fmt.Println("Waiting for device to come back...")
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}

		deadline := time.Now().Add(60 * time.Second)
		for {
			newClient, err := p.connectClient(app.Device)
			if err == nil {
				client = newClient
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("reconnect to device after reboot: %w", err)
			}
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	var consoleDetach func(abrupt bool) error
	var forwardDone chan struct{}
	if !detach {
		consoleCh, cd, err := client.ConsoleAttach(true, true)
		if err != nil {
			return fmt.Errorf("console attach: %w", err)
		}
		consoleDetach = cd

		// Console events and command responses share one ordered stream,
		// so console chunks must be drained whenever a command response is
		// outstanding — otherwise a backed-up console pipeline stalls the
		// read loop and the StartApp response below never arrives. Forward
		// from the moment of attach. The deferred receive keeps Run from
		// closing output while the forwarder may still send to it; it runs
		// after the deferred detach, which is what closes consoleCh.
		forwardDone = make(chan struct{})
		defer func() { <-forwardDone }()
		defer consoleDetach(false)
		go func() {
			defer close(forwardDone)
			for chunk := range consoleCh {
				typ := RunOutputStdout
				if chunk.Stderr {
					typ = RunOutputStderr
				}
				output <- RunOutput{Type: typ, Data: chunk.Data}
			}
		}()
	}

	fmt.Println("Starting app...")
	if err := client.StartApp(); err != nil {
		return fmt.Errorf("app start: %w", err)
	}

	output <- RunOutput{Type: RunOutputStarted}

	if forwardDone == nil {
		return nil
	}
	select {
	case <-forwardDone:
		// Device detached on its own or connection lost.
		return consoleDetach(false)
	case <-ctx.Done():
		// The run was cancelled from our side: the user hit Ctrl-C or the
		// caller gave up on the whole operation. Waiting for a detach ack
		// could hang without deadline exactly when cancellation is most
		// likely (device wedged, network gone), so send the detach without
		// waiting for the ack — giving the device a chance to stop streaming
		// right away — then close the connection so everything else fails
		// fast. If the detach never arrives, the rolling attach lease makes
		// the device stop streaming on its own.
		if err := consoleDetach(true); err != nil {
			fmt.Fprintf(os.Stderr, "warning: console detach: %v\n", err)
		}
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: close: %v\n", err)
		}
		return nil
	}
}

func (p *MicroWendyProvider) Stop(_ context.Context, app *BuiltApp) error {
	return nil
}

func (p *MicroWendyProvider) GetDeviceInfo(ctx context.Context, device models.ExternalDevice) (*ProviderDeviceInfo, error) {
	// An unflashed board has nothing listening on the Wendy Lite protocol, so
	// attempting to connect would only time out. Report the real reason
	// directly: this surfaces as AppRequirementsUnsupportedError, which the
	// `wendy run` install-offer flow already knows how to handle.
	if device.ConnectionInfo["needsInstall"] == "true" {
		return nil, &AppRequirementsUnsupportedError{Device: device, Missing: "Wendy Lite firmware (none installed)"}
	}
	client, err := p.connectClient(device)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	di, err := client.GetDeviceInfo(3 * time.Second)
	if err != nil {
		return nil, err
	}
	return &ProviderDeviceInfo{
		// On wendy-lite, OS version and agent version are the same.
		AgentVersion:     di.OSVersion,
		OS:               di.OS,
		OSVersion:        di.OSVersion,
		CPUArchitecture:  di.CPUArchitecture,
		DeviceType:       di.Board,
		WasmAppSupport:   di.WasmAppSupport,
		NativeAppSupport: di.NativeAppSupport,
	}, nil
}

func (p *MicroWendyProvider) WifiConnect(_ context.Context, device models.ExternalDevice, ssid, password string) error {
	return p.pushWifiConf(device, &litepb.WendyConfWifi{
		Networks: []*litepb.WendyConfWifiNetwork{{Ssid: ssid, Password: password}},
	})
}

func (p *MicroWendyProvider) WifiDisconnect(_ context.Context, device models.ExternalDevice) error {
	return p.pushWifiConf(device, &litepb.WendyConfWifi{})
}

// pushWifiConf stops the app, pushes a conf updating only the wifi root field,
// and reboots the device so the new configuration takes effect.
func (p *MicroWendyProvider) pushWifiConf(device models.ExternalDevice, wifi *litepb.WendyConfWifi) error {
	fmt.Println("Connecting to the device...")
	client, err := p.connectClient(device)
	if err != nil {
		return err
	}
	defer client.Close()

	client.StopApp() // silently ignore errors, the app may not be running

	fmt.Println("Configuring the device...")
	conf := &litepb.WendyConf{Wifi: wifi}
	if err := client.PushConf(conf, liteclient.ConfPushModeUpdate, nil); err != nil {
		return fmt.Errorf("push conf: %w", err)
	}

	fmt.Println("Rebooting the device...")
	if err := client.ResetTargetDevice(true, 0); err != nil {
		return fmt.Errorf("device reset: %w", err)
	}
	return nil
}

// connectClient opens a WendyLiteClient connection to the device over serial,
// LAN or BLE (with mTLS when the device advertises it). The caller must Close
// the client.
func (p *MicroWendyProvider) connectClient(device models.ExternalDevice) (*liteclient.WendyLiteClient, error) {
	client := liteclient.NewWendyLiteClient()
	switch device.ConnectionInfo["type"] {
	case "USB":
		serialPort := device.ConnectionInfo["serialPort"]
		if serialPort == "" {
			return nil, fmt.Errorf("wendy-lite provider: missing serial port in connection info")
		}
		if err := connectSerialWithRetry(client, serialPort); err != nil {
			return nil, fmt.Errorf("connect to device via serial: %w", err)
		}
	case "LAN":
		ip := device.ConnectionInfo["ip"]
		port := device.ConnectionInfo["port"]
		if ip == "" || port == "" {
			return nil, fmt.Errorf("wendy-lite provider: missing IP or port in connection info")
		}
		addr := net.JoinHostPort(ip, port)
		if device.ConnectionInfo["mtls"] == "true" {
			if err := connectWithCLIIdentities(func(cert tls.Certificate, rootCAs x509.CertPool) error {
				return client.ConnectWithMutualAuthentication(addr, cert, rootCAs)
			}); err != nil {
				return nil, err
			}
		} else {
			if err := client.ConnectInsecure(addr); err != nil {
				return nil, fmt.Errorf("connect to device: %w", err)
			}
		}
	case "BLE":
		address := device.ConnectionInfo["address"]
		if address == "" {
			return nil, fmt.Errorf("wendy-lite provider: missing BLE address in connection info")
		}
		// A missing or unparsable PSM is not an error: ConnectViaBLE reads the
		// device's info service itself and falls back to the well-known PSM.
		psm64, _ := strconv.ParseUint(device.ConnectionInfo["psm"], 10, 16)
		psm := uint16(psm64)
		if device.ConnectionInfo["mtls"] == "true" {
			if err := connectWithCLIIdentities(func(cert tls.Certificate, rootCAs x509.CertPool) error {
				return client.ConnectViaBLEWithMutualAuthentication(address, psm, cert, rootCAs)
			}); err != nil {
				return nil, err
			}
		} else {
			if err := client.ConnectViaBLEInsecure(address, psm); err != nil {
				return nil, fmt.Errorf("connect to device over BLE: %w", err)
			}
		}
	default:
		return nil, fmt.Errorf("wendy-lite provider: unsupported connection type: %s", device.ConnectionInfo["type"])
	}
	return client, nil
}

// connectWithCLIIdentities tries connect once per CLI certificate, since the
// user may hold identities from several organizations and only one of them is
// the device's issuer. It reports the failures of every identity it tried:
// with one certificate per organization, "which one was this device enrolled
// with" is exactly what the reader needs to see.
func connectWithCLIIdentities(connect func(cert tls.Certificate, rootCAs x509.CertPool) error) error {
	certInfos, err := loadAllCLICerts()
	if err != nil {
		return fmt.Errorf("wendy-lite provider: loading mTLS certs: %w", err)
	}
	var connectErrs []error
	for _, certInfo := range certInfos {
		keyPEM, err := certInfo.PrivateKeyPEM()
		if err != nil {
			return fmt.Errorf("wendy-lite provider: loading client key: %w", err)
		}
		cert, err := tls.X509KeyPair([]byte(certInfo.PemCertificate), []byte(keyPEM))
		if err != nil {
			return fmt.Errorf("wendy-lite provider: parsing mTLS cert: %w", err)
		}
		rootCAs := x509.NewCertPool()
		certs.AppendChainToPool(rootCAs, certInfo.PemCertificateChain)
		if err := connect(cert, *rootCAs); err != nil {
			connectErrs = append(connectErrs, err)
			continue
		}
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Wendy Lite connection error")
	for i, e := range connectErrs {
		if i == 0 {
			fmt.Fprintf(&b, ": identity %d: %v", i+1, e)
		} else {
			fmt.Fprintf(&b, "; identity %d: %v", i+1, e)
		}
	}
	return errors.New(b.String())
}

// serialConnectMaxAttempts bounds how many times connectSerialWithRetry
// retries a USB serial handshake before giving up. A discovery pass already
// confirmed the port completes the Wendy Lite identity handshake, but a
// single handshake round trip can still drop — the same USB-CDC flakiness
// documented in serial_discovery.go's probeWatchdog (WDY-2319) — so retrying
// a few times absorbs that instead of failing the whole build/run outright.
const serialConnectMaxAttempts = 3

// serialConnectRetryDelay is the base backoff between attempts, scaled
// linearly by attempt number. A var so tests can shrink it.
var serialConnectRetryDelay = 400 * time.Millisecond

// connectSerialFn performs a single ConnectToSerial attempt. Indirected so
// tests can simulate transient and permanent failures without real hardware.
var connectSerialFn = func(client *liteclient.WendyLiteClient, port string) error {
	return client.ConnectToSerial(port)
}

// connectSerialWithRetry opens a Wendy Lite serial connection, retrying on
// failure up to serialConnectMaxAttempts times. ConnectToSerial already
// leaves the client in a clean state after a failed attempt (port closed,
// lock released, link cleared), so it's safe to call again on the same
// client. Prints a short notice to stderr once a retry is actually needed, so
// a transient hiccup doesn't read as a silent hang.
func connectSerialWithRetry(client *liteclient.WendyLiteClient, serialPort string) error {
	var lastErr error
	for attempt := 1; attempt <= serialConnectMaxAttempts; attempt++ {
		if lastErr = connectSerialFn(client, serialPort); lastErr == nil {
			return nil
		}
		if attempt < serialConnectMaxAttempts {
			fmt.Fprintf(os.Stderr, "connecting to %s failed (%v), retrying (%d/%d)...\n", serialPort, lastErr, attempt+1, serialConnectMaxAttempts)
			time.Sleep(serialConnectRetryDelay * time.Duration(attempt))
		}
	}
	return fmt.Errorf(
		"%w after %d attempts — if this keeps happening, make sure no other process (e.g. `idf.py monitor`, `wendy device camera view`) is using %s, or try unplugging and reconnecting the board",
		lastErr, serialConnectMaxAttempts, serialPort,
	)
}

func loadAllCLICerts() ([]config.CertificateInfo, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	var out []config.CertificateInfo
	for _, auth := range cfg.Auth {
		out = append(out, auth.Certificates...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("not logged in (no certificate found)")
	}
	return out, nil
}
