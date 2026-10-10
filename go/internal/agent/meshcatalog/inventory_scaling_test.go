package meshcatalog

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

// Real framed /3 messages with synthetic deployed-size public identities.
func TestInventory150OriginsColdWarmAndPersistentRestart(t *testing.T) {
	const n = 151
	now := time.Now().Truncate(time.Second)
	rootKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "audit-only"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	rootPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}))
	creds := make([]*localmesh.Credentials, n)
	records := make([]SignedRecord, n)
	for i := range creds {
		asset := int32(1000 + i)
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, _ := url.Parse(fmt.Sprintf("urn:wendy:org:64:asset:%d", asset))
		leaf := &x509.Certificate{SerialNumber: big.NewInt(int64(asset)), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 55555, 1}, Value: make([]byte, 400)}}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, rootKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, _ := x509.MarshalECPrivateKey(key)
		creds[i], err = localmesh.NewCredentials(64, asset, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), rootPEM, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
		if err != nil {
			t.Fatal(err)
		}
		if len(creds[i].Certificate.Certificate) != 1 {
			t.Fatal("self-signed root leaked into outgoing bundle")
		}
		r := Record{Version: 1, Key: Key{Mesh: "default", Org: 64, Asset: asset, AppID: "com.wendy.test", ServiceID: "http"}, Generation: 1, Type: "_http._tcp", Instance: fmt.Sprintf("Camera %d", asset), HostPort: 18080, Issued: now.UnixMilli(), Expires: now.Add(90 * time.Second).UnixMilli()}
		records[i], err = Sign(r, creds[i].Certificate.Certificate, creds[i].Signer, now)
		if err != nil {
			t.Fatal(err)
		}
	}

	senderCache, err := localmesh.OpenIdentityCache("", localmesh.DefaultCacheLimits(), creds[0].Verify, now)
	if err != nil {
		t.Fatal(err)
	}
	senderCatalog, err := NewCatalog("default", 64, 1000, 2048, creds[0], senderCache, func(string, string, uint16) error { return nil }, nil, func([]Receipt) error { return nil }, now)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 150 {
		if _, err = senderCache.Put(creds[i].Certificate.Certificate, now); err != nil {
			t.Fatal(err)
		}
		if _, err = senderCatalog.Accept(records[i], now); err != nil {
			t.Fatal(err)
		}
	}
	cachePath := filepath.Join(t.TempDir(), "identities.json")
	open := func(path string) (*Catalog, *localmesh.IdentityCache) {
		cache, e := localmesh.OpenIdentityCache(path, localmesh.DefaultCacheLimits(), creds[150].Verify, now)
		if e != nil {
			t.Fatal(e)
		}
		c, e := NewCatalog("default", 64, 1150, 2048, creds[150], cache, func(string, string, uint16) error { return nil }, nil, func([]Receipt) error { return nil }, now)
		if e != nil {
			t.Fatal(e)
		}
		return c, cache
	}
	type measurement struct{ bytes, bundles, inventory, records int }
	exchange := func(c *Catalog, cache *localmesh.IdentityCache) measurement {
		sender := NewSynchronizer(senderCatalog, senderCache)
		receiver := NewSynchronizer(c, cache)
		var inventory receivedInventory
		var result measurement
		for _, m := range inventoryMessages(cache.Fingerprints(now, maxInventoryEntries)) {
			var wire bytes.Buffer
			if e := writeInventoryMessage(&wire, m); e != nil {
				t.Fatal(e)
			}
			result.bytes += wire.Len()
			result.inventory += wire.Len()
			decoded, e := readInventoryMessage(&wire)
			if e != nil {
				t.Fatal(e)
			}
			if e = inventory.receive(decoded); e != nil {
				t.Fatal(e)
			}
		}
		sender.useCurrentInventory(inventory)
		deliver := func(m inventoryMessage) {
			var wire bytes.Buffer
			if e := writeInventoryMessage(&wire, m); e != nil {
				t.Fatal(e)
			}
			result.bytes += wire.Len()
			if m.Kind == "bundle" {
				result.bundles++
			}
			if m.Kind == "record" {
				result.records++
			}
			decoded, e := readInventoryMessage(&wire)
			if e != nil {
				t.Fatal(e)
			}
			replies, _, e := receiver.Receive(decoded.Message, now)
			if e != nil {
				t.Fatal(e)
			}
			if len(replies) != 0 {
				t.Fatal("current inventory generated an unexpected repair")
			}
		}
		for _, w := range sender.snapshotRecords(now) {
			for _, m := range sender.Record(w, now) {
				deliver(inventoryMessage{Message: m})
			}
		}
		deliver(inventoryMessage{Message: Message{Kind: "snapshot-done"}})
		if receiver.PendingIdentity() || len(c.Snapshot(now)) != 150 || result.records != 150 {
			t.Fatal("incomplete signed snapshot")
		}
		return result
	}
	receiver, cache := open(cachePath)
	cold := exchange(receiver, cache)
	warm := exchange(receiver, cache)
	restarted, reopened := open(cachePath)
	restart := exchange(restarted, reopened)
	empty, emptyCache := open(filepath.Join(t.TempDir(), "empty.json"))
	coldRestart := exchange(empty, emptyCache)
	if cold.bundles != 150 || coldRestart.bundles != 150 || warm.bundles != 0 || restart.bundles != 0 {
		t.Fatal("cold/warm certificate count", cold, warm, restart, coldRestart)
	}
	if warm.bytes*3 >= cold.bytes || restart.bytes != warm.bytes || coldRestart.bytes != cold.bytes {
		t.Fatal("warm/restart savings", cold, warm, restart, coldRestart)
	}
	t.Logf("origins=150 outgoingLeafDER=%d coldBytes=%d coldBundles=%d warmBytes=%d warmBundles=%d warmInventoryBytes=%d persistedRestartBytes=%d emptyRestartBytes=%d", len(creds[0].Certificate.Certificate[0]), cold.bytes, cold.bundles, warm.bytes, warm.bundles, warm.inventory, restart.bytes, coldRestart.bytes)
}
