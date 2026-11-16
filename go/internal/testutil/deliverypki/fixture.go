// Package deliverypki mints local ML-DSA credentials for transport integration
// tests. It never reads an operator's login or a device's provisioned keys.
package deliverypki

import (
	"crypto/mldsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/delivery"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/builddelivery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
)

const Tenant = "11111111-1111-4111-8111-111111111111"
const TargetPrincipal = "spiffe://wendy.sh/tenant/" + Tenant + "/device/22222222-2222-4222-8222-222222222222"
const BuilderPrincipal = "spiffe://wendy.sh/tenant/" + Tenant + "/device/33333333-3333-4333-8333-333333333333"
const OperatorPrincipal = "spiffe://wendy.sh/tenant/" + Tenant + "/operator/alice"

type Credential struct {
	PEM, Key string
	Leaf     *x509.Certificate
}
type Fixture struct {
	Now                                     time.Time
	TargetID                                meshid.Device
	Root                                    *x509.Certificate
	Chain                                   string
	Target, Builder, OtherBuilder, Operator Credential
	rootKey                                 *mldsa.PrivateKey
	serial                                  int64
}

func New(t testing.TB) *Fixture {
	t.Helper()
	f := &Fixture{Now: time.Now().Truncate(time.Second), serial: 1}
	key, err := mldsa.GenerateKey(mldsa.MLDSA65())
	must(t, err)
	f.rootKey = key
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "local delivery integration authority"}, NotBefore: f.Now.Add(-time.Hour), NotAfter: f.Now.Add(48 * time.Hour), BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, key.Public(), key)
	must(t, err)
	f.Root, err = x509.ParseCertificate(der)
	must(t, err)
	f.Chain = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	f.TargetID, err = meshid.DeviceFromPrincipal(TargetPrincipal)
	must(t, err)
	f.Target = f.Mint(t, TargetPrincipal)
	f.Builder = f.Mint(t, BuilderPrincipal)
	f.OtherBuilder = f.Mint(t, BuilderPrincipal)
	f.Operator = f.Mint(t, OperatorPrincipal)
	return f
}
func (f *Fixture) Mint(t testing.TB, principal string) Credential {
	return f.MintUntil(t, principal, f.Now.Add(15*time.Minute))
}
func (f *Fixture) MintUntil(t testing.TB, principal string, notAfter time.Time) Credential {
	t.Helper()
	f.serial++
	key, err := mldsa.GenerateKey(mldsa.MLDSA65())
	must(t, err)
	uri, err := url.Parse(principal)
	must(t, err)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(f.serial), Subject: pkix.Name{CommonName: "local delivery integration peer"}, NotBefore: f.Now.Add(-time.Minute), NotAfter: notAfter, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{uri}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, f.Root, key.Public(), f.rootKey)
	must(t, err)
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	must(t, err)
	parsed, err := x509.ParseCertificate(der)
	must(t, err)
	return Credential{PEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), Key: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})), Leaf: parsed}
}
func (f *Fixture) ClientTLS(t testing.TB, c Credential) *tls.Config {
	t.Helper()
	cfg, err := mtls.NewClientTLSConfigExpectingDevice(c.PEM, f.Chain, c.Key, zap.NewNop(), f.TargetID)
	must(t, err)
	cfg.ClientSessionCache = nil
	return cfg
}
func (f *Fixture) Manager(t testing.TB) *delivery.Manager {
	t.Helper()
	m := delivery.NewManager(t.TempDir(), "localhost:5000/", func() (string, string) { return f.Target.PEM, f.Chain })
	t.Cleanup(m.Close)
	return m
}
func (f *Fixture) Claims(reference string) builddelivery.Claims {
	return builddelivery.Claims{Version: builddelivery.Version, JobID: uuid.NewString(), Operator: OperatorPrincipal, Target: TargetPrincipal, Builder: BuilderPrincipal, BuilderSPKI: builddelivery.Fingerprint(f.Builder.Leaf), Reference: reference, InputSHA256: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), IssuedAt: f.Now.Unix(), AdmitBefore: f.Now.Add(time.Minute).Unix(), ValidUntil: f.Now.Add(time.Hour).Unix(), MaxBytes: 128 << 20, Operations: []string{"stage", "publish"}}
}
func (f *Fixture) Sign(t testing.TB, c builddelivery.Claims) string {
	t.Helper()
	j, err := builddelivery.Sign(c, f.Operator.PEM, f.Chain, f.Operator.Key)
	must(t, err)
	return j
}
func (f *Fixture) Admit(t testing.TB, m *delivery.Manager, c builddelivery.Claims) string {
	t.Helper()
	snapshot, err := m.Get()
	must(t, err)
	id, err := snapshot.Store.Admit(f.Sign(t, c), f.Operator.Leaf)
	must(t, err)
	return id
}
func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
