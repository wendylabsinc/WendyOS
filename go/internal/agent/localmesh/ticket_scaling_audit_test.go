package localmesh

import (
	"crypto/tls"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

type fixtureSessionCache struct{ state *tls.ClientSessionState }

func (c *fixtureSessionCache) Put(_ string, s *tls.ClientSessionState) { c.state = s }
func (c *fixtureSessionCache) Get(_ string) (*tls.ClientSessionState, bool) {
	return c.state, c.state != nil
}

// Generated ECDSA test identities only. Capture actual authenticated TLS states;
// fan them out for cache sizing/hit measurement, never authenticate fake peers.
func ticketFixture(t testing.TB) (*Credentials, *tls.SessionState, *tls.ClientSessionState) {
	t.Helper()
	a, b := testCredentials(t)
	cc, _ := a.PeerTLSWithTickets(b.Asset, LinkALPN, "audit")
	sc, _ := b.PeerTLSWithTickets(a.Asset, LinkALPN, "audit")
	var state *tls.SessionState
	wrap := sc.WrapSession
	sc.WrapSession = func(cs tls.ConnectionState, s *tls.SessionState) ([]byte, error) {
		encoded, e := s.Bytes()
		if e != nil {
			return nil, e
		}
		state, e = tls.ParseSessionState(encoded)
		if e != nil {
			return nil, e
		}
		return wrap(cs, s)
	}
	cache := &fixtureSessionCache{}
	cc.ClientSessionCache = cache
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	left.SetDeadline(time.Now().Add(5 * time.Second))
	right.SetDeadline(time.Now().Add(5 * time.Second))
	done := make(chan error, 1)
	go func() {
		server := tls.Server(right, sc)
		if e := server.Handshake(); e != nil {
			done <- e
			return
		}
		_, e := server.Write([]byte{1})
		done <- e
	}()
	client := tls.Client(left, cc)
	if e := client.Handshake(); e != nil {
		t.Fatal(e)
	}
	if _, e := io.ReadFull(client, make([]byte, 1)); e != nil {
		t.Fatal(e)
	}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if state == nil || cache.state == nil {
		t.Fatal("missing fixture tickets")
	}
	return a, state, cache.state
}

func TestTicketScalingAudit(t *testing.T) {
	for _, padding := range []int{0, -1, 32 << 10} {
		t.Run(map[int]string{0: "public_ECDSA", -1: "provisioned_public_chain", 32 << 10: "32KiB_state_stress"}[padding], func(t *testing.T) {
			creds, server, client := ticketFixture(t)
			if padding == -1 {
				dir := os.Getenv("WENDY_PUBLIC_TLS_FIXTURES")
				if dir == "" {
					t.Skip("optional public provisioning chain fixture")
				}
				server = publicChainSizingState(t, server, dir)
				ticket, cs, err := client.ResumptionState()
				if err != nil {
					t.Fatal(err)
				}
				client, err = tls.NewResumptionState(ticket, publicChainSizingState(t, cs, dir))
				if err != nil {
					t.Fatal(err)
				}
			}
			if padding > 0 {
				server.Extra = [][]byte{make([]byte, padding)}
			}
			encoded, _ := server.Bytes()
			_, cs, _ := client.ResumptionState()
			ce, _ := cs.Bytes()
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			type item struct {
				peer            int32
				transport, alpn string
				token           []byte
			}
			entries := make([]item, 0, 600)
			for peer := int32(1000); peer < 1150; peer++ {
				for transport, alpn := range map[string]string{"ble-tls": "wendy-local-mesh-ble/1", LinkQUICSessionScope: LinkALPN, "app-quic": "wendy-app-mesh/1", "catalog-tls": "wendy-mesh-catalog/2"} {
					cfg, e := creds.PeerTLSWithTickets(peer, alpn, transport)
					if e != nil {
						t.Fatal(e)
					}
					token, e := cfg.WrapSession(tls.ConnectionState{}, server)
					if e != nil {
						t.Fatal(e)
					}
					cfg.ClientSessionCache.Put("fixture", client)
					entries = append(entries, item{peer, transport, alpn, token})
				}
			}
			runtime.GC()
			runtime.ReadMemStats(&after)
			serverHits, clientHits := 0, 0
			for _, entry := range entries {
				cfg, e := creds.PeerTLSWithTickets(entry.peer, entry.alpn, entry.transport)
				if e != nil {
					t.Fatal(e)
				}
				if state, e := cfg.UnwrapSession(entry.token, tls.ConnectionState{}); e != nil {
					t.Fatal(e)
				} else if state != nil {
					serverHits++
				}
				if _, ok := cfg.ClientSessionCache.Get("fixture"); ok {
					clientHits++
				}
			}
			t.Logf("peers=150 scopes=600 server_state_bytes=%d client_state_bytes=%d retained_heap_delta=%d sequential_revisit_server_hits=%d/600 client_hits=%d/600", len(encoded), len(ce), int64(after.HeapAlloc)-int64(before.HeapAlloc), serverHits, clientHits)
			if padding <= 0 && (serverHits != len(entries) || clientHits != len(entries)) {
				t.Fatalf("150-peer ticket working set churned: server=%d client=%d", serverHits, clientHits)
			}
			runtime.KeepAlive(creds)
		})
	}
}

// Diagnostic-only substitution of public chains into generated TLS state. Go's
// opaque encoding is used solely for this version-pinned sizing audit; these
// states must never be used to authenticate a connection.
func publicChainSizingState(t testing.TB, state *tls.SessionState, directory string) *tls.SessionState {
	t.Helper()
	encoded, err := state.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	at := 13
	at += 1 + int(encoded[at])
	read24 := func(b []byte) int { return int(b[0])<<16 | int(b[1])<<8 | int(b[2]) }
	append24 := func(b []byte, n int) []byte { return append(b, byte(n>>16), byte(n>>8), byte(n)) }
	at += 3 + read24(encoded[at:])
	at += 2
	oldEnd := at + 3 + read24(encoded[at:])
	var certs []byte
	for _, name := range []string{"device.pem", "ca.pem"} {
		data, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		for len(data) > 0 {
			block, rest := pem.Decode(data)
			if block == nil {
				break
			}
			data = rest
			if block.Type != "CERTIFICATE" {
				t.Fatal("public fixture contains non-certificate PEM")
			}
			certs = append24(certs, len(block.Bytes))
			certs = append(certs, block.Bytes...)
			certs = append(certs, 0, 0)
		}
	}
	replaced := append([]byte(nil), encoded[:at]...)
	replaced = append24(replaced, len(certs))
	replaced = append(replaced, certs...)
	replaced = append(replaced, encoded[oldEnd:]...)
	result, err := tls.ParseSessionState(replaced)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func BenchmarkMeshTicketScopeReuse(b *testing.B) {
	creds, state, _ := ticketFixture(b)
	const scopes = 600
	tokens := make([][]byte, scopes)
	transports := []string{"ble-tls", LinkQUICSessionScope, "app-quic", "catalog-tls"}
	get := func(i int) *tls.Config {
		cfg, err := creds.PeerTLSWithTickets(int32(1000+i/4), LinkALPN, transports[i%4])
		if err != nil {
			b.Fatal(err)
		}
		return cfg
	}
	for i := range tokens {
		var err error
		tokens[i], err = get(i).WrapSession(tls.ConnectionState{}, state)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	misses := 0
	for n := 0; n < b.N; n++ {
		i := n % scopes
		cfg := get(i)
		resumed, err := cfg.UnwrapSession(tokens[i], tls.ConnectionState{})
		if err != nil {
			b.Fatal(err)
		}
		if resumed == nil {
			misses++
			tokens[i], err = cfg.WrapSession(tls.ConnectionState{}, state)
			if err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(100*float64(misses)/float64(b.N), "miss_pct")
}
