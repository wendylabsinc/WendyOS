package useripc

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The same normal per-user broker transport contract runs on Unix sockets and
// Windows named pipes. It needs no device, network interface or TLS fixture.
func TestPrivateIPCNormalStreamOwnershipAndListenerDrain(t *testing.T) {
	root, err := os.MkdirTemp("", "wi-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	directory := filepath.Join(root, "session")
	if err := EnsureDirectory(directory); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(directory, "state.json")
	if err := os.WriteFile(state, []byte("{\"ready\":true}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFile(state, false); err != nil {
		t.Fatal(err)
	}
	if !OwnedPath(directory) {
		t.Fatal("normal private path lost its owner")
	}
	path := filepath.Join(directory, "broker.sock")
	listener, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			accepted <- connection
		}
	}()
	setup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	client, err := Dial(setup, path)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer client.Close()
	cancel()
	var server net.Conn
	select {
	case server = <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("normal local stream was not accepted")
	}
	defer server.Close()
	// Unpublishing only retires admission. Existing invocation-owned streams
	// continue until their leases finish, including quiet local work.
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := io.Copy(server, server); done <- err }()
	payload := bytes.Repeat([]byte("retained local invocation\n"), 4096)
	written := make(chan error, 1)
	go func() { _, err := client.Write(payload); written <- err }()
	if err := client.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	echoed := make([]byte, len(payload))
	if _, err := io.ReadFull(client, echoed); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(echoed, payload) {
		t.Fatal("local transport changed bytes")
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if err := client.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var byteBuffer [1]byte
	_, err = client.Read(byteBuffer[:])
	timeout, ok := err.(net.Error)
	if !ok || !timeout.Timeout() {
		t.Fatalf("normal idle deadline: %v", err)
	}
	client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("closed local stream did not release its worker")
	}
}
