package localmesh

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/WendyOS/go/babel"
)

// Run against the host tool built from Wendy Lite's production encoder and
// signer. Temporary private keys stay in the test directory and are never
// included in tool output or test diagnostics.
func TestLeafCManifestInteroperability(t *testing.T) {
	tool := os.Getenv("WENDY_LITE_FIXTURE_TOOL")
	if tool == "" {
		t.Skip("set WENDY_LITE_FIXTURE_TOOL to the built C signer fixture")
	}
	principal := testPrincipal(testTenantUUID, testDeviceBID)
	device := testDevice(t, principal)
	cert, key := testIssue(t, principal)
	credentials, err := NewCredentials(cert, testPKI().chainPEM, key)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(key))
	if block == nil {
		t.Fatal("missing test key")
	}
	dir := t.TempDir()
	keyPath, chainPath := filepath.Join(dir, "key.der"), filepath.Join(dir, "chain.pem")
	if err := os.WriteFile(keyPath, block.Bytes, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chainPath, []byte(cert+testPKI().chainPEM), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	const revision = uint64(9007199254740993)
	const router = babel.RouterID(^uint64(0) - 1)
	output, err := exec.Command(tool, keyPath, chainPath, testTenantUUID, device.UUID().String(), strconv.FormatUint(revision, 10), strconv.FormatUint(uint64(router), 10), "65535", strconv.FormatInt(now.Unix(), 10), strconv.FormatInt(now.Add(time.Minute).Unix(), 10)).Output()
	if err != nil {
		t.Fatalf("C fixture execution: %v", err)
	}
	var fixture struct {
		Tenant, Device string
		Bundle         [][]byte
		Manifest       SignedManifest
	}
	if err := json.Unmarshal(output, &fixture); err != nil {
		t.Fatalf("C fixture JSON: %v", err)
	}
	if fixture.Tenant != testTenantUUID || fixture.Device != device.UUID().String() {
		t.Fatal("C fixture identity mismatch")
	}
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), credentials.Verify, now)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := cache.Put(fixture.Bundle, now); err != nil || got != principal {
		t.Fatalf("C bundle verification: %q %v", got, err)
	}
	newDirectory := func() *Directory {
		d, err := NewDirectory(device.Tenant(), 8, cache)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	d := newDirectory()
	var receipts []DirectoryReceipt
	if err := d.SetPersistence(nil, func(r []DirectoryReceipt) error { receipts = append([]DirectoryReceipt(nil), r...); return nil }, now); err != nil {
		t.Fatal(err)
	}
	if changed, err := d.Accept(fixture.Manifest, now); err != nil || !changed {
		t.Fatalf("Go rejected C signature: changed=%v err=%v", changed, err)
	}
	// The other tenant trusts a certificate with the same key and device UUID.
	// Its derived /128 and the entire manifest body therefore also match. Only
	// the signed full principal prevents relabeling the C-generated proof.
	signer, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	otherCredentials := principalBoundCredentials(t, testPrincipal(testOtherTenant, device.UUID().String()), signer)
	otherDirectory := principalBoundDirectory(t, otherCredentials, now)
	relabelled := fixture.Manifest
	relabelled.Principal = otherCredentials.Device.Principal()
	if _, err := otherDirectory.Accept(relabelled, now); err == nil {
		t.Fatal("accepted C proof relabeled to another tenant with the same key and UUID")
	}
	if len(otherDirectory.Snapshot(now)) != 0 || len(otherDirectory.RouteAuthorizations(now)) != 0 {
		t.Fatal("relabeled C proof changed presence or routing")
	}
	var body Manifest
	if err := json.Unmarshal(fixture.Manifest.Body, &body); err != nil {
		t.Fatal(err)
	}
	wantRoute := RouteOrigin{Prefix: netip.PrefixFrom(device.MeshAddress(), 128), RouterID: router, Seqno: 65535}
	if body.Revision != revision || body.Route == nil || *body.Route != wantRoute || !d.RouteAuthorizations(now)[wantRoute] {
		t.Fatal("C manifest lost exact route or uint64 precision")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"device", func(m *Manifest) { m.Device = uuid.MustParse(testDeviceAID) }},
		{"prefix", func(m *Manifest) {
			m.Route.Prefix = netip.PrefixFrom(testDevice(t, testPrincipal(testTenantUUID, testDeviceAID)).MeshAddress(), 128)
		}},
		{"router", func(m *Manifest) { m.Route.RouterID-- }},
		{"sequence", func(m *Manifest) { m.Route.Seqno-- }},
		{"expiry", func(m *Manifest) { m.Expires += 1000 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var modified Manifest
			if err := json.Unmarshal(fixture.Manifest.Body, &modified); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&modified)
			wire := fixture.Manifest
			wire.Body, err = json.Marshal(modified)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := newDirectory().Accept(wire, now); err == nil {
				t.Fatal("tampered C manifest accepted")
			}
		})
	}
	forged := fixture.Manifest
	forged.Signature = append([]byte(nil), forged.Signature...)
	forged.Signature[0] ^= 1
	if _, err := newDirectory().Accept(forged, now); err == nil {
		t.Fatal("forged C signature accepted")
	}
	if changed, err := d.Accept(fixture.Manifest, now.Add(30*time.Second)); err != nil || changed {
		t.Fatalf("duplicate C proof: changed=%v err=%v", changed, err)
	}
	restarted := newDirectory()
	if err := restarted.SetPersistence(receipts, func([]DirectoryReceipt) error { return nil }, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Accept(fixture.Manifest, now.Add(30*time.Second)); err != nil {
		t.Fatalf("persisted receipt replay: %v", err)
	}
	if len(restarted.RouteAuthorizations(now.Add(time.Minute))) != 0 || len(d.RouteAuthorizations(now.Add(time.Minute))) != 0 {
		t.Fatal("replay or restart extended C route lease")
	}
	if _, err := newDirectory().Accept(fixture.Manifest, now.Add(time.Minute)); err == nil {
		t.Fatal("expired C proof accepted")
	}
}
