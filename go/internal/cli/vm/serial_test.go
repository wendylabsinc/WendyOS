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
)

func TestSerialPersistence(t *testing.T) {
	s := newTestStore(t)
	createTestVM(t, s, "serial", Meta{})
	c := SerialConfig{Port: "COM12", Baud: 1000000}
	if err := s.ConfigureSerial("serial", &c); err != nil {
		t.Fatal(err)
	}
	other := &Store{Root: s.Root}
	got, err := other.ReadSerial("serial")
	if err != nil || got == nil || *got != c {
		t.Fatalf("read %v: %v", got, err)
	}
	lock, err := s.acquireRunLock("serial")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.ConfigureSerial("serial", nil); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("live configuration: %v", err)
	}
	lock.Close()
	if err := other.ConfigureSerial("serial", nil); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadSerial("serial"); got != nil || err != nil {
		t.Fatalf("clear: %v %v", got, err)
	}
	if err := os.WriteFile(s.serialPath("serial"), []byte(`{"port":"COM12","baud":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadSerial("serial"); err == nil {
		t.Fatal("accepted invalid saved config")
	}
	if err := s.ConfigureSerial("missing", &c); err == nil {
		t.Fatal("configured missing VM")
	}
}
func TestSerialValidation(t *testing.T) {
	for _, c := range []SerialConfig{{"COM0", 1}, {"COM1,server=on", 1}, {"/dev/ttyUSB0", 1}, {"COM1", 0}} {
		if c.Validate() == nil {
			t.Fatalf("accepted %v", c)
		}
	}
}
func fakeSerial(t *testing.T) net.Conn {
	t.Helper()
	a, b := net.Pipe()
	saved := openVMSerial
	openVMSerial = func(SerialConfig) (io.ReadWriteCloser, error) { return a, nil }
	t.Cleanup(func() { openVMSerial = saved; a.Close(); b.Close() })
	return b
}
func TestSerialBridgeBinaryAndCleanup(t *testing.T) {
	peer := fakeSerial(t)
	bridge, err := startSerialBridge(SerialConfig{"COM12", 1000000})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	addr := bridge.listener.Addr().String()
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("not loopback: %s", addr)
	}
	qemu, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer qemu.Close()
	qemu.SetDeadline(time.Now().Add(3 * time.Second))
	peer.SetDeadline(time.Now().Add(3 * time.Second))
	data := make([]byte, 65536)
	for i := range data {
		data[i] = byte(i)
	}
	for _, direction := range [][2]net.Conn{{qemu, peer}, {peer, qemu}} {
		written := make(chan error, 1)
		go func() { _, err := direction[0].Write(data); written <- err }()
		got := make([]byte, len(data))
		if _, err := io.ReadFull(direction[1], got); err != nil {
			t.Fatal(err)
		}
		if err := <-written; err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data) {
			t.Fatal("binary corruption")
		}
	}
	peer.Close()
	select {
	case err := <-bridge.done:
		if !strings.Contains(err.Error(), "COM12") {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect not reported")
	}
	bridge.Close()
	if c, err := net.DialTimeout("tcp4", addr, time.Second); err == nil {
		c.Close()
		t.Fatal("listener leaked")
	}
}
func TestSerialBridgeCloseBeforeConnection(t *testing.T) {
	peer := fakeSerial(t)
	bridge, err := startSerialBridge(SerialConfig{"COM9", 115200})
	if err != nil {
		t.Fatal(err)
	}
	bridge.Close()
	bridge.Close()
	peer.SetReadDeadline(time.Now().Add(time.Second))
	assertSerialPeerClosed(t, peer)
}
func TestSerialBridgeOpenFailure(t *testing.T) {
	saved := openVMSerial
	defer func() { openVMSerial = saved }()
	want := errors.New("busy COM port")
	openVMSerial = func(SerialConfig) (io.ReadWriteCloser, error) { return nil, want }
	if _, err := startSerialBridge(SerialConfig{"COM9", 1}); !errors.Is(err, want) {
		t.Fatal(err)
	}
}
func TestSerialBridgeDifferentPorts(t *testing.T) {
	saved := openVMSerial
	defer func() { openVMSerial = saved }()
	openVMSerial = func(SerialConfig) (io.ReadWriteCloser, error) {
		a, b := net.Pipe()
		t.Cleanup(func() { b.Close() })
		return a, nil
	}
	a, err := startSerialBridge(SerialConfig{"COM9", 1})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := startSerialBridge(SerialConfig{"COM10", 1})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if a.Port() == b.Port() {
		t.Fatal("port collision")
	}
}

func TestSerialQEMUArgs(t *testing.T) {
	spec := Spec{Name: "serial", DiskPath: "disk", FirmwareCode: "code", FirmwareVars: "vars", Accel: AccelTCG, Net: NetConfig{Mode: NetUser, AgentPort: 50051, MAC: MACFor("serial")}}
	plain, err := spec.Args()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(plain, " "), "usb-serial") {
		t.Fatal("serial attached without configuration")
	}
	spec.SerialPort = 54321
	args, err := spec.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"qemu-xhci,id=wendyserialusb", "socket,id=wendyserial,host=127.0.0.1,port=54321,server=off,nodelay=on", "usb-serial,id=wendyserialdevice,bus=wendyserialusb.0,chardev=wendyserial"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s", want)
		}
	}
	spec.SerialPort = 65536
	if _, err := spec.Args(); err == nil {
		t.Fatal("invalid bridge port accepted")
	}
}

func assertSerialPeerClosed(t *testing.T, peer net.Conn) {
	t.Helper()
	peer.SetReadDeadline(time.Now().Add(time.Second))
	_, err := peer.Read(make([]byte, 1))
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("expected closed port, got %v (a timeout would mean leaked I/O)", err)
	}
}

func TestSerialWriteTimeoutCoversTransmission(t *testing.T) {
	const bytes = 32768 // io.Copy's normal buffer
	for _, baud := range []uint32{1, 110, 1200, 9600, 115200, 1000000, ^uint32(0)} {
		multiplier := uint64(serialWriteTimeoutMultiplier(baud))
		budget := multiplier * bytes
		wireMillis := (uint64(bytes)*10000 + uint64(baud) - 1) / uint64(baud)
		if budget < wireMillis {
			t.Fatalf("%d baud: write budget %dms below physical duration %dms", baud, budget, wireMillis)
		}
		if multiplier == 0 || multiplier > 10000 {
			t.Fatalf("invalid multiplier %d for %d baud", multiplier, baud)
		}
	}
}
