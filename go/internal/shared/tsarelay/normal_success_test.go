package tsarelay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Normal opaque timestamp exchange through the same backend used by app TLS.
// Cryptographic TSA verification belongs to the requesting device.
func TestNormalTimestampRelaySuccess(t *testing.T) {
	query := make([]byte, 93)
	reply := make([]byte, 9<<10)
	_, _ = rand.Read(query)
	_, _ = rand.Read(reply)
	authority := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(body, query) || r.Method != http.MethodPost ||
			r.Header.Get("Content-Type") != "application/timestamp-query" {
			t.Error("normal timestamp request was not preserved", err)
		}
		_, _ = w.Write(reply)
	}))
	defer authority.Close()
	client, backend := net.Pipe()
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- forward(ctx, backend, authority.Client(), authority.URL) }()
	var header [8]byte
	copy(header[:4], "WNT1")
	binary.BigEndian.PutUint32(header[4:], uint32(len(query)))
	if _, err := client.Write(append(header[:], query...)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(client, header[:4]); err != nil {
		t.Fatal(err)
	}
	received, err := io.ReadAll(client)
	if err != nil || int(binary.BigEndian.Uint32(header[:4])) != len(reply) || !bytes.Equal(received, reply) {
		t.Fatal("normal timestamp reply was not preserved", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
