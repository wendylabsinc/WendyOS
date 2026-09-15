package services

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

// TestAppDataSocketRefusesConnectionsOverPerAppLimit covers the unbounded
// accept loop: the per-app rate limiter bounds records, not connections, so a
// reconnect loop cost one goroutine and one descriptor per attempt with nothing
// to stop it. The cap+1th live connection must be refused with a reason the
// client can read.
func TestAppDataSocketRefusesConnectionsOverPerAppLimit(t *testing.T) {
	manager := newTestDataSocketManager(t)
	dir, err := manager.Ensure("com.example.a", "")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, DataSocketFilename)

	// Hold the cap open. Each connection sends one record and waits for its
	// ack, which is what proves the manager has counted it live before the next
	// dial: an accepted-but-unserved connection would make this racy.
	held := make([]net.Conn, 0, dataMaxLiveConnectionsPerApp)
	for i := 0; i < dataMaxLiveConnectionsPerApp; i++ {
		conn, dialErr := net.Dial("unix", socket)
		if dialErr != nil {
			t.Fatalf("connection %d refused: %v", i, dialErr)
		}
		t.Cleanup(func() { conn.Close() })
		held = append(held, conn)
		if ack := exchangeRecord(t, conn, fmt.Sprintf("ready-%d", i)); ack.State != "buffered" {
			t.Fatalf("connection %d got ack %+v, want a buffered record", i, ack)
		}
	}

	over, err := net.Dial("unix", socket)
	if err != nil {
		// A refusal at the kernel backlog would also be a refusal, but the
		// point of the cap is that the client is told why.
		t.Fatalf("connection %d was refused without a reason: %v", dataMaxLiveConnectionsPerApp, err)
	}
	defer over.Close()
	// Without the cap this connection is simply accepted and waits for a record
	// that never comes, so bound the read: the test must fail, not hang.
	if err = over.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	body, err := readDataFrame(over)
	if err != nil {
		t.Fatalf("connection over the cap was closed without an ack: %v", err)
	}
	var ack dataAck
	if err = json.Unmarshal(body, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.State != "rejected" || !strings.Contains(ack.Error, "too many open connections") {
		t.Fatalf("connection over the cap got ack %+v, want a rejection naming the connection limit", ack)
	}

	// Closing one accepted connection returns its slot, so the cap throttles
	// rather than permanently locking the app out.
	held[0].Close()
	waitForLiveConnections(t, manager, "com.example.a", dataMaxLiveConnectionsPerApp-1)
	replacement, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if ack := exchangeRecord(t, replacement, "after-close"); ack.State != "buffered" {
		t.Fatalf("replacement connection got ack %+v, want a buffered record", ack)
	}
}

// exchangeRecord writes one valid application record and reads the ack.
func exchangeRecord(t *testing.T, conn net.Conn, name string) dataAck {
	t.Helper()
	return sendRecord(t, conn, data.ApplicationRecord{Version: 1, Type: "event", Name: name, ClientBootID: "unavailable"})
}

// waitForLiveConnections blocks until the manager has observed a closed
// connection. The serving goroutine returns the slot asynchronously, so
// polling is the honest way to wait for it.
func waitForLiveConnections(t *testing.T, m *AppDataSocketManager, appID string, want int) {
	t.Helper()
	key := appDataKey(appID)
	for i := 0; i < 200; i++ {
		m.mu.Lock()
		socket := m.sockets[key]
		live := -1
		if socket != nil {
			live = socket.live
		}
		m.mu.Unlock()
		if live == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("live connection count never reached %d", want)
}

// TestAppDataSocketSweepRemovesOnlyOrphanedRoots covers the directory that no
// Release call can ever name: deleting the only service of an app removes the
// container, and with it the only record of the app identity whose hash named
// the directory.
func TestAppDataSocketSweepRemovesOnlyOrphanedRoots(t *testing.T) {
	manager := newTestDataSocketManager(t)
	if _, err := manager.Ensure("com.example.live", ""); err != nil {
		t.Fatal(err)
	}
	served := filepath.Join(AppDataSocketRootPath, appDataKey("com.example.live"))
	orphan := filepath.Join(AppDataSocketRootPath, appDataKey("com.example.gone"))
	if err := os.MkdirAll(orphan, 0o750); err != nil {
		t.Fatal(err)
	}
	// A directory for an app that still has containers but whose socket this
	// manager is not serving: it must survive, because the caller named it.
	inactive := filepath.Join(AppDataSocketRootPath, appDataKey("com.example.stopped"))
	if err := os.MkdirAll(inactive, 0o750); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(AppDataSocketRootPath, "not-a-directory")
	if err := os.WriteFile(stray, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	manager.SweepOrphanedRoots([]string{"com.example.live", "com.example.stopped"})

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphaned directory survived the sweep: %v", err)
	}
	for _, keep := range []string{served, inactive, stray} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("sweep removed %s: %v", keep, err)
		}
	}

	// A sweep that names no app at all must still spare a directory this
	// manager is actively serving: the in-memory entry is proof of an owner.
	manager.SweepOrphanedRoots(nil)
	if _, err := os.Stat(served); err != nil {
		t.Fatalf("sweep removed a directory it is serving: %v", err)
	}
}
