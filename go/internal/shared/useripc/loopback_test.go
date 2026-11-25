package useripc

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMobileLoopbackLongPathAndMutualAuthentication(t *testing.T) {
	directory := filepath.Join(t.TempDir(), strings.Repeat("long-app-group-path-", 8))
	if err := EnsureDirectory(directory); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "broker.sock")
	listener, err := listenLoopback(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.SetDeadline(time.Now().Add(time.Second))
			io.Copy(conn, conn)
			conn.Close()
		}
	}()
	defer func() { listener.Close(); <-done }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := dialLoopback(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Write([]byte("mesh")); err != nil {
		t.Fatal(err)
	}
	var reply [4]byte
	if _, err = io.ReadFull(client, reply[:]); err != nil || string(reply[:]) != "mesh" {
		t.Fatal("authenticated IPC", err)
	}
	client.Close()
	data, _ := os.ReadFile(path)
	var identity loopbackIdentity
	json.Unmarshal(data, &identity)
	cfg, err := identity.tlsConfig(false)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Certificates = nil
	untrusted, err := (&tls.Dialer{Config: cfg}).DialContext(ctx, "tcp4", identity.Address)
	if err == nil {
		defer untrusted.Close()
		untrusted.SetDeadline(time.Now().Add(time.Second))
		untrusted.Write([]byte("bad"))
		if _, err = untrusted.Read(reply[:]); err == nil {
			t.Fatal("IPC accepted a caller without the shared identity")
		}
	}
	otherPath := filepath.Join(directory, "other.sock")
	other, err := listenLoopback(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	otherData, _ := os.ReadFile(otherPath)
	var wrong loopbackIdentity
	json.Unmarshal(otherData, &wrong)
	wrong.Address = identity.Address
	wrongData, _ := json.Marshal(wrong)
	os.WriteFile(otherPath, wrongData, 0600)
	if conn, err := dialLoopback(ctx, otherPath); err == nil {
		conn.Close()
		t.Fatal("IPC accepted a different server identity")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("closed listener retained credential file")
	}
	if conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", identity.Address); err == nil {
		conn.Close()
		t.Fatal("closed listener still accepts")
	}
}
