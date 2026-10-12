package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
)

// SerialConfig fixes the physical host line at this baud, 8N1, no handshake,
// DTR and RTS disabled. Guest termios changes do not reconfigure the host.
// Port is a host name for the device: COM12 on Windows, a /dev path on macOS
// and Linux. The mapping lives with the VM on one host, so only that host's
// form is valid.
type SerialConfig struct {
	Port string `json:"port"`
	Baud uint32 `json:"baud"`
}

func (c SerialConfig) Validate() error {
	if err := validateHostSerialPort(c.Port); err != nil {
		return err
	}
	if c.Baud == 0 {
		return errors.New("serial baud must be an exact positive integer supported by the host driver")
	}
	return nil
}

// Ten bits per byte at 8N1. Windows adds this millisecond multiplier for
// every requested byte, plus a fixed timeout allowance. Round up so slow
// devices can transmit a full copy buffer without an artificial five-second
// cap. uint64 avoids overflow for high (driver-validated) baud values.
func serialWriteTimeoutMultiplier(baud uint32) uint32 {
	return uint32((10000 + uint64(baud) - 1) / uint64(baud))
}

func (s *Store) serialPath(name string) string { return filepath.Join(s.Dir(name), "serial.json") }
func (s *Store) ReadSerial(name string) (*SerialConfig, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.serialPath(name))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c SerialConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("reading VM serial configuration: %w", err)
	}
	return &c, c.Validate()
}

// ConfigureSerial serializes changes with VM creation/start/removal and refuses
// live changes. A nil configuration removes the mapping.
func (s *Store) ConfigureSerial(name string, c *SerialConfig) error {
	if c != nil {
		if err := c.Validate(); err != nil {
			return err
		}
	}
	life, err := s.acquireLifecycleLock(name)
	if err != nil {
		return err
	}
	defer life.Close()
	lock, err := s.acquireRunLockUnderLifecycle(name)
	if err != nil {
		return err
	}
	defer lock.Close()
	if c == nil {
		err := os.Remove(s.serialPath(name))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return writeJSON(s.serialPath(name), c)
}

// Preserve the failing endpoint: a QEMU socket closing during normal exit must
// not be confused with a physical device disconnect or a COM write failure.
type qemuSerialDisconnect struct{ error }
type serialIOError struct{ error }
type serialReader struct{ io.Reader }

func (r serialReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil {
		err = serialIOError{err}
	}
	return n, err
}

type serialWriter struct{ io.Writer }

func (w serialWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		err = serialIOError{err}
	}
	return n, err
}

// serialBridge owns one listener and one exclusive COM handle. Closing either
// direction tears down both; it never silently reconnects to a changed device.
type serialBridge struct {
	listener net.Listener
	port     io.ReadWriteCloser
	done     chan error
	stop     chan struct{}
	once     sync.Once
	wg       sync.WaitGroup
	mu       sync.Mutex
	conn     net.Conn
}

var openVMSerial = openHostSerial

func startSerialBridge(c SerialConfig) (*serialBridge, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	p, err := openVMSerial(c)
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("binding VM serial loopback listener: %w", err)
	}
	b := &serialBridge{listener: l, port: p, done: make(chan error, 1), stop: make(chan struct{})}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		conn, err := l.Accept()
		if err != nil {
			b.report(c, err)
			return
		}
		b.mu.Lock()
		select {
		case <-b.stop:
			b.mu.Unlock()
			conn.Close()
			return
		default:
		}
		b.conn = conn
		b.mu.Unlock()
		// One connection only. Release the listener after QEMU connects.
		_ = l.Close()
		errs := make(chan error, 2)
		var copies sync.WaitGroup
		copies.Add(2)
		copyBytes := func(dst io.Writer, src io.Reader) {
			defer copies.Done()
			_, e := io.Copy(dst, src)
			var physical serialIOError
			if !errors.As(e, &physical) {
				if e == nil {
					e = io.EOF
				}
				e = qemuSerialDisconnect{e}
			}
			errs <- e
		}
		go copyBytes(conn, serialReader{p})
		go copyBytes(serialWriter{p}, conn)
		err = <-errs
		b.report(c, err)
		b.shutdown()
		copies.Wait()
	}()
	return b, nil
}
func (b *serialBridge) report(c SerialConfig, err error) {
	select {
	case <-b.stop:
		return
	default:
	}
	if err == nil {
		err = io.EOF
	}
	b.done <- fmt.Errorf("VM serial %s at %d baud disconnected or forwarding failed: %w; check the cable and Windows driver, then restart the VM", c.Port, c.Baud, err)
}
func (b *serialBridge) shutdown() {
	b.once.Do(func() {
		close(b.stop)
		_ = b.listener.Close()
		b.mu.Lock()
		if b.conn != nil {
			_ = b.conn.Close()
		}
		b.mu.Unlock()
		_ = b.port.Close()
	})
}
func (b *serialBridge) Close()    { b.shutdown(); b.wg.Wait() }
func (b *serialBridge) Port() int { return b.listener.Addr().(*net.TCPAddr).Port }
