package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

// newTestDataSocketManager returns a manager rooted at a throwaway socket
// directory, with peer-credential verification neutralized (fail open) so the
// rate-limit and record-kind tests exercise only the behavior they target.
func newTestDataSocketManager(t *testing.T) *AppDataSocketManager {
	t.Helper()
	capture, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	socketRoot, err := os.MkdirTemp("/tmp", "wendy-data-hardening-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socketRoot) })
	oldRoot := AppDataSocketRootPath
	AppDataSocketRootPath = socketRoot
	t.Cleanup(func() { AppDataSocketRootPath = oldRoot })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := NewAppDataSocketManager(ctx, nil, capture)
	m.peerCred = func(net.Conn) (peerCredentials, error) {
		return peerCredentials{}, fmt.Errorf("%w: test seam", errPeerCredUnavailable)
	}
	return m
}

func sendRecord(t *testing.T, conn net.Conn, record data.ApplicationRecord) dataAck {
	t.Helper()
	body, _ := json.Marshal(record)
	if err := writeDataFrame(conn, json.RawMessage(body)); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	ackBody, err := readDataFrame(conn)
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	var ack dataAck
	if err := json.Unmarshal(ackBody, &ack); err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	return ack
}

// TestAppDataRateLimitIsPerAppNotPerConnection proves a second connection for
// the same app draws from the same rate budget instead of getting a fresh one.
func TestAppDataRateLimitIsPerAppNotPerConnection(t *testing.T) {
	m := newTestDataSocketManager(t)
	// A deterministic 2-record budget that does not refill during the test.
	m.newLimiter = func() *notificationRateLimiter {
		l := newNotificationRateLimiter(2, time.Hour)
		return &l
	}
	dir, err := m.Ensure("com.example.a", "")
	if err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(dir, DataSocketFilename)

	conn1, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn1.Close()
	rec := data.ApplicationRecord{Version: 1, Type: "event", Name: "ready", ClientBootID: "unavailable"}
	if ack := sendRecord(t, conn1, rec); ack.State != "buffered" {
		t.Fatalf("conn1 record 1: state = %q, want buffered", ack.State)
	}
	if ack := sendRecord(t, conn1, rec); ack.State != "buffered" {
		t.Fatalf("conn1 record 2: state = %q, want buffered", ack.State)
	}

	// The per-app budget is now exhausted; a brand-new connection must not get
	// its own allotment. serveConn checks the shared budget BEFORE its first
	// read and closes the connection right after writing the rejection ack, so
	// this write races that close and can fail with EPIPE (or ECONNRESET).
	// That early close only happens on the rejection path: a connection with
	// its own fresh allotment would sit in readDataFrame, accept the record,
	// and answer "buffered" on a still-open connection. So a broken write
	// followed by the rejection ack still waiting in the socket buffer, or by
	// end-of-stream, proves the same contract as a clean write/read pair.
	conn2, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	body, _ := json.Marshal(rec)
	writeErr := writeDataFrame(conn2, json.RawMessage(body))
	if writeErr != nil && !errors.Is(writeErr, syscall.EPIPE) && !errors.Is(writeErr, syscall.ECONNRESET) {
		t.Fatalf("write frame: %v", writeErr)
	}
	ackBody, err := readDataFrame(conn2)
	if err != nil {
		if writeErr != nil && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET)) {
			// The server closed before the write landed and no ack survived;
			// only the rejection path closes before reading a frame.
			return
		}
		t.Fatalf("read ack: %v (write err: %v)", err, writeErr)
	}
	var ack dataAck
	if err := json.Unmarshal(ackBody, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.State != "rejected" || ack.Error != "rate limit exceeded" {
		t.Fatalf("conn2 record: ack = %+v, want rejected/rate limit exceeded (budget must be shared)", ack)
	}
}

// TestAppDataUnknownKindRejectedNotFatal shows an unknown record kind gets a
// clean rejected ack and does not tear down the connection.
func TestAppDataUnknownKindRejectedNotFatal(t *testing.T) {
	m := newTestDataSocketManager(t)
	dir, err := m.Ensure("com.example.a", "")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", filepath.Join(dir, DataSocketFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ack := sendRecord(t, conn, data.ApplicationRecord{Version: 1, Type: "telemetry", Name: "x", ClientBootID: "unavailable"})
	if ack.State != "rejected" {
		t.Fatalf("unknown kind: state = %q, want rejected", ack.State)
	}

	// The connection survives: a valid record on the same connection is still
	// accepted.
	ack = sendRecord(t, conn, data.ApplicationRecord{Version: 1, Type: "event", Name: "ready", ClientBootID: "unavailable"})
	if ack.State != "buffered" {
		t.Fatalf("valid record after unknown kind: state = %q, want buffered (connection must survive)", ack.State)
	}
}

// TestAppDataPeerCredentialMismatchRefused injects a peer whose cgroup names a
// different app and asserts the connection is refused.
func TestAppDataPeerCredentialMismatchRefused(t *testing.T) {
	m := newTestDataSocketManager(t)
	m.peerCred = func(net.Conn) (peerCredentials, error) {
		return peerCredentials{UID: 0, PID: 4242}, nil
	}
	m.cgroupOfPID = func(int32) (string, error) {
		return "0::/system.slice/edge-agent-com.example.other.scope\n", nil
	}
	dir, err := m.Ensure("com.example.a", "")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", filepath.Join(dir, DataSocketFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ackBody, err := readDataFrame(conn)
	if err != nil {
		t.Fatalf("expected a rejection ack, got read error: %v", err)
	}
	var ack dataAck
	if err := json.Unmarshal(ackBody, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.State != "rejected" {
		t.Fatalf("mismatched peer: state = %q, want rejected", ack.State)
	}
}

// TestAppDataPeerCredentialMatchAllowed shows a peer whose cgroup names the
// socket's own app is admitted.
func TestAppDataPeerCredentialMatchAllowed(t *testing.T) {
	m := newTestDataSocketManager(t)
	m.peerCred = func(net.Conn) (peerCredentials, error) {
		return peerCredentials{UID: 0, PID: 4242}, nil
	}
	m.cgroupOfPID = func(int32) (string, error) {
		return "0::/system.slice/edge-agent-com.example.a@worker.scope\n", nil
	}
	dir, err := m.Ensure("com.example.a", "")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", filepath.Join(dir, DataSocketFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ack := sendRecord(t, conn, data.ApplicationRecord{Version: 1, Type: "event", Name: "ready", ClientBootID: "unavailable"})
	if ack.State != "buffered" {
		t.Fatalf("matching peer (service suffix stripped): state = %q, want buffered", ack.State)
	}
}

// readRejectAck reads a single ack frame off conn and fails unless it is a
// rejection. Used by the fail-closed peer-credential tests below.
func readRejectAck(t *testing.T, conn net.Conn) dataAck {
	t.Helper()
	ackBody, err := readDataFrame(conn)
	if err != nil {
		t.Fatalf("expected a rejection ack, got read error: %v", err)
	}
	var ack dataAck
	if err := json.Unmarshal(ackBody, &ack); err != nil {
		t.Fatal(err)
	}
	return ack
}

// TestAppDataPeerCredentialUnreadableCgroupRefused proves the accept-to-/proc
// TOCTOU is closed. Once SO_PEERCRED yields a pid, a cgroup that cannot be read
// (the peer exited between accept and the /proc read, or its pid was recycled)
// must be REFUSED, not admitted. Fail-open here would let a peer that buffered
// forged records and then exited have those records attributed to this app.
func TestAppDataPeerCredentialUnreadableCgroupRefused(t *testing.T) {
	m := newTestDataSocketManager(t)
	m.peerCred = func(net.Conn) (peerCredentials, error) {
		return peerCredentials{UID: 0, PID: 4242}, nil
	}
	m.cgroupOfPID = func(int32) (string, error) {
		// The peer has exited by the time verifyPeer reads /proc/<pid>/cgroup.
		return "", os.ErrNotExist
	}
	dir, err := m.Ensure("com.example.a", "")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", filepath.Join(dir, DataSocketFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if ack := readRejectAck(t, conn); ack.State != "rejected" {
		t.Fatalf("unreadable peer cgroup: state = %q, want rejected (fail closed)", ack.State)
	}
}

// TestAppDataPeerCredentialNoScopeRefused proves a peer that resolves to no
// wendy app scope (a host process, or a recycled pid now held by one) is
// refused once its credentials are known, rather than admitted under the
// socket's app identity.
func TestAppDataPeerCredentialNoScopeRefused(t *testing.T) {
	m := newTestDataSocketManager(t)
	m.peerCred = func(net.Conn) (peerCredentials, error) {
		return peerCredentials{UID: 0, PID: 4242}, nil
	}
	m.cgroupOfPID = func(int32) (string, error) {
		return "0::/user.slice/user-1000.slice/session-3.scope\n", nil
	}
	dir, err := m.Ensure("com.example.a", "")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", filepath.Join(dir, DataSocketFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if ack := readRejectAck(t, conn); ack.State != "rejected" {
		t.Fatalf("no-scope peer: state = %q, want rejected (fail closed)", ack.State)
	}
}

// TestAppDataPeerCredentialCgroupfsMatchAllowed proves the socket admits a peer
// whose cgroupfs-driver cgroup (literal "system.slice:{svc}:{appID}" path
// segment) resolves to the socket's own app. This exercises the full
// verifyPeer -> appIDFromCgroup path for the cgroupfs driver, not just the
// systemd form the other socket-level tests use.
func TestAppDataPeerCredentialCgroupfsMatchAllowed(t *testing.T) {
	m := newTestDataSocketManager(t)
	m.peerCred = func(net.Conn) (peerCredentials, error) {
		return peerCredentials{UID: 0, PID: 4242}, nil
	}
	m.cgroupOfPID = func(int32) (string, error) {
		return "0::/system.slice/system.slice:edge-agent:com.example.a@worker\n", nil
	}
	dir, err := m.Ensure("com.example.a", "")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", filepath.Join(dir, DataSocketFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ack := sendRecord(t, conn, data.ApplicationRecord{Version: 1, Type: "event", Name: "ready", ClientBootID: "unavailable"})
	if ack.State != "buffered" {
		t.Fatalf("matching cgroupfs peer: state = %q, want buffered", ack.State)
	}
}

// TestAppDataPeerCredentialCgroupfsMismatchRefused proves the socket fails
// closed under the cgroupfs driver: a peer that belongs to a DIFFERENT app is
// refused, exactly as it is under systemd. This is the core D6 forgery guard,
// verified on the cgroupfs code path that PR #1755 introduces.
func TestAppDataPeerCredentialCgroupfsMismatchRefused(t *testing.T) {
	m := newTestDataSocketManager(t)
	m.peerCred = func(net.Conn) (peerCredentials, error) {
		return peerCredentials{UID: 0, PID: 4242}, nil
	}
	m.cgroupOfPID = func(int32) (string, error) {
		// Attacker owns com.attacker and tries to append a victim-named child
		// cgroup. The real (parent) scope still wins, so the peer resolves to
		// com.attacker and is refused on the com.victim socket.
		return "0::/system.slice/system.slice:edge-agent:com.attacker/system.slice:edge-agent:com.victim\n", nil
	}
	dir, err := m.Ensure("com.victim", "")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", filepath.Join(dir, DataSocketFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if ack := readRejectAck(t, conn); ack.State != "rejected" {
		t.Fatalf("cgroupfs cross-app forgery: state = %q, want rejected (fail closed)", ack.State)
	}
}

// TestAppDataRealCgroupOfTestProcessRefused pins the platform truth that every
// other socket-level test here stubs away: with the PRODUCTION cgroup reader in
// place, a peer that is a real local process outside any wendy app scope is
// refused. The go-test process itself is such a peer, so this asserts the same
// fail-closed outcome on both platforms without branching on either: on Linux
// readProcCgroup reads /proc/<pid>/cgroup and finds no wendy app scope, and on
// non-Linux it reports the lookup unsupported, which is equally unattributable.
//
// This is the case a socket-level happy-path test must inject around, and the
// reason macOS-only verification cannot see the hardened behavior at all.
func TestAppDataRealCgroupOfTestProcessRefused(t *testing.T) {
	m := newTestDataSocketManager(t)
	// Only the SO_PEERCRED lookup is stubbed, and it reports this very process.
	// cgroupOfPID stays at the production readProcCgroup.
	m.peerCred = func(net.Conn) (peerCredentials, error) {
		return peerCredentials{UID: uint32(os.Getuid()), PID: int32(os.Getpid())}, nil
	}
	dir, err := m.Ensure("com.example.a", "")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", filepath.Join(dir, DataSocketFilename))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if ack := readRejectAck(t, conn); ack.State != "rejected" {
		t.Fatalf("test process as peer: state = %q, want rejected (the go-test process is in no wendy app scope)", ack.State)
	}
}

// TestAppIDFromCgroup covers the cgroup identity extraction directly.
func TestAppIDFromCgroup(t *testing.T) {
	cases := []struct {
		name    string
		cgroup  string
		want    string
		present bool
	}{
		{"single service", "0::/system.slice/edge-agent-com.example.a.scope\n", "com.example.a", true},
		{"multi service strips suffix", "0::/system.slice/edge-agent-com.example.a@worker.scope\n", "com.example.a", true},
		{"non-wendy process", "0::/user.slice/user-1000.slice/session-3.scope\n", "", false},
		{"empty", "", "", false},
		// cgroupfs driver: runc takes the "slice:prefix:name" CgroupsPath as a
		// literal directory name instead of translating it to a systemd scope
		// (observed on WendyOS 0.16.0 devices).
		{"cgroupfs single service", "0::/system.slice/system.slice:edge-agent:com.example.a\n", "com.example.a", true},
		{"cgroupfs multi service strips suffix", "0::/system.slice/system.slice:edge-agent:com.example.a@worker\n", "com.example.a", true},
		{"cgroupfs other systemd service", "0::/system.slice/system.slice:other-agent:com.example.a\n", "", false},
		// FORGERY ATTEMPT (cgroupfs): a peer that owns app com.attacker and has
		// write access to its own delegated cgroup subtree creates a child
		// cgroup literally named after the victim's scope and moves itself into
		// it. The peer's true (parent) scope segment always precedes any child
		// it can create, so first-match returns the attacker's real id, never
		// the victim's. Fail closed against the mismatch downstream.
		{"cgroupfs child cgroup forgery yields parent id", "0::/system.slice/system.slice:edge-agent:com.attacker/system.slice:edge-agent:com.victim\n", "com.attacker", true},
		// FORGERY ATTEMPT (systemd): same delegation attack expressed as nested
		// scope units. Parent scope still wins.
		{"systemd child scope forgery yields parent id", "0::/system.slice/edge-agent-com.attacker.scope/edge-agent-com.victim.scope\n", "com.attacker", true},
		// FAIL CLOSED (cgroupfs): an empty suffix is not a valid app identity;
		// it must not resolve to any app.
		{"cgroupfs empty suffix", "0::/system.slice/system.slice:edge-agent:\n", "", false},
		// FAIL CLOSED (cgroupfs): empty appID with only a service suffix.
		{"cgroupfs empty appID with service", "0::/system.slice/system.slice:edge-agent:@worker\n", "", false},
		// FAIL CLOSED (cgroupfs): the exact-service prefix must match; a service
		// whose name merely shares a prefix with edge-agent is not edge-agent.
		{"cgroupfs service prefix is not a match", "0::/system.slice/system.slice:edge-agent-x:com.example.a\n", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := appIDFromCgroup(tc.cgroup)
			if ok != tc.present || got != tc.want {
				t.Fatalf("appIDFromCgroup(%q) = (%q, %v), want (%q, %v)", tc.cgroup, got, ok, tc.want, tc.present)
			}
		})
	}
}
