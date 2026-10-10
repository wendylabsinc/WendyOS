package meshcatalog

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestInventoryTLSResumptionAcrossALPNFallback(t *testing.T) {
	f := newFixture(t)
	catalog, _ := f.newCatalog(t, 534, "default", nil, nil)
	runtime, _ := NewRuntime(catalog, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
	clientConfig := func() *tls.Config {
		c, err := f.creds[533].PeerTLSWithTickets(534, syncALPNv3, "catalog-tls")
		if err != nil {
			t.Fatal(err)
		}
		c.NextProtos = []string{syncALPNv3, syncALPNv2, syncALPN}
		return c
	}
	serverReject, serverCheckedResume := false, false
	handshake := func(alpns []string, client *tls.Config) (tls.ConnectionState, tls.ConnectionState, error) {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		type result struct {
			state tls.ConnectionState
			err   error
		}
		serverResult := make(chan result, 1)
		config := runtime.serverTLS()
		config.NextProtos = alpns
		baseVerify := config.VerifyConnection
		config.VerifyConnection = func(s tls.ConnectionState) error {
			if err := baseVerify(s); err != nil {
				return err
			}
			if serverReject {
				serverCheckedResume = s.DidResume
				return errors.New("server current trust revoked")
			}
			return nil
		}
		go func() {
			raw, err := listener.Accept()
			if err != nil {
				serverResult <- result{err: err}
				return
			}
			defer raw.Close()
			_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
			conn := tls.Server(raw, config)
			err = conn.Handshake()
			if err == nil {
				_, err = conn.Write([]byte("x"))
			}
			serverResult <- result{conn.ConnectionState(), err}
		}()
		raw, err := net.Dial("tcp4", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
		conn := tls.Client(raw, client)
		err = conn.Handshake()
		if err == nil {
			var b [1]byte
			_, err = io.ReadFull(conn, b[:])
		}
		if err != nil {
			_ = raw.Close()
		}
		server := <-serverResult
		if err == nil {
			err = server.err
		}
		return conn.ConnectionState(), server.state, err
	}
	for _, tc := range []struct {
		name   string
		protos []string
		want   string
		resume bool
	}{
		{"new cold", []string{syncALPNv3, syncALPNv2, syncALPN}, syncALPNv3, false},
		{"new warm", []string{syncALPNv3, syncALPNv2, syncALPN}, syncALPNv3, true},
		{"fallback2 cold", []string{syncALPNv2, syncALPN}, syncALPNv2, false},
		{"fallback2 warm", []string{syncALPNv2, syncALPN}, syncALPNv2, true},
		{"fallback1 cold", []string{syncALPN}, syncALPN, false},
		{"fallback1 warm", []string{syncALPN}, syncALPN, true},
		{"upgrade3 cold", []string{syncALPNv3, syncALPNv2, syncALPN}, syncALPNv3, false},
		{"upgrade3 warm", []string{syncALPNv3, syncALPNv2, syncALPN}, syncALPNv3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server, err := handshake(tc.protos, clientConfig())
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range []tls.ConnectionState{client, server} {
				if s.NegotiatedProtocol != tc.want || s.DidResume != tc.resume {
					t.Fatalf("ALPN=%s resumed=%t want %s/%t", s.NegotiatedProtocol, s.DidResume, tc.want, tc.resume)
				}
			}
		})
	}
	serverReject = true
	if _, _, err := handshake([]string{syncALPNv3}, clientConfig()); err == nil || !serverCheckedResume {
		t.Fatal("resumed server bypassed current-trust callback")
	}
	serverReject = false
	if _, _, err := handshake([]string{syncALPNv3}, clientConfig()); err != nil {
		t.Fatal(err)
	}
	// Seed a warm ticket after the rejected connection above.
	if _, _, err := handshake([]string{syncALPNv3}, clientConfig()); err != nil {
		t.Fatal(err)
	}
	// VerifyConnection must still run on the warm path and be able to reject
	// current trust. A TLS ticket never replaces the application's trust check.
	cfg := clientConfig()
	verify := cfg.VerifyConnection
	checkedResume := false
	cfg.VerifyConnection = func(s tls.ConnectionState) error {
		checkedResume = s.DidResume
		if err := verify(s); err != nil {
			return err
		}
		return errors.New("current trust revoked")
	}
	if _, _, err := handshake([]string{syncALPNv3}, cfg); err == nil || !checkedResume {
		t.Fatal("resumed handshake bypassed current-trust callback")
	}
	// Fill a fresh ticket, then deliberately transplant it into a wrong-asset
	// client config to prove the pinned identity check also runs on resumption.
	if _, _, err := handshake([]string{syncALPNv3}, clientConfig()); err != nil {
		t.Fatal(err)
	}
	correct := clientConfig()
	wrong, err := f.creds[533].PeerTLSWithTickets(535, syncALPNv3, "catalog-tls")
	if err != nil {
		t.Fatal(err)
	}
	wrong.ServerName = correct.ServerName
	wrong.ClientSessionCache = correct.ClientSessionCache
	wrong.NextProtos = correct.NextProtos
	verify = wrong.VerifyConnection
	checkedResume = false
	wrong.VerifyConnection = func(s tls.ConnectionState) error { checkedResume = s.DidResume; return verify(s) }
	if _, _, err := handshake([]string{syncALPNv3}, wrong); err == nil || !checkedResume {
		t.Fatal("resumed ticket bypassed asset pin")
	}
}
