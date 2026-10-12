//go:build darwin || linux

package vm

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// openTestTTY gives the real opener a terminal device to work on. The test
// side holds the pty master: bytes written there arrive as device input, and
// device output can be read back from it.
func openTestTTY(t *testing.T) (master *os.File, device string) {
	t.Helper()
	m, s, err := pty.Open()
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(); _ = m.Close() })
	// Ptys have no modem lines to drop.
	saved := serialModemBits
	serialModemBits = nil
	t.Cleanup(func() { serialModemBits = saved })
	return m, s.Name()
}

// allBytes covers every value, including the ones a cooked terminal would
// eat or translate: NUL, ^C, XON/XOFF, CR, LF and DEL.
func allBytes() []byte {
	b := make([]byte, 256)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func readFullWithin(t *testing.T, r io.Reader, n int) []byte {
	t.Helper()
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		b := make([]byte, n)
		_, err := io.ReadFull(r, b)
		done <- result{b, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatal(res.err)
		}
		return res.b
	case <-time.After(5 * time.Second):
		t.Fatal("timed out reading from the serial device")
		return nil
	}
}

func TestHostSerialRawBinaryBothWays(t *testing.T) {
	master, device := openTestTTY(t)
	p, err := openHostSerial(SerialConfig{Port: device, Baud: 115200})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	data := allBytes()
	if _, err := master.Write(data); err != nil {
		t.Fatal(err)
	}
	if got := readFullWithin(t, p, len(data)); !bytes.Equal(got, data) {
		t.Fatalf("device input altered:\n got %x\nwant %x", got, data)
	}
	if _, err := p.Write(data); err != nil {
		t.Fatal(err)
	}
	if got := readFullWithin(t, master, len(data)); !bytes.Equal(got, data) {
		t.Fatalf("device output altered:\n got %x\nwant %x", got, data)
	}
}

func TestHostSerialExclusiveUntilClosed(t *testing.T) {
	_, device := openTestTTY(t)
	first, err := openHostSerial(SerialConfig{Port: device, Baud: 115200})
	if err != nil {
		t.Fatal(err)
	}
	if second, err := openHostSerial(SerialConfig{Port: device, Baud: 115200}); err == nil {
		second.Close()
		first.Close()
		t.Fatal("opened a device another VM already holds")
	} else if !strings.Contains(err.Error(), "in use") {
		t.Fatalf("busy error does not say so: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := openHostSerial(SerialConfig{Port: device, Baud: 115200})
	if err != nil {
		t.Fatalf("device not released on close: %v", err)
	}
	again.Close()
}

func TestHostSerialMissingDevice(t *testing.T) {
	_, err := openHostSerial(SerialConfig{Port: "/dev/wendy-test-no-such-serial", Baud: 115200})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing device: %v", err)
	}
}

// The bridge over a real terminal device: binary bytes reach QEMU's side and
// back, and losing the device is reported as a device failure, not as QEMU
// closing its end.
func TestSerialBridgeOverHostTTY(t *testing.T) {
	master, device := openTestTTY(t)
	bridge, err := startSerialBridge(SerialConfig{Port: device, Baud: 115200})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	qemu, err := net.Dial("tcp4", bridge.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer qemu.Close()

	data := allBytes()
	if _, err := qemu.Write(data); err != nil {
		t.Fatal(err)
	}
	if got := readFullWithin(t, master, len(data)); !bytes.Equal(got, data) {
		t.Fatalf("guest to device altered:\n got %x\nwant %x", got, data)
	}
	if _, err := master.Write(data); err != nil {
		t.Fatal(err)
	}
	if got := readFullWithin(t, qemu, len(data)); !bytes.Equal(got, data) {
		t.Fatalf("device to guest altered:\n got %x\nwant %x", got, data)
	}

	master.Close() // the adapter goes away
	select {
	case err := <-bridge.done:
		var qemuSide qemuSerialDisconnect
		if errors.As(err, &qemuSide) {
			t.Fatalf("device loss reported as a QEMU disconnect: %v", err)
		}
		if !strings.Contains(err.Error(), device) {
			t.Fatalf("error does not name the device: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("device loss not reported")
	}
}
