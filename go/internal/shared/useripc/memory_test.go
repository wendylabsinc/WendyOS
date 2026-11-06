//go:build !windows

package useripc

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInProcessBrokerTransportLongSandboxPathAndUnpublish(t *testing.T) {
	previous := inProcess.Load()
	EnableInProcess()
	t.Cleanup(func() { inProcess.Store(previous) })
	dir := filepath.Join(t.TempDir(), strings.Repeat("ios-sandbox-", 15))
	if err := EnsureDirectory(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "broker.sock")
	l, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_, err = io.Copy(conn, conn)
		done <- err
	}()
	conn, err := Dial(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Write([]byte("authenticated broker protocol")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, len("authenticated broker protocol"))
	if _, err = io.ReadFull(conn, reply); err != nil || string(reply) != "authenticated broker protocol" {
		t.Fatalf("round trip: %q %v", reply, err)
	}
	conn.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if conn, err := Dial(ctx, path); err == nil {
		conn.Close()
		t.Fatal("unpublished broker still accepted a caller")
	}
	if replacement, err := Listen(path); err == nil {
		replacement.Close()
		t.Fatal("replacement started before unpublished broker drained")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("refused replacement republished an invalidated broker: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Accept(); err == nil {
		t.Fatal("closed listener accepted a stream")
	}
	if memoryExists(path) {
		t.Fatal("closed broker retained its registration")
	}
}
