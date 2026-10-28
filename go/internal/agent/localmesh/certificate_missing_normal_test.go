package localmesh

import (
	"bytes"
	"testing"

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
	future := map[string]any{"k": "certificate", "c": map[string]any{"future-availability": true}}
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
