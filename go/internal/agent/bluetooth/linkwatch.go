package bluetooth

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/agent/logfields"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// The link watcher keeps BLE HID links (gamepads, keyboards, RC controllers)
// at a short supervision timeout, so a dead link is torn down — and the app
// reading the input device sees it vanish — in about half a second rather
// than the several seconds such devices ask for. It also logs every
// Bluetooth disconnect with its reason. See
// specs/2026-09-22-bt-hid-link-supervision-timeout-design.md (WDY-3189).

const (
	hidSupervisionTimeoutEnv = "WENDY_BT_HID_SUPERVISION_TIMEOUT_MS"
	// Supervision timeouts are in 10 ms controller units. The default is
	// 500 ms; the bounds are the Core Specification's 100 ms – 32 s.
	defaultHIDSupervisionTimeout uint16 = 50
	minSupervisionTimeout        uint16 = 10
	maxSupervisionTimeout        uint16 = 3200
)

// Watcher tunables; the design spec explains each value.
const (
	linkSettleDelay       = 1 * time.Second
	linkGuardWindow       = 60 * time.Second
	linkGuardMaxRaises    = 3
	linkRetryDelay        = 250 * time.Millisecond
	linkMaxRetries        = 3
	linkInFlightTimeout   = 5 * time.Second
	linkAddressWait       = 500 * time.Millisecond
	linkAddressPoll       = 50 * time.Millisecond
	linkClassifyTimeout   = 2 * time.Second
	adapterRescanInterval = 30 * time.Second
	adapterMaxBackoff     = 30 * time.Second
)

// parseHIDSupervisionTimeout turns the environment value into a target in
// 10 ms units; 0 means the override is off. An invalid value yields the
// default and an error describing what was rejected.
func parseHIDSupervisionTimeout(raw string) (uint16, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultHIDSupervisionTimeout, nil
	}
	ms, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return defaultHIDSupervisionTimeout, fmt.Errorf("%s=%q is not a whole number of milliseconds", hidSupervisionTimeoutEnv, raw)
	}
	if ms == 0 {
		return 0, nil
	}
	if ms < 100 || ms > 32000 {
		return defaultHIDSupervisionTimeout, fmt.Errorf("%s=%d is outside 100-32000 ms", hidSupervisionTimeoutEnv, ms)
	}
	return uint16(ms / 10), nil
}

// desiredTimeout is the supervision timeout, in 10 ms units, the watcher
// wants on a link with the given maximum interval and latency: the target,
// raised when the link's own timing needs more (the timeout must exceed
// 2 × (1 + latency) × interval), clamped to the specification's range.
func desiredTimeout(target, interval, latency uint16) uint16 {
	// 2 × (1 + latency) × interval × 1.25 ms in 10 ms units is
	// (1 + latency) × interval / 4; the smallest step strictly above it is
	// that quotient plus one.
	floor := (uint32(latency)+1)*uint32(interval)/4 + 1
	d := max(uint32(target), floor)
	return uint16(min(max(d, uint32(minSupervisionTimeout)), uint32(maxSupervisionTimeout)))
}

// parseStoredConnParams reads the [ConnectionParameters] group bluetoothd
// writes to /var/lib/bluetooth/<adapter>/<device>/info: the parameters the
// device last asked for. ok is false when the group or a key is missing.
func parseStoredConnParams(info string) (connUpdate, bool) {
	vals := map[string]uint16{}
	inGroup := false
	for line := range strings.Lines(info) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inGroup = line == "[ConnectionParameters]"
			continue
		}
		k, v, found := strings.Cut(line, "=")
		if !inGroup || !found {
			continue
		}
		if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 16); err == nil {
			vals[strings.TrimSpace(k)] = uint16(n)
		}
	}
	for _, k := range []string{"MinInterval", "MaxInterval", "Latency", "Timeout"} {
		if _, ok := vals[k]; !ok {
			return connUpdate{}, false
		}
	}
	return connUpdate{
		IntervalMin: vals["MinInterval"],
		IntervalMax: vals["MaxInterval"],
		Latency:     vals["Latency"],
		Timeout:     vals["Timeout"],
	}, true
}

// parseAdapterNames picks adapter indexes out of /sys/class/bluetooth entry
// names: "hci0", "hci1", but not connection entries such as "hci0:64".
func parseAdapterNames(names []string) []int {
	var idx []int
	for _, name := range names {
		n, ok := strings.CutPrefix(name, "hci")
		if !ok {
			continue
		}
		if i, err := strconv.Atoi(n); err == nil && i >= 0 {
			idx = append(idx, i)
		}
	}
	slices.Sort(idx)
	return idx
}

func adapterName(index int) string { return fmt.Sprintf("hci%d", index) }

// adapterBackoff is the wait before reopening an adapter after its nth
// consecutive failure: 1 s, doubling, capped at adapterMaxBackoff.
func adapterBackoff(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	return min(time.Second<<min(failures-1, 5), adapterMaxBackoff)
}

// LinkInfo is what the watcher knows about one connected LE peripheral.
type LinkInfo struct {
	// SupervisionTimeoutMS is the timeout the controller last reported.
	SupervisionTimeoutMS uint32
	// RequestedSupervisionTimeoutMS is what the peripheral asked for when
	// that was above the watcher's target, 0 if it never did. It equals
	// SupervisionTimeoutMS once the watcher has stopped overriding it.
	RequestedSupervisionTimeoutMS uint32
}

// LinkReporter looks up link parameters by peripheral address.
type LinkReporter interface {
	LinkInfo(address string) (LinkInfo, bool)
}

// annotateLink copies a connected peripheral's link parameters onto p.
func annotateLink(p *agentpb.DiscoveredBluetoothPeripheral, links LinkReporter) {
	if links == nil || !p.GetConnected() {
		return
	}
	li, ok := links.LinkInfo(p.GetAddress())
	if !ok {
		return
	}
	p.SupervisionTimeoutMs = proto.Uint32(li.SupervisionTimeoutMS)
	if li.RequestedSupervisionTimeoutMS != 0 {
		p.RequestedSupervisionTimeoutMs = proto.Uint32(li.RequestedSupervisionTimeoutMS)
	}
}

// linkTransport is raw HCI access to one adapter.
type linkTransport interface {
	// Events delivers decoded controller events. It is closed when the
	// adapter goes away.
	Events() <-chan hciEvent
	// UpdateConnection sends an LE Connection Update for handle.
	UpdateConnection(handle uint16, u connUpdate) error
	// Connections lists the adapter's current connections.
	Connections() ([]connInfo, error)
	// StoredParams returns the connection parameters bluetoothd stored for
	// a peer.
	StoredParams(address string) (connUpdate, bool)
	Close() error
}

// hidClassifier reports whether a peer is a HID device. known is false while
// BlueZ has not resolved the device's services yet.
type hidClassifier interface {
	IsHID(ctx context.Context, adapterIndex int, address string) (isHID, known bool)
}

// linkPlatform is what the Watcher needs from the operating system. A zero
// linkPlatform (no adapters func) makes Run return immediately.
type linkPlatform struct {
	adapters func() ([]int, error)
	open     func(index int) (linkTransport, error)
	classify hidClassifier
}

// Watcher keeps BLE HID links at a short supervision timeout and logs every
// Bluetooth disconnect with its reason.
type Watcher struct {
	logger   *zap.Logger
	target   uint16 // 10 ms units; 0 = override off
	platform linkPlatform

	mu    sync.Mutex
	links map[string]LinkInfo // by upper-case address
}

func newWatcher(logger *zap.Logger, target uint16, p linkPlatform) *Watcher {
	return &Watcher{logger: logger, target: target, platform: p, links: map[string]LinkInfo{}}
}

// LinkInfo implements LinkReporter.
func (w *Watcher) LinkInfo(address string) (LinkInfo, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	li, ok := w.links[strings.ToUpper(address)]
	return li, ok
}

func (w *Watcher) setLink(address string, li LinkInfo) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.links[strings.ToUpper(address)] = li
}

func (w *Watcher) deleteLink(address string) {
	if address == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.links, strings.ToUpper(address))
}

// Run watches every Bluetooth adapter until ctx ends, picking up adapters
// that appear later and reopening ones that go away.
func (w *Watcher) Run(ctx context.Context) {
	if w.platform.adapters == nil {
		return
	}
	type slot struct {
		running  bool
		failures int
		nextTry  time.Time
		lastErr  string
	}
	slots := map[int]*slot{}
	exited := make(chan int)
	loggedNone := false
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case idx := <-exited:
			s := slots[idx]
			s.running = false
			s.failures++
			s.nextTry = time.Now().Add(adapterBackoff(s.failures))
		case <-timer.C:
		}

		now := time.Now()
		next := now.Add(adapterRescanInterval)
		indexes, err := w.platform.adapters()
		if (err != nil || len(indexes) == 0) && !loggedNone {
			w.logger.Debug("No Bluetooth adapter to watch yet", zap.Error(err))
			loggedNone = true
		}
		for _, idx := range indexes {
			s := slots[idx]
			if s == nil {
				s = &slot{}
				slots[idx] = s
			}
			if s.running {
				continue
			}
			if now.Before(s.nextTry) {
				next = earliest(next, s.nextTry)
				continue
			}
			tr, err := w.platform.open(idx)
			if err != nil {
				if err.Error() != s.lastErr {
					w.logger.Warn("Cannot watch Bluetooth adapter links",
						zap.String(logfields.Adapter, adapterName(idx)), zap.Error(err))
					s.lastErr = err.Error()
				}
				s.failures++
				s.nextTry = now.Add(adapterBackoff(s.failures))
				next = earliest(next, s.nextTry)
				continue
			}
			s.running, s.failures, s.lastErr = true, 0, ""
			go func() {
				w.watchAdapter(ctx, idx, tr)
				select {
				case exited <- idx:
				case <-ctx.Done():
				}
			}()
		}
		timer.Reset(next.Sub(now))
	}
}

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

type hidState uint8

const (
	hidUnknown hidState = iota
	hidYes
	hidNo
)

// linkConn is the watcher's state for one connection.
type linkConn struct {
	handle        uint16
	address       string
	linkType      uint8
	central       bool
	hid           hidState
	params        connParams // zero until the controller reports them
	requested     uint16     // device's contested timeout, 10 ms units
	raises        []time.Time
	gaveUp        bool
	retries       int
	warnedRefusal bool
	connectedAt   time.Time // zero for links that predate the watch
}

type linkWakeKind uint8

const (
	wakeCheck  linkWakeKind = iota + 1 // re-evaluate a link (settle, retry)
	wakeExpire                         // an in-flight update may be lost
)

type linkWake struct {
	kind   linkWakeKind
	handle uint16
	seq    uint64
}

// pendingUpdate is our LE Connection Update awaiting its completion event.
type pendingUpdate struct {
	handle  uint16
	timeout uint16
	seq     uint64
}

// adapterWatch is the state for one adapter. Only its own goroutine touches
// it; timers reach it through the wake channel.
type adapterWatch struct {
	w        *Watcher
	index    int
	tr       linkTransport
	conns    map[uint16]*linkConn
	inFlight *pendingUpdate // at most one of our updates per adapter
	seq      uint64
	wake     chan linkWake
	done     chan struct{}
}

func (w *Watcher) watchAdapter(ctx context.Context, index int, tr linkTransport) {
	a := &adapterWatch{
		w: w, index: index, tr: tr,
		conns: map[uint16]*linkConn{},
		wake:  make(chan linkWake, 16),
		done:  make(chan struct{}),
	}
	defer func() {
		close(a.done)
		tr.Close()
		for _, c := range a.conns {
			w.deleteLink(c.address)
		}
	}()
	a.adoptExisting(ctx)
	events := tr.Events()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				w.logger.Info("Bluetooth adapter went away; will reopen it",
					zap.String(logfields.Adapter, adapterName(index)))
				return
			}
			a.handle(ctx, ev)
		case wk := <-a.wake:
			a.handleWake(ctx, wk)
		}
	}
}

func (a *adapterWatch) after(d time.Duration, wk linkWake) {
	time.AfterFunc(d, func() {
		select {
		case a.wake <- wk:
		case <-a.done:
		}
	})
}

func (a *adapterWatch) fields(c *linkConn) []zap.Field {
	return []zap.Field{
		zap.String(logfields.Adapter, adapterName(a.index)),
		zap.String(logfields.Address, c.address),
	}
}

func (a *adapterWatch) handle(ctx context.Context, ev hciEvent) {
	switch ev.Kind {
	case hciConnComplete:
		a.onConnect(ev)
	case hciConnUpdateComplete:
		a.onUpdate(ctx, ev)
	case hciCmdStatus:
		a.onCmdStatus(ev)
	case hciDisconnComplete:
		a.onDisconnect(ctx, ev)
	}
}

func (a *adapterWatch) onConnect(ev hciEvent) {
	if ev.Status != 0 {
		return
	}
	c := &linkConn{
		handle: ev.Handle, linkType: hciLinkLE, central: ev.Central,
		params: ev.Params, connectedAt: time.Now(),
	}
	c.address = a.resolveAddress(ev.Handle)
	a.conns[ev.Handle] = c
	a.publish(c)
	if c.central && a.w.target != 0 {
		// Give the device a moment to send its own parameter request first,
		// so one update usually covers both.
		a.after(linkSettleDelay, linkWake{kind: wakeCheck, handle: ev.Handle})
	}
}

func (a *adapterWatch) onUpdate(ctx context.Context, ev hciEvent) {
	c := a.conn(ev.Handle)
	if c == nil {
		return
	}
	// Ours is the in-flight update's completion: a failure on that handle,
	// or success with the timeout we asked for. Anything else on the same
	// handle is the device's own request landing first.
	ours := a.inFlight != nil && a.inFlight.handle == ev.Handle &&
		(ev.Status != 0 || ev.Params.Timeout == a.inFlight.timeout)
	if ours {
		a.inFlight = nil
	}
	if ev.Status != 0 {
		if ours && !c.warnedRefusal {
			c.warnedRefusal = true
			a.w.logger.Warn("Bluetooth device refused the link timeout update",
				append(a.fields(c), zap.String(logfields.Status, fmt.Sprintf("0x%02x", ev.Status)))...)
		}
		return
	}
	c.params = ev.Params
	if ours {
		c.retries = 0
		a.publish(c)
		a.w.logger.Info("Bluetooth link timeout set", append(a.fields(c),
			zap.Uint32(logfields.SupervisionTimeoutMS, uint32(c.params.Timeout)*10),
			zap.Uint32(logfields.RequestedSupervisionTimeoutMS, uint32(c.requested)*10))...)
		return
	}
	u, contested, ok := a.nextUpdate(ctx, c)
	if !ok {
		a.publish(c)
		return
	}
	// The device (or the kernel on its behalf) raised the timeout again.
	c.requested = contested
	c.retries = 0
	if a.raised(c) {
		a.publish(c)
		return
	}
	a.publish(c)
	a.override(c, u)
}

// raised records a device-caused raise and reports whether the guard has
// now tripped for this link.
func (a *adapterWatch) raised(c *linkConn) bool {
	now := time.Now()
	c.raises = slices.DeleteFunc(c.raises, func(t time.Time) bool { return now.Sub(t) >= linkGuardWindow })
	c.raises = append(c.raises, now)
	if len(c.raises) < linkGuardMaxRaises {
		return false
	}
	c.gaveUp = true
	a.w.logger.Warn("Bluetooth device keeps restoring its own link timeout; leaving it",
		append(a.fields(c), zap.Uint32(logfields.RequestedSupervisionTimeoutMS, uint32(c.requested)*10))...)
	return true
}

func (a *adapterWatch) onCmdStatus(ev hciEvent) {
	if ev.Opcode != hciOpLEConnUpdate || ev.Status == 0 || a.inFlight == nil {
		return
	}
	// Command Status carries no handle: while one of ours is in flight a
	// failure is taken to be ours. Misattributing the kernel's own costs at
	// most one extra update.
	h := a.inFlight.handle
	a.inFlight = nil
	c := a.conns[h]
	if c == nil {
		return
	}
	status := zap.String(logfields.Status, fmt.Sprintf("0x%02x", ev.Status))
	switch ev.Status {
	case hciStatusCommandDisallowed, hciStatusControllerBusy:
		if c.retries < linkMaxRetries {
			c.retries++
			a.after(linkRetryDelay, linkWake{kind: wakeCheck, handle: h})
			return
		}
		a.w.logger.Warn("Gave up setting the Bluetooth link timeout; the controller stayed busy",
			append(a.fields(c), status)...)
	default:
		a.w.logger.Warn("Controller rejected the Bluetooth link timeout update",
			append(a.fields(c), status)...)
	}
}

func (a *adapterWatch) onDisconnect(ctx context.Context, ev hciEvent) {
	if ev.Status != 0 {
		return
	}
	c := a.conns[ev.Handle]
	delete(a.conns, ev.Handle)
	if a.inFlight != nil && a.inFlight.handle == ev.Handle {
		a.inFlight = nil
	}
	if c == nil {
		// A Classic link, or one that predates the watch. The kernel still
		// lists it: the raw socket sees this event before the kernel does.
		ci, ok := a.lookup(ev.Handle)
		if !ok {
			ci = connInfo{Handle: ev.Handle, LinkType: hciLinkUnknown}
		}
		c = &linkConn{handle: ev.Handle, address: ci.Address, linkType: ci.LinkType, central: ci.Central}
	}
	a.w.deleteLink(c.address)

	fields := append(a.fields(c),
		zap.String(logfields.LinkType, linkTypeName(c.linkType)),
		zap.String(logfields.ReasonCode, fmt.Sprintf("0x%02x", ev.Reason)),
		zap.String(logfields.Reason, disconnectReasonText(ev.Reason)))
	if c.params.Timeout != 0 {
		fields = append(fields, zap.Uint32(logfields.SupervisionTimeoutMS, uint32(c.params.Timeout)*10))
	}
	if !c.connectedAt.IsZero() {
		fields = append(fields, zap.Duration(logfields.Duration, time.Since(c.connectedAt)))
	}
	if ev.Reason == hciReasonConnectionTimeout && a.isHID(ctx, c) {
		a.w.logger.Warn("Bluetooth HID link lost", fields...)
		return
	}
	a.w.logger.Info("Bluetooth link closed", fields...)
}

func (a *adapterWatch) handleWake(ctx context.Context, wk linkWake) {
	switch wk.kind {
	case wakeExpire:
		if a.inFlight != nil && a.inFlight.seq == wk.seq {
			a.inFlight = nil
		}
	case wakeCheck:
		c := a.conns[wk.handle]
		if c == nil {
			return
		}
		a.ensureAddress(c)
		if u, contested, ok := a.nextUpdate(ctx, c); ok {
			c.requested = contested
			a.override(c, u)
		}
	}
}

// adoptExisting takes over links that were up before this watch started.
// HCI cannot read an LE link's live parameters, so a HID link is overridden
// from what bluetoothd stored for the device; the completion event then
// reports the real values.
func (a *adapterWatch) adoptExisting(ctx context.Context) {
	conns, err := a.tr.Connections()
	if err != nil {
		a.w.logger.Debug("Cannot list existing Bluetooth connections",
			zap.String(logfields.Adapter, adapterName(a.index)), zap.Error(err))
		return
	}
	for _, ci := range conns {
		if ci.LinkType != hciLinkLE {
			continue
		}
		c := &linkConn{handle: ci.Handle, address: ci.Address, linkType: hciLinkLE, central: ci.Central}
		a.conns[ci.Handle] = c
		if a.w.target == 0 || !c.central || !a.isHID(ctx, c) {
			continue
		}
		if _, ok := a.tr.StoredParams(c.address); !ok {
			a.w.logger.Info("No stored parameters for a connected Bluetooth HID device; its link timeout is set when it next connects",
				a.fields(c)...)
			continue
		}
		if u, contested, ok := a.nextUpdate(ctx, c); ok {
			c.requested = contested
			a.override(c, u)
		}
	}
}

// nextUpdate returns the update c needs, if any, and the timeout it
// replaces.
func (a *adapterWatch) nextUpdate(ctx context.Context, c *linkConn) (u connUpdate, contested uint16, ok bool) {
	if a.w.target == 0 || !c.central || c.gaveUp {
		return connUpdate{}, 0, false
	}
	if c.params.Timeout == 0 {
		// An adopted link: start from what bluetoothd stored.
		if !a.isHID(ctx, c) {
			return connUpdate{}, 0, false
		}
		s, found := a.tr.StoredParams(c.address)
		if !found {
			return connUpdate{}, 0, false
		}
		want := desiredTimeout(a.w.target, s.IntervalMax, s.Latency)
		if s.Timeout <= want {
			return connUpdate{}, 0, false
		}
		return connUpdate{IntervalMin: s.IntervalMin, IntervalMax: s.IntervalMax, Latency: s.Latency, Timeout: want}, s.Timeout, true
	}
	want := desiredTimeout(a.w.target, c.params.Interval, c.params.Latency)
	if c.params.Timeout <= want || !a.isHID(ctx, c) {
		return connUpdate{}, 0, false
	}
	return connUpdate{
		IntervalMin: c.params.Interval, IntervalMax: c.params.Interval,
		Latency: c.params.Latency, Timeout: want,
	}, c.params.Timeout, true
}

// override sends u for c unless one of our updates is already in flight on
// this adapter, in which case c is re-checked shortly.
func (a *adapterWatch) override(c *linkConn, u connUpdate) {
	if a.inFlight != nil {
		if a.inFlight.handle != c.handle {
			a.after(linkRetryDelay, linkWake{kind: wakeCheck, handle: c.handle})
		}
		return
	}
	if err := a.tr.UpdateConnection(c.handle, u); err != nil {
		a.w.logger.Warn("Failed to send a Bluetooth link timeout update", append(a.fields(c), zap.Error(err))...)
		return
	}
	a.seq++
	a.inFlight = &pendingUpdate{handle: c.handle, timeout: u.Timeout, seq: a.seq}
	a.after(linkInFlightTimeout, linkWake{kind: wakeExpire, handle: c.handle, seq: a.seq})
}

// isHID classifies c once BlueZ can tell, caching the answer on the link.
func (a *adapterWatch) isHID(ctx context.Context, c *linkConn) bool {
	if c.hid == hidUnknown && c.address != "" && a.w.platform.classify != nil {
		cctx, cancel := context.WithTimeout(ctx, linkClassifyTimeout)
		isHID, known := a.w.platform.classify.IsHID(cctx, a.index, c.address)
		cancel()
		switch {
		case isHID:
			c.hid = hidYes
		case known:
			c.hid = hidNo
		}
	}
	return c.hid == hidYes
}

func (a *adapterWatch) publish(c *linkConn) {
	if c.address == "" || c.linkType != hciLinkLE || c.params.Timeout == 0 {
		return
	}
	a.w.setLink(c.address, LinkInfo{
		SupervisionTimeoutMS:          uint32(c.params.Timeout) * 10,
		RequestedSupervisionTimeoutMS: uint32(c.requested) * 10,
	})
}

// conn returns the state for handle, adopting a link the kernel lists but
// the watch has not seen connect.
func (a *adapterWatch) conn(handle uint16) *linkConn {
	if c := a.conns[handle]; c != nil {
		a.ensureAddress(c)
		return c
	}
	ci, ok := a.lookup(handle)
	if !ok {
		return nil
	}
	c := &linkConn{handle: handle, address: ci.Address, linkType: ci.LinkType, central: ci.Central}
	a.conns[handle] = c
	return c
}

func (a *adapterWatch) ensureAddress(c *linkConn) {
	if c.address != "" {
		return
	}
	if ci, ok := a.lookup(c.handle); ok {
		c.address = ci.Address
	}
}

// resolveAddress finds a new connection's peer address. The raw socket sees
// an event before the kernel processes it, so a brand-new connection can take
// a moment to appear in the kernel's list.
func (a *adapterWatch) resolveAddress(handle uint16) string {
	deadline := time.Now().Add(linkAddressWait)
	for {
		if ci, ok := a.lookup(handle); ok {
			return ci.Address
		}
		if !time.Now().Before(deadline) {
			return ""
		}
		time.Sleep(linkAddressPoll)
	}
}

func (a *adapterWatch) lookup(handle uint16) (connInfo, bool) {
	conns, err := a.tr.Connections()
	if err != nil {
		a.w.logger.Debug("Cannot list Bluetooth connections",
			zap.String(logfields.Adapter, adapterName(a.index)), zap.Error(err))
		return connInfo{}, false
	}
	for _, ci := range conns {
		if ci.Handle == handle {
			return ci, true
		}
	}
	return connInfo{}, false
}
