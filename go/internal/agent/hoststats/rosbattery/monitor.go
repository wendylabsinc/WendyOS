package rosbattery

import (
	"context"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/hoststats"
	"github.com/wendylabsinc/wendy/go/internal/rtps"
)

// Timings for the scan/subscribe loop.
const (
	// discoverWindow is how long one scan waits for SEDP announcements before
	// deciding what it found. DDS discovery is not instant; a robot with ~120
	// writers takes seconds to enumerate.
	discoverWindow = 2 * time.Minute
	// idleRescan is the interval between scans once a scan has come up empty.
	// A device with no ROS 2 anywhere then costs one short burst of discovery
	// traffic every few minutes rather than a continuous participant.
	idleRescan = 5 * time.Minute
	// resubscribeDelay is how long to wait before rescanning after a
	// subscription goes quiet.
	resubscribeDelay = 5 * time.Second
)

// Monitor keeps a battery reading current by subscribing to whichever ROS 2
// topic on the device's DDS domain carries one.
//
// It owns the scan → subscribed → scan loop described in the design: a scan
// discovers writers and picks a battery topic, the subscription feeds decoded
// samples into a Cache, and silence past the staleness window drops back to
// scanning.
type Monitor struct {
	acquire func(context.Context, rtps.Config) (*rtps.Lease, error)
	cfg     Config
	cache   *Cache
	logf    func(format string, args ...any)
	// lastIface is the interface that last produced a reading, tried first on
	// the next scan so steady-state rescans do not re-walk the candidate list.
	lastIface string
}

// Automatic host discovery policy is shared with camera discovery.
type candidateIface = rtps.InterfaceCandidate

func candidateInterfaces() []string {
	ifaces, _ := rtps.HostInterfaces()
	return ifaces
}
func eligibleInterfaces(ifaces []candidateIface) []string {
	return rtps.EligibleInterfaces(ifaces, true)
}
func isWireless(name string) bool { return rtps.IsWirelessInterface(name) }

// moveToFront returns names with want first, if present.
func moveToFront(names []string, want string) []string {
	out := make([]string, 0, len(names))
	found := false
	for _, n := range names {
		if n == want {
			found = true
			continue
		}
		out = append(out, n)
	}
	if !found {
		return names
	}
	return append([]string{want}, out...)
}

// Config parameterises a Monitor. The zero value is usable: it discovers
// automatically on domain 0 across every multicast-capable interface.
type Config struct {
	// Enabled false disables the monitor entirely.
	Enabled bool
	// Interfaces pins discovery to named network interfaces, tried in order
	// until one yields a battery topic. Empty means auto-select, which skips
	// virtual and wireless interfaces; naming one here bypasses that filter and
	// is how a host whose robot link is WiFi opts back in.
	Interfaces []string
	// DomainID is the DDS domain. Zero is the ROS 2 default.
	DomainID int
	// Topic pins the topic name, skipping type-based matching. The decoder is
	// still chosen from the type the writer advertises.
	Topic string
	// Type pins the message type, narrowing matching to a single candidate.
	Type string
}

// NewMonitor creates a monitor writing into cache. logf may be nil.
func NewMonitor(cfg Config, cache *Cache, pool *rtps.Pool, logf func(string, ...any)) *Monitor {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Monitor{cfg: cfg, cache: cache, acquire: pool.Acquire, logf: logf}
}

// Battery returns the newest non-stale reading, or nil.
func (m *Monitor) Battery() *hoststats.Battery { return m.cache.Battery() }

// ThermalZones returns current device-specific temperatures learned from the
// same DDS sample as Battery. Standard BatteryState topics add no zones;
// Unitree LowState adds IMU and motor temperatures.
func (m *Monitor) ThermalZones() []hoststats.ThermalZone { return m.cache.ThermalZones() }

// Run drives the monitor until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	if !m.cfg.Enabled {
		m.logf("ros2 battery monitor disabled by config")
		return
	}
	for ctx.Err() == nil {
		found := m.scanAndSubscribe(ctx)
		if ctx.Err() != nil {
			return
		}
		// Clear the cache on the way out: whatever we had is about to go
		// stale, and a device that has genuinely lost its battery topic should
		// report nothing rather than a frozen number.
		m.cache.Put(nil)

		delay := resubscribeDelay
		if !found {
			delay = idleRescan
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// scanAndSubscribe tries each candidate interface in turn until one yields a
// battery topic, reporting whether any did.
//
// Trying them all matters more than it might appear. A robot's DDS lives on a
// specific network — 192.168.123.x on a Go2 — while the host also has WiFi and,
// on a device running containers, a handful of bridge interfaces. Picking the
// first multicast-capable interface finds the battery only by luck.
func (m *Monitor) scanAndSubscribe(ctx context.Context) bool {
	// A configured list is taken verbatim: naming an interface is how an
	// operator overrides the filter below, including to force a wireless one on
	// hardware whose robot link really is WiFi.
	ifaces := m.cfg.Interfaces
	if len(ifaces) == 0 {
		ifaces = candidateInterfaces()
	}
	// There is deliberately no "let rtps auto-select" fallback here. Auto-select
	// takes the first multicast-capable interface, which on a host with nothing
	// eligible means WiFi or a container bridge — exactly what the filter just
	// excluded, and traffic we would rather not emit at all.
	if len(ifaces) == 0 {
		m.logf("ros2 battery: no wired multicast interface; set interfaces in %s to override", ConfigFile)
		return false
	}

	// Start from whichever interface last worked, so a steady-state rescan does
	// not walk the whole list again.
	if m.lastIface != "" {
		ifaces = moveToFront(ifaces, m.lastIface)
	}

	for _, iface := range ifaces {
		if ctx.Err() != nil {
			return false
		}
		if m.tryInterface(ctx, iface) {
			m.lastIface = iface
			return true
		}
	}
	return false
}

// tryInterface runs one discovery cycle on a single interface, returning true
// only if it found a battery topic and consumed from it.
func (m *Monitor) tryInterface(ctx context.Context, iface string) bool {
	p, err := m.acquire(ctx, rtps.Config{
		DomainID:  m.cfg.DomainID,
		Interface: iface,
		Logf:      m.logf,
	})
	if err != nil {
		m.logf("ros2 battery: joining domain %d: %v", m.cfg.DomainID, err)
		return false
	}
	defer p.Close()

	deadline := time.After(discoverWindow)
collect:
	for {
		select {
		case <-deadline:
			break collect
		case <-p.Done():
			return false
		case <-ctx.Done():
			return false
		}
	}

	// Snapshot at the deadline excludes endpoints disposed or expired during the scan.
	found := map[string]rtps.Endpoint{}
	for _, ep := range p.Endpoints() {
		found[ep.Topic] = ep
	}
	target, ok := PickBatteryTopic(found, m.cfg.Topic, m.cfg.Type)
	if !ok {
		m.logf("ros2 battery: no battery topic among %d writers on %q", len(found), iface)
		return false
	}
	if err := p.Subscribe(target); err != nil {
		m.logf("ros2 battery: subscribing to %s: %v", target.Topic, err)
		return false
	}
	m.logf("ros2 battery: reading %s [%s] on %q", target.Topic, target.Type, iface)

	m.consume(ctx, p, target)
	return true
}

// consume decodes samples into the cache until the topic goes quiet for longer
// than the staleness window.
func (m *Monitor) consume(ctx context.Context, p *rtps.Lease, ep rtps.Endpoint) {
	decode := decoderFor(ep.Type)
	if decode == nil {
		m.logf("ros2 battery: no decoder for %s", ep.Type)
		return
	}
	var loggedErr bool
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.Done():
			return
		case s := <-p.Samples():
			reading, err := decode(s.Payload)
			if err != nil {
				// Log once per subscription. A layout mismatch repeats at the
				// topic's publish rate, which would otherwise flood the log.
				if !loggedErr {
					m.logf("ros2 battery: decoding %s: %v", ep.Topic, err)
					loggedErr = true
				}
				continue
			}
			m.cache.putTelemetry(reading.Battery, reading.ThermalZones)
		case <-time.After(StaleAfter):
			m.logf("ros2 battery: %s silent for %s, rescanning", ep.Topic, StaleAfter)
			return
		}
	}
}

type decodedTelemetry struct {
	Battery      *hoststats.Battery
	ThermalZones []hoststats.ThermalZone
}

// decoderFor selects a decoder by DDS type name, or nil when the type is one
// this package cannot read.
func decoderFor(typeName string) func([]byte) (*decodedTelemetry, error) {
	switch {
	case strings.Contains(typeName, "BatteryState"):
		return func(payload []byte) (*decodedTelemetry, error) {
			battery, err := DecodeBatteryState(payload)
			return &decodedTelemetry{Battery: battery}, err
		}
	case strings.Contains(typeName, "LowState"):
		return func(payload []byte) (*decodedTelemetry, error) {
			reading, err := DecodeLowStateTelemetry(payload)
			if err != nil {
				return nil, err
			}
			return &decodedTelemetry{Battery: reading.Battery, ThermalZones: reading.ThermalZones}, nil
		}
	default:
		return nil
	}
}

// PickBatteryTopic chooses which discovered writer to read.
//
// Preference order: an explicitly configured topic; then a standard
// sensor_msgs/BatteryState, which carries a real percentage and enough to
// estimate time remaining; then unitree_go/LowState. Among LowState writers the
// low-frequency variant wins — /lowstate is the high-rate control topic at
// ~1.2KB a message, and subscribing to it to read one byte of soc would cost
// real CPU on an edge device.
func PickBatteryTopic(found map[string]rtps.Endpoint, wantTopic, wantType string) (rtps.Endpoint, bool) {
	if wantTopic != "" {
		if ep, ok := found[wantTopic]; ok {
			return ep, true
		}
		// Topic names are mangled on the DDS wire: ROS 2 prefixes "rt/".
		if ep, ok := found["rt"+wantTopic]; ok {
			return ep, true
		}
		return rtps.Endpoint{}, false
	}

	var lowState, lfLowState *rtps.Endpoint
	for topic, ep := range found {
		if wantType != "" && !strings.Contains(ep.Type, wantType) {
			continue
		}
		switch {
		case strings.Contains(ep.Type, "BatteryState"):
			return ep, true
		case strings.Contains(ep.Type, "LowState"):
			e := ep
			if isLowFrequencyTopic(topic) {
				lfLowState = &e
			} else if lowState == nil {
				lowState = &e
			}
		}
	}
	if lfLowState != nil {
		return *lfLowState, true
	}
	if lowState != nil {
		return *lowState, true
	}
	return rtps.Endpoint{}, false
}

// isLowFrequencyTopic reports whether a topic is the "lf" (low frequency)
// variant, in either its ROS or DDS-mangled spelling.
func isLowFrequencyTopic(topic string) bool {
	return strings.HasPrefix(topic, "rt/lf/") || strings.HasPrefix(topic, "/lf/")
}
