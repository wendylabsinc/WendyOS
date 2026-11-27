package agentmodule

import (
	"context"
	"crypto/mldsa"
	"crypto/x509"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

// Give only the first source a distinct generated leaf/key and a fresh durable
// source epoch. The claimant retains its original certificate and authority.
// Both original proofs pass the real verifier and persistent admission path.
func preparedAuthoritySource(t *testing.T, f *preparedFixture, certificateLife, delegationLife time.Duration) serviceorigin.VerifiedRecord {
	t.Helper()
	key, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	old := f.certificate.Leaf
	leaf := testidentity.Issue(t, &x509.Certificate{
		SerialNumber: big.NewInt(100), Subject: old.Subject,
		NotBefore: old.NotBefore, NotAfter: moduleTestNow.Add(certificateLife),
		KeyUsage: old.KeyUsage, ExtKeyUsage: old.ExtKeyUsage, URIs: old.URIs,
	}, f.certificate.Issuer, key.Public(), f.certificate.IssuerKey)
	g := f.m.cfg.Policy.Grants[0]
	scope := f.m.cfg.Policy.scope(g, 1)
	epoch, err := f.m.journal.ReserveEpoch(context.Background(), scope.Tenant, scope.Device, scope.App, scope.Service)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{
		Scope: scope, Epoch: epoch, Lifetime: delegationLife, Clock: f.clock,
		Certificates: f.m.certificates, ChainDER: [][]byte{leaf.Raw, f.certificate.Issuer.Raw}, DeviceKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = publisher.Close() })
	from, until, err := publisher.AuthorityWindow()
	if err != nil {
		t.Fatal(err)
	}
	f.m.mu.Lock()
	f.m.bindings[bindingKey(g.App, g.Service)] = &binding{
		scope: scope, epoch: epoch, token: f.identity.token.Load(),
		subscribe: append([]serviceaccess.Source(nil), g.Subscribe...), authorityFrom: from, authorityUntil: until,
	}
	f.m.mu.Unlock()
	signed, err := publisher.Publish(context.Background(), f.sourceRequests[0])
	if err != nil {
		t.Fatal(err)
	}
	if err = f.m.admitArchived(context.Background(), signed); err != nil {
		t.Fatal(err)
	}
	v, err := f.m.verifier.Verify(context.Background(), signed)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(signed)
	ref := exactReference(raw, v)
	stored, err := f.m.archive.Get(context.Background(), ref.Source(), ref)
	if err != nil {
		t.Fatal(err)
	}
	f.descriptors[0] = stored.Descriptor
	result, err := resultrecord.Decode(f.request.Payload)
	if err != nil {
		t.Fatal(err)
	}
	result.Inputs[0] = ref
	f.request.Payload, err = resultrecord.Encode(result)
	if err != nil {
		t.Fatal(err)
	}
	f.request.Provenance.Inputs = append([]world.Input(nil), f.request.Provenance.Inputs...)
	f.request.Provenance.Inputs[0].ID = ref.ID()
	f.claim, err = f.claimPublisher.Publish(context.Background(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	f.verified, err = f.m.verifier.Verify(context.Background(), f.claim)
	if err != nil {
		t.Fatal(err)
	}
	if v.Device.CertificateSHA256 == f.verified.Device.CertificateSHA256 || v.Device.SigningKeySHA256 == f.verified.Device.SigningKeySHA256 {
		t.Fatal("fixture did not isolate input identity")
	}
	return v
}

func TestPreparedCurrentInputDenyListDoesNotReuseInitialAuthorization(t *testing.T) {
	for _, kind := range []string{"certificate", "key"} {
		t.Run(kind, func(t *testing.T) {
			f := newPreparedFixture(t)
			input := preparedAuthoritySource(t, f, time.Hour, time.Hour)
			ctx, p := f.prepare(t)
			// The immutable policy file normally invalidates the module on change.
			// Change only this in-memory fixture list to isolate the cached input's
			// mandatory current deny-list check from that earlier guard.
			f.m.mu.Lock()
			if kind == "certificate" {
				f.m.cfg.Policy.DeniedCertificates = []string{input.Device.CertificateSHA256}
			} else {
				f.m.cfg.Policy.DeniedKeys = []string{input.Device.SigningKeySHA256}
			}
			f.m.mu.Unlock()
			if _, err := f.m.verifier.Verify(context.Background(), f.claim); err != nil {
				t.Fatal("test revoked the claimant instead of only its input", err)
			}
			if _, err := p.current(ctx, f.m, f.verified); err == nil {
				t.Fatal("prepared input retained revoked authority")
			}
		})
	}
}

func TestPreparedOriginalInputAuthorityExpiryPrecedesOtherDeadlines(t *testing.T) {
	for _, kind := range []string{"certificate", "delegation"} {
		t.Run(kind, func(t *testing.T) {
			f := newPreparedFixture(t)
			certificateLife, delegationLife := time.Hour, time.Hour
			if kind == "certificate" {
				certificateLife = 4 * time.Second
			} else {
				delegationLife = 4 * time.Second
			}
			input := preparedAuthoritySource(t, f, certificateLife, delegationLife)
			ctx, p := f.prepare(t)
			deadline := authorityDeadline(input)
			if !deadline.Equal(moduleTestNow.Add(4*time.Second)) || !authorityDeadline(f.verified).After(deadline) {
				t.Fatal("test did not isolate the original input authority window")
			}
			if kind == "certificate" && !input.Device.ValidUntil.Equal(deadline) || kind == "delegation" && !input.Device.ValidUntil.After(deadline) {
				t.Fatal("wrong authority deadline in fixture")
			}
			f.clock.mu.Lock()
			delta := deadline.Sub(f.clock.window.Latest)
			f.clock.window.Earliest = f.clock.window.Earliest.Add(delta)
			f.clock.window.Latest = deadline
			f.clock.window.Monotonic += delta
			w := f.clock.window
			f.clock.mu.Unlock()
			capture, err := sensorrecord.FromComponent(input.Component())
			if err != nil || sensorrecord.PhysicalCurrent(capture, w) != nil || !time.Unix(0, f.sample.Load()).Before(p.state.validation.SampleValidUntil) {
				t.Fatal("a capture or sample deadline expired before input authority")
			}
			if _, err = f.m.verifier.Verify(context.Background(), f.claim); err != nil {
				t.Fatal("claimant authority expired with input", err)
			}
			if _, err = p.current(ctx, f.m, f.verified); err == nil {
				t.Fatal("prepared input crossed its original authority deadline")
			}
		})
	}
}
