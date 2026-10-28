package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Public positive fixtures for the native bounded-cache model. No private key
// is persisted. Each origin has a distinct real P-256 signer and exact URI.
func writeMeshChurnFixtures(directory string) {
	check(os.MkdirAll(directory, 0755))
	scope, err := meshid.ParseTenant(tenant)
	check(err)
	const count = 80
	for i := 1; i <= count; i++ {
		id := uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012x", i))
		device := meshid.NewDevice(scope, id)
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		check(err)
		uri, err := url.Parse(device.Principal())
		check(err)
		certificate := &x509.Certificate{SerialNumber: big.NewInt(int64(i)), NotBefore: time.UnixMilli(1799999400000), NotAfter: time.UnixMilli(1800003600000), KeyUsage: x509.KeyUsageDigitalSignature, URIs: []*url.URL{uri}}
		der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, key.Public(), key)
		check(err)
		spki, err := x509.MarshalPKIXPublicKey(key.Public())
		check(err)
		check(os.WriteFile(filepath.Join(directory, fmt.Sprintf("origin-%03d-spki.der", i)), spki, 0644))
		for revision := 1; revision <= 4; revision++ {
			m := manifest(device, 3)
			m.Revision = uint64(revision)
			m.Route.RouterID = babel.RouterID(1000 + i)
			m.Issued += int64(revision-1) * 10000
			m.Expires += int64(revision-1) * 10000
			if revision >= 2 {
				m.AppEndpoints[0].Port = 44022
				m.Services[0].Port = 29080
				m.Services[0].TXT = []string{"format=f32le", "state=updated"}
			}
			if revision == 2 {
				m.Services = m.Services[:2]
			}
			if revision == 3 {
				m.Withdraw = true
				m.Services = nil
			}
			signed, err := localmesh.SignManifest(m, [][]byte{der}, key, time.UnixMilli(m.Issued))
			check(err)
			check(os.WriteFile(filepath.Join(directory, fmt.Sprintf("origin-%03d-r%d.cbor", i, revision)), encode(signed), 0644))
		}
	}
	check(os.WriteFile(filepath.Join(directory, "README.txt"), []byte("80 distinct P-256 origins, actual signed complete manifests. r1=3 services; r2=changed endpoint/TXT and omission; r3=withdrawal; r4=revival. Public SPKIs/artifacts only; private keys are not saved.\n"), 0644))
}
