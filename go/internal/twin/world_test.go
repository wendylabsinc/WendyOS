package twin

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTwinWorldDisabledPreservesRoomService(t *testing.T) {
	directory := t.TempDir()
	store, err := NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	service := &Server{Store: store}
	ready := make(chan string, 1)
	go func() { done <- service.ServeWithWorld(ctx, ln, WorldOptions{Ready: func(s string) { ready <- s }}) }()
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://" + ln.Addr().String() + "/v1/rooms")
	if err != nil {
		cancel()
		<-done
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	cancel()
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("twin did not stop")
	}
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("existing room service: status=%d error=%v", response.StatusCode, err)
	}
	if len(ready) != 0 {
		t.Fatal("disabled World reported a listener")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("disabled World created state: %v %v", entries, err)
	}
}

func TestTwinInvalidWorldConfigurationClosesListener(t *testing.T) {
	directory := t.TempDir()
	store, err := NewStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	err = (&Server{Store: store}).ServeWithWorld(t.Context(), ln, WorldOptions{ConfigPath: filepath.Join(directory, "missing.json")})
	if err == nil {
		t.Fatal("explicit missing World configuration accepted")
	}
	conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("startup failure retained twin listener")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("missing configuration created state: %v %v", entries, err)
	}
}
