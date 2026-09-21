package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (s *Store) QMPPath(name string) string { return filepath.Join(s.Dir(name), "qmp.sock") }

// QMP grants full VM control to anyone who can connect. Enforce the user-only
// directory boundary on every launch, including VMs created by older versions.
func (s *Store) prepareQMP(name string) (string, error) {
	dir := s.Dir(name)
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("VM control socket requires a real directory: %s", dir)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return "", fmt.Errorf("protecting VM control socket directory: %w", err)
	}
	return s.QMPPath(name), nil
}

type qmpClient struct {
	dec *json.Decoder
	enc *json.Encoder
}

func (q *qmpClient) execute(command string, arguments any, result any) error {
	if err := q.enc.Encode(map[string]any{"execute": command, "arguments": arguments}); err != nil {
		return err
	}
	for {
		var reply struct {
			Event  string          `json:"event"`
			Return json.RawMessage `json:"return"`
			Error  *struct {
				Desc string `json:"desc"`
			} `json:"error"`
		}
		if err := q.dec.Decode(&reply); err != nil {
			return err
		}
		if reply.Event != "" {
			continue
		}
		if reply.Error != nil {
			return fmt.Errorf("QEMU %s: %s", command, reply.Error.Desc)
		}
		if reply.Return == nil {
			return fmt.Errorf("QEMU %s returned no result", command)
		}
		if result != nil {
			return json.Unmarshal(reply.Return, result)
		}
		return nil
	}
}

func (q *qmpClient) monitor(command string) (string, error) {
	var result string
	err := q.execute("human-monitor-command", map[string]string{"command-line": command}, &result)
	return result, err
}

// EnsureTCPPorts adds loopback-only, same-port forwards to a running user-mode
// VM. They live in QEMU, so detached apps stay reachable after the CLI exits.
// Query QEMU's actual listeners rather than trusting a stale file across boots.
func (s *Store) EnsureTCPPorts(ctx context.Context, name string, ports []int) error {
	if err := ValidName(name); err != nil {
		return err
	}
	for _, port := range ports {
		if port < 1 || port > 65535 {
			return fmt.Errorf("invalid TCP port %d", port)
		}
	}
	if len(ports) == 0 {
		return nil
	}
	if !DetachSupported() {
		return fmt.Errorf("automatic VM port forwarding currently requires macOS or Linux")
	}
	return s.withQMP(ctx, name, func(q *qmpClient) error { return q.ensureTCPPorts(name, ports) })
}

// EnsureTCPPortMapping returns this VM's loopback forward for guestPort. An
// existing forward wins over preferredHostPort; otherwise the preferred port is
// tried once, falling back to an available port. Zero requests any available
// host port. The caller can persist the returned port for the next boot.
//
// Lifecycle serialization prevents another CLI from replacing this VM or
// allocating a competing mapping for it while QMP is being queried.
func (s *Store) EnsureTCPPortMapping(ctx context.Context, name string, guestPort, preferredHostPort int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := ValidName(name); err != nil {
		return 0, err
	}
	if guestPort < 1 || guestPort > 65535 {
		return 0, fmt.Errorf("invalid guest TCP port %d", guestPort)
	}
	if preferredHostPort < 0 || preferredHostPort > 65535 {
		return 0, fmt.Errorf("invalid preferred host TCP port %d", preferredHostPort)
	}
	lock, err := s.acquireLifecycleLock(name)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	return s.ensureTCPPortMapping(ctx, name, guestPort, preferredHostPort)
}

// TCPPortMapping reads a VM's actual loopback listener without adding a
// forward. A persisted host port alone is not evidence of endpoint ownership.
func (s *Store) TCPPortMapping(ctx context.Context, name string, guestPort int) (int, error) {
	if err := ValidName(name); err != nil {
		return 0, err
	}
	if guestPort < 1 || guestPort > 65535 {
		return 0, fmt.Errorf("invalid guest TCP port %d", guestPort)
	}
	st, err := s.Status(name)
	if err != nil {
		return 0, err
	}
	if !st.Running || st.State.NetMode != NetUser {
		return 0, fmt.Errorf("VM %q is not running with user networking", name)
	}
	var port int
	err = s.withQMP(ctx, name, func(q *qmpClient) error {
		info, err := q.monitor("info usernet")
		if err == nil {
			port = tcpForwardHostPort(info, guestPort)
		}
		return err
	})
	return port, err
}

// The caller holds the lifecycle lock, including when the verified result will
// be written into robot.json by EnsureRobotSandboxPort.
func (s *Store) ensureTCPPortMapping(ctx context.Context, name string, guestPort, preferredHostPort int) (int, error) {
	if !DetachSupported() {
		return 0, fmt.Errorf("automatic VM port forwarding currently requires macOS or Linux")
	}
	var port int
	err := s.withQMP(ctx, name, func(q *qmpClient) error {
		var lastOutput string
		for attempt := range 4 {
			info, err := q.monitor("info usernet")
			if err != nil {
				return err
			}
			if port = tcpForwardHostPort(info, guestPort); port != 0 {
				return nil
			}
			candidate := 0
			if attempt == 0 {
				candidate = preferredHostPort
			}
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", candidate))
			if err != nil && candidate != 0 {
				ln, err = net.Listen("tcp", "127.0.0.1:0")
			}
			if err != nil {
				return err
			}
			candidate = ln.Addr().(*net.TCPAddr).Port
			if err := ln.Close(); err != nil {
				return err
			}
			lastOutput, err = q.monitor(fmt.Sprintf("hostfwd_add net0 tcp:127.0.0.1:%d-:%d", candidate, guestPort))
			if err != nil {
				return err
			}
			// QMP can report HMP failures as successful string responses. Only the
			// VM's actual listener table proves ownership. A competing allocator
			// can win the bind after our temporary reservation is released.
			info, err = q.monitor("info usernet")
			if err != nil {
				return err
			}
			if port = tcpForwardHostPort(info, guestPort); port != 0 {
				return nil
			}
		}
		return fmt.Errorf("could not allocate a loopback forward for VM %q guest TCP port %d: %s", name, guestPort, strings.TrimSpace(lastOutput))
	})
	return port, err
}

func (s *Store) requestPowerdown(ctx context.Context, name string) error {
	return s.withQMP(ctx, name, func(q *qmpClient) error {
		return q.execute("system_powerdown", map[string]any{}, nil)
	})
}

// RegistryPort resolves the registry through this VM's own monitor. Never
// assume the host's port 5000 belongs to the VM selected by its agent port.
func (s *Store) RegistryPort(ctx context.Context, name string) (int, error) {
	if err := ValidName(name); err != nil {
		return 0, err
	}
	var port int
	err := s.withQMP(ctx, name, func(q *qmpClient) error {
		for range 4 {
			info, err := q.monitor("info usernet")
			if err != nil {
				return err
			}
			if port = tcpForwardHostPort(info, DeviceRegistryPort); port != 0 {
				return nil
			}
			// Reserve a candidate briefly; QEMU must own the actual listener.
			// Retry if another host process wins the gap before hostfwd_add.
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return err
			}
			candidate := ln.Addr().(*net.TCPAddr).Port
			if err := ln.Close(); err != nil {
				return err
			}
			if _, err := q.monitor(fmt.Sprintf("hostfwd_add net0 tcp:127.0.0.1:%d-:%d", candidate, DeviceRegistryPort)); err != nil {
				return err
			}
			info, err = q.monitor("info usernet")
			if err != nil {
				return err
			}
			if port = tcpForwardHostPort(info, DeviceRegistryPort); port != 0 {
				return nil
			}
		}
		return fmt.Errorf("could not allocate a loopback registry forward for VM %q", name)
	})
	return port, err
}

func tcpForwardHostPort(info string, guestPort int) int {
	inNet0 := false
	for _, line := range strings.Split(info, "\n") {
		if strings.HasPrefix(line, "Hub ") {
			inNet0 = strings.HasSuffix(strings.TrimSpace(line), "(net0):")
			continue
		}
		f := strings.Fields(line)
		if inNet0 && len(f) >= 6 && f[0] == "TCP[HOST_FORWARD]" && f[2] == "127.0.0.1" && f[4] == "10.0.2.15" && f[5] == strconv.Itoa(guestPort) {
			if port, err := strconv.Atoi(f[3]); err == nil && port > 0 && port <= 65535 {
				return port
			}
		}
	}
	return 0
}

func (s *Store) withQMP(ctx context.Context, name string, fn func(*qmpClient) error) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", s.QMPPath(name))
	if err != nil {
		return fmt.Errorf("connecting to VM %q control socket (restart VMs created with an older launcher): %w", name, err)
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(deadline)
	}
	q := qmpClient{dec: json.NewDecoder(c), enc: json.NewEncoder(c)}
	var greeting map[string]json.RawMessage
	if err := q.dec.Decode(&greeting); err != nil {
		return err
	}
	if greeting["QMP"] == nil {
		return fmt.Errorf("VM %q did not send a QMP greeting", name)
	}
	if err := q.execute("qmp_capabilities", map[string]any{}, nil); err != nil {
		return err
	}
	return fn(&q)
}

func (q *qmpClient) ensureTCPPorts(name string, ports []int) error {
	for _, port := range ports {
		info, err := q.monitor("info usernet")
		if err != nil {
			return err
		}
		if hasTCPForward(info, port) {
			continue
		}
		output, err := q.monitor(fmt.Sprintf("hostfwd_add net0 tcp:127.0.0.1:%d-:%d", port, port))
		if err != nil {
			return err
		}
		// HMP errors are successful QMP string results. Verify the listener,
		// also accepting a concurrent invocation that added the same forward.
		info, err = q.monitor("info usernet")
		if err != nil {
			return err
		}
		if !hasTCPForward(info, port) {
			return fmt.Errorf("forwarding VM %q TCP port %d to 127.0.0.1:%d failed: %s; free the host port or choose a different application port", name, port, port, strings.TrimSpace(output))
		}
	}
	return nil
}

func hasTCPForward(info string, port int) bool {
	inNet0 := false
	for _, line := range strings.Split(info, "\n") {
		if strings.HasPrefix(line, "Hub ") {
			inNet0 = strings.HasSuffix(strings.TrimSpace(line), "(net0):")
			continue
		}
		fields := strings.Fields(line)
		if inNet0 && len(fields) >= 6 && fields[0] == "TCP[HOST_FORWARD]" && fields[2] == "127.0.0.1" && fields[3] == strconv.Itoa(port) && fields[4] == "10.0.2.15" && fields[5] == strconv.Itoa(port) {
			return true
		}
	}
	return false
}
