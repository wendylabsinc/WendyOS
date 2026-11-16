//go:build linux || darwin

package discovery

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// The only substituted boundary is USB enumeration. A PTY is not an ESP ROM:
// the production serial driver, flock, WendyCom handshake and discovery callbacks
// run unchanged, but successful flashing/USB reset remain physical acceptance.
func TestSerialPTYLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owned-serial")
	attach := func() <-chan error {
		t.Helper()
		master, slave, err := pty.Open()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { master.Close(); slave.Close() })
		if err := os.Symlink(slave.Name(), path); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			// Read the actual WendyCom echo-mode command and fresh sentinel, then
			// hang up the peer. No protocol reply or probe result is injected.
			data := make([]byte, 53)
			_, err := io.ReadFull(master, data)
			if err == nil && !bytes.Equal(data[:5], []byte{16, 16, 16, 16, 'e'}) {
				err = fmt.Errorf("unexpected handshake prefix %x", data[:5])
			}
			master.Close()
			slave.Close()
			done <- err
		}()
		return done
	}
	peer := attach()
	old := resolvePortsFn
	resolvePortsFn = func() ([]SerialPortInfo, error) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return nil, nil
		}
		return []SerialPortInfo{{Port: path}}, nil
	}
	defer func() { resolvePortsFn = old }()
	d := newTestSerialDiscovery()
	var mu sync.Mutex
	var snapshots [][]SerialDevice
	listener := d.AddListener(func(devices []SerialDevice) {
		mu.Lock()
		defer mu.Unlock()
		snapshots = append(snapshots, append([]SerialDevice(nil), devices...))
	})
	defer d.RemoveListener(listener)
	scan := func() {
		t.Helper()
		d.StartScan(0)
		ctx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
		defer cancel()
		d.WaitForIdle(ctx)
		if ctx.Err() != nil {
			t.Fatal("production serial scan did not drain")
		}
	}
	scan()
	select {
	case err := <-peer:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("PTY peer did not drain")
	}
	devices := d.Devices()
	if len(devices) != 1 || devices[0].Responsive || devices[0].Port != path || len(d.ContendedPorts()) != 0 {
		t.Fatalf("opened PTY handshake failure misclassified: devices=%+v contended=%v", devices, d.ContendedPorts())
	}
	d.mu.Lock()
	strikes := len(d.watchdogStrikes)
	d.mu.Unlock()
	if strikes != 0 {
		t.Fatal("PTY disconnect was abandoned by watchdog")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	scan()
	if len(d.Devices()) != 0 || len(d.ContendedPorts()) != 0 {
		t.Fatal("disappeared serial path left a ghost row")
	}
	peer = attach()
	scan()
	select {
	case err := <-peer:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reconnected PTY peer did not drain")
	}
	if got := d.Devices(); len(got) != 1 || got[0].Responsive {
		t.Fatalf("reappearing serial path was not reprobed: %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(snapshots) != 3 || len(snapshots[0]) != 1 || len(snapshots[1]) != 0 || len(snapshots[2]) != 1 {
		t.Fatalf("picker listener did not observe remove/reappear: %+v", snapshots)
	}
	t.Log("SERIAL_PTY_DISCOVERY_VERIFIED actual open/handshake bytes/hangup; controlled USB inventory")
}
