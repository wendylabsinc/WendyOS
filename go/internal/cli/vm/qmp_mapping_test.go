package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fake QMP monitor owns real loopback listeners, so different VM monitors
// compete for ports exactly as independent QEMU processes do. No VM is started.
type mappingQMP struct {
	mu         sync.Mutex
	ports      map[int]int
	listeners  map[int]net.Listener
	adds       int
	stealFirst bool
	denyAll    bool
}

func startMappingQMP(t *testing.T, s *Store, name string) *mappingQMP {
	t.Helper()
	if err := os.MkdirAll(s.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", s.QMPPath(name))
	if err != nil {
		t.Fatal(err)
	}
	fake := &mappingQMP{ports: map[int]int{}, listeners: map[int]net.Listener{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			fake.serve(c, t)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, listener := range fake.listeners {
			_ = listener.Close()
		}
	})
	return fake
}

func (f *mappingQMP) serve(c net.Conn, t *testing.T) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	e, d := json.NewEncoder(c), json.NewDecoder(c)
	_ = e.Encode(map[string]any{"QMP": map[string]any{}})
	for {
		var req struct {
			Execute   string
			Arguments map[string]string
		}
		if err := d.Decode(&req); err != nil {
			if err != io.EOF {
				t.Errorf("fake QMP decode: %v", err)
			}
			return
		}
		var result any = map[string]any{}
		if req.Execute == "human-monitor-command" {
			result = f.monitor(req.Arguments["command-line"], t)
		}
		if err := e.Encode(map[string]any{"return": result}); err != nil {
			t.Errorf("fake QMP encode: %v", err)
			return
		}
	}
}

func (f *mappingQMP) monitor(command string, t *testing.T) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if command == "info usernet" {
		info := "Hub -1 (net0):\n"
		for guest, host := range f.ports {
			info += fmt.Sprintf(" TCP[HOST_FORWARD] 12 127.0.0.1 %d 10.0.2.15 %d 0 0\n", host, guest)
		}
		return info
	}
	var host, guest int
	if n, err := fmt.Sscanf(command, "hostfwd_add net0 tcp:127.0.0.1:%d-:%d", &host, &guest); err != nil || n != 2 {
		t.Errorf("unexpected QMP command %q", command)
		return "invalid command"
	}
	f.adds++
	if f.denyAll {
		return "Could not set up host forwarding rule"
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", host))
	if err != nil {
		return "Could not set up host forwarding rule"
	}
	f.listeners[host] = ln
	if f.stealFirst && f.adds == 1 {
		// Model a different host process winning the bind between the CLI's
		// reservation and QEMU's addition. It never appears in this VM's table.
		return "Could not set up host forwarding rule"
	}
	f.ports[guest] = host
	return ""
}

func freeMappingPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func TestTCPPortMappingSeparatesTwoVMsAndReusesTheirOwnForwards(t *testing.T) {
	s := shortQMPStore(t)
	one := startMappingQMP(t, s, "one")
	two := startMappingQMP(t, s, "two")
	preferred := freeMappingPort(t)
	p1, err := s.EnsureTCPPortMapping(context.Background(), "one", 8890, preferred)
	if err != nil || p1 != preferred {
		t.Fatalf("first VM mapping: %d %v", p1, err)
	}
	p2, err := s.EnsureTCPPortMapping(context.Background(), "two", 8890, preferred)
	if err != nil || p2 == p1 || p2 == 0 {
		t.Fatalf("second VM reused another VM's endpoint: %d %d %v", p1, p2, err)
	}
	for name, expected := range map[string]int{"one": p1, "two": p2} {
		got, err := s.EnsureTCPPortMapping(context.Background(), name, 8890, freeMappingPort(t))
		if err != nil || got != expected {
			t.Fatalf("%s did not reuse its live QMP mapping: %d %v", name, got, err)
		}
	}
	for _, fake := range []*mappingQMP{one, two} {
		fake.mu.Lock()
		if fake.adds != 1 || len(fake.ports) != 1 || fake.ports[8890] == 0 {
			t.Errorf("unexpected forwards: adds=%d ports=%v", fake.adds, fake.ports)
		}
		fake.mu.Unlock()
	}
}

func TestTCPPortMappingRetriesLostBindAndVerifiesHMPResults(t *testing.T) {
	s := shortQMPStore(t)
	fake := startMappingQMP(t, s, "test")
	fake.mu.Lock()
	fake.stealFirst = true
	fake.mu.Unlock()
	preferred := freeMappingPort(t)
	port, err := s.EnsureTCPPortMapping(context.Background(), "test", 8890, preferred)
	if err != nil || port == 0 || port == preferred {
		t.Fatalf("lost-bind retry: %d %v", port, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.adds != 2 || fake.ports[8890] != port {
		t.Fatalf("unverified host port returned: adds=%d ports=%v result=%d", fake.adds, fake.ports, port)
	}
}

func TestRobotSandboxMappingPersistsOnlyVerifiedOwnedPorts(t *testing.T) {
	s := shortQMPStore(t)
	if err := s.WriteMeta(Meta{Name: "test"}); err != nil {
		t.Fatal(err)
	}
	profile := testRobotProfile(t)
	profile.SandboxHostPort = freeMappingPort(t)
	if err := s.CreateRobotProfile("test", profile); err != nil {
		t.Fatal(err)
	}
	fake := startMappingQMP(t, s, "test")
	fake.mu.Lock()
	fake.denyAll = true
	fake.mu.Unlock()
	if port, err := s.EnsureRobotSandboxPort(context.Background(), "test"); port != 0 || err == nil {
		t.Fatalf("unverified HMP success accepted: %d %v", port, err)
	}
	got, _, err := s.ReadRobotProfile("test")
	if err != nil || got != profile {
		t.Fatalf("failed allocation rewrote profile: %+v %v", got, err)
	}
	fake.mu.Lock()
	if fake.adds != 4 {
		t.Errorf("allocation was not bounded: %d", fake.adds)
	}
	fake.denyAll = false
	fake.mu.Unlock()
	// A previous preferred port belongs to an unrelated host process now.
	busy, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", profile.SandboxHostPort))
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port, err := s.EnsureRobotSandboxPort(context.Background(), "test")
	if err != nil || port == 0 || port == profile.SandboxHostPort {
		t.Fatalf("stale preferred endpoint was reused: %d %v", port, err)
	}
	got, _, err = s.ReadRobotProfile("test")
	if err != nil || got.SandboxHostPort != port || got.SourceDigest != profile.SourceDigest || got.PolicyBundle != profile.PolicyBundle {
		t.Fatalf("verified allocation lost profile fields: %+v %v", got, err)
	}
	if second, err := s.EnsureRobotSandboxPort(context.Background(), "test"); err != nil || second != port {
		t.Fatalf("sandbox endpoint was not stable: %d %v", second, err)
	}
}

func TestTCPPortMappingRejectsInvalidInputsAndLifecycleRaces(t *testing.T) {
	s := shortQMPStore(t)
	for _, pair := range [][2]int{{0, 0}, {65536, 8890}, {8890, -1}, {8890, 65536}} {
		if _, err := s.EnsureTCPPortMapping(context.Background(), "test", pair[0], pair[1]); err == nil {
			t.Errorf("accepted invalid ports: %v", pair)
		}
	}
	if _, err := s.EnsureTCPPortMapping(context.Background(), "../test", 8890, 0); err == nil {
		t.Fatal("accepted invalid VM name")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.EnsureTCPPortMapping(ctx, "test", 8890, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("ignored cancellation: %v", err)
	}
	lock, err := s.acquireLifecycleLock("test")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := s.EnsureTCPPortMapping(context.Background(), "test", 8890, 0); !errors.Is(err, ErrLifecycleBusy) {
		t.Fatalf("mapping raced lifecycle operation: %v", err)
	}
}

func TestTCPPortMappingIgnoresForeignNetInterfacesAndEndpoints(t *testing.T) {
	for _, info := range []string{
		"Hub -1 (other):\n TCP[HOST_FORWARD] 12 127.0.0.1 19990 10.0.2.15 8890 0 0\n",
		"Hub -1 (net0):\n TCP[HOST_FORWARD] 12 0.0.0.0 19990 10.0.2.15 8890 0 0\n",
		"Hub -1 (net0):\n TCP[HOST_FORWARD] 12 127.0.0.1 19990 10.0.2.16 8890 0 0\n",
		"Hub -1 (net0):\n TCP[ESTABLISHED] 12 127.0.0.1 19990 10.0.2.15 8890 0 0\n",
	} {
		if got := tcpForwardHostPort(info, 8890); got != 0 {
			t.Errorf("accepted foreign forward %q: %d", strings.TrimSpace(info), got)
		}
	}
}
