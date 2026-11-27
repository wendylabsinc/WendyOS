package localmesh

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
)

// Exercise the operation boundary: a digest reused by offer/track must still
// identify the exact owned payload, including optional outer metadata.
func TestGossipEncodingReuseKeepsExactOwnedArtifact(t *testing.T) {
	for _, operator := range []bool{false, true} {
		for _, metadata := range []bool{false, true} {
			t.Run(fmt.Sprintf("operator=%t/metadata=%t", operator, metadata), func(t *testing.T) {
				creds, _ := testCredentials(t)
				if operator {
					principal := "spiffe://wendy.sh/tenant/" + testTenantUUID + "/operator/encoding"
					cert, key := testIssue(t, principal)
					var err error
					creds, err = NewOperatorCredentials(cert, testPKI().chainPEM, key, uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
					if err != nil {
						t.Fatal(err)
					}
				}
				now := time.Now()
				newDirectory := func() *Directory {
					cache, err := OpenIdentityCache("", DefaultCacheLimits(), creds.Verify, now)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := cache.Put(creds.Certificate.Certificate, now); err != nil {
						t.Fatal(err)
					}
					d, err := NewDirectory(creds.Device.Tenant(), 2, cache)
					if err != nil {
						t.Fatal(err)
					}
					return d
				}
				source, destination := newDirectory(), newDirectory()
				sender := NewSynchronizer(source, source.cache, netgossip.NewPool())
				receiver := NewSynchronizer(destination, destination.cache, netgossip.NewPool())
				defer sender.Close()
				defer receiver.Close()
				manifest := routeManifest(creds.Device, now)
				if operator {
					manifest.Device, manifest.AgentPort = uuid.Nil, 0
					manifest.Owner = &OperatorOwner{Principal: creds.Device.Principal(), Instance: creds.Device.UUID()}
				}
				wire, err := SignManifest(manifest, creds.Certificate.Certificate, creds.Signer, now)
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := netwire.Marshal(wire)
				if err != nil {
					t.Fatal(err)
				}
				if metadata {
					var fields map[string]netwire.RawMessage
					if err := netwire.Unmarshal(encoded, &fields); err != nil {
						t.Fatal(err)
					}
					fields["futureLabel"], err = netwire.Marshal("preserve exact bytes")
					if err != nil {
						t.Fatal(err)
					}
					encoded, err = netwire.Marshal(fields)
					if err != nil {
						t.Fatal(err)
					}
					if err := netwire.Unmarshal(encoded, &wire); err != nil {
						t.Fatal(err)
					}
				}
				if changed, err := source.Accept(wire, now); err != nil || !changed {
					t.Fatal(changed, err)
				}
				offer := sender.Record(wire, now)
				if len(offer) != 1 || offer[0].Gossip.ID() != netgossip.Identify(netgossip.Manifest, encoded) {
					t.Fatal("offer lost exact envelope")
				}
				// The caller owns these mutable inputs. Neither the directory nor
				// the gossip payload may depend on them after Record returns.
				wire.Body[0] ^= 1
				wire.Signature[0] ^= 1
				wants, _, err := receiver.Receive(offer[0], now)
				if err != nil || len(wants) != 1 {
					t.Fatal(wants, err)
				}
				replies, _, err := sender.Receive(wants[0], now)
				if err != nil {
					t.Fatal(err)
				}
				var transferred []byte
				var admitted int
				for _, reply := range replies {
					if reply.Gossip != nil && reply.Gossip.Op == "data" {
						transferred = append(transferred, reply.Gossip.Data...)
					}
					_, changes, err := receiver.Receive(reply, now)
					if err != nil {
						t.Fatal(err)
					}
					admitted += len(changes)
				}
				if admitted != 1 || !bytes.Equal(transferred, encoded) {
					t.Fatal("transfer changed or aliased caller bytes")
				}
				if got := destination.manifestPayload(creds.Device.Key(), now); !bytes.Equal(got, encoded) {
					t.Fatal("receiver did not retain exact artifact")
				}
				manifest.Revision++
				replacement, err := SignManifest(manifest, creds.Certificate.Certificate, creds.Signer, now)
				if err != nil {
					t.Fatal(err)
				}
				if changed, err := source.Accept(replacement, now); err != nil || !changed {
					t.Fatal(changed, err)
				}
				next := sender.Record(replacement, now)
				if len(next) != 1 || next[0].Gossip.ID() == offer[0].Gossip.ID() || !offer[0].Expired(now) {
					t.Fatal("supersession reused old marker or digest")
				}
				later := now.Add(time.Minute)
				if got := sender.Record(replacement, later); len(got) != 0 || !next[0].Expired(later) {
					t.Fatal("encoding reuse extended expiry")
				}
			})
		}
	}
}
