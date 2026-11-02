package tsarelay

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBoundedFixedTimestampRelay(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "POST" || r.URL.Path != "/timestamp" || r.Header.Get("Content-Type") != "application/timestamp-query" {
			t.Error("relay changed fixed HTTP request")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "nonce-bound-request" {
			t.Error("request changed")
		}
		_, _ = w.Write([]byte("signed-timestamp"))
	}))
	defer server.Close()
	for _, size := range []uint32{19, 0, 129, 0xffffffff} {
		client, remote := net.Pipe()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		done := make(chan error, 1)
		go func() { done <- forward(ctx, remote, server.Client(), server.URL+"/timestamp") }()
		var header [8]byte
		copy(header[:], "WNT1")
		binary.BigEndian.PutUint32(header[4:], size)
		_, _ = client.Write(header[:])
		if size == 19 {
			_, _ = client.Write([]byte("nonce-bound-request"))
			var length [4]byte
			if _, err := io.ReadFull(client, length[:]); err != nil {
				t.Fatal(err)
			}
			if binary.BigEndian.Uint32(length[:]) != 16 {
				t.Fatal("wrong reply frame", length)
			}
			body, err := io.ReadAll(client)
			if err != nil || string(body) != "signed-timestamp" {
				t.Fatal(string(body), err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		} else if err := <-done; err == nil {
			t.Fatal("accepted invalid request size", size)
		}
		client.Close()
		cancel()
	}
	if calls.Load() != 1 {
		t.Fatal("invalid frames reached authority", calls.Load())
	}
}

func TestTimestampReplyLimitAndHTTPFailure(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if oversized {
				_, _ = w.Write(make([]byte, 65537))
			} else {
				w.WriteHeader(http.StatusBadGateway)
			}
		}))
		client, remote := net.Pipe()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		done := make(chan error, 1)
		go func() { done <- forward(ctx, remote, server.Client(), server.URL) }()
		_, _ = client.Write([]byte{'W', 'N', 'T', '1', 0, 0, 0, 1, 42})
		if reply, err := io.ReadAll(client); len(reply) != 0 || err != nil {
			t.Fatal("invalid HTTP reply reached device", len(reply), err)
		}
		if err := <-done; err == nil {
			t.Fatal("invalid authority result accepted")
		}
		client.Close()
		cancel()
		server.Close()
	}
}

func TestCarrierCancellationClosesIncompleteRequests(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, listener) }()
	var clients []net.Conn
	for i := 0; i < 5; i++ {
		conn, err := net.DialTimeout("tcp4", listener.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		clients = append(clients, conn)
	}
	// Four workers may wait on a header. A fifth must be refused promptly.
	_ = clients[4].SetReadDeadline(time.Now().Add(2 * time.Second))
	var byte [1]byte
	if _, err := clients[4].Read(byte[:]); err != io.EOF {
		t.Fatalf("excess request was not refused: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("carrier stop retained incomplete clients")
	}
	for _, conn := range clients[:4] {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := conn.Read(byte[:]); err != io.EOF {
			t.Fatalf("client retained after cancellation: %v", err)
		}
	}
}
