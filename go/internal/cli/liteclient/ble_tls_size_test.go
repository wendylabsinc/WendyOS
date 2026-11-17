package liteclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"net"
	"slices"
	"testing"
	"time"
)

// Capture the real Go ClientHello record without certificates or lab secrets.
func bleClientHello(t *testing.T, cfg *tls.Config) []byte {
	t.Helper()
	client, server := net.Pipe()
	defer server.Close()
	finished := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { defer client.Close(); finished <- tls.Client(client, cfg).HandshakeContext(ctx) }()
	header := make([]byte, 5)
	server.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(server, header); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, int(binary.BigEndian.Uint16(header[3:])))
	if _, err := io.ReadFull(server, body); err != nil {
		t.Fatal(err)
	}
	cancel()
	server.Close()
	<-finished
	if header[0] != 22 || len(body) < 4 || body[0] != 1 {
		t.Fatal("not ClientHello")
	}
	return append(header, body...)
}

func TestBLEClientHelloAvoidsUnsupportedHybridShare(t *testing.T) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: "lite.test"}
	smaller := bleTLSConfig(cfg)
	if len(cfg.CurvePreferences) != 0 {
		t.Fatal("shared caller policy mutated")
	}
	if !slices.Equal(smaller.CurvePreferences, []tls.CurveID{tls.X25519, tls.CurveP256, tls.CurveP384, tls.CurveP521}) {
		t.Fatal("unexpected BLE group policy")
	}
	baseline := bleClientHello(t, cfg)
	optimized := bleClientHello(t, smaller)
	if len(baseline)-len(optimized) < 1100 {
		t.Fatalf("expected removal of hybrid share: baseline=%d optimized=%d", len(baseline), len(optimized))
	}
	// Signature capabilities/identity verification are unrelated to key exchange.
	if smaller.MinVersion != cfg.MinVersion || smaller.ServerName != cfg.ServerName {
		t.Fatal("unrelated policy changed")
	}
	t.Logf("ClientHello bytes: default=%d BLE=%d saving=%d", len(baseline), len(optimized), len(baseline)-len(optimized))
	custom := cfg.Clone()
	custom.CurvePreferences = []tls.CurveID{tls.X25519MLKEM768, tls.CurveP256}
	if !slices.Equal(bleTLSConfig(custom).CurvePreferences, custom.CurvePreferences) {
		t.Fatal("explicit policy overridden")
	}
	// ALPN issuer hints are still sent verbatim.
	hinted := cfg.Clone()
	hinted.NextProtos = []string{"wendycom/1\x00issuer"}
	if !bytes.Contains(bleClientHello(t, bleTLSConfig(hinted)), []byte(hinted.NextProtos[0])) {
		t.Fatal("issuer hints lost")
	}
}
