package bleprovider

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestHandshakeMeterDisabled(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	for _, level := range []zap.AtomicLevel{zap.NewAtomicLevelAt(zap.InfoLevel), zap.NewAtomicLevelAt(zap.ErrorLevel)} {
		core, logs := observer.New(level)
		raw, m := meterHandshake(a, zap.New(core))
		if raw != a || m != nil {
			t.Fatal("disabled debug changed the raw connection")
		}
		m.start()
		m.finish().log(zap.New(core), "outbound", 1, tls.ConnectionState{}, false)
		if logs.Len() != 0 {
			t.Fatal("disabled debug emitted telemetry")
		}
	}
	raw, m := meterHandshake(a, nil)
	if raw != a || m != nil {
		t.Fatal("nil logger wrapped connection")
	}
}

func metricsTLSConfigs(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mesh.test"}, DNSNames: []string{"mesh.test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	identity := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}
	server := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{identity}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, NextProtos: []string{ALPN}}
	client := &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{identity}, RootCAs: pool, ServerName: "mesh.test", NextProtos: []string{ALPN}, ClientSessionCache: tls.NewLRUClientSessionCache(4)}
	return server, client
}

type metricsResult struct {
	conn        *tls.Conn
	meter       *handshakeMeter
	measurement *handshakeMeasurement
	err         error
}

func metricsHandshakePair(t *testing.T, serverCfg, clientCfg *tls.Config) (metricsResult, metricsResult, *observer.ObservedLogs) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	rawClient, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	rawServer, err := listener.Accept()
	if err != nil {
		rawClient.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { rawClient.Close(); rawServer.Close() })
	rawClient.SetDeadline(time.Now().Add(3 * time.Second))
	rawServer.SetDeadline(time.Now().Add(3 * time.Second))
	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)
	run := func(raw net.Conn, cfg *tls.Config, server bool) metricsResult {
		measured, meter := meterHandshake(raw, logger)
		secure := tls.Client(measured, cfg)
		direction := "outbound"
		if server {
			secure = tls.Server(measured, cfg)
			direction = "inbound"
		}
		meter.start()
		err := secure.HandshakeContext(context.Background())
		result := meter.finish()
		result.log(logger, direction, 42, secure.ConnectionState(), err == nil)
		return metricsResult{secure, meter, result, err}
	}
	done := make(chan metricsResult, 1)
	go func() { done <- run(rawServer, serverCfg, true) }()
	client := run(rawClient, clientCfg, false)
	server := <-done
	return server, client, logs
}

func TestHandshakeMeterRealTLSFullAndResumed(t *testing.T) {
	serverCfg, clientCfg := metricsTLSConfigs(t)
	var fullTotal uint64
	for attempt := 0; attempt < 2; attempt++ {
		server, client, logs := metricsHandshakePair(t, serverCfg, clientCfg)
		for _, r := range []metricsResult{server, client} {
			if r.err != nil {
				t.Fatal(r.err)
			}
			if r.conn.ConnectionState().DidResume != (attempt == 1) {
				t.Fatalf("attempt %d resumed=%v", attempt, r.conn.ConnectionState().DidResume)
			}
			if r.measurement.read == 0 || r.measurement.written == 0 || !r.measurement.completed.After(r.measurement.started) {
				t.Fatal("empty handshake measurements")
			}
		}
		if logs.Len() != 2 {
			t.Fatalf("records=%d", logs.Len())
		}
		for _, entry := range logs.All() {
			fields := entry.ContextMap()
			if fields["tls_complete"] != true || fields["identity_accepted"] != true || fields["session_resumed"] != (attempt == 1) {
				t.Fatalf("bad record: %v", fields)
			}
			measured := client.measurement
			if fields["direction"] == "inbound" {
				measured = server.measurement
			} else if fields["direction"] != "outbound" {
				t.Fatal(fields)
			}
			if fields["socket_read_bytes"] != measured.read || fields["socket_write_bytes"] != measured.written || fields["started_unix_ns"] != measured.started.UnixNano() || fields["completed_unix_ns"] != measured.completed.UnixNano() {
				t.Fatalf("logged endpoint scope differs from frozen measurement: %v", fields)
			}
		}
		total := server.measurement.read + server.measurement.written + client.measurement.read + client.measurement.written
		if attempt == 0 {
			fullTotal = total
		} else if total >= fullTotal {
			t.Fatalf("resumed bytes %d >= full %d", total, fullTotal)
		}
		frozen := *client.measurement
		// Reading application data also processes the server's post-handshake ticket.
		done := make(chan error, 1)
		go func() { _, err := server.conn.Write(make([]byte, 8192)); done <- err }()
		if _, err := io.CopyN(io.Discard, client.conn, 8192); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if *client.measurement != frozen {
			t.Fatal("application traffic changed completed handshake snapshot")
		}
		if client.meter.read.Load() <= frozen.read {
			t.Fatal("test failed to exercise later socket traffic")
		}
	}
}

func TestHandshakeMeterRealTLSRejected(t *testing.T) {
	for _, reject := range []string{"client-rejects-server", "server-rejects-client"} {
		t.Run(reject, func(t *testing.T) {
			serverCfg, clientCfg := metricsTLSConfigs(t)
			if reject == "client-rejects-server" {
				clientCfg.RootCAs = x509.NewCertPool()
			} else {
				serverCfg.ClientCAs = x509.NewCertPool()
			}
			server, client, logs := metricsHandshakePair(t, serverCfg, clientCfg)
			if server.err == nil {
				t.Fatal("server unexpectedly accepted failed mutual authentication")
			}
			if reject == "client-rejects-server" && client.err == nil {
				t.Fatal("untrusted server accepted")
			}
			if logs.Len() != 2 {
				t.Fatal("missing rejection endpoint measurement")
			}
			for i, r := range []metricsResult{server, client} {
				if r.measurement.read == 0 || r.measurement.written == 0 {
					t.Fatalf("endpoint %d empty rejection bytes", i)
				}
			}
			for _, entry := range logs.All() {
				fields := entry.ContextMap()
				if fields["direction"] == "inbound" && (fields["identity_accepted"] != false || fields["tls_complete"] != false) {
					t.Fatal(fields)
				}
				// TLS 1.3 clients can finish locally before the server rejects their certificate.
				if fields["direction"] == "outbound" && fields["identity_accepted"] != (client.err == nil) {
					t.Fatal(fields)
				}
			}
		})
	}
}
