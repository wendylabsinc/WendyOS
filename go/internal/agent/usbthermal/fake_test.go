package usbthermal

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeDevice stands in for usbfs. It answers the control protocol the way the
// TC001 Max does, records every call in order, and serves bulk reads from a
// byte source, so the start/stop sequence and the capture loop run unchanged
// against it.
type fakeDevice struct {
	mu        sync.Mutex
	calls     []string
	model     string
	busyPolls int   // status reads that report busy after each command
	busyLeft  int   // remaining for the current command
	claimErr  error // returned by ClaimInterface(1)
	lastCmd   []byte
	bulk      func(buf []byte) (int, error) // the stream; nil means timeouts
	bulkCalls int
	closed    bool
}

func newFakeDevice() *fakeDevice { return &fakeDevice{model: "TC001 Max"} }

func (f *fakeDevice) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeDevice) Control(requestType, request uint8, value, index uint16, data []byte, _ time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case requestType == reqTypeVendorOutInterface && request == reqCommand:
		f.record("cmd %s", hex.EncodeToString(data))
		f.lastCmd = bytes.Clone(data)
		f.busyLeft = f.busyPolls
		return len(data), nil
	case requestType == reqTypeVendorInInterface && request == reqStatus:
		f.record("status")
		if f.busyLeft != 0 {
			if f.busyLeft > 0 {
				f.busyLeft--
			}
			data[0] = 0x01
		} else {
			data[0] = 0x02
		}
		return 1, nil
	case requestType == reqTypeVendorInInterface && request == reqResponse:
		f.record("resp %d", len(data))
		switch {
		case bytes.Equal(f.lastCmd, cmdReadModel):
			return copy(data, f.model), nil // zero-padded, as the camera pads it
		case bytes.Equal(f.lastCmd, cmdStartStream):
			data[0] = 0x01
			return 1, nil
		}
		return 0, nil
	default:
		f.record("ctrl %02x %02x %d %d len%d", requestType, request, value, index, len(data))
		return len(data), nil
	}
}

func (f *fakeDevice) Bulk(endpoint uint8, data []byte, _ time.Duration) (int, error) {
	f.mu.Lock()
	f.bulkCalls++
	if f.bulkCalls == 1 {
		f.record("bulk %02x %d", endpoint, len(data))
	}
	src := f.bulk
	f.mu.Unlock()
	if src == nil {
		time.Sleep(time.Millisecond)
		return 0, ErrTimeout
	}
	// Pace the stream a little so a capture loop does not spin a CPU flat out
	// under the race detector.
	time.Sleep(50 * time.Microsecond)
	return src(data)
}

func (f *fakeDevice) ClaimInterface(iface uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("claim %d", iface)
	if iface == streamInterface && f.claimErr != nil {
		return f.claimErr
	}
	return nil
}

func (f *fakeDevice) ReleaseInterface(iface uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("release %d", iface)
	return nil
}

func (f *fakeDevice) SetInterface(iface, alt uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("setintf %d %d", iface, alt)
	return nil
}

func (f *fakeDevice) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	f.record("close")
	return nil
}

// callLog returns the calls so far.
func (f *fakeDevice) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeDevice) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// replay returns a bulk source serving the real block window over and over,
// each copy carrying the next frame counter, in 16 KiB reads.
func replay(t *testing.T, firstSeq uint16) func([]byte) (int, error) {
	window := blockWindow(t)
	return replayFrames(firstSeq, func(seq uint16) []byte { return withCounter(window, seq) })
}

// replayFrames is replay with each frame's stream bytes made by frame.
func replayFrames(firstSeq uint16, frame func(seq uint16) []byte) func([]byte) (int, error) {
	var mu sync.Mutex
	seq := firstSeq
	var pending []byte
	return func(buf []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		if len(pending) == 0 {
			pending = frame(seq)
			seq++
		}
		n := copy(buf, pending)
		pending = pending[n:]
		return n, nil
	}
}

// fakeBus is a sysfs tree plus the devices its usbfs nodes open.
type fakeBus struct {
	t     *testing.T
	sysfs string
	dev   string

	mu      sync.Mutex
	devices map[string]*fakeDevice // by usbfs path
	opened  []string
	open    int // currently open devices
	maxOpen int
}

func newFakeBus(t *testing.T) *fakeBus {
	root := t.TempDir()
	b := &fakeBus{t: t, sysfs: filepath.Join(root, "sys"), dev: filepath.Join(root, "dev"), devices: map[string]*fakeDevice{}}
	if err := os.MkdirAll(filepath.Join(b.sysfs, "bus", "usb", "devices"), 0o755); err != nil {
		t.Fatal(err)
	}
	return b
}

// plug writes a sysfs entry for a USB device.
func (b *fakeBus) plug(port, vid, pid string, bus, devnum int, serial, manufacturer, product string) {
	b.t.Helper()
	dir := filepath.Join(b.sysfs, "bus", "usb", "devices", port)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		b.t.Fatal(err)
	}
	attrs := map[string]string{
		"idVendor": vid, "idProduct": pid,
		"busnum": fmt.Sprint(bus), "devnum": fmt.Sprint(devnum),
		"serial": serial, "manufacturer": manufacturer, "product": product,
	}
	for name, v := range attrs {
		if v == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v+"\n"), 0o644); err != nil {
			b.t.Fatal(err)
		}
	}
}

// plugTC001Max plugs the exxon-01 camera in at the given device number.
func (b *fakeBus) plugTC001Max(port string, devnum int) {
	b.plug(port, "3474", "4962", 1, devnum, "YFDVS000000000001", "TOPDON TECHNOLOGY Co., Ltd.", "TC001 Max")
}

func (b *fakeBus) unplug(port string) {
	if err := os.RemoveAll(filepath.Join(b.sysfs, "bus", "usb", "devices", port)); err != nil {
		b.t.Fatal(err)
	}
}

func (b *fakeBus) node(bus, devnum int) string {
	return filepath.Join(b.dev, "bus", "usb", fmt.Sprintf("%03d", bus), fmt.Sprintf("%03d", devnum))
}

func (b *fakeBus) attach(path string, d *fakeDevice) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.devices[path] = d
}

func (b *fakeBus) openDevice(path string) (Device, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.opened = append(b.opened, path)
	d, ok := b.devices[path]
	if !ok {
		return nil, fmt.Errorf("opening %s: no such device", path)
	}
	b.open++
	b.maxOpen = max(b.maxOpen, b.open)
	return &countingDevice{fakeDevice: d, bus: b}, nil
}

func (b *fakeBus) openedPaths() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.opened...)
}

// countingDevice tracks how many devices are open at once: only one session
// may own the camera.
type countingDevice struct {
	*fakeDevice
	bus  *fakeBus
	once sync.Once
}

func (c *countingDevice) Close() error {
	c.once.Do(func() {
		c.bus.mu.Lock()
		c.bus.open--
		c.bus.mu.Unlock()
	})
	return c.fakeDevice.Close()
}

// newTestManager returns a manager over the fake bus whose pauses are skipped.
func newTestManager(t *testing.T, b *fakeBus, idTaken func(uint32) bool) *Manager {
	t.Helper()
	m := NewManager(context.Background(), Options{
		SysfsRoot: b.sysfs,
		DevRoot:   b.dev,
		Open:      b.openDevice,
		IDTaken:   idTaken,
		// The start sequence's pauses and the reconnect backoff become 5 ms:
		// short, but real, so a failing loop does not spin. Status polls are
		// skipped.
		sleep: func(ctx context.Context, d time.Duration) error {
			if d >= time.Second {
				return sleepCtx(ctx, 5*time.Millisecond)
			}
			return ctx.Err()
		},
	})
	t.Cleanup(m.Close)
	return m
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
