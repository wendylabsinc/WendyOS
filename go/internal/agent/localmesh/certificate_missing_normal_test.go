package localmesh

import (
	"bytes"
	"crypto/x509"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
)

// A neighbour can forget public DER between its descriptor and our request.
// Its explicit miss must let another announcing neighbour take over now.
func TestCertificateMissingTransfersToAlternateSupplier(t *testing.T) {
	f := newCertificateFixture(t)
	a, b := testPrincipal(testTenantUUID, testDeviceAID), testPrincipal(testTenantUUID, testDeviceBID)
	source := NewCertificateExchange(f.cache(t, a, nil), testTenant(t))
	forgotten := NewCertificateExchange(f.cache(t, "", nil), testTenant(t))
	pool := NewCertificatePool()
	first := NewCertificateExchange(f.cache(t, b, pool), testTenant(t))
	otherCache := f.cache(t, b, pool)
	other := NewCertificateExchange(otherCache, testTenant(t))
	defer first.Close()
	defer other.Close()
	defer source.Close()
	defer forgotten.Close()
	description := source.Describe(a, f.now)
	requests, _, err := first.Receive(*description, f.now)
	if err != nil || len(requests) != 1 || requests[0].Request == nil {
		t.Fatal(requests, err)
	}
	queued, _, err := other.Receive(*description, f.now)
	if err != nil || len(queued) != 0 {
		t.Fatal(queued, err)
	}
	reply, _, err := forgotten.Receive(*requests[0], f.now)
	if err != nil || len(reply) != 1 || reply[0].Missing == nil || *reply[0].Missing != *requests[0].Request {
		t.Fatal(reply, err)
	}
	wire, err := netwire.Marshal(reply[0])
	if err != nil {
		t.Fatal(err)
	}
	// Hashes retain their binary 32-byte representation on the new control too.
	if bytes.Contains(wire, []byte(reply[0].Missing.Hash)) {
		t.Fatal("hexadecimal hash on wire")
	}
	var decoded CertificateMessage
	if err = netwire.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	queued, done, err := first.Receive(decoded, f.now)
	if err != nil || len(queued) != 0 || len(done) != 0 || first.Pending() {
		t.Fatal(queued, done, err)
	}
	if _, _, ok := first.cache.Get(a, f.now); ok {
		t.Fatal("unverified chain became an identity")
	}
	queued, _, err = other.Poll(f.now)
	if err != nil || len(queued) != 1 || queued[0].Request == nil {
		t.Fatal("alternate waited for timeout", queued, err)
	}
	completed := 0
	for steps := 0; len(queued) != 0; steps++ {
		if steps > 32 {
			t.Fatal("transfer did not complete")
		}
		var responses []*CertificateMessage
		for _, q := range queued {
			out, _, err := source.Receive(*q, f.now)
			if err != nil {
				t.Fatal(err)
			}
			responses = append(responses, out...)
		}
		queued = nil
		for _, response := range responses {
			out, done, err := other.Receive(*response, f.now)
			if err != nil {
				t.Fatal(err)
			}
			queued = append(queued, out...)
			completed += len(done)
		}
	}
	if completed != 1 {
		t.Fatal("alternate did not complete exact chain", completed)
	}
	if _, _, ok := otherCache.Get(a, f.now); !ok {
		t.Fatal("verified alternate identity absent")
	}
}

func TestFutureCertificateControlKeepsStreamUsable(t *testing.T) {
	var stream bytes.Buffer
	future := map[string]any{"kind": "certificate", "certificate": map[string]any{"future-availability": true}}
	payload, err := netwire.Marshal(future)
	if err != nil {
		t.Fatal(err)
	}
	// Use the ordinary control framing without inventing a second wire decoder.
	stream.Write([]byte{0, 0, byte(len(payload) >> 8), byte(len(payload))})
	stream.Write(payload)
	if err = WriteControl(&stream, ControlMessage{Kind: "identity-request", Principal: testPrincipal(testTenantUUID, testDeviceAID)}); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadControl(&stream); err != nil {
		t.Fatal(err)
	}
	next, err := ReadControl(&stream)
	if err != nil || next.Kind != "identity-request" {
		t.Fatal("future optional frame interrupted following request", next.Kind, err)
	}
}

func TestOperatorCertificateMissingReleasesArtifactSupplier(t *testing.T) {
	f := operatorProofCredentials(t, "P256")
	parts := NewCertificatePool()
	destination, err := newOperatorDirectory(testTenant(t), []*x509.Certificate{f.root}, parts, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	forgotten, err := newOperatorDirectory(testTenant(t), []*x509.Certificate{f.root}, NewCertificatePool(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	source := newOperatorSynchronizer(forgotten, netgossip.NewPool())
	defer source.close()
	gossip := netgossip.NewPool()
	first := newOperatorSynchronizer(destination, gossip)
	defer first.close()
	other := newOperatorSynchronizer(destination, gossip)
	defer other.close()
	data, err := forgotten.artifact(f.proof(t, 1, 1), f.identity)
	if err != nil {
		t.Fatal(err)
	}
	have := source.record(data, f.now)
	if len(have) != 1 {
		t.Fatal("missing valid proof availability")
	}
	requested, _, err := first.receive(have[0], f.now, 4)
	if err != nil || len(requested) != 1 || requested[0].Operator == nil {
		t.Fatal(requested, err)
	}
	queued, _, err := other.receive(have[0], f.now, 4)
	if err != nil || len(queued) != 0 {
		t.Fatal(queued, err)
	}
	want := *requested[0].Operator
	var repair []ControlMessage
	for offset := 0; offset < len(data); offset += netgossip.ChunkSize {
		packet := want
		packet.Op = "data"
		packet.Offset = offset
		packet.Total = len(data)
		packet.Data = data[offset:min(len(data), offset+netgossip.ChunkSize)]
		out, _, err := first.receive(ControlMessage{Kind: "operator-gossip", Operator: &packet}, f.now, 4)
		if err != nil {
			t.Fatal(err)
		}
		repair = append(repair, out...)
	}
	if len(repair) != 1 || repair[0].OperatorCertificate == nil || repair[0].OperatorCertificate.Request == nil {
		t.Fatal("no serial DER repair", repair)
	}
	miss, _, err := source.receive(repair[0], f.now, 4)
	if err != nil || len(miss) != 1 || miss[0].OperatorCertificate.Missing == nil {
		t.Fatal(miss, err)
	}
	queued, _, err = first.receive(miss[0], f.now, 4)
	if err != nil || len(queued) != 0 || len(first.pending) != 0 || first.waiting != nil {
		t.Fatal("supplier retained after DER miss", queued, err)
	}
	queued, _, err = other.poll(f.now, 4, true)
	if err != nil || len(queued) != 1 || queued[0].Operator == nil || queued[0].Operator.Op != "want" {
		t.Fatal("alternate proof supplier did not resume immediately", queued, err)
	}
	if len(destination.entries) != 0 || parts.bytes != 0 {
		t.Fatal("unverified artifact retained authority or temporary DER")
	}
}
