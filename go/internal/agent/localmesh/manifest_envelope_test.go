package localmesh

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
)

func TestManifestEnvelopeOptionalMetadataRetainsSignedBytesAndGossipDigest(t *testing.T) {
	source, chain, key, device, now := directoryFixture(t)
	wire, err := SignManifest(routeManifest(device, now), chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(value any) netwire.RawMessage {
		t.Helper()
		data, err := netwire.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	var body map[string]netwire.RawMessage
	if err := netwire.Unmarshal(wire.Body, &body); err != nil {
		t.Fatal(err)
	}
	body["futureBodyMetadata"] = encode(map[string]any{"label": "original", "nested": []any{true, nil, uint64(3)}})
	wire.Body = encode(body)
	input, opts := signingInput(key.Public(), wire.Principal, wire.Body)
	wire.Signature, err = key.Sign(rand.Reader, input, opts)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]netwire.RawMessage
	if err := netwire.Unmarshal(encode(wire), &fields); err != nil {
		t.Fatal(err)
	}
	fields["futureEnvelopeMetadata"] = encode(map[string]any{"nested": map[string]any{"labels": []string{"one", "two"}}})
	artifact := encode(fields)
	var received SignedManifest
	if err := netwire.Unmarshal(artifact, &received); err != nil {
		t.Fatal("optional outer metadata:", err)
	}
	if received.Principal != wire.Principal || !bytes.Equal(received.Body, wire.Body) || !bytes.Equal(received.Signature, wire.Signature) {
		t.Fatal("decoding changed the signed fields")
	}
	// Neither the source buffer nor a returned encoding can mutate retained bytes.
	owned := bytes.Clone(artifact)
	artifact[len(artifact)-1] ^= 1
	if got := encode(received); !bytes.Equal(got, owned) {
		t.Fatal("retained envelope aliases decoder input")
	}
	encoded := encode(received)
	encoded[len(encoded)-1] ^= 1
	if got := encode(received); !bytes.Equal(got, owned) {
		t.Fatal("marshal result aliases retained envelope")
	}
	if _, err := source.Accept(received, now); err != nil {
		t.Fatal(err)
	}
	dest, err := NewDirectory(device.Tenant(), 10, source.cache)
	if err != nil {
		t.Fatal(err)
	}
	sender := NewSynchronizer(source, source.cache, netgossip.NewPool())
	receiver := NewSynchronizer(dest, dest.cache, netgossip.NewPool())
	defer sender.Close()
	defer receiver.Close()
	have := sender.Record(received, now)
	if len(have) != 1 || have[0].Gossip == nil || have[0].Gossip.ID() != netgossip.Identify(netgossip.Manifest, owned) {
		t.Fatal("HAVE did not identify original envelope")
	}
	queue, toReceiver, changed := have, true, 0
	for len(queue) > 0 {
		var next []ControlMessage
		for _, message := range queue {
			target := sender
			if toReceiver {
				target = receiver
			}
			replies, admitted, err := target.Receive(message, now)
			if err != nil {
				t.Fatal(err)
			}
			if toReceiver {
				changed += len(admitted)
			}
			next = append(next, replies...)
		}
		queue, toReceiver = next, !toReceiver
	}
	if changed != 1 || len(dest.RouteAuthorizations(now)) != 1 {
		t.Fatal("extensible envelope was not admitted through HAVE/WANT")
	}
	if replies, _, err := receiver.Receive(have[0], now); err != nil || len(replies) != 0 {
		t.Fatal("original digest claim was not resolved", replies, err)
	}
	if got := dest.manifestPayload(received.Principal, now); !bytes.Equal(got, owned) {
		t.Fatal("directory forwarding changed exact envelope")
	}
	original := dest.Records(now)[0]
	cloned := cloneSignedManifest(original)
	cloned.Signature[0] ^= 1
	if bytes.Equal(encode(cloned), owned) || !bytes.Equal(encode(original), owned) {
		t.Fatal("cloned signature mutation reused stale envelope or changed original")
	}
	cloned = cloneSignedManifest(original)
	cloned.Principal += "-edited"
	if bytes.Equal(encode(cloned), owned) {
		t.Fatal("principal mutation reused stale envelope")
	}
	cloned = cloneSignedManifest(original)
	at := bytes.Index(cloned.Body, []byte("original"))
	if at < 0 {
		t.Fatal("missing signed optional metadata")
	}
	cloned.Body[at] = 'O'
	if bytes.Equal(encode(cloned), owned) || !bytes.Equal(encode(original), owned) {
		t.Fatal("in-place body mutation reused stale envelope or changed original")
	}
	if _, err := dest.Accept(cloned, now); !errors.Is(err, ErrInvalidManifestSignature) {
		t.Fatalf("edited optional signed body: %v", err)
	}
}

func TestManifestEnvelopeDecodeBoundAndFailurePreserveDestination(t *testing.T) {
	_, chain, key, device, now := directoryFixture(t)
	wire, err := SignManifest(routeManifest(device, now), chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	original, err := netwire.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var received SignedManifest
	if err := netwire.Unmarshal(original, &received); err != nil {
		t.Fatal(err)
	}
	if received.envelope != nil {
		t.Fatal("ordinary envelope unnecessarily retained duplicate bytes")
	}
	oversized, err := netwire.Marshal(map[string]any{"future": bytes.Repeat([]byte{1}, MaxControlMessage)})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{oversized, {0xa1, 0x61, 'p'}} {
		if err := received.UnmarshalCBOR(bad); err == nil {
			t.Fatal("invalid envelope accepted")
		}
		got, err := netwire.Marshal(received)
		if err != nil || !bytes.Equal(got, original) {
			t.Fatal("failed decode changed destination", err)
		}
	}
}
