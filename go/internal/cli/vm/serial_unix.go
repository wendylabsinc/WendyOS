//go:build darwin || linux

package vm

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"strings"

	"go.bug.st/serial"
	"golang.org/x/sys/unix"

	"github.com/wendylabsinc/wendy/go/internal/shared/seriallock"
)

func validateHostSerialPort(port string) error {
	if !strings.HasPrefix(port, "/dev/") || path.Clean(port) != port {
		return fmt.Errorf("invalid serial device %q: use its /dev path, for example %s", port, exampleSerialPort())
	}
	return nil
}

// exampleSerialPort is what a USB-serial adapter usually looks like here. On
// macOS the cu. node is the one that does not wait for carrier detect.
func exampleSerialPort() string {
	if runtime.GOOS == "darwin" {
		return "/dev/cu.usbmodem1101"
	}
	return "/dev/ttyACM0 or /dev/serial/by-id/…"
}

// NormalizeSerialPort leaves device paths alone: they are case-sensitive.
func NormalizeSerialPort(port string) string { return strings.TrimSpace(port) }

// serialModemBits drops DTR and RTS once the port is open. A variable only so
// pseudo-terminal tests can skip it: ptys have no modem lines.
var serialModemBits = &serial.ModemOutputBits{}

// openHostSerial takes the advisory lock pyserial-based tools also take, then
// opens the device raw at the exact baud. go.bug.st/serial sets TIOCEXCL and
// clears RTS/CTS and XON/XOFF; a rate outside the standard table goes through
// IOSSIOSPEED on macOS and BOTHER on Linux, and a driver that refuses it fails
// the open rather than falling back to a nearby rate.
func openHostSerial(c SerialConfig) (io.ReadWriteCloser, error) {
	lock, err := seriallock.Acquire(c.Port)
	if err != nil {
		switch {
		// EBUSY: another holder set TIOCEXCL, so even opening to take the
		// lock is refused (Linux reports this before flock gets a say).
		case errors.Is(err, seriallock.ErrLocked), errors.Is(err, unix.EBUSY):
			return nil, fmt.Errorf("%s is in use by another program; close it or stop the VM using it", c.Port)
		case errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("serial device %s not found; check the cable and the device path (ls /dev)", c.Port)
		case errors.Is(err, os.ErrPermission):
			return nil, fmt.Errorf("opening %s: permission denied%s", c.Port, serialPermissionHint())
		}
		return nil, fmt.Errorf("opening %s: %w", c.Port, err)
	}
	p, err := serial.Open(c.Port, &serial.Mode{
		BaudRate:          int(c.Baud),
		DataBits:          8,
		Parity:            serial.NoParity,
		StopBits:          serial.OneStopBit,
		InitialStatusBits: serialModemBits,
	})
	if err != nil {
		lock.Release()
		var perr *serial.PortError
		if errors.As(err, &perr) {
			switch perr.Code() {
			case serial.PortBusy:
				return nil, fmt.Errorf("%s is in use by another program; close it or stop the VM using it", c.Port)
			case serial.PermissionDenied:
				return nil, fmt.Errorf("opening %s: permission denied%s", c.Port, serialPermissionHint())
			}
		}
		return nil, fmt.Errorf("configuring %s at exact %d baud, 8N1: %w; choose a baud the driver supports (no nearest-baud fallback)", c.Port, c.Baud, err)
	}
	return &unixSerial{port: p, lock: lock}, nil
}

func serialPermissionHint() string {
	if runtime.GOOS == "linux" {
		return "; add your user to the device's group (usually dialout) and log in again"
	}
	return ""
}

type unixSerial struct {
	port serial.Port
	lock *seriallock.Lock
}

func (p *unixSerial) Read(b []byte) (int, error) { return p.port.Read(b) }

// Write finishes partial writes: a tty write can return early when a signal
// lands, and the bridge treats a short write as a failed device.
func (p *unixSerial) Write(b []byte) (int, error) {
	written := 0
	for written < len(b) {
		n, err := p.port.Write(b[written:])
		written += n
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func (p *unixSerial) Close() error {
	err := p.port.Close()
	p.lock.Release()
	return err
}

// guardSerialProcess has nothing to do here: a foreground launcher that exits
// normally stops QEMU itself, and one killed outright leaves a VM that
// 'wendy vm stop' can still shut down through its QMP monitor.
func guardSerialProcess(*os.Process) (io.Closer, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
