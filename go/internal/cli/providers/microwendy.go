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
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	// The PKI pin target is the device id in the principal an enrolled board
	// advertises, never id=, which older firmware sets to its hardware id.
	var enrolledDeviceID string
	if identity, err := certs.ParseDevicePrincipal(discovery.LANDeviceFromWendyLiteService(svc).PrincipalURI); err == nil {
		enrolledDeviceID = identity.EntityID
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
			"tenantId": svc.TXTRecords["tenant"],
			"orgId":    svc.TXTRecords["orgid"],
			"assetId":  svc.TXTRecords["assetid"],
			// Set only from a principal inside the advertised tenant.
			"enrolledDeviceId": enrolledDeviceID,
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
			// deviceId stays the hardware id over USB even once enrolled; the
			// enrolled UUID is what lets this row meet the board's LAN and BLE
			// rows (see ExternalDevice.LiteDedupID).
			"enrolledDeviceId": dev.EnrolledDeviceID,
		},
		IsWendyDevice: true,
	}
}

// bleExternalDevice maps a Wendy Lite board found over BLE to an
// ExternalDevice. The PSM the board published travels with the row, but a 0 is
// no problem: ConnectViaBLE reads the info service itself and falls back to
// liteclient.DefaultL2CAPPSM.
func (p *MicroWendyProvider) bleExternalDevice(dev discovery.BLELiteDevice) models.ExternalDevice {
	// Like LAN's principal=, the advertised principal only picks the pin; the
	// TLS leaf must still match it. Without one, connectClient asks Cloud.
	var tenant, enrolledDeviceID string
	if identity, err := certs.ParseDevicePrincipal(dev.Info.Principal); err == nil {
		tenant, enrolledDeviceID = identity.TenantUUID, identity.EntityID
	}
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
			"tenantId": tenant,
			// The device UUID from the advertised principal.
			"enrolledDeviceId": enrolledDeviceID,
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
	message := fmt.Sprintf("device %s does not support %s", e.Device.DisplayName, e.Missing)
	if strings.HasPrefix(e.Device.ID, "vm:") {
		profile := "wendy-lite"
		if e.Missing == "native apps" {
			profile = "wendy-lite-native"
		}
		message += fmt.Sprintf("; create a compatible simulator with 'wendy vm create <name> --profile %s', then run with 'wendy run --device vm:<name>'", profile)
	}
	return message
}

// MissingWendyCoreError lets the CLI offer to configure an ESP-IDF project.
// Toolchain is the one that configured the project, so setup uses it too.
type MissingWendyCoreError struct {
	Toolchain *espidftoolchain.ProjectToolchain
}

func (*MissingWendyCoreError) Error() string {
	return "this project does not include the wendy_core component; run 'wendy run' interactively to add it, or add it to main/idf_component.yml and call ESP_ERROR_CHECK(wendy_core_init()) first in app_main()"
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

	target := di.DeviceType
	if strings.ContainsAny(target, " \t\r\n") {
		return nil, fmt.Errorf("invalid device target %q", target)
	}
	toolchain, err := espidftoolchain.LoadProjectToolchain(projectPath, target)
	if err != nil {
		return nil, err
	}
	if err := toolchain.Ensure(ctx); err != nil {
		return nil, err
	}

	configuredTarget := espidftoolchain.ProjectTarget(projectPath)
	if target != "" && target != configuredTarget {
		if err := toolchain.Run(ctx, projectPath, "set-target", target); err != nil {
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
		// Some wendy_core versions do not declare CONFIG_WENDY_CORE. The
		// configured component list still records whether they are linked.
		hasCore, err := espidftoolchain.HasBuildComponent(projectPath, "wendy_core")
		if err != nil {
			return nil, err
		}
		if !hasCore {
			return nil, &MissingWendyCoreError{Toolchain: toolchain}
		}
	}

	// build the project
	// Build only the app binary, not the whole firmware image.
	if err := toolchain.Run(ctx, projectPath, "gen_project_binary"); err != nil {
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

	client, err := p.connectClient(ctx, app.Device)
	if err != nil {
		return err
	}
	// Closure, not a bound method: the native path below replaces client with
	// a post-reboot connection that this defer must close instead.
	defer func() { client.Close() }()

	// For WASM, PushBegin stops execution without unloading the old instance.
	// An explicit StopApp here would prevent firmware from retaining its linear
	// memory mapping when StartApp installs the replacement.

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
		client.WaitReboot(ctx)
		if err := ctx.Err(); err != nil {
			return err
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
			newClient, err := p.connectClient(ctx, app.Device)
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
	client, err := p.connectClient(ctx, device)
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
		DeviceType:       di.Target,
		WasmAppSupport:   di.WasmAppSupport,
		NativeAppSupport: di.NativeAppSupport,
	}, nil
}

func (p *MicroWendyProvider) WifiConnect(ctx context.Context, device models.ExternalDevice, ssid, password string) error {
	return p.pushWifiConf(ctx, device, &litepb.WendyConfWifi{
		Networks: []*litepb.WendyConfWifiNetwork{{Ssid: ssid, Password: password}},
	})
}

func (p *MicroWendyProvider) WifiDisconnect(ctx context.Context, device models.ExternalDevice) error {
	return p.pushWifiConf(ctx, device, &litepb.WendyConfWifi{})
}

// pushWifiConf stops the app, pushes a conf updating only the wifi root field,
// and reboots the device so the new configuration takes effect.
func (p *MicroWendyProvider) pushWifiConf(ctx context.Context, device models.ExternalDevice, wifi *litepb.WendyConfWifi) error {
	fmt.Println("Connecting to the device...")
	client, err := p.connectClient(ctx, device)
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
	client.WaitReboot(ctx)
	return nil
}

// ConnectSensorLink opens the provider's normal authenticated connection for
// SensorLink commands. The caller must close the client.
func (p *MicroWendyProvider) ConnectSensorLink(device models.ExternalDevice) (*liteclient.WendyLiteClient, error) {
	return p.connectClient(context.Background(), device)
}

// ConnectEnrollment opens a physical USB serial connection for enrollment and
// status checks. Enrollment credentials and trust bundles must stay off network
// transports, even if a peer claims to support enrollment.
// The caller must close the client.
func (p *MicroWendyProvider) ConnectEnrollment(device models.ExternalDevice) (*liteclient.WendyLiteClient, error) {
	if device.ConnectionType() != "USB" {
		return nil, fmt.Errorf("Wendy Lite enrollment requires a USB connection; select the board's USB device")
	}
	return p.connectClient(context.Background(), device)
}

// CloudRelayDialer opens the Wendy Cloud connection that a "Cloud" device's
// relay runs over, already set up with the user's Cloud credentials, and the
// context whose call metadata carries them. The commands package owns Cloud
// sessions and sets it; this package cannot import commands.
// renew asks for a fresh Lite operator leaf instead of the stored one.
var CloudRelayDialer func(ctx context.Context, device models.ExternalDevice, renew bool) (*grpc.ClientConn, context.Context, liteclient.RelayOptions, error)

// LiteIdentityResolver refreshes a session's primary identity for direct
// LAN/BLE connections and returns device trust separately from the client chain.
// The commands package owns credential persistence and installs this hook.
var LiteIdentityResolver func(ctx context.Context, auth *config.AuthConfig) (config.CertificateInfo, string, error)

// LiteBoardIdentityResolver maps the hardware id a Lite board advertises to the
// tenant and device UUID of its single enrollment in the signed-in Wendy Cloud
// organization. The commands package installs it.
var LiteBoardIdentityResolver func(ctx context.Context, hardwareID string) (tenant, deviceID string, err error)

// LiteDeviceCAChainResolver returns the session tenant's Lite Device CA chain
// loader, so LAN and BLE verify Lite-issued boards like the Cloud relay does.
// The commands package installs it; nil leaves only the session's trust.
var LiteDeviceCAChainResolver func(auth *config.AuthConfig) liteclient.LiteDeviceCAChain

// connectClient opens a WendyLiteClient connection to the device over serial,
// LAN or BLE (with mTLS when the device advertises it), or through the Wendy
// Cloud relay. The caller must Close the client.
func (p *MicroWendyProvider) connectClient(ctx context.Context, device models.ExternalDevice) (*liteclient.WendyLiteClient, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client := liteclient.NewWendyLiteClient()
	switch device.ConnectionInfo["type"] {
	case "Mesh":
		if err := connectMeshLite(ctx, client, device); err != nil {
			return nil, fmt.Errorf("connect to mesh Wendy Lite: %w", err)
		}
	case "USB":
		serialPort := device.ConnectionInfo["serialPort"]
		if serialPort == "" {
			return nil, fmt.Errorf("wendy-lite provider: missing serial port in connection info")
		}
		if err := connectSerialWithRetry(ctx, client, serialPort); err != nil {
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
			legacy, err := legacyLiteIdentity(device)
			if err != nil {
				return nil, err
			}
			if legacy == nil && device.ConnectionInfo["enrolledDeviceId"] == "" {
				return nil, fmt.Errorf("this board does not advertise its enrolled identity (mDNS principal=); update its firmware, or connect over USB or Wendy Cloud")
			}
			if legacy != nil {
				if err := connectWithLegacyCLIIdentities(ctx, legacy.orgID, func(cert tls.Certificate, chain string, _ liteclient.LiteDeviceCAChain) error {
					return client.ConnectWithLegacyAuthenticationContext(ctx, addr, cert, chain, legacy.orgID, legacy.assetID)
				}); err != nil {
					return nil, err
				}
				return client, nil
			}
			if err := connectWithCLIIdentities(ctx, device.ConnectionInfo["tenantId"], func(cert tls.Certificate, chain string, lite liteclient.LiteDeviceCAChain) error {
				if tenant := device.ConnectionInfo["tenantId"]; tenant != "" {
					leaf, err := x509.ParseCertificate(cert.Certificate[0])
					if err != nil {
						return err
					}
					identity, ok, err := certs.IdentityFromCert(leaf)
					if err != nil {
						return err
					}
					if !ok || identity.TenantUUID != tenant {
						return fmt.Errorf("client identity does not match advertised PKI tenant; sign in to the board's organization with wendy auth login --email <email>")
					}
				}
				return client.ConnectWithPKIAuthenticationContext(ctx, addr, cert, chain, device.ConnectionInfo["enrolledDeviceId"], lite)
			}); err != nil {
				return nil, err
			}
		} else {
			if err := client.ConnectInsecureContext(ctx, addr); err != nil {
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
			// Firmware without the principal characteristic advertises only its
			// hardware id, so Cloud maps it to the enrolled device UUID. Either way
			// the pin is checked against the board's TLS leaf.
			tenant, deviceID := device.ConnectionInfo["tenantId"], device.ConnectionInfo["enrolledDeviceId"]
			if deviceID == "" {
				if LiteBoardIdentityResolver == nil {
					return nil, fmt.Errorf("wendy-lite provider: Wendy Cloud board lookup is not available")
				}
				var err error
				if tenant, deviceID, err = LiteBoardIdentityResolver(ctx, device.ConnectionInfo["deviceId"]); err != nil {
					return nil, err
				}
			}
			if err := connectWithCLIIdentities(ctx, tenant, func(cert tls.Certificate, chain string, lite liteclient.LiteDeviceCAChain) error {
				return client.ConnectViaBLEWithPKIAuthenticationContext(ctx, address, psm, cert, chain, deviceID, lite)
			}); err != nil {
				return nil, err
			}
		} else {
			if err := client.ConnectViaBLEContext(ctx, address, psm, nil); err != nil {
				return nil, fmt.Errorf("connect to device over BLE: %w", err)
			}
		}
	case "Cloud":
		assetID := device.ConnectionInfo["assetId"]
		if assetID == "" {
			return nil, fmt.Errorf("wendy-lite provider: missing Cloud asset ID in connection info")
		}
		if CloudRelayDialer == nil {
			return nil, fmt.Errorf("wendy-lite provider: Wendy Cloud connections are not available")
		}
		connect := func(renew bool) (err error) {
			cc, ctx, options, err := CloudRelayDialer(ctx, device, renew)
			if err != nil {
				return err
			}
			defer func() {
				if err != nil {
					cc.Close()
				}
			}()
			if device.ConnectionInfo["tenantId"] == "" || device.ConnectionInfo["deviceId"] == "" {
				return fmt.Errorf("wendy-lite provider: missing Cloud tenant or device identity")
			}
			// The one-shot relay grant binds both the operator principal and
			// its key. Use the signer's identity, never another stored login.
			selected := options.OperatorIdentity
			if selected.PrincipalURI == "" {
				return fmt.Errorf("missing selected Cloud operator identity")
			}
			key, err := selected.PrivateKeyPEM()
			if err != nil {
				return err
			}
			cert, err := certs.TLSKeyPair(selected.PemCertificate, selected.PemCertificateChain, key)
			if err != nil {
				return err
			}
			leaf, err := x509.ParseCertificate(cert.Certificate[0])
			if err != nil {
				return err
			}
			identity, ok, err := certs.IdentityFromCert(leaf)
			if err != nil {
				return err
			}
			if !ok || identity.TenantUUID != device.ConnectionInfo["tenantId"] {
				return fmt.Errorf("operator identity does not match selected device tenant")
			}
			if identity.Principal != selected.PrincipalURI {
				return fmt.Errorf("operator certificate does not match selected Cloud principal")
			}
			return client.ConnectViaRelay(ctx, cc, assetID, cert, options.DeviceTrustChainPEM, device.ConnectionInfo["deviceId"], options)
		}
		err := connect(false)
		// Cloud refuses a Lite leaf it no longer accepts (revoked, or not
		// pki-core's Lite profile) before attestation: request a fresh one once.
		if s := status.Convert(err); s.Code() == codes.PermissionDenied && s.Message() == "invalid Lite principal request" {
			err = connect(true)
		}
		if err != nil {
			return nil, fmt.Errorf("connect to device via Wendy Cloud: %w", err)
		}

	default:
		return nil, fmt.Errorf("wendy-lite provider: unsupported connection type: %s", device.ConnectionInfo["type"])
	}
	return client, nil
}

type liteLegacyIdentity struct {
	orgID   int32
	assetID int32
}

// Legacy IDs select a trust path; TLS must still authenticate the exact asset.
// An advertised tenant always selects PKI, even if legacy hints are also present.
func legacyLiteIdentity(device models.ExternalDevice) (*liteLegacyIdentity, error) {
	info := device.ConnectionInfo
	if info["tenantId"] != "" || (info["orgId"] == "" && info["assetId"] == "") {
		return nil, nil
	}
	org, orgErr := strconv.ParseInt(info["orgId"], 10, 32)
	asset, assetErr := strconv.ParseInt(info["assetId"], 10, 32)
	if orgErr != nil || assetErr != nil || org <= 0 || asset <= 0 {
		return nil, fmt.Errorf("legacy Wendy Lite device must advertise a valid organization and asset ID")
	}
	return &liteLegacyIdentity{orgID: int32(org), assetID: int32(asset)}, nil
}

// connectWithCLIIdentities tries eligible operator sessions until one connects.
// The resolver retains the tenant credential and handles certificate renewal;
// tenant sessions also get the Lite Device CA chain loader the relay uses.
func connectWithCLIIdentities(ctx context.Context, advertisedTenant string, connect func(cert tls.Certificate, chain string, lite liteclient.LiteDeviceCAChain) error) error {
	return connectWithScopedCLIIdentities(ctx, advertisedTenant, 0, connect)
}

func connectWithLegacyCLIIdentities(ctx context.Context, orgID int32, connect func(cert tls.Certificate, chain string, lite liteclient.LiteDeviceCAChain) error) error {
	if orgID <= 0 {
		return fmt.Errorf("legacy Wendy Lite connection requires an organization ID")
	}
	return connectWithScopedCLIIdentities(ctx, "", orgID, connect)
}

func connectWithScopedCLIIdentities(ctx context.Context, advertisedTenant string, legacyOrg int32, connect func(cert tls.Certificate, chain string, lite liteclient.LiteDeviceCAChain) error) error {
	return connectWithScopedCLIPrincipal(ctx, advertisedTenant, legacyOrg, "", connect)
}

// A mesh dial uses the selected primary context, rather than trying unrelated
// saved logins in the same tenant as physical discovery historically permits.
func connectWithScopedCLIPrincipal(ctx context.Context, advertisedTenant string, legacyOrg int32, primary string, connect func(cert tls.Certificate, chain string, lite liteclient.LiteDeviceCAChain) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if LiteIdentityResolver == nil {
		return fmt.Errorf("wendy-lite provider: Lite operator identity resolver is not available")
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("wendy-lite provider: loading operator sessions: %w", err)
	}
	if primary != "" {
		auth, err := config.ResolveAuth(cfg, "", nil)
		if err != nil {
			return err
		}
		cfg.Auth = []config.AuthConfig{*auth}
	}
	var connectErrs []error
	for i := range cfg.Auth {
		if err := ctx.Err(); err != nil {
			return err
		}
		auth := &cfg.Auth[i]
		if len(auth.Certificates) == 0 {
			continue
		}
		leaves, err := certs.ParseCertsFromPEM([]byte(auth.Certificates[0].PemCertificate))
		if err != nil || len(leaves) != 1 {
			continue
		}
		identity, hasIdentity, err := certs.IdentityFromCert(leaves[0])
		if err != nil || !hasIdentity {
			continue
		}
		if legacyOrg > 0 {
			if identity.TenantUUID != "" || identity.OrgID != legacyOrg || identity.EntityType != certs.EntityUser {
				continue
			}
		} else if identity.TenantUUID == "" || !strings.HasPrefix(identity.Principal, "spiffe://wendy.sh/tenant/"+identity.TenantUUID+"/operator/") {
			continue
		}
		if primary != "" && identity.Principal != primary {
			continue
		}
		// Discovery is only a selection hint. The TLS verifier still checks the
		// device's authenticated tenant and identity after this filtering.
		if advertisedTenant != "" && identity.TenantUUID != advertisedTenant {
			continue
		}
		name := auth.Name
		if name == "" {
			name = auth.CloudGRPC
		}
		certInfo, deviceTrust, err := LiteIdentityResolver(ctx, auth)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			connectErrs = append(connectErrs, fmt.Errorf("context %q: %w", name, err))
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		keyPEM, err := certInfo.PrivateKeyPEM()
		if err != nil {
			connectErrs = append(connectErrs, fmt.Errorf("context %q: loading Lite client key: %w", name, err))
			continue
		}
		cert, err := certs.TLSKeyPair(certInfo.PemCertificate, certInfo.PemCertificateChain, keyPEM)
		if err != nil {
			connectErrs = append(connectErrs, fmt.Errorf("context %q: parsing Lite mTLS certificate: %w", name, err))
			continue
		}
		var lite liteclient.LiteDeviceCAChain
		if legacyOrg == 0 && LiteDeviceCAChainResolver != nil {
			lite = LiteDeviceCAChainResolver(auth)
		}
		if err := connect(cert, deviceTrust, lite); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			connectErrs = append(connectErrs, fmt.Errorf("context %q: %w", name, err))
			continue
		}
		return nil
	}

	if len(connectErrs) == 0 {
		if legacyOrg > 0 {
			return fmt.Errorf("no legacy Cloud user login matches organization %s; run 'wendy auth login' for that organization", config.OrganizationLabel("", strconv.FormatInt(int64(legacyOrg), 10)))
		}
		if advertisedTenant != "" {
			return fmt.Errorf("no operator login matches the device's advertised tenant; sign in to its organization with 'wendy auth login --email <email>'")
		}
		return fmt.Errorf("Wendy Lite requires a tenant-scoped operator login; run 'wendy auth login'")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Wendy Lite connection error")
	for i, e := range connectErrs {
		if i == 0 {
			fmt.Fprintf(&b, ": %v", e)
		} else {
			fmt.Fprintf(&b, "; %v", e)
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
var connectSerialFn = func(ctx context.Context, client *liteclient.WendyLiteClient, port string) error {
	return client.ConnectToSerialContext(ctx, port)
}

// connectSerialWithRetry opens a Wendy Lite serial connection, retrying on
// failure up to serialConnectMaxAttempts times. ConnectToSerial already
// leaves the client in a clean state after a failed attempt (port closed,
// lock released, link cleared), so it's safe to call again on the same
// client. Prints a short notice to stderr once a retry is actually needed, so
// a transient hiccup doesn't read as a silent hang.
func connectSerialWithRetry(ctx context.Context, client *liteclient.WendyLiteClient, serialPort string) error {
	var lastErr error
	for attempt := 1; attempt <= serialConnectMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if lastErr = connectSerialFn(ctx, client, serialPort); lastErr == nil {
			return nil
		}
		if attempt < serialConnectMaxAttempts {
			fmt.Fprintf(os.Stderr, "connecting to %s failed (%v), retrying (%d/%d)...\n", serialPort, lastErr, attempt+1, serialConnectMaxAttempts)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(serialConnectRetryDelay * time.Duration(attempt)):
			}
		}
	}
	return fmt.Errorf(
		"%w after %d attempts — if this keeps happening, make sure no other process (e.g. `idf.py monitor`, `wendy device camera view`) is using %s, or try unplugging and reconnecting the board",
		lastErr, serialConnectMaxAttempts, serialPort,
	)
}
