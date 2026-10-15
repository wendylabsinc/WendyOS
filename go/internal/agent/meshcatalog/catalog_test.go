package meshcatalog

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

type fixture struct {
	now   time.Time
	creds map[int32]*localmesh.Credentials
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour),
		NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	creds := map[int32]*localmesh.Credentials{}
	for _, asset := range []int32{533, 534, 535} {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, _ := url.Parse("urn:wendy:org:64:asset:" + itoa(asset))
		leaf := &x509.Certificate{SerialNumber: big.NewInt(int64(asset)), NotBefore: caTmpl.NotBefore,
			NotAfter: caTmpl.NotAfter, URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
		creds[asset], err = localmesh.NewCredentials(64, asset, certPEM, chain, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
	}
	return fixture{now, creds}
}

func itoa(v int32) string { return fmt.Sprint(v) }

func (f fixture) newCatalog(t *testing.T, asset int32, mesh string, receipts []Receipt,
	persist func([]Receipt) error) (*Catalog, *localmesh.IdentityCache) {
	t.Helper()
	cred := f.creds[asset]
	cache, err := localmesh.OpenIdentityCache("", localmesh.DefaultCacheLimits(), cred.Verify, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if persist == nil {
		persist = func([]Receipt) error { return nil }
	}
	authorize := func(appID, serviceType string, port uint16) error {
		if appID != "com.wendy.test" || serviceType != "_http._tcp" || port != 18080 {
			return errors.New("port not owned by mesh app")
		}
		return nil
	}
	catalog, err := NewCatalog(mesh, 64, asset, 32, cred, cache, authorize, receipts, persist, f.now)
	if err != nil {
		t.Fatal(err)
	}
	return catalog, cache
}

func testSpec() PublishSpec {
	return PublishSpec{AppID: "com.wendy.test", ServiceID: "http", Type: "_http._tcp",
		Instance: "Example", HostPort: 18080, TXT: []string{"path=/", "role=demo"}, Lease: 90 * time.Second}
}

func TestSignedPublicationAndProjection(t *testing.T) {
	f := newFixture(t)
	a, _ := f.newCatalog(t, 533, "default", nil, nil)
	c, _ := f.newCatalog(t, 535, "default", nil, nil)
	w, err := c.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Accept(w, f.now); !errors.Is(err, ErrIdentityNeeded) {
		t.Fatalf("uncached origin = %v, want identity request", err)
	}
	if _, err := a.cache.Put(c.creds.Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	changed, err := a.Accept(w, f.now)
	if err != nil || !changed {
		t.Fatalf("accept = %v, %v", changed, err)
	}
	if changed, err := a.Accept(w, f.now.Add(time.Second)); err != nil || changed {
		t.Fatalf("duplicate changed lease = %v, %v", changed, err)
	}
	got := a.Snapshot(f.now.Add(10 * time.Second))
	if len(got) != 1 || got[0].Key.Asset != 535 || got[0].Key.AppID != "com.wendy.test" {
		t.Fatalf("snapshot = %+v", got)
	}
	rrs, err := ProjectDNS(got, "com.wendy.browser", func(_ string, r Record) bool { return r.Type == "_http._tcp" }, f.now.Add(10*time.Second))
	if err != nil || len(rrs) != 4 {
		t.Fatalf("projection len=%d err=%v", len(rrs), err)
	}
	if ptr, ok := rrs[0].(*dns.PTR); !ok || ptr.Hdr.Ttl > 80 || ptr.Hdr.Ttl == 0 {
		t.Fatalf("PTR lease TTL = %v", rrs[0])
	}
	if srv, ok := rrs[1].(*dns.SRV); !ok || srv.Port != 18080 {
		t.Fatalf("SRV = %v", rrs[1])
	}
	if address, ok := rrs[3].(*dns.A); !ok || address.A.String() != "10.99.2.23" {
		t.Fatalf("VIP A = %v", rrs[3])
	}
	packet, err := (&dns.Msg{MsgHdr: dns.MsgHdr{Response: true}, Answer: rrs}).Pack()
	if err != nil {
		t.Fatal(err)
	}
	var decoded dns.Msg
	if err := decoded.Unpack(packet); err != nil || len(decoded.Answer) != 4 {
		t.Fatalf("DNS-SD packet roundtrip: %v %+v", err, decoded.Answer)
	}
	if denied, err := ProjectDNS(got, "com.wendy.browser", func(string, Record) bool { return false }, f.now); err != nil || len(denied) != 0 {
		t.Fatalf("browse policy leak: %v %v", denied, err)
	}
}

func TestProjectionSharesOriginAddressAndLeavesAAAAEmpty(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	makeRecord := func(id string, lease time.Duration) Record {
		return Record{Version: 1, Key: Key{Mesh: "default", Org: 64, Asset: 535,
			AppID: "com.wendy.test", ServiceID: id}, Generation: 1,
			Type: "_http._tcp", Instance: id, HostPort: 18080,
			Issued: now.UnixMilli(), Expires: now.Add(lease).UnixMilli()}
	}
	rrs, err := ProjectDNS([]Record{makeRecord("one", 30*time.Second), makeRecord("two", 90*time.Second)},
		"com.wendy.browser", func(string, Record) bool { return true }, now)
	if err != nil {
		t.Fatal(err)
	}
	addresses := 0
	for _, rr := range rrs {
		if address, ok := rr.(*dns.A); ok {
			addresses++
			if address.Hdr.Ttl != 90 {
				t.Fatalf("shared VIP TTL = %d, want longest live lease", address.Hdr.Ttl)
			}
		}
	}
	if addresses != 1 || len(rrs) != 7 {
		t.Fatalf("expected six service records and one A, got %d records, %d A", len(rrs), addresses)
	}
	query := &dns.Msg{Question: []dns.Question{{Name: "device-535.mesh.local.", Qtype: dns.TypeAAAA, Qclass: dns.ClassINET}}}
	if answer := AnswerMDNS(query, rrs); answer != nil {
		t.Fatalf("IPv4-only mesh advertised an IPv6 address: %+v", answer)
	}
}

func TestRemoveGenerationAndDurableReplayProtection(t *testing.T) {
	f := newFixture(t)
	store, err := NewReceiptStore(filepath.Join(t.TempDir(), "receipts.json"), 32)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := f.newCatalog(t, 535, "default", nil, store.Save)
	first, err := c.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	withdraw, err := c.Remove("com.wendy.test", "http", f.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Snapshot(f.now.Add(time.Second)); len(got) != 0 {
		t.Fatalf("removed service still visible: %v", got)
	}
	if got := c.Records(f.now.Add(time.Second)); len(got) != 1 {
		t.Fatalf("tombstone not synchronized: %v", got)
	}
	receipts, err := store.Load()
	if err != nil || len(receipts) != 1 || receipts[0].Generation != 2 {
		t.Fatalf("durable receipt: %v %v", receipts, err)
	}
	restarted, _ := f.newCatalog(t, 535, "default", receipts, store.Save)
	if _, err := restarted.Accept(first, f.now.Add(2*time.Second)); !errors.Is(err, ErrStale) {
		t.Fatalf("replayed publish = %v, want stale", err)
	}
	if changed, err := restarted.Accept(withdraw, f.now.Add(2*time.Second)); err != nil || !changed {
		t.Fatalf("same-generation tombstone recovery after restart = %v, %v", changed, err)
	}
	if got := restarted.Records(f.now.Add(2 * time.Second)); len(got) != 1 {
		t.Fatalf("recovered tombstone not synchronized: %v", got)
	}
	next, err := restarted.Publish(testSpec(), f.now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var r Record
	if err := json.Unmarshal(next.Body, &r); err != nil || r.Generation != 3 {
		t.Fatalf("new generation = %d, %v", r.Generation, err)
	}
}

func TestLocalGenerationSurvivesReceiptExpiryAndReactivation(t *testing.T) {
	f := newFixture(t)
	store, err := NewReceiptStore(filepath.Join(t.TempDir(), "receipts.json"), 32)
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := f.newCatalog(t, 533, "default", nil, store.Save)
	peer, peerCache := f.newCatalog(t, 534, "default", nil, nil)
	if _, err := peerCache.Put(f.creds[533].Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	spec.Lease = MaxLease
	key := Key{Mesh: "default", Org: 64, Asset: 533, AppID: spec.AppID, ServiceID: spec.ServiceID}
	old, err := spec.record(key, 231, f.now)
	if err != nil {
		t.Fatal(err)
	}
	w, err := Sign(old, f.creds[533].Certificate.Certificate, f.creds[533].Signer, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := origin.Accept(w, f.now); err != nil {
		t.Fatal(err)
	}
	// A peer can retain its high-water for longer than the origin remains
	// active. This models a short disconnect followed by the same service ID.
	if _, err := peer.Accept(w, f.now.Add(40*time.Second)); err != nil {
		t.Fatal(err)
	}
	receipts, err := store.Load()
	if err != nil || len(receipts) != 1 || receipts[0].Generation != 231 || !receipts[0].Until.IsZero() {
		t.Fatalf("durable local high-water = %v, %v", receipts, err)
	}
	reactivatedAt := f.now.Add(MaxLease + 30*time.Second)
	reactivated, _ := (fixture{now: reactivatedAt, creds: f.creds}).newCatalog(t, 533, "default", receipts, store.Save)
	next, err := reactivated.Publish(spec, reactivatedAt)
	if err != nil {
		t.Fatal(err)
	}
	var announced Record
	if err := json.Unmarshal(next.Body, &announced); err != nil || announced.Generation != 232 {
		t.Fatalf("reactivated generation = %d, %v", announced.Generation, err)
	}
	if _, err := peer.Accept(next, reactivatedAt); err != nil {
		t.Fatalf("live peer high-water rejected reannouncement: %v", err)
	}
	if _, err := peer.Accept(w, reactivatedAt); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired old signed record accepted: %v", err)
	}
	// Existing receipt files gave local high-water a finite Until. Retain
	// those across upgrade too, even if their Until has passed.
	legacy := receipts[0]
	legacy.Until = f.now.Add(MaxLease + 10*time.Second)
	upgraded, _ := (fixture{now: reactivatedAt, creds: f.creds}).newCatalog(t, 533, "default", []Receipt{legacy}, store.Save)
	upgradedWire, err := upgraded.Publish(spec, reactivatedAt)
	if err != nil {
		t.Fatal(err)
	}
	var upgradedRecord Record
	if err := json.Unmarshal(upgradedWire.Body, &upgradedRecord); err != nil || upgradedRecord.Generation != 232 {
		t.Fatalf("legacy local high-water generation = %d, %v", upgradedRecord.Generation, err)
	}
}

func TestReconcileRestoresLiveRecordAfterReceiptOnlyRestart(t *testing.T) {
	f := newFixture(t)
	store, err := NewReceiptStore(filepath.Join(t.TempDir(), "receipts.json"), 32)
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := f.newCatalog(t, 533, "default", nil, nil)
	peer, peerCache := f.newCatalog(t, 534, "default", nil, store.Save)
	w, err := origin.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peerCache.Put(origin.creds.Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	if changed, err := peer.Accept(w, f.now); err != nil || !changed {
		t.Fatalf("initial publication = %v, %v", changed, err)
	}
	receipts, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	restarted, restartedCache := f.newCatalog(t, 534, "default", receipts, store.Save)
	if len(restarted.Snapshot(f.now.Add(time.Second))) != 0 {
		t.Fatal("receipt alone exposed a service after restart")
	}
	if _, err := restartedCache.Put(origin.creds.Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	frames := NewSynchronizer(origin, origin.cache).Reconcile(f.now.Add(time.Second))
	sync := NewSynchronizer(restarted, restartedCache)
	var admitted int
	for _, frame := range frames {
		_, changed, err := sync.Receive(frame, f.now.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		admitted += len(changed)
	}
	if got := restarted.Snapshot(f.now.Add(time.Second)); admitted != 1 || len(got) != 1 || got[0].Key.Asset != 533 {
		t.Fatalf("restarted peer failed to recover live publication: admitted=%d records=%v", admitted, got)
	}
	if changed, err := restarted.Accept(w, f.now.Add(2*time.Second)); err != nil || changed {
		t.Fatalf("duplicate after recovery = %v, %v", changed, err)
	}
	if got := restarted.Snapshot(f.now.Add(91 * time.Second)); len(got) != 0 {
		t.Fatalf("recovered publication outlived signed expiry: %v", got)
	}
}

func TestForgeryScopeAuthorizationAndPersistFailure(t *testing.T) {
	f := newFixture(t)
	a, _ := f.newCatalog(t, 533, "default", nil, nil)
	c, _ := f.newCatalog(t, 535, "default", nil, nil)
	w, err := c.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.cache.Put(c.creds.Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	tampered := cloneWire(w)
	tampered.Body = append(json.RawMessage(nil), w.Body...)
	var r Record
	if err := json.Unmarshal(tampered.Body, &r); err != nil {
		t.Fatal(err)
	}
	r.HostPort = 18081
	tampered.Body, _ = json.Marshal(r)
	if _, err := a.Accept(tampered, f.now); err == nil {
		t.Fatal("tampered signed host port accepted")
	}
	r.Key.Asset = 533
	wrongOrigin, err := Sign(r, c.creds.Certificate.Certificate, c.creds.Signer, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Accept(wrongOrigin, f.now); err == nil {
		t.Fatal("certificate claimed another origin")
	}
	otherMesh, _ := f.newCatalog(t, 533, "other", nil, nil)
	if _, err := otherMesh.cache.Put(c.creds.Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := otherMesh.Accept(w, f.now); err == nil {
		t.Fatal("cross-mesh record accepted")
	}
	spec := testSpec()
	spec.HostPort = 18081
	if _, err := c.Publish(spec, f.now); err == nil {
		t.Fatal("undeclared port published")
	}
	failed, _ := f.newCatalog(t, 534, "default", nil, func([]Receipt) error { return errors.New("disk full") })
	if _, err := failed.Publish(testSpec(), f.now); err == nil {
		t.Fatal("accepted publication without durable receipt")
	}
	if len(failed.Snapshot(f.now)) != 0 {
		t.Fatal("failed persistence made service visible")
	}
}

func TestExpiredLeaseNeverRenewsByRelay(t *testing.T) {
	f := newFixture(t)
	a, _ := f.newCatalog(t, 533, "default", nil, nil)
	c, _ := f.newCatalog(t, 535, "default", nil, nil)
	spec := testSpec()
	spec.Lease = time.Second
	w, err := c.Publish(spec, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.cache.Put(c.creds.Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Accept(w, f.now); err != nil {
		t.Fatal(err)
	}
	if changed, err := a.Accept(w, f.now.Add(500*time.Millisecond)); err != nil || changed {
		t.Fatalf("duplicate = %v %v", changed, err)
	}
	if len(a.Snapshot(f.now.Add(1100*time.Millisecond))) != 0 {
		t.Fatal("relay extended service lease")
	}
	if _, err := a.Accept(w, f.now.Add(1100*time.Millisecond)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired relay = %v", err)
	}
}
