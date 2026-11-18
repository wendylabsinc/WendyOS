//go:build linux || darwin

package commands

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/wendylabsinc/wendy/go/internal/shared/seriallock"
	"go.bug.st/serial"
)

func TestSerialPTYRetry(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	path := filepath.Join(t.TempDir(), "serial-reappears")
	mode := &serial.Mode{BaudRate: 115200}
	if _, err := openLockedPort(path, mode); !errors.Is(err, fs.ErrNotExist) || errors.Is(err, errPortBusy) {
		t.Fatalf("missing PTY cause lost: %v", err)
	}
	// Retain the real opener; a barrier records the first genuine missing-node
	// result before the fixture publishes its new owned device path.
	original := serialOpenFn
	defer func() { serialOpenFn = original }()
	missing := make(chan struct{})
	attempts := 0
	serialOpenFn = func(path string, mode *serial.Mode) (serial.Port, error) {
		p, e := openLockedPort(path, mode)
		attempts++
		if attempts == 1 {
			if !errors.Is(e, fs.ErrNotExist) {
				t.Errorf("first open wasn't missing: %v", e)
			}
			close(missing)
		}
		return p, e
	}
	appeared := make(chan error, 1)
	go func() { <-missing; appeared <- os.Symlink(slave.Name(), path) }()
	port, err := openPortRetrying(path, mode, 2*time.Second)
	if publishErr := <-appeared; publishErr != nil {
		t.Fatal(publishErr)
	}
	if err != nil {
		t.Fatalf("actual reenumerating PTY did not recover: %v", err)
	}
	defer func() {
		if port != nil {
			port.Close()
		}
	}()
	if attempts < 2 {
		t.Fatalf("no real missing-device retry: %d", attempts)
	}
	if err := port.SetReadTimeout(time.Second); err != nil {
		t.Fatal(err)
	}
	payload := []byte("owned-serial-reconnected\n")
	if _, err := master.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(port, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("PTY payload=%q", got)
	}
	if err := port.Close(); err != nil {
		t.Fatal(err)
	}
	port = nil
	// TIOCEXCL and advisory ownership both need to be released for a new owner.
	holder, err := seriallock.Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openLockedPort(path, mode); !errors.Is(err, errPortBusy) {
		holder.Release()
		t.Fatalf("genuine PTY contention not busy: %v", err)
	}
	holder.Release()
	reopened, err := openLockedPort(path, mode)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	t.Log("ESP_SERIAL_PTY_RETRY_VERIFIED real missing node, retry, bytes, contention and reacquisition")
}
