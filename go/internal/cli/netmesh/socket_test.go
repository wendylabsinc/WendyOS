package netmesh

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSocketDialPreservesBufferedPayloadAndOutlivesContext(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "wmesh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	listener, err := net.Listen("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		reader := bufio.NewReader(connection)
		line, err := readLine(reader, 4096)
		if err != nil {
			return
		}
		var request Request
		if json.Unmarshal(line, &request) != nil {
			return
		}
		connection.Write([]byte("{\"status\":{}}\nhello"))
		io.Copy(connection, reader)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	connection, err := Dial(ctx, dir, "robot", 50052)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() { connection.Close(); <-done }()
	cancel()
	connection.SetDeadline(time.Now().Add(time.Second))
	first := make([]byte, 5)
	if _, err := io.ReadFull(connection, first); err != nil || string(first) != "hello" {
		t.Fatalf("lost buffered payload %q %v", first, err)
	}
	if _, err := connection.Write([]byte("world")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(connection, first); err != nil || string(first) != "world" {
		t.Fatalf("dial context closed established stream: %q %v", first, err)
	}
}
func TestSocketLimitsAndPrivateDirectory(t *testing.T) {
	if _, err := readLine(bufio.NewReader(strings.NewReader(strings.Repeat("a", 4097)+"\n")), 4096); err == nil {
		t.Fatal("oversized request accepted")
	}
	if _, err := readLine(bufio.NewReader(bytes.NewReader([]byte("{}\n"))), 4096); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	if err := PrivateDirectory(link); err == nil {
		t.Fatal("symlink directory accepted")
	}
}
