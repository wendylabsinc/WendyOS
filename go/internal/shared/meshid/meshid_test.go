package meshid

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"net/netip"
	"net/url"
	"testing"

	"github.com/google/uuid"
)

const testTenant = "11111111-2222-4333-8444-555555555555"

// TestMeshAddressVectors pins the plan's address table. Any implementation of
// the contract — agent, CLI, Cloud, or a peer on another platform — must
// produce these exact addresses from these exact UUIDs.
func TestMeshAddressVectors(t *testing.T) {
	for _, tc := range []struct {
		device string
		want   string
	}{
		{"12345678-9abc-4def-8123-456789abcdef", "fd45:b891:567f:4def:8123:4567:89ab:cdef"},
		{"019535d9-3df7-79fb-b466-fa907fa17f9e", "fd45:b891:567f:79fb:b466:fa90:7fa1:7f9e"},
		// Same final ten bytes as the first row: same address. Enrollment must
		// reject the collision; the derivation itself is verbatim.
		{"87654321-dcba-4def-8123-456789abcdef", "fd45:b891:567f:4def:8123:4567:89ab:cdef"},
	} {
		id, err := uuid.Parse(tc.device)
		if err != nil {
			t.Fatalf("uuid.Parse(%q): %v", tc.device, err)
		}
		got := MeshAddressFor(id)
		if got.String() != tc.want {
			t.Errorf("MeshAddressFor(%s) = %s, want %s", tc.device, got, tc.want)
		}
		if !InMesh(got) {
			t.Errorf("%s derived from %s is outside %s", got, tc.device, MeshPrefix)
		}
	}
}

// TestUUIDVersionsAccepted: the contract requires a UUID, not UUIDv4. v1, v4,
// v6, v7, and v8 spellings all derive addresses from their literal bytes,
// version and variant bits preserved.
func TestUUIDVersionsAccepted(t *testing.T) {
	tenant, err := ParseTenant(testTenant)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"45618f44-2a4a-11d5-85c3-00104a0044c5", // v1
		"12345678-9abc-4def-8123-456789abcdef", // v4-shaped
		"019535d9-3df7-79fb-b466-fa907fa17f9e", // v7
		"a0f2d3e4-5678-6abc-9def-0123456789ab", // v6-shaped
		"f8a1b2c3-d4e5-8f01-a234-56789abcdef0", // v8-shaped
	} {
		d, err := ParseDevice(tenant, text)
		if err != nil {
			t.Errorf("ParseDevice(%q): %v", text, err)
			continue
		}
		id, err := uuid.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		if d.MeshAddress() != MeshAddressFor(id) {
			t.Errorf("address mismatch for %q", text)
		}
	}
}

// TestNonCanonicalAndNonUUIDRejected: non-UUID device components fail clearly
// with no hash fallback, and non-canonical UUID spellings fail at the parse
// boundary so two spellings of one identity can never drift apart downstream.
func TestNonCanonicalAndNonUUIDRejected(t *testing.T) {
	tenant, err := ParseTenant(testTenant)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"12345678-9ABC-4DEF-8123-456789ABCDEF", // uppercase
		"123456789abc4def8123456789abcdef",     // unhyphenated
		"urn:uuid:12345678-9abc-4def-8123-456789abcdef",
		"{12345678-9abc-4def-8123-456789abcdef}",
		"box-01",
		"fleet-a/box-01",
		"asset-42",
		"",
		"12345678-9abc-4def-8123-456789abcde", // wrong length
	} {
		if _, err := ParseDevice(tenant, text); err == nil {
			t.Errorf("ParseDevice(%q) accepted a non-canonical or non-UUID identity", text)
		}
	}
	for _, principal := range []string{
		"spiffe://wendy.sh/tenant/" + testTenant + "/device/box-01",
		"spiffe://wendy.sh/tenant/" + testTenant + "/device/fleet-a/box-01",
		"spiffe://wendy.sh/tenant/" + testTenant + "/service/asset-42",
		"spiffe://wendy.sh/tenant/" + testTenant + "/operator/auth0|abc",
		"urn:wendy:org:7:asset:42",
	} {
		_, err := DeviceFromPrincipal(principal)
		if err == nil {
			t.Errorf("DeviceFromPrincipal(%q) accepted an unsupported identity", principal)
		}
	}
}

// TestSuffixCollision: two different full UUIDs with identical final ten bytes
// share the suffix enrollment arbitrates on — and therefore the address. An
// ordinary full-UUID duplicate shares it too.
func TestSuffixCollision(t *testing.T) {
	tenant, err := ParseTenant(testTenant)
	if err != nil {
		t.Fatal(err)
	}
	a, err := ParseDevice(tenant, "12345678-9abc-4def-8123-456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseDevice(tenant, "87654321-dcba-4def-8123-456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if a.UUID() == b.UUID() {
		t.Fatal("test UUIDs are not distinct")
	}
	if a.Suffix() != b.Suffix() {
		t.Errorf("suffix collision missed: %v vs %v", a.Suffix(), b.Suffix())
	}
	if a.MeshAddress() != b.MeshAddress() {
		t.Errorf("colliding suffixes must share the address: %s vs %s", a.MeshAddress(), b.MeshAddress())
	}
	c, err := ParseDevice(tenant, "12345678-9abc-4def-8123-456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if c.Suffix() != a.Suffix() || Compare(a, c) != 0 {
		t.Error("full duplicate must compare equal and share its suffix")
	}
}

// TestOrderingAndLinkLocals: full 16-byte UUID ordering assigns complementary
// link-locals independently of tenant, and equal identities are refused.
func TestOrderingAndLinkLocals(t *testing.T) {
	tenant, err := ParseTenant(testTenant)
	if err != nil {
		t.Fatal(err)
	}
	other, err := ParseTenant("99999999-8888-4777-8666-555555555555")
	if err != nil {
		t.Fatal(err)
	}
	low, err := ParseDevice(tenant, "019535d9-3df7-79fb-b466-fa907fa17f9e")
	if err != nil {
		t.Fatal(err)
	}
	high, err := ParseDevice(other, "ff9f8e7d-6c5b-4a39-8123-0123456789ab")
	if err != nil {
		t.Fatal(err)
	}
	if Compare(low, high) >= 0 || Compare(high, low) <= 0 {
		t.Fatal("ordering broken")
	}
	selfAddr, peerAddr, err := LinkLocalAddresses(low, high)
	if err != nil {
		t.Fatal(err)
	}
	if selfAddr.String() != "fe80::1" || peerAddr.String() != "fe80::2" {
		t.Errorf("lower UUID got %s/%s, want fe80::1/fe80::2", selfAddr, peerAddr)
	}
	// The peer, computing independently with no exchange, must assign the
	// complementary pair.
	peerSelf, peerPeer, err := LinkLocalAddresses(high, low)
	if err != nil {
		t.Fatal(err)
	}
	if peerSelf != peerAddr || peerPeer != selfAddr {
		t.Errorf("independent assignment not complementary: %s/%s vs %s/%s", peerSelf, peerPeer, selfAddr, peerAddr)
	}
	if _, _, err := LinkLocalAddresses(low, low); err == nil {
		t.Error("self/duplicate identity must be refused")
	}
	// Same device UUID under a different tenant label is still a duplicate
	// identity for link ordering.
	same, err := ParseDevice(other, "019535d9-3df7-79fb-b466-fa907fa17f9e")
	if err != nil {
		t.Fatal(err)
	}
	if Compare(low, same) != 0 {
		t.Error("link ordering must use the device UUID only")
	}
}

// TestPrincipalRoundTrip: a parsed device re-renders the exact principal, so
// caches, pins, and logs all speak one string.
func TestPrincipalRoundTrip(t *testing.T) {
	principal := "spiffe://wendy.sh/tenant/" + testTenant + "/device/12345678-9abc-4def-8123-456789abcdef"
	d, err := DeviceFromPrincipal(principal)
	if err != nil {
		t.Fatal(err)
	}
	if d.Principal() != principal {
		t.Errorf("Principal() = %s, want %s", d.Principal(), principal)
	}
	if d.Tenant().String() != testTenant {
		t.Errorf("tenant = %s, want %s", d.Tenant(), testTenant)
	}
	if d.UUID().String() != "12345678-9abc-4def-8123-456789abcdef" {
		t.Errorf("device UUID = %s", d.UUID())
	}
}

func TestDeviceFromCert(t *testing.T) {
	leafWithURIs := func(uris ...string) *x509.Certificate {
		leaf := &x509.Certificate{Subject: pkix.Name{CommonName: "test"}}
		for _, raw := range uris {
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("parsing test URI %q: %v", raw, err)
			}
			leaf.URIs = append(leaf.URIs, u)
		}
		return leaf
	}
	leaf := leafWithURIs("spiffe://wendy.sh/tenant/" + testTenant + "/device/12345678-9abc-4def-8123-456789abcdef")
	d, err := DeviceFromCert(leaf)
	if err != nil {
		t.Fatal(err)
	}
	if d.MeshAddress().String() != "fd45:b891:567f:4def:8123:4567:89ab:cdef" {
		t.Errorf("address from cert = %s", d.MeshAddress())
	}
	if _, err := DeviceFromCert(leafWithURIs("spiffe://wendy.sh/tenant/" + testTenant + "/device/box-01")); err == nil {
		t.Error("path-shaped device certificate accepted for mesh")
	}
	if _, err := DeviceFromCert(leafWithURIs("spiffe://wendy.sh/tenant/" + testTenant + "/service/asset-42")); err == nil {
		t.Error("cloud-relayed service principal accepted as a device principal")
	}
	if _, err := DeviceFromCert(leafWithURIs()); err == nil {
		t.Error("identity-less certificate accepted")
	}
}

func TestInMeshBoundary(t *testing.T) {
	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"fd45:b891:567f::", true},
		{"fd45:b891:567f:ffff:ffff:ffff:ffff:ffff", true},
		{"fd45:b891:567e::1", false},
		{"fd45:b891:5680::1", false},
		{"fd45:b891:5700::1", false},
		{"2001:db8::1", false},
		{"fe80::1", false},
		{"10.99.0.1", false},
	} {
		if got := InMesh(netip.MustParseAddr(tc.addr)); got != tc.want {
			t.Errorf("InMesh(%s) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestParseTenantRequiresCanonicalForm(t *testing.T) {
	if _, err := ParseTenant("UPPER"); err == nil {
		t.Error("non-canonical tenant accepted")
	}
}

func TestAbsentIdentityDiffersFromExplicitNilUUID(t *testing.T) {
	var absent Device
	if absent.IsValid() || absent.Principal() != "" || absent.MeshAddress().IsValid() {
		t.Fatal("zero-value device fabricated an identity or address")
	}
	tenant, err := ParseTenant(uuid.Nil.String())
	if err != nil || !tenant.IsValid() || tenant == (Tenant{}) {
		t.Fatalf("explicit nil tenant = %#v, %v", tenant, err)
	}
	device, err := ParseDevice(tenant, uuid.Nil.String())
	if err != nil || !device.IsValid() || device == absent {
		t.Fatalf("explicit nil device = %#v, %v", device, err)
	}
	if device.MeshAddress() != MeshAddressFor(uuid.Nil) {
		t.Fatal("nil UUID did not use its literal final 80 bits")
	}
	if parsed, err := DeviceFromPrincipal(device.Principal()); err != nil || parsed != device {
		t.Fatalf("nil principal round trip = %#v, %v", parsed, err)
	}
	if _, err := ParseDevice(Tenant{}, uuid.Nil.String()); err == nil {
		t.Fatal("device parser accepted an absent tenant")
	}
	if NewDevice(Tenant{}, uuid.Nil).IsValid() {
		t.Fatal("constructor accepted an absent tenant")
	}
	if _, _, err := LinkLocalAddresses(absent, device); err == nil {
		t.Fatal("link accepted an absent identity")
	}
}
