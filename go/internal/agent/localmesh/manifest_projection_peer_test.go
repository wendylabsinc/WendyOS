package localmesh

import (
	"bytes"
	"crypto/rand"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"testing"
	"time"
)

func projectionPeerEncode(t *testing.T, v any) netwire.RawMessage {
	t.Helper()
	b, e := netwire.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func projectionPeerFixture(t *testing.T, change func(map[string]netwire.RawMessage)) (*Directory, SignedManifest, Manifest, time.Time, SignedManifest) {
	t.Helper()
	d, chain, key, dev, now := directoryFixture(t)
	m := routeManifest(dev, now)
	m.AgentTransport = AgentMTLS
	m.Platform = PlatformLite
	m.AppEndpoints = []AppEndpoint{{Transport: AppTLS, Port: AppSessionTLSPort}}
	m.Services = []ManifestService{{Mesh: "lab", AppID: "sh.wendy.fixture", ServiceID: "echo", Type: "_echo._tcp", Instance: "Known echo", Port: 28461}}
	w, e := SignManifest(m, chain, key, now)
	if e != nil {
		t.Fatal(e)
	}
	var f map[string]netwire.RawMessage
	if e = netwire.Unmarshal(w.Body, &f); e != nil {
		t.Fatal(e)
	}
	change(f)
	w.Body = projectionPeerEncode(t, f)
	input, opts := signingInput(key.Public(), w.Principal, w.Body)
	w.Signature, e = key.Sign(rand.Reader, input, opts)
	if e != nil {
		t.Fatal(e)
	}
	nextManifest := cloneManifest(m)
	nextManifest.Revision++
	next, e := SignManifest(nextManifest, chain, key, now)
	if e != nil {
		t.Fatal(e)
	}
	return d, w, m, now, next
}
func projectionPeerServices(t *testing.T, f map[string]netwire.RawMessage) []map[string]netwire.RawMessage {
	t.Helper()
	var s []map[string]netwire.RawMessage
	if e := netwire.Unmarshal(f["s"], &s); e != nil {
		t.Fatal(e)
	}
	return s
}

func TestManifestOptionalProjectionRetainsKnownAndExactArtifact(t *testing.T) {
	for _, port := range []any{"new-port-schema", uint64(1 << 24), map[string]any{"path": "future"}} {
		t.Run(string(projectionPeerEncode(t, port)), func(t *testing.T) {
			d, w, m, now, next := projectionPeerFixture(t, func(f map[string]netwire.RawMessage) {
				f["a"] = projectionPeerEncode(t, []any{map[string]any{"t": "future-v2", "p": port, "extra": []any{1, true}}, map[string]any{"t": AppTLS, "p": AppSessionTLSPort}})
				s := projectionPeerServices(t, f)
				s = append(s, map[string]netwire.RawMessage{"m": projectionPeerEncode(t, "lab"), "a": projectionPeerEncode(t, "sh.wendy.fixture"), "s": projectionPeerEncode(t, "future-socket"), "t": projectionPeerEncode(t, "_future._quic"), "p": projectionPeerEncode(t, port), "n": projectionPeerEncode(t, map[string]any{"future": true})}, map[string]netwire.RawMessage{"m": projectionPeerEncode(t, "lab"), "a": projectionPeerEncode(t, "wendy.mesh.system"), "s": projectionPeerEncode(t, "future-tunnel"), "t": projectionPeerEncode(t, map[string]any{"future": true}), "p": projectionPeerEncode(t, port)})
				f["s"] = projectionPeerEncode(t, s)
			})
			view, e := DecodeManifest(w.Body)
			if e != nil || len(view.AppEndpoints) != 1 || view.AppEndpoints[0] != m.AppEndpoints[0] || len(view.Services) != 1 || view.Services[0].ServiceID != "echo" {
				t.Fatalf("optional projection: %+v, %v", view, e)
			}
			var saved []DirectoryReceipt
			if e := d.SetPersistence(nil, func(r []DirectoryReceipt) error { saved = append([]DirectoryReceipt(nil), r...); return nil }, now); e != nil {
				t.Fatal(e)
			}
			if changed, e := d.Accept(w, now); e != nil || !changed {
				t.Fatalf("admission=%t,%v", changed, e)
			}
			if len(saved) != 1 || saved[0].Hash != manifestHash(w.Body) {
				t.Fatalf("receipt changed raw digest: %+v", saved)
			}
			records := d.Records(now)
			if len(records) != 1 || !bytes.Equal(records[0].Body, w.Body) || !bytes.Equal(records[0].Signature, w.Signature) {
				t.Fatal("forwarded artifact was projected")
			}
			if !d.RouteAuthorizations(now)[*m.Route] {
				t.Fatal("optional capability blocked route")
			}
			sync := NewSynchronizer(d, d.cache, netgossip.NewPool())
			defer sync.Close()
			have := sync.Record(w, now)
			if len(have) != 1 || have[0].Gossip == nil {
				t.Fatalf("ordinary gossip=%+v", have)
			}
			artifact, e := netwire.Marshal(w)
			if e != nil {
				t.Fatal(e)
			}
			if have[0].Gossip.ID() != netgossip.Identify(netgossip.Manifest, artifact) {
				t.Fatal("gossip lost exact raw envelope digest")
			}
			dest, e := NewDirectory(d.tenant, 10, d.cache)
			if e != nil {
				t.Fatal(e)
			}
			receiver := NewSynchronizer(dest, d.cache, netgossip.NewPool())
			defer receiver.Close()
			deliver := func(queue []ControlMessage) {
				t.Helper()
				toReceiver := true
				for len(queue) != 0 {
					var pending []ControlMessage
					for _, message := range queue {
						target := sync
						if toReceiver {
							target = receiver
						}
						replies, _, err := target.Receive(message, now)
						if err != nil {
							t.Fatal(err)
						}
						pending = append(pending, replies...)
					}
					queue, toReceiver = pending, !toReceiver
				}
			}
			deliver(have)
			if !bytes.Equal(dest.manifestPayload(w.Principal, now), artifact) {
				t.Fatal("ordinary gossip rewrote projected optional artifact")
			}
			if changed, err := d.Accept(next, now); err != nil || !changed {
				t.Fatalf("next known manifest after future capabilities = %t,%v", changed, err)
			}
			deliver(sync.Record(next, now))
			got := dest.Snapshot(now)
			if len(got) != 1 || got[0].Revision != m.Revision+1 || len(got[0].Services) != 1 {
				t.Fatalf("next known artifact blocked in same gossip session: %+v", got)
			}

		})
	}
}
func TestManifestOptionalProjectionPreservesKnownValidationAndBounds(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, map[string]netwire.RawMessage)
	}{
		{"known-endpoint-text-port", func(t *testing.T, f map[string]netwire.RawMessage) {
			f["a"] = projectionPeerEncode(t, []any{map[string]any{"t": AppTLS, "p": "future"}})
		}},
		{"known-endpoint-wide-port", func(t *testing.T, f map[string]netwire.RawMessage) {
			f["a"] = projectionPeerEncode(t, []any{map[string]any{"t": AppTLS, "p": uint64(1 << 24)}})
		}},
		{"known-endpoint-zero-port", func(t *testing.T, f map[string]netwire.RawMessage) {
			f["a"] = projectionPeerEncode(t, []any{map[string]any{"t": AppTLS, "p": 0}})
		}},
		{"known-service-text-port", func(t *testing.T, f map[string]netwire.RawMessage) {
			s := projectionPeerServices(t, f)
			s[0]["p"] = projectionPeerEncode(t, "future")
			f["s"] = projectionPeerEncode(t, s)
		}},
		{"known-service-wide-port", func(t *testing.T, f map[string]netwire.RawMessage) {
			s := projectionPeerServices(t, f)
			s[0]["p"] = projectionPeerEncode(t, uint64(1<<24))
			f["s"] = projectionPeerEncode(t, s)
		}},
		{"known-service-zero-port", func(t *testing.T, f map[string]netwire.RawMessage) {
			s := projectionPeerServices(t, f)
			s[0]["p"] = projectionPeerEncode(t, 0)
			f["s"] = projectionPeerEncode(t, s)
		}},
		{"future-service-missing-identity", func(t *testing.T, f map[string]netwire.RawMessage) {
			s := projectionPeerServices(t, f)
			delete(s[0], "a")
			s[0]["t"] = projectionPeerEncode(t, "_future._quic")
			f["s"] = projectionPeerEncode(t, s)
		}},
		{"future-endpoint-count", func(t *testing.T, f map[string]netwire.RawMessage) {
			var entries []any
			for range 17 {
				entries = append(entries, map[string]any{"t": "future-v2"})
			}
			f["a"] = projectionPeerEncode(t, entries)
		}},
		{"duplicate-future-service", func(t *testing.T, f map[string]netwire.RawMessage) {
			s := projectionPeerServices(t, f)
			s[0]["t"] = projectionPeerEncode(t, "_future._quic")
			s = append(s, s[0])
			f["s"] = projectionPeerEncode(t, s)
		}},
		{"required-extension", func(t *testing.T, f map[string]netwire.RawMessage) {
			f["q"] = projectionPeerEncode(t, []string{"future/required"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, w, _, now, _ := projectionPeerFixture(t, func(f map[string]netwire.RawMessage) { tc.change(t, f) })
			if _, e := DecodeManifest(w.Body); e == nil {
				t.Fatal("invalid entry or bound accepted")
			}
			if _, e := d.Accept(w, now); e == nil {
				t.Fatal("invalid manifest admitted")
			}
			if len(d.Records(now)) != 0 || len(d.history) != 0 {
				t.Fatal("rejected artifact advanced replay")
			}
		})
	}
}
func TestManifestFutureAgentTransportRetainsRouteWithoutManagementFallback(t *testing.T) {
	d, w, m, now, next := projectionPeerFixture(t, func(f map[string]netwire.RawMessage) { f["T"] = projectionPeerEncode(t, "future-agent/2") })
	if changed, e := d.Accept(w, now); e != nil || !changed {
		t.Fatalf("future agent=%t,%v", changed, e)
	}
	view := d.Snapshot(now)
	if len(view) != 1 || view[0].AgentPort != 0 || !d.RouteAuthorizations(now)[*m.Route] {
		t.Fatalf("unsupported management leaked or route lost: %+v", view)
	}
	if len(d.Records(now)) != 1 || !bytes.Equal(d.Records(now)[0].Body, w.Body) {
		t.Fatal("unknown agent rewrote forwarded artifact")
	}
	if changed, err := d.Accept(next, now); err != nil || !changed {
		t.Fatalf("next known agent transport = %t,%v", changed, err)
	}
	known := d.Snapshot(now)
	if len(known) != 1 || known[0].AgentPort == 0 {
		t.Fatalf("known management did not recover: %+v", known)
	}
	_, chain, key, dev, _ := directoryFixture(t)
	local := routeManifest(dev, now)
	local.AgentTransport = "future-agent/2"
	if _, e := SignManifest(local, chain, key, now); e == nil || !errors.Is(e, ErrUnsupportedManifest) {
		t.Fatalf("publisher originated unsupported agent: %v", e)
	}
}
