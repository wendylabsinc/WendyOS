package discovery

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
)

// SerialDevice is an ESP32 board reachable over a serial port.
type SerialDevice struct {
	Port        string
	ID          string
	Name        string
	DisplayName string
	// EnrolledDeviceID is the enrolled device UUID the board reported; empty
	// when it is unenrolled or its firmware predates the field.
	EnrolledDeviceID string
	// Responsive is false when Port matched the ESP32 USB VID/PID but never
	// completed a Wendy Lite identity handshake — a board with no (or
	// incompatible) Wendy Lite firmware installed. ID/Name/DisplayName are
	// empty in that case.
	Responsive bool
}

// resolvePortsFn resolves the VID/PID-matching ESP32 serial ports. Indirected
// so tests can inject fake ports without real hardware.
var resolvePortsFn = ResolveESP32SerialPorts

// probeIdentityFn attempts a Wendy Lite identity handshake on port. opened
// reports whether the serial port itself was successfully opened; false means
// something else currently holds it (a concurrent probe, another process
// still attached to the port) or it is otherwise inaccessible right now — a
// transient condition that must NOT be read as "no firmware installed", only
// a genuine handshake failure on an opened port means that. Indirected so
// tests can simulate each outcome without real hardware.
var probeIdentityFn = func(port string) (identity *liteclient.DeviceIdentity, opened bool, err error) {
	client := liteclient.NewWendyLiteClient()
	if err := client.ConnectToSerial(port); err != nil {
		// ConnectToSerial also performs the WendyCom handshake. A handshake
		// failure means the port did open and should surface as an unresponsive
		// (potentially unflashed) ESP32; only a pre-open failure is contention.
		return nil, !errors.Is(err, liteclient.ErrSerialPortUnavailable), err
	}
	defer client.Close()
	identity, err = client.GetDeviceIdentity(3 * time.Second)
	return identity, true, err
}

// probeWatchdog bounds how long the scan loop waits on a single port's
// probeIdentityFn call before giving up on it and reporting the port as
// unresponsive anyway (WDY-2319). probeIdentityFn's own internal budgets
// (~3s for the serial handshake, up to 3s more for the identity round-trip)
// should keep it well under this in the overwhelming majority of cases —
// but on some hardware/OS combinations the underlying go.bug.st/serial
// blocking read syscall can enter a genuine kernel wait that is
// uninterruptible even by SIGKILL and never returns at all: observed with a
// real ESP32-S3 "USB JTAG/serial debug unit" running non-Wendy-Lite firmware,
// probed from macOS. ConnectToSerial's declared 3s handshake deadline and
// GetDeviceIdentity's declared 3s timeout are Go-level deadlines that never
// even get checked in that case, because the goroutine never gets back from
// the syscall to check them — no context, additional timeout, or signal can
// preempt it from user space.
//
// The watchdog therefore doesn't attempt to cancel probeIdentityFn — it just
// stops waiting on it. The call itself is abandoned (its goroutine, and
// whatever it's still blocked in, leaks for the life of the process; if it
// ever does return, its result is silently discarded), which is the
// unavoidable cost of not hanging the entire scan pass over one wedged port.
//
// Comfortably above the ~6s legitimate worst case (3s handshake + 3s
// identity) while leaving real margin under DiscoverDevices' 8s
// serialIdleTimeout, so the fix is observable without that outer timeout
// papering over it.
var probeWatchdog = 7 * time.Second

// errProbeWatchdogTimeout marks a probeWithWatchdog error as coming from the
// watchdog itself rather than probeIdentityFn — StartScan checks this via
// errors.Is to track consecutive timeouts per port separately from a normal
// handshake failure (see maxWatchdogStrikes).
var errProbeWatchdogTimeout = errors.New("probe watchdog timeout")

// maxWatchdogStrikes is how many consecutive probeWithWatchdog timeouts a
// port tolerates before StartScan stops re-probing it every cycle. Each
// timeout abandons a permanently-stuck goroutine (see probeWatchdog's doc
// comment) that outlives the process — a port that keeps timing out is wedged
// rather than merely slow, so backing off caps how many of these accumulate
// over a long-running scan session, at the cost of noticing newly-flashed
// firmware on that specific port less promptly.
const maxWatchdogStrikes = 2

// watchdogCooldown is how long a wedged port (maxWatchdogStrikes reached) is
// left unprobed before being retried once more. A var so tests can shrink it.
var watchdogCooldown = 60 * time.Second

// probeOutcome carries a probeIdentityFn result across the watchdog's
// internal channel.
type probeOutcome struct {
	identity *liteclient.DeviceIdentity
	opened   bool
	err      error
}

// probeWithWatchdog calls probeIdentityFn(port) but does not wait on it past
// probeWatchdog — see that var's doc comment for why such a bound is needed
// at all. On timeout it reports the port as opened-but-unresponsive (the
// same shape a genuine failed handshake produces), which is exactly the
// classification an actually-connected, non-Wendy-Lite board should get.
func probeWithWatchdog(port string) (identity *liteclient.DeviceIdentity, opened bool, err error) {
	// Read the indirection var here, on the caller's goroutine, rather than
	// inside the goroutine below: the `go` statement's happens-before
	// guarantee then ensures this read is ordered before anything the caller
	// does after probeWithWatchdog returns (including a test's deferred
	// restore of probeIdentityFn) — reading it directly inside the spawned
	// goroutine has no such ordering against a caller that stops waiting on
	// it, and races under -race.
	probe := probeIdentityFn
	ch := make(chan probeOutcome, 1)
	go func() {
		id, ok, e := probe(port)
		ch <- probeOutcome{identity: id, opened: ok, err: e}
	}()

	select {
	case r := <-ch:
		return r.identity, r.opened, r.err
	case <-time.After(probeWatchdog):
		return nil, true, fmt.Errorf("probe watchdog: %s did not respond within %s: %w", port, probeWatchdog, errProbeWatchdogTimeout)
	}
}

// ListenerID identifies a registered listener and is used to remove it.
type ListenerID uint64

// SerialDiscovery probes all ESP32 serial ports and builds a list of
// reachable Wendy Lite devices. Each port is probed concurrently; all
// registered listeners are invoked (serially) whenever the list changes.
type SerialDiscovery struct {
	mu             sync.Mutex
	notifyMu       sync.Mutex
	running        bool
	repeatInterval time.Duration
	probing        map[string]bool
	probeSem       chan struct{} // limits concurrent probes
	devices        []SerialDevice
	listeners      map[ListenerID]func([]SerialDevice)
	nextID         ListenerID
	// contended tracks ESP32 ports whose most recent open attempt failed
	// because something else currently holds them (see ContendedPorts).
	// Kept separate from devices: a contended port must never appear there
	// (that would risk mislabeling a fine, working board as unflashed), but
	// still needs to be distinguishable from "no board connected" for
	// DiscoverDevices to explain a truly empty result (WDY-2319).
	contended map[string]bool
	// watchdogStrikes counts consecutive probeWithWatchdog timeouts per port;
	// wedgedUntil (set once strikes reach maxWatchdogStrikes) is when the port
	// next becomes eligible for a probe again. Both reset the moment a probe
	// for that port returns before the watchdog fires.
	watchdogStrikes map[string]int
	wedgedUntil     map[string]time.Time
	// scanStarted/scanFinished track scan-pass generations so WaitForIdle can
	// tell "a pass is in flight" apart from "no pass has run yet": scanStarted
	// is bumped at the top of each resolve+probe pass, and scanFinished is set
	// to that pass's generation once every probe it spawned has completed.
	scanStarted  int
	scanFinished int
}

var (
	serialDiscoveryInstance     *SerialDiscovery
	serialDiscoveryInstanceOnce sync.Once
)

// GetSerialDiscovery returns the singleton SerialDiscovery.
func GetSerialDiscovery() *SerialDiscovery {
	serialDiscoveryInstanceOnce.Do(func() {
		serialDiscoveryInstance = &SerialDiscovery{
			probing:         make(map[string]bool),
			probeSem:        make(chan struct{}, 4),
			listeners:       make(map[ListenerID]func([]SerialDevice)),
			contended:       make(map[string]bool),
			watchdogStrikes: make(map[string]int),
			wedgedUntil:     make(map[string]time.Time),
		}
	})
	return serialDiscoveryInstance
}

// AddListener registers a function to be called whenever the device list
// changes. Returns an ID that can be passed to RemoveListener.
func (d *SerialDiscovery) AddListener(cb func([]SerialDevice)) ListenerID {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nextID++
	d.listeners[d.nextID] = cb
	return d.nextID
}

// RemoveListener unregisters the listener identified by id.
func (d *SerialDiscovery) RemoveListener(id ListenerID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.listeners, id)
}

// StartScan resolves all ESP32 serial ports and probes each one concurrently
// in the background. A port is added to the list if a WendyCom connection can
// be established; the connection is closed immediately after the check. Ports
// that are no longer present are removed. Returns immediately.
//
// repeatInterval controls automatic re-scanning: zero means run once, any
// positive value causes the scan to repeat after each run. Calling StartScan
// again while a scan loop is active updates the interval immediately; the
// change takes effect after the current iteration completes.
func (d *SerialDiscovery) StartScan(repeatInterval time.Duration) {
	d.mu.Lock()
	d.repeatInterval = repeatInterval
	// Bumped synchronously, before this call returns, so a caller that calls
	// WaitForIdle right after StartScan always targets a generation the scan
	// goroutine has not resolved yet — otherwise the target read in
	// WaitForIdle could race the goroutine's own first increment (below) and
	// see the pre-call value, making WaitForIdle return immediately before
	// any probing has even started.
	d.scanStarted++
	if d.running {
		d.mu.Unlock()
		return
	}
	d.running = true
	d.mu.Unlock()

	go func() {
		for {
			d.mu.Lock()
			gen := d.scanStarted
			d.mu.Unlock()
			// finishGen marks this pass done unless spawned probes below take
			// over that job (passing the remaining count they must reach zero
			// first). Deferred so every early path — resolve failure included
			// — still unblocks WaitForIdle instead of hanging it.
			finishGen := func() {
				d.mu.Lock()
				if d.scanFinished < gen {
					d.scanFinished = gen
				}
				d.mu.Unlock()
			}

			ports, err := resolvePortsFn()
			if err == nil {
				portSet := make(map[string]bool, len(ports))
				for _, p := range ports {
					portSet[p.Port] = true
				}

				// Remove devices whose ports are no longer present.
				d.mu.Lock()
				kept := make([]SerialDevice, 0, len(d.devices))
				var removed int
				for _, dev := range d.devices {
					if portSet[dev.Port] {
						kept = append(kept, dev)
					} else {
						removed++
					}
				}
				d.devices = kept
				for port := range d.contended {
					if !portSet[port] {
						delete(d.contended, port)
					}
				}
				for port := range d.watchdogStrikes {
					if !portSet[port] {
						delete(d.watchdogStrikes, port)
						delete(d.wedgedUntil, port)
					}
				}
				d.mu.Unlock()

				if removed > 0 {
					d.notify()
				}

				// Probe every present port not already confirmed responsive
				// and not currently mid-probe. A port that matched the VID/PID
				// but hasn't completed the identity handshake (no Wendy Lite
				// firmware yet, or it's still booting) is re-probed every
				// cycle, so it surfaces the moment compatible firmware lands.
				d.mu.Lock()
				responsive := make(map[string]bool, len(d.devices))
				for _, dev := range d.devices {
					if dev.Responsive {
						responsive[dev.Port] = true
					}
				}
				d.mu.Unlock()

				var toProbe []string
				for _, p := range ports {
					d.mu.Lock()
					wedged := time.Now().Before(d.wedgedUntil[p.Port])
					skip := responsive[p.Port] || d.probing[p.Port] || wedged
					if !skip {
						d.probing[p.Port] = true
					}
					d.mu.Unlock()
					if skip {
						continue
					}
					toProbe = append(toProbe, p.Port)
				}

				if len(toProbe) == 0 {
					finishGen()
				}
				// remaining is decremented by each probe goroutine spawned
				// below; the one that brings it to zero marks this pass
				// finished. It is only ever touched under d.mu, alongside the
				// scanFinished write itself, so WaitForIdle never observes a
				// pass as done before every probe it started has landed.
				remaining := len(toProbe)

				for _, port := range toProbe {
					go func(port string) {
						d.probeSem <- struct{}{}
						defer func() {
							<-d.probeSem
							d.mu.Lock()
							delete(d.probing, port)
							remaining--
							if remaining == 0 && d.scanFinished < gen {
								d.scanFinished = gen
							}
							d.mu.Unlock()
						}()

						identity, opened, err := probeWithWatchdog(port)

						d.mu.Lock()
						if errors.Is(err, errProbeWatchdogTimeout) {
							d.watchdogStrikes[port]++
							if d.watchdogStrikes[port] >= maxWatchdogStrikes {
								d.wedgedUntil[port] = time.Now().Add(watchdogCooldown)
							}
						} else {
							delete(d.watchdogStrikes, port)
							delete(d.wedgedUntil, port)
						}
						d.mu.Unlock()

						if !opened {
							// Something else currently holds the port (a
							// concurrent probe from another wendy process, a
							// dangling fd inherited by a subprocess, etc.).
							// Leave any existing entry untouched rather than
							// mislabel a contended-but-fine device as unflashed;
							// the next cycle retries once it frees up. Recorded
							// separately via contended so a caller seeing zero
							// devices can still tell a board is present but
							// inaccessible right now (WDY-2319).
							d.mu.Lock()
							d.contended[port] = true
							d.mu.Unlock()
							return
						}

						d.mu.Lock()
						delete(d.contended, port)
						d.mu.Unlock()

						var dev SerialDevice
						if err != nil || identity == nil {
							dev = SerialDevice{Port: port, Responsive: false}
						} else {
							dev = SerialDevice{
								Port:             port,
								ID:               identity.ID,
								Name:             identity.Name,
								DisplayName:      identity.DisplayName,
								EnrolledDeviceID: identity.EnrolledDeviceID,
								Responsive:       true,
							}
						}

						d.mu.Lock()
						changed := d.upsertDeviceLocked(dev)
						d.mu.Unlock()
						if changed {
							d.notify()
						}
					}(port)
				}
			} else {
				finishGen()
			}

			d.mu.Lock()
			next := d.repeatInterval
			if next <= 0 {
				d.running = false
				d.mu.Unlock()
				return
			}
			d.mu.Unlock()

			time.Sleep(next)

			d.mu.Lock()
			if d.repeatInterval == 0 {
				d.running = false
				d.mu.Unlock()
				return
			}
			d.mu.Unlock()
		}
	}()
}

func (d *SerialDiscovery) StopScan() {
	d.mu.Lock()
	d.repeatInterval = 0
	d.mu.Unlock()
}

// notify snapshots the device list and calls all registered listeners serially.
func (d *SerialDiscovery) notify() {
	d.mu.Lock()
	snap := d.snapshotLocked()
	cbs := make([]func([]SerialDevice), 0, len(d.listeners))
	for _, cb := range d.listeners {
		cbs = append(cbs, cb)
	}
	d.mu.Unlock()

	d.notifyMu.Lock()
	for _, cb := range cbs {
		callListener(cb, snap)
	}
	d.notifyMu.Unlock()
}

func callListener(cb func([]SerialDevice), snap []SerialDevice) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("discovery: listener panic: %v", r)
		}
	}()
	cb(snap)
}

// Devices returns a snapshot of the current device list.
func (d *SerialDiscovery) Devices() []SerialDevice {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snapshotLocked()
}

// ContendedPorts returns the ESP32 serial ports whose most recent open
// attempt failed because something else currently holds them exclusively (a
// concurrent wendy process, a subprocess with an inherited fd, etc.) — as
// opposed to a port that opened fine but had no Wendy Lite firmware, which is
// surfaced through Devices()/WaitForIdle() instead. Callers use this to
// distinguish "no board connected" from "a board is connected but
// inaccessible right now" (WDY-2319).
func (d *SerialDiscovery) ContendedPorts() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	ports := make([]string, 0, len(d.contended))
	for port := range d.contended {
		ports = append(ports, port)
	}
	sort.Strings(ports)
	return ports
}

// waitForIdlePollInterval bounds how stale a WaitForIdle result can be once
// the pass it is waiting on finishes.
const waitForIdlePollInterval = 20 * time.Millisecond

// WaitForIdle blocks until the scan pass most recently started by StartScan
// has fully resolved every port it probed — or ctx is done — then returns the
// resulting snapshot. StartScan itself always returns immediately and lets
// probing continue in the background; reading Devices() right after some
// unrelated fixed-length wait (e.g. a concurrent mDNS browse) races that
// pass's own per-port handshake budget against whatever window the caller
// happens to use, and can miss a genuinely connected, responsive board
// (WDY-2319). WaitForIdle removes that race for one-shot callers that need a
// definitive result. Callers should bound ctx themselves; a pass that never
// resolves (e.g. a wedged serial port) otherwise blocks until it does.
func (d *SerialDiscovery) WaitForIdle(ctx context.Context) []SerialDevice {
	d.mu.Lock()
	target := d.scanStarted
	d.mu.Unlock()

	for {
		d.mu.Lock()
		finished := d.scanFinished >= target
		snap := d.snapshotLocked()
		d.mu.Unlock()
		if finished {
			return snap
		}
		select {
		case <-ctx.Done():
			return snap
		case <-time.After(waitForIdlePollInterval):
		}
	}
}

// snapshotLocked returns a copy of the device list. Must be called with mu held.
func (d *SerialDiscovery) snapshotLocked() []SerialDevice {
	snap := make([]SerialDevice, len(d.devices))
	copy(snap, d.devices)
	return snap
}

// upsertDeviceLocked replaces the entry for dev.Port with dev (appending if
// none exists yet) and reports whether the list actually changed. Must be
// called with mu held.
func (d *SerialDiscovery) upsertDeviceLocked(dev SerialDevice) bool {
	for i, existing := range d.devices {
		if existing.Port == dev.Port {
			if existing == dev {
				return false
			}
			d.devices[i] = dev
			return true
		}
	}
	d.devices = append(d.devices, dev)
	return true
}
