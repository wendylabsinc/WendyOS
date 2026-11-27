package agentmodule

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testtrack"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func TestReplicaModuleOriginAttestedTrackRead(t *testing.T) {
	for _, boundary := range []string{"sample", "physical"} {
		t.Run(boundary, func(t *testing.T) {
			a, b := newReplicaPairConfigured(t, func(index int, cfg *Config) {
				cfg.Policy.ExecutionDomain = "simulation"
				cfg.Policy.SampleClockDomain = "simulation:repair"
				for i := range cfg.Policy.Grants {
					for j := range cfg.Policy.Grants[i].Publish {
						cfg.Policy.Grants[i].Publish[j] = serviceorigin.PublishScope{ComponentType: resultrecord.ComponentType, Stream: "tracks"}
					}
					for j := range cfg.Policy.Grants[i].Subscribe {
						cfg.Policy.Grants[i].Subscribe[j].ComponentType = resultrecord.ComponentType
						cfg.Policy.Grants[i].Subscribe[j].Stream = "tracks"
					}
				}
				peer := &cfg.Replication.Peers[0]
				peer.Import[0].Component, peer.Import[0].Stream = resultrecord.ComponentType, "tracks"
				peer.Export[0].Component, peer.Export[0].Stream = resultrecord.ComponentType, "tracks"
				if index == 0 {
					peer.Import = nil
					cfg.Policy.Grants[1].Subscribe = cfg.Policy.Grants[1].Subscribe[:1]
				} else {
					peer.Export = nil
				}
			})
			startReplicaPair(t, a, b)
			writer := a.publisher(t, 0, 1)
			b.publisher(t, 1, 1)
			scope := a.config.Policy.scope(a.config.Policy.Grants[0], 1)
			request := testtrack.Request(t, scope, a.config.Policy.Frames[0], "repair", moduleTestNow, moduleTestNow)
			original, err := writer.Publish(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			// This intentionally supplies only an origin-attested test result:
			// neither node has camera bytes. Local admission must still refuse it.
			if err = a.identity.WithCurrent(1, func() error { return a.m.admitArchived(context.Background(), original) }); err == nil {
				t.Fatal("local result bypassed its absent input archive")
			}
			b.sync(t) // Repairs the real signed current source lease, not a record.
			raw, _ := json.Marshal(original)
			var record serviceorigin.Record
			if json.Unmarshal(original.Record, &record) != nil {
				t.Fatal("record")
			}
			digest := func(raw []byte) string { d := sha256.Sum256(raw); return hex.EncodeToString(d[:]) }
			ref := proofref.Reference{Key: world.Key{Tenant: scope.Tenant, Site: scope.Site, Entity: request.Entity, Type: request.ComponentType, Source: world.Source{Device: scope.Device, App: scope.App, Service: scope.Service, Stream: request.Stream}}, Version: world.Version{Epoch: record.Epoch, Sequence: record.Sequence}, EnvelopeSHA256: digest(raw), RecordSHA256: digest(original.Record)}
			item := replication.Item{Entry: replication.Entry{Kind: replication.Record, Reference: ref, ExpiresAtUnixNS: request.Timing.SampleTime.Add(request.TTL).UnixNano()}, Envelope: raw}
			if err = b.m.replica.peers[a.cert.Principal].store.Admit(context.Background(), item, func(context.Context) error { return nil }); err != nil {
				t.Fatal(err)
			}
			reader := b.config.Policy.scope(b.config.Policy.Grants[1], 1)
			got, err := b.m.filterQueryCurrent(context.Background(), []serviceorigin.SignedRecord{original}, reader)
			if err != nil || len(got) != 1 || !bytes.Equal(got[0].Record, original.Record) || !bytes.Equal(got[0].Signature, original.Signature) {
				t.Fatal("remote origin-attested result unavailable", len(got), err)
			}
			// Change only the final module observation after the remote snapshot.
			b.m.deps.SampleNow = func() time.Time {
				if boundary == "sample" {
					return moduleTestNow.Add(time.Second)
				}
				b.clock.mu.Lock()
				b.clock.window.Earliest = moduleTestNow.Add(4 * time.Second)
				b.clock.window.Latest = moduleTestNow.Add(4*time.Second + time.Millisecond)
				b.clock.window.Monotonic += 4 * time.Second
				b.clock.mu.Unlock()
				return moduleTestNow.Add(100 * time.Millisecond)
			}
			got, err = b.m.filterQueryCurrent(context.Background(), []serviceorigin.SignedRecord{original}, reader)
			if err == nil && len(got) != 0 {
				t.Fatal("final original track deadline bypassed")
			}
		})
	}
}
