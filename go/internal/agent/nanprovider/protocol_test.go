package nanprovider

import (
	"bytes"
	"encoding/hex"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshdiscovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"testing"
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
	bad := [][]byte{nil, []byte("wendy-nan:1:" + presenceDevice), append(bytes.Clone(good), 0), bytes.Clone(good[:38])}
	noMesh := bytes.Clone(good)
	noMesh[5] = 0
	bad = append(bad, noMesh)
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
	for flags := 2; flags < 256; flags++ {
		b := bytes.Clone(good)
		b[5] = byte(flags)
		bad = append(bad, b)
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
		if err != nil || !bytes.Equal(b, encoded) || !p.Device.IsValid() {
			t.Fatalf("noncanonical encoding: %x -> %x, %v", b, encoded, err)
		}
		ssi := hex.EncodeToString(b)
		if p.Mesh != nil {
			peer, err := decodePresence(p.Device.Tenant(), *p.Mesh, ssi)
			if err != nil || peer != p.Device {
				t.Fatalf("lost admitted identity: %v, %v", peer, err)
			}
		} else if _, err := decodePresence(p.Device.Tenant(), meshdiscovery.Hint{}, ssi); err == nil {
			t.Fatal("mesh-less presence admitted")
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
		if err != nil || len(b) != 46 || peer.Tenant() != p.Device.Tenant() || !bytes.Equal(b[38:], p.Mesh[:]) {
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
