// Actual combined-format sizing and positive Go-to-Lite signature fixtures.
package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const tenant = "11111111-1111-4111-8111-111111111111"

func check(err error) {
	if err != nil {
		panic(err)
	}
}
func encode(v any) []byte { b, e := netwire.Marshal(v); check(e); return b }
func manifest(device meshid.Device, count int) localmesh.Manifest {
	m := localmesh.Manifest{Version: localmesh.ManifestVersionRoute, Device: device.UUID(), Revision: 65537,
		Issued: 1800000000000, Expires: 1800000900000, Name: "Sensor lab node", Platform: localmesh.PlatformLite,
		Caps: []string{localmesh.CapabilitySensors}, AgentPort: 5054, AgentTransport: localmesh.AgentMTLS,
		AppEndpoints: []localmesh.AppEndpoint{{Transport: localmesh.AppTLS, Port: 43022}},
		Route:        &localmesh.RouteOrigin{Prefix: netip.PrefixFrom(device.MeshAddress(), 128), RouterID: babel.RouterID(42), Seqno: 7}}
	for i := 0; i < count; i++ {
		m.Services = append(m.Services, localmesh.ManifestService{Mesh: "default", AppID: "sh.example.sensors",
			ServiceID: fmt.Sprintf("temperature-%02d", i), Type: "_sensorlink._tcp", Instance: fmt.Sprintf("Room temperature %02d", i), Port: uint16(28080 + i), TXT: []string{"format=f32le", "channel=42"}})
	}
	check(localmesh.CheckManifestServices(m.Services))
	return m
}
func framed(g netgossip.Message) int {
	return len(encode(localmesh.ControlMessage{Kind: "gossip", Gossip: &g})) + 9
}
func transfer(wire []byte) int {
	h := sha256.Sum256(wire)
	g := netgossip.Message{Version: 1, Type: netgossip.Manifest, Digest: h[:]}
	g.Op = "have"
	total := framed(g)
	g.Op = "want"
	g.Request = 1
	total += framed(g)
	g.Op = "data"
	g.Total = len(wire)
	for g.Offset = 0; g.Offset < len(wire); g.Offset += len(g.Data) {
		g.Data = wire[g.Offset:min(len(wire), g.Offset+1024)]
		total += framed(g)
	}
	return total
}
func main() {
	fixtureDir := flag.String("fixtures-dir", "", "write public positive interoperability fixtures")
	meshDir := flag.String("mesh-churn-fixtures-dir", "", "write public signed 80-origin combined-manifest churn fixtures")
	flag.Parse()
	if *meshDir != "" {
		writeMeshChurnFixtures(*meshDir)
	}
	scope, e := meshid.ParseTenant(tenant)
	check(e)
	type row struct {
		Algorithm         string `json:"algorithm"`
		Services          int    `json:"services"`
		UnsignedBody      int    `json:"unsigned_body_bytes"`
		SignedArtifact    int    `json:"signed_artifact_bytes"`
		FramedPublication int    `json:"framed_publication_bytes"`
		Mesh64Payload     int    `json:"mesh_64_payload_bytes"`
	}
	var rows []row
	for _, profile := range []struct {
		name, id string
		width    int
	}{{"ECDSA-P256", "44444444-4444-4444-8444-444444444444", 71}, {"ML-DSA-65", "55555555-5555-4555-8555-555555555555", 3309}} {
		device := meshid.NewDevice(scope, uuid.MustParse(profile.id))
		for _, count := range []int{0, 1, 3, 8, 16, 32, 64, 128} {
			m := manifest(device, count)
			body := encode(m)
			wire := encode(localmesh.SignedManifest{Principal: device.Principal(), Body: body, Signature: make([]byte, profile.width)})
			rows = append(rows, row{profile.name, count, len(body), len(wire), transfer(wire), 64 * len(wire)})
		}
		if *fixtureDir != "" {
			check(os.MkdirAll(*fixtureDir, 0755))
			var signer crypto.Signer
			if profile.width == 71 {
				signer, e = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			} else {
				signer, e = mldsa.GenerateKey(mldsa.MLDSA65())
			}
			check(e)
			uri, e := url.Parse(device.Principal())
			check(e)
			cert := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.UnixMilli(1799999400000), NotAfter: time.UnixMilli(1800003600000), KeyUsage: x509.KeyUsageDigitalSignature, URIs: []*url.URL{uri}}
			der, e := x509.CreateCertificate(rand.Reader, cert, cert, signer.Public(), signer)
			check(e)
			spki, e := x509.MarshalPKIXPublicKey(signer.Public())
			check(e)
			prefix := "ecdsa"
			if profile.width != 71 {
				prefix = "mldsa65"
			}
			check(os.WriteFile(filepath.Join(*fixtureDir, prefix+"-spki.der"), spki, 0644))
			for index, count := range []int{0, 3, 64} {
				m := manifest(device, count)
				m.Revision = uint64(index + 1)
				m.Issued += int64(index * 1000)
				m.Expires += int64(index * 1000)
				signed, e := localmesh.SignManifest(m, [][]byte{der}, signer, time.UnixMilli(m.Issued))
				check(e)
				check(os.WriteFile(filepath.Join(*fixtureDir, fmt.Sprintf("%s-%d.cbor", prefix, count)), encode(signed), 0644))
			}
		}
	}
	out := json.NewEncoder(os.Stdout)
	out.SetIndent("", "  ")
	check(out.Encode(rows))
}
