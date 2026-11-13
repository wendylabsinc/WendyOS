package discovery

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
)

// newTestSerialDiscovery returns a fresh, unshared SerialDiscovery so tests
// don't race on the package singleton returned by GetSerialDiscovery.
func newTestSerialDiscovery() *SerialDiscovery {
	return &SerialDiscovery{
		probing:         make(map[string]bool),
		probeSem:        make(chan struct{}, 4),
		listeners:       make(map[ListenerID]func([]SerialDevice)),
		contended:       make(map[string]bool),
		watchdogStrikes: make(map[string]int),
		wedgedUntil:     make(map[string]time.Time),
	}
}

// waitForDevices polls d.Devices() until want(devices) reports true or the
// timeout elapses, returning the last observed snapshot.
func waitForDevices(t *testing.T, d *SerialDiscovery, timeout time.Duration, want func([]SerialDevice) bool) []SerialDevice {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []SerialDevice
	for time.Now().Before(deadline) {
		last = d.Devices()
		if want(last) {
			return last
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for expected device state; last snapshot: %+v", last)
	return last
}

func byPort(devices []SerialDevice) map[string]SerialDevice {
	m := make(map[string]SerialDevice, len(devices))
	for _, d := range devices {
		m[d.Port] = d
	}
	return m
}

func TestSerialDiscoveryAddsUnresponsivePort(t *testing.T) {
	origResolve, origProbe := resolvePortsFn, probeIdentityFn
	defer func() { resolvePortsFn, probeIdentityFn = origResolve, origProbe }()

	resolvePortsFn = func() ([]SerialPortInfo, error) {
		return []SerialPortInfo{{Port: "/dev/blank"}, {Port: "/dev/responsive"}}, nil
	}
	probeIdentityFn = func(port string) (*liteclient.DeviceIdentity, bool, error) {
		if port == "/dev/responsive" {
			return &liteclient.DeviceIdentity{ID: "esp", Name: "esp", DisplayName: "esp", EnrolledDeviceID: "enrolled-esp"}, true, nil
		}
		return nil, true, errProbeTimeout
	}

	d := newTestSerialDiscovery()
	d.StartScan(0)

	devices := waitForDevices(t, d, 2*time.Second, func(devices []SerialDevice) bool {
		return len(devices) == 2
	})

	m := byPort(devices)
	blank, ok := m["/dev/blank"]
	if !ok {
		t.Fatalf("expected /dev/blank in devices, got %+v", devices)
	}
	if blank.Responsive {
		t.Errorf("expected /dev/blank to be unresponsive, got Responsive=true")
	}
	if blank.ID != "" || blank.Name != "" || blank.DisplayName != "" || blank.EnrolledDeviceID != "" {
		t.Errorf("expected empty identity fields for an unresponsive port, got %+v", blank)
	}

	responsive, ok := m["/dev/responsive"]
	if !ok {
		t.Fatalf("expected /dev/responsive in devices, got %+v", devices)
	}
	if !responsive.Responsive {
		t.Errorf("expected /dev/responsive to be responsive")
	}
	if responsive.ID != "esp" || responsive.DisplayName != "esp" || responsive.EnrolledDeviceID != "enrolled-esp" {
		t.Errorf("expected identity to be populated for a responsive port, got %+v", responsive)
	}
}

func TestSerialDiscoveryReprobesUnresponsivePort(t *testing.T) {
	origResolve, origProbe := resolvePortsFn, probeIdentityFn
	defer func() { resolvePortsFn, probeIdentityFn = origResolve, origProbe }()

	resolvePortsFn = func() ([]SerialPortInfo, error) {
		return []SerialPortInfo{{Port: "/dev/blank"}}, nil
	}

	var mu sync.Mutex
	responds := false
	probeIdentityFn = func(port string) (*liteclient.DeviceIdentity, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if responds {
			return &liteclient.DeviceIdentity{ID: "esp", Name: "esp", DisplayName: "esp"}, true, nil
		}
		return nil, true, errProbeTimeout
	}

	d := newTestSerialDiscovery()
	d.StartScan(0)
	waitForDevices(t, d, 2*time.Second, func(devices []SerialDevice) bool {
		return len(devices) == 1 && !devices[0].Responsive
	})

	// Simulate Wendy Lite firmware landing on the board mid-session, then
	// rescan (as the continuous picker discovery loop would on its next cycle).
	mu.Lock()
	responds = true
	mu.Unlock()
	d.StartScan(0)

	devices := waitForDevices(t, d, 2*time.Second, func(devices []SerialDevice) bool {
		return len(devices) == 1 && devices[0].Responsive
	})
	if devices[0].DisplayName != "esp" {
		t.Errorf("expected the upgraded entry to carry the resolved identity, got %+v", devices[0])
	}
}

func TestSerialDiscoveryDoesNotReprobeResponsivePort(t *testing.T) {
	origResolve, origProbe := resolvePortsFn, probeIdentityFn
	defer func() { resolvePortsFn, probeIdentityFn = origResolve, origProbe }()

	resolvePortsFn = func() ([]SerialPortInfo, error) {
		return []SerialPortInfo{{Port: "/dev/responsive"}}, nil
	}

	var mu sync.Mutex
	calls := 0
	probeIdentityFn = func(port string) (*liteclient.DeviceIdentity, bool, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return &liteclient.DeviceIdentity{ID: "esp", Name: "esp", DisplayName: "esp"}, true, nil
	}

	d := newTestSerialDiscovery()
	d.StartScan(0)
	waitForDevices(t, d, 2*time.Second, func(devices []SerialDevice) bool {
		return len(devices) == 1 && devices[0].Responsive
	})

	d.StartScan(0)
	// Wait for the second pass to fully resolve, then confirm it skipped the
	// probe. A fixed sleep here previously raced the scan goroutine's own
	// completion under -race (reading resolvePortsFn after this test's
	// deferred restore had already run it back to the original).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d.WaitForIdle(ctx)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Errorf("expected exactly 1 probe of an already-responsive port, got %d", got)
	}
}

func TestSerialDiscoveryRemovesUnpluggedPorts(t *testing.T) {
	origResolve, origProbe := resolvePortsFn, probeIdentityFn
	defer func() { resolvePortsFn, probeIdentityFn = origResolve, origProbe }()

	var mu sync.Mutex
	ports := []SerialPortInfo{{Port: "/dev/blank"}, {Port: "/dev/responsive"}}
	resolvePortsFn = func() ([]SerialPortInfo, error) {
		mu.Lock()
		defer mu.Unlock()
		out := make([]SerialPortInfo, len(ports))
		copy(out, ports)
		return out, nil
	}
	probeIdentityFn = func(port string) (*liteclient.DeviceIdentity, bool, error) {
		if port == "/dev/responsive" {
			return &liteclient.DeviceIdentity{ID: "esp", Name: "esp", DisplayName: "esp"}, true, nil
		}
		return nil, true, errProbeTimeout
	}

	d := newTestSerialDiscovery()
	d.StartScan(0)
	waitForDevices(t, d, 2*time.Second, func(devices []SerialDevice) bool {
		return len(devices) == 2
	})

	// Unplug the blank board.
	mu.Lock()
	ports = []SerialPortInfo{{Port: "/dev/responsive"}}
	mu.Unlock()
	d.StartScan(0)

	devices := waitForDevices(t, d, 2*time.Second, func(devices []SerialDevice) bool {
		return len(devices) == 1
	})
	if devices[0].Port != "/dev/responsive" {
		t.Errorf("expected only /dev/responsive to remain, got %+v", devices)
	}
}

// TestSerialDiscoverySkipsContendedPort locks in the fix for a false
// "unflashed" report: a port that a fully-flashed, working board occupies can
// still fail to open (e.g. "resource busy" — a concurrent probe from another
// wendy process, or a subprocess that inherited the fd) with no bearing on
// whether firmware is installed. That must not be recorded as unresponsive.
func TestSerialDiscoverySkipsContendedPort(t *testing.T) {
	origResolve, origProbe := resolvePortsFn, probeIdentityFn
	defer func() { resolvePortsFn, probeIdentityFn = origResolve, origProbe }()

	resolvePortsFn = func() ([]SerialPortInfo, error) {
		return []SerialPortInfo{{Port: "/dev/contended"}}, nil
	}
	probeIdentityFn = func(port string) (*liteclient.DeviceIdentity, bool, error) {
		return nil, false, errPortBusy
	}

	d := newTestSerialDiscovery()
	d.StartScan(0)

	// Give the probe goroutine time to run and (not) record anything.
	time.Sleep(100 * time.Millisecond)

	if devices := d.Devices(); len(devices) != 0 {
		t.Errorf("expected a port that failed to open to be left out entirely, got %+v", devices)
	}
}

// TestSerialDiscoveryWaitForIdleWaitsForInFlightProbe locks in the fix for
// WDY-2319: a caller that starts a scan and immediately reads Devices() races
// the in-flight probe and can observe an empty list even though a real board
// is connected and about to be confirmed responsive — exactly what
// MicroWendyProvider.DiscoverDevices did, surfacing as "no Wendy Lite devices
// found" for a genuinely connected board. WaitForIdle must block until the
// scan pass StartScan just kicked off has actually finished.
func TestSerialDiscoveryWaitForIdleWaitsForInFlightProbe(t *testing.T) {
	origResolve, origProbe := resolvePortsFn, probeIdentityFn
	defer func() { resolvePortsFn, probeIdentityFn = origResolve, origProbe }()

	resolvePortsFn = func() ([]SerialPortInfo, error) {
		return []SerialPortInfo{{Port: "/dev/slow"}}, nil
	}
	probeIdentityFn = func(port string) (*liteclient.DeviceIdentity, bool, error) {
		time.Sleep(150 * time.Millisecond)
		return &liteclient.DeviceIdentity{ID: "esp", Name: "esp", DisplayName: "esp"}, true, nil
	}

	d := newTestSerialDiscovery()
	d.StartScan(0)

	// The probe takes 150ms; reading immediately after StartScan (which
	// returns right away) must race it and see nothing yet.
	if devices := d.Devices(); len(devices) != 0 {
		t.Fatalf("expected an immediate read to race the in-flight probe and see no devices yet, got %+v", devices)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	devices := d.WaitForIdle(ctx)

	if len(devices) != 1 {
		t.Fatalf("WaitForIdle() = %+v, want exactly 1 device", devices)
	}
	if !devices[0].Responsive || devices[0].DisplayName != "esp" {
		t.Errorf("WaitForIdle() returned before the probe resolved: %+v", devices[0])
	}
}

// TestSerialDiscoveryReportsContendedPort locks in the other half of the
// WDY-2319 fix: a port left out of Devices() entirely (see
// TestSerialDiscoverySkipsContendedPort) must still be observable somewhere,
// or a caller that sees zero devices can't tell "nothing plugged in" apart
// from "a board is plugged in but something else has the port open" —
// exactly what silently produced "no Wendy Lite devices found" for a
// genuinely connected board.
func TestSerialDiscoveryReportsContendedPort(t *testing.T) {
	origResolve, origProbe := resolvePortsFn, probeIdentityFn
	defer func() { resolvePortsFn, probeIdentityFn = origResolve, origProbe }()

	resolvePortsFn = func() ([]SerialPortInfo, error) {
		return []SerialPortInfo{{Port: "/dev/contended"}}, nil
	}
	probeIdentityFn = func(port string) (*liteclient.DeviceIdentity, bool, error) {
		return nil, false, errPortBusy
	}

	d := newTestSerialDiscovery()
	d.StartScan(0)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d.WaitForIdle(ctx)

	got := d.ContendedPorts()
	if len(got) != 1 || got[0] != "/dev/contended" {
		t.Fatalf("ContendedPorts() = %v, want [/dev/contended]", got)
	}
}

// TestSerialDiscoveryClearsContendedOncePortOpens confirms contended state
// isn't sticky: once whatever was holding the port lets go and a probe
// actually opens it, the port must stop being reported as contended.
func TestSerialDiscoveryClearsContendedOncePortOpens(t *testing.T) {
	origResolve, origProbe := resolvePortsFn, probeIdentityFn
	defer func() { resolvePortsFn, probeIdentityFn = origResolve, origProbe }()

	resolvePortsFn = func() ([]SerialPortInfo, error) {
		return []SerialPortInfo{{Port: "/dev/wasbusy"}}, nil
	}

	var mu sync.Mutex
	busy := true
	probeIdentityFn = func(port string) (*liteclient.DeviceIdentity, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if busy {
			return nil, false, errPortBusy
		}
		return &liteclient.DeviceIdentity{ID: "esp", Name: "esp", DisplayName: "esp"}, true, nil
	}

	d := newTestSerialDiscovery()
	d.StartScan(0)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d.WaitForIdle(ctx)

	if got := d.ContendedPorts(); len(got) != 1 {
		t.Fatalf("expected the port to start out contended, got %v", got)
	}

	mu.Lock()
	busy = false
	mu.Unlock()
	d.StartScan(0)

	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	d.WaitForIdle(ctx2)

	if got := d.ContendedPorts(); len(got) != 0 {
		t.Errorf("expected contended state to clear once the port opened, got %v", got)
	}
}

// TestSerialDiscoveryDoesNotHangOnStuckProbe locks in the fix for WDY-2319's
// actual root cause: on some hardware/OS combinations (observed: a real
// ESP32-S3 "USB JTAG/serial debug unit" running non-Wendy-Lite firmware,
// probed from macOS) the go.bug.st/serial blocking read syscall inside
// probeIdentityFn can enter a genuine kernel wait that is uninterruptible even
// by SIGKILL and never returns at all -- ConnectToSerial's declared 3s
// handshake budget and GetDeviceIdentity's declared 3s timeout are Go-level
// deadlines that can never even run, because the goroutine never gets back
// from the syscall to check them. A single such port must not prevent the
// scan pass from ever finishing -- for that port or any other -- which is
// what silently dropped a genuinely connected board from DiscoverDevices
// instead of surfacing it as unresponsive/unflashed.
func TestSerialDiscoveryDoesNotHangOnStuckProbe(t *testing.T) {
	origResolve, origProbe, origWatchdog := resolvePortsFn, probeIdentityFn, probeWatchdog
	defer func() { resolvePortsFn, probeIdentityFn, probeWatchdog = origResolve, origProbe, origWatchdog }()

	probeWatchdog = 50 * time.Millisecond

	resolvePortsFn = func() ([]SerialPortInfo, error) {
		return []SerialPortInfo{{Port: "/dev/stuck"}}, nil
	}
	probeIdentityFn = func(port string) (*liteclient.DeviceIdentity, bool, error) {
		select {} // never returns: simulates the uninterruptible kernel hang
	}

	d := newTestSerialDiscovery()
	d.StartScan(0)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	devices := d.WaitForIdle(ctx)

	if len(devices) != 1 || devices[0].Port != "/dev/stuck" || devices[0].Responsive {
		t.Fatalf("expected /dev/stuck to surface as unresponsive despite the stuck probe, got %+v", devices)
	}
}

// TestSerialDiscoveryBacksOffAfterRepeatedWatchdogTimeouts locks in the
// follow-up to TestSerialDiscoveryDoesNotHangOnStuckProbe: each watchdog
// timeout abandons a permanently-stuck goroutine (blocked forever in an
// uninterruptible syscall — see probeWatchdog's doc comment), so a port that
// keeps timing out must stop being re-probed every scan cycle, or a
// long-running session leaks one such thread every cycle indefinitely. After
// maxWatchdogStrikes consecutive timeouts, StartScan must back off from that
// port until watchdogCooldown has passed.
func TestSerialDiscoveryBacksOffAfterRepeatedWatchdogTimeouts(t *testing.T) {
	origResolve, origProbe, origWatchdog, origCooldown := resolvePortsFn, probeIdentityFn, probeWatchdog, watchdogCooldown
	defer func() {
		resolvePortsFn, probeIdentityFn, probeWatchdog, watchdogCooldown = origResolve, origProbe, origWatchdog, origCooldown
	}()

	probeWatchdog = 10 * time.Millisecond
	// Long enough that a 3rd probe landing within this test's window can only
	// mean the cooldown was NOT applied, not that it merely expired early.
	watchdogCooldown = time.Hour

	resolvePortsFn = func() ([]SerialPortInfo, error) {
		return []SerialPortInfo{{Port: "/dev/wedged"}}, nil
	}

	var mu sync.Mutex
	calls := 0
	probeIdentityFn = func(port string) (*liteclient.DeviceIdentity, bool, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		select {} // never returns: simulates the uninterruptible kernel hang
	}

	d := newTestSerialDiscovery()
	for i := 0; i < 3; i++ {
		d.StartScan(0)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		d.WaitForIdle(ctx)
		cancel()
	}

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != maxWatchdogStrikes {
		t.Errorf("expected exactly %d probes before backing off (maxWatchdogStrikes), got %d", maxWatchdogStrikes, got)
	}
}

// errProbeTimeout stands in for the real serial timeout/connection error a
// blank board's failed handshake returns.
var errProbeTimeout = &probeError{"identity probe timed out"}

// errPortBusy stands in for the real "resource busy" error macOS/Linux return
// when another process already has the serial port open exclusively.
var errPortBusy = &probeError{"open serial: resource busy"}

type probeError struct{ msg string }

func (e *probeError) Error() string { return e.msg }
