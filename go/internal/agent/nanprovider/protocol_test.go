package nanprovider

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshdiscovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const (
	presenceTenant = "5b8c6c8e-2f9f-4b4a-9a4f-6e2d8c9a0b12"
	presenceDevice = "018b8657-2f1a-4d3e-9c1a-2b4f6a8c0d1e"
	// Independently calculated with Python hashlib and uuid.UUID.bytes.
	presenceVector = "574e414e01015b8c6c8e2f9f4b4a9a4f6e2d8c9a0b12018b86572f1a4d3e9c1a2b4f6a8c0d1e8e6d83e9d13ffe98"
)

func presenceFixture(t testing.TB, tenantText, deviceText string) presence {
	t.Helper()
	tenant, err := meshid.ParseTenant(tenantText)
	if err != nil {
		t.Fatal(err)
	}
	device, err := meshid.ParseDevice(tenant, deviceText)
	if err != nil {
		t.Fatal(err)
	}
	hint := meshdiscovery.DefaultHint(tenant)
	return presence{Device: device, Mesh: &hint}
}

func operatorPresenceFixture(t testing.TB) meshid.Device {
	t.Helper()
	peer, err := meshid.PeerFromPrincipal("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/alice")
	if err != nil {
		t.Fatal(err)
	}
	peer, err = peer.WithInstance(uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	return peer
}

func TestPresenceWireVectorAndPrincipal(t *testing.T) {
	p := presenceFixture(t, presenceTenant, presenceDevice)
	ssi, err := encodePresence(p)
	if err != nil || ssi != presenceVector {
		t.Fatalf("wire vector = %q, %v", ssi, err)
	}
	peer, err := decodePresence(p.Device.Tenant(), *p.Mesh, ssi)
	if err != nil || peer.Principal() != "spiffe://wendy.sh/tenant/"+presenceTenant+"/device/"+presenceDevice {
		t.Fatalf("full principal lost: %v, %v", peer, err)
	}
	b, _ := hex.DecodeString(ssi)
	if len(b) != 46 || hex.EncodeToString(b[38:]) != "8e6d83e9d13ffe98" {
		t.Fatalf("hint is not eight raw bytes: %x", b)
	}
}

func TestPresenceScopeAdmission(t *testing.T) {
	self := presenceFixture(t, presenceTenant, presenceDevice)
	foreign := presenceFixture(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", presenceDevice)
	wrongMesh := *self.Mesh
	wrongMesh[0] ^= 1
	for _, tt := range []struct {
		name string
		p    presence
	}{
		{"other tenant", foreign},
		{"other tenant with our hint", presence{Device: foreign.Device, Mesh: self.Mesh}},
		{"other mesh", presence{Device: self.Device, Mesh: &wrongMesh}},
		{"no mesh", presence{Device: self.Device}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ssi, err := encodePresence(tt.p)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodePresence(self.Device.Tenant(), *self.Mesh, ssi); err == nil {
				t.Fatal("out-of-scope presence reached admission")
			}
		})
	}
	if _, err := decodePresence(meshid.Tenant{}, *self.Mesh, presenceVector); err == nil {
		t.Fatal("unprovisioned tenant admitted a peer")
	}
}

func TestPresenceAbsentMeshAndExplicitZeroHint(t *testing.T) {
	p := presenceFixture(t, presenceTenant, presenceDevice)
	p.Mesh = nil
	b, err := p.marshalBinary()
	if err != nil || len(b) != 38 || b[5] != 0 {
		t.Fatalf("absent mesh encoding: %x, %v", b, err)
	}
	parsed, err := parsePresence(b)
	if err != nil || parsed.Mesh != nil || parsed.Device != p.Device {
		t.Fatalf("absent mesh decoding: %+v, %v", parsed, err)
	}
	zero := meshdiscovery.Hint{}
	p.Mesh = &zero
	ssi, err := encodePresence(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodePresence(p.Device.Tenant(), zero, ssi); err != nil {
		t.Fatalf("zero hint confused with absent mesh: %v", err)
	}
}

func TestPresenceAllowsExplicitNilUUIDs(t *testing.T) {
	const nilUUID = "00000000-0000-0000-0000-000000000000"
	p := presenceFixture(t, nilUUID, nilUUID)
	ssi, err := encodePresence(p)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := decodePresence(p.Device.Tenant(), *p.Mesh, ssi)
	if err != nil || peer != p.Device || !peer.IsValid() {
		t.Fatalf("explicit nil UUIDs: %v, %v", peer, err)
	}
	if _, err := encodePresence(presence{}); err == nil {
		t.Fatal("absent identity advertised")
	}
}

func TestPresenceRejectsMalformedAndLegacyPayloads(t *testing.T) {
	p := presenceFixture(t, presenceTenant, presenceDevice)
	good, _ := p.marshalBinary()
	bad := [][]byte{nil, []byte("wendy-nan:1:" + presenceDevice), bytes.Clone(good[:38]), make([]byte, maxPresenceSize+1)}
	for i := range 4 {
		b := bytes.Clone(good)
		b[i] ^= 1
		bad = append(bad, b)
	}
	for version := range 256 {
		if version != 1 {
			b := bytes.Clone(good)
			b[4] = byte(version)
			bad = append(bad, b)
		}
	}
	for size := 0; size < len(good); size++ {
		bad = append(bad, bytes.Clone(good[:size]))
	}
	for _, b := range bad {
		if _, err := parsePresence(b); err == nil {
			t.Fatalf("accepted malformed binary %x", b)
		}
		if _, err := decodePresence(p.Device.Tenant(), *p.Mesh, hex.EncodeToString(b)); err == nil {
			t.Fatalf("admitted malformed SSI %x", b)
		}
	}
	for _, ssi := range []string{"not hex", presenceVector[:len(presenceVector)-1], "z" + presenceVector[1:], string(make([]byte, 1<<20))} {
		if _, err := decodePresence(p.Device.Tenant(), *p.Mesh, ssi); err == nil {
			t.Fatal("accepted invalid hex transport")
		}
	}
}

func TestPresenceV1OptionalExtensionsAndAdvisoryFlags(t *testing.T) {
	p := presenceFixture(t, presenceTenant, presenceDevice)
	good, _ := p.marshalBinary()
	for flags := range 256 {
		for _, extra := range []int{0, 2, maxPresenceSize - presenceMeshSize} {
			b := append(bytes.Clone(good), make([]byte, extra)...)
			b[5] = byte(flags)
			parsed, err := parsePresence(b)
			if err != nil || parsed.Device != p.Device {
				t.Fatalf("flags=%#x extra=%d: known identity lost: %+v, %v", flags, extra, parsed, err)
			}
			hasMesh := flags&presenceMeshPresent != 0
			if (parsed.Mesh != nil) != hasMesh || (hasMesh && *parsed.Mesh != *p.Mesh) {
				t.Fatalf("flags=%#x extra=%d: known mesh hint changed", flags, extra)
			}
			peer, err := decodePresence(p.Device.Tenant(), *p.Mesh, hex.EncodeToString(b))
			if hasMesh && flags&presenceTLSOnly == 0 {
				if err != nil || peer != p.Device {
					t.Fatalf("flags=%#x extra=%d: compatible peer refused: %v", flags, extra, err)
				}
			} else if err == nil {
				t.Fatalf("flags=%#x extra=%d: absent mesh or TLS-only peer admitted", flags, extra)
			}
		}
	}
}

func TestOperatorPresenceOptionalFlagsRetainCompletePrincipalBinding(t *testing.T) {
	peer := operatorPresenceFixture(t)
	hint := meshdiscovery.DefaultHint(peer.Tenant())
	raw, _ := (presence{Device: peer, Mesh: &hint}).marshalBinary()
	raw[5] |= 0x80
	decoded, err := decodePresence(peer.Tenant(), hint, hex.EncodeToString(raw))
	if err != nil || decoded != peer {
		t.Fatalf("advisory flag lost operator identity: %+v, %v", decoded, err)
	}
	if _, err := parsePresence(append(bytes.Clone(raw), 0)); err == nil {
		t.Fatal("version 2 ignored an invalid principal suffix")
	}
	// The v2 suffix is a complete principal, not the v1 optional field area.
	// A syntactically valid appended entity segment must be consumed as part
	// of a different identity rather than ignored as an unknown extension.
	changed, err := parsePresence(append(bytes.Clone(raw), []byte("/extra")...))
	if err != nil || changed.Device == peer || changed.Device.Principal() != peer.Principal()+"/extra" {
		t.Fatalf("version 2 ignored part of its principal: %+v, %v", changed, err)
	}
	raw[5] |= presenceTLSOnly
	if _, err := decodePresence(peer.Tenant(), hint, hex.EncodeToString(raw)); err == nil {
		t.Fatal("TLS-only operator admitted to QUIC provider")
	}
}

func FuzzPresence(f *testing.F) {
	valid, _ := hex.DecodeString(presenceVector)
	f.Add(valid)
	noMesh := bytes.Clone(valid[:38])
	noMesh[5] = 0
	f.Add(noMesh)
	f.Add([]byte("wendy-nan:1:" + presenceDevice))
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := parsePresence(b)
		if err != nil {
			return
		}
		encoded, err := p.marshalBinary()
		canonical, parseErr := parsePresence(encoded)
		if err != nil || parseErr != nil || canonical.Device != p.Device || !p.Device.IsValid() ||
			(canonical.Mesh == nil) != (p.Mesh == nil) || (p.Mesh != nil && *canonical.Mesh != *p.Mesh) {
			t.Fatalf("known presence fields changed: %x -> %x, %v, %v", b, encoded, err, parseErr)
		}
		ssi := hex.EncodeToString(b)
		if p.Mesh != nil && b[5]&presenceTLSOnly == 0 {
			peer, err := decodePresence(p.Device.Tenant(), *p.Mesh, ssi)
			if err != nil || peer != p.Device {
				t.Fatalf("lost admitted identity: %v, %v", peer, err)
			}
		} else if _, err := decodePresence(p.Device.Tenant(), meshdiscovery.Hint{}, ssi); err == nil {
			t.Fatal("mesh-less or unsupported-transport presence admitted")
		}
	})
}

func FuzzPresenceSSI(f *testing.F) {
	f.Add(presenceVector)
	f.Add("not hex")
	f.Fuzz(func(t *testing.T, ssi string) {
		p := presenceFixture(t, presenceTenant, presenceDevice)
		peer, err := decodePresence(p.Device.Tenant(), *p.Mesh, ssi)
		if err != nil {
			return
		}
		b, err := hex.DecodeString(ssi)
		if err != nil || len(b) < presenceMeshSize || b[5]&presenceTLSOnly != 0 || peer.Tenant() != p.Device.Tenant() || !bytes.Equal(b[38:presenceMeshSize], p.Mesh[:]) {
			t.Fatal("invalid SSI reached admission")
		}
	})
}

func TestNDPEventParsing(t *testing.T) {
	kind, values := parseEvent("IFNAME=nan0 <3>NAN-NDP-CONNECTED peer=aa:bb:cc:dd:ee:ff ndp_id=1 peer_ndi=00:11:22:33:44:55 ssi=\n")
	if kind != "NAN-NDP-CONNECTED" || values["ndp_id"] != "1" || values["peer_ndi"] != "00:11:22:33:44:55" {
		t.Fatalf("%s %+v", kind, values)
	}
}

func TestOperatorPresencePreservesPrincipal(t *testing.T) {
	peer := operatorPresenceFixture(t)
	hint := meshdiscovery.DefaultHint(peer.Tenant())
	encoded, err := encodePresence(presence{Device: peer, Mesh: &hint})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodePresence(peer.Tenant(), hint, encoded)
	if err != nil || decoded != peer {
		t.Fatalf("operator identity lost: %+v %v", decoded, err)
	}
	raw, _ := (presence{Device: peer, Mesh: &hint}).marshalBinary()
	if decoded.UUID() != peer.UUID() || decoded.MeshAddress() != peer.MeshAddress() {
		t.Fatal("operator installation identity or address lost")
	}
	other, err := peer.WithInstance(uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"))
	if err != nil {
		t.Fatal(err)
	}
	otherRaw, _ := (presence{Device: other, Mesh: &hint}).marshalBinary()
	otherDecoded, err := parsePresence(otherRaw)
	if err != nil || otherDecoded.Device != other || otherDecoded.Device.MeshAddress() == decoded.MeshAddress() {
		t.Fatalf("operator installations aliased: %+v, %v", otherDecoded, err)
	}
	// Tenant and principal must agree; the untrusted advertised installation
	// is subsequently bound by the authenticated hello and signed manifest.
	raw[6] ^= 1
	if _, err := parsePresence(raw); err == nil {
		t.Fatal("operator tenant binding not checked")
	}
}

func TestOperatorPresenceRequiresInstallation(t *testing.T) {
	peer := operatorPresenceFixture(t)
	principalOnly, err := meshid.PeerFromPrincipal(peer.Principal())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (presence{Device: principalOnly}).marshalBinary(); err == nil {
		t.Fatal("operator without an installation was advertised")
	}
	raw, _ := (presence{Device: peer}).marshalBinary()
	clear(raw[22:38])
	if _, err := parsePresence(raw); err == nil {
		t.Fatal("operator without an installation was accepted")
	}
}
