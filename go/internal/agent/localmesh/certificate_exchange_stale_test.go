package localmesh

import (
	"bytes"
	"testing"
	"time"
)

func TestCertificateExchangeStaleVerifiedLeafKeepsRenewedIdentityAndCarrier(t *testing.T) {
	for _, cached := range []bool{false, true} {
		name := "chunked"
		if cached {
			name = "cached"
		}
		t.Run(name, func(t *testing.T) {
			f := newWarmRenewalFixture(t) // Real verified X509, including changed keys.
			source, err := OpenIdentityCache("", DefaultCacheLimits(), f.verify, f.now)
			if err != nil {
				t.Fatal(err)
			}
			receiver, err := OpenIdentityCache("", DefaultCacheLimits(), f.verify, f.now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := source.Put(f.old.Certificate.Certificate, f.now); err != nil {
				t.Fatal(err)
			}
			if cached {
				if _, err := receiver.Put(f.old.Certificate.Certificate, f.now); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := receiver.Put(f.renewed.Certificate.Certificate, f.now); err != nil {
				t.Fatal(err)
			}
			principal := f.renewed.Device.Principal()
			before := receiver.entries[principal]
			sender := NewCertificateExchange(source, f.old.Device.Tenant())
			exchange := NewCertificateExchange(receiver, f.old.Device.Tenant())
			defer sender.Close()
			defer exchange.Close()
			receivedAt := f.now.Add(time.Minute)
			requests, _, completed := driveCertificateExchange(t, sender, exchange, sender.Describe(principal, receivedAt), receivedAt)
			if (!cached && len(requests) == 0) || (cached && len(requests) != 0) || len(completed) != 0 || exchange.Pending() {
				t.Fatalf("stale verified chain was admitted or retained: cached=%v requests=%d completed=%v", cached, len(requests), completed)
			}
			after := receiver.entries[principal]
			if !sameChain(before.Chain, after.Chain) || before.Added != after.Added || before.Used != after.Used {
				t.Fatal("stale exchange replaced or renewed current identity retention")
			}
			// A direct cache insertion still refuses the downgrade. Only the
			// carrier exchange treats this verified stale descriptor as benign.
			if _, err := receiver.Put(f.old.Certificate.Certificate, receivedAt); err == nil {
				t.Fatal("direct identity insertion accepted a downgrade")
			}
			renewedAt := receivedAt.Add(time.Minute)
			if _, err := source.Put(f.third.Certificate.Certificate, renewedAt); err != nil {
				t.Fatal(err)
			}
			_, _, completed = driveCertificateExchange(t, sender, exchange, sender.Describe(principal, renewedAt), renewedAt)
			chain, _, ok := receiver.Get(principal, renewedAt)
			if len(completed) != 1 || completed[0] != principal || !ok || !bytes.Equal(chain[0], f.third.Certificate.Certificate[0]) {
				t.Fatal("same certificate exchange could not accept a subsequent real renewal")
			}
			// Only a verified older chain is benign. Principal substitution and
			// invalid signatures must retain their normal protocol failures.
			wrong := DescribeCertificates(testPrincipal(testTenantUUID, testDeviceBID), f.old.Certificate.Certificate)
			if _, _, err := exchange.Receive(*wrong, renewedAt); err == nil {
				t.Fatal("stale handling accepted a mismatched descriptor principal")
			}
			exchange.Close()
			exchange = NewCertificateExchange(receiver, f.old.Device.Tenant())
			defer exchange.Close()
			corrupt := cloneChain(f.old.Certificate.Certificate)
			corrupt[0][len(corrupt[0])-1] ^= 1
			receiver.rememberCertificates(corrupt, renewedAt.Add(MaxLease), renewedAt)
			if _, _, err := exchange.Receive(*DescribeCertificates(principal, corrupt), renewedAt); err == nil {
				t.Fatal("stale handling accepted an invalid certificate signature")
			}
		})
	}
}
