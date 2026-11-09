package builddelivery

import (
	"bytes"
	"crypto"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type fixture struct {
	now            time.Time
	roots          []*x509.Certificate
	target         meshid.Device
	builder        *x509.Certificate
	builderChain   [][]byte
	claims         Claims
	op, key, chain string
	mint           func(string, []string) (string, string)
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	rootKey, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "delivery test authority"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(48 * time.Hour), BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, rootKey.Public(), rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}))
	serial := int64(1)
	mint := func(principal string, rights []string) (string, string) {
		t.Helper()
		serial++
		key, err := mldsa.GenerateKey(mldsa.MLDSA65())
		if err != nil {
			t.Fatal(err)
		}
		uri, err := url.Parse(principal)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "delivery test leaf"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(15 * time.Minute), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{uri}}
		if rights != nil {
			values := make([]asn1.RawValue, len(rights))
			for i, s := range rights {
				values[i] = asn1.RawValue{Tag: asn1.TagUTF8String, Bytes: []byte(s)}
			}
			der, err := asn1.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			leaf.ExtraExtensions = []pkix.Extension{{Id: entitlementOID, Value: der}}
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, root, key.Public(), rootKey)
		if err != nil {
			t.Fatal(err)
		}
		kb, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}))
	}
	const tenant = "11111111-1111-4111-8111-111111111111"
	target, _ := meshid.DeviceFromPrincipal("spiffe://wendy.sh/tenant/" + tenant + "/device/22222222-2222-4222-8222-222222222222")
	builderPrincipal := "spiffe://wendy.sh/tenant/" + tenant + "/device/33333333-3333-4333-8333-333333333333"
	builderPEM, _ := mint(builderPrincipal, nil)
	block, _ := pem.Decode([]byte(builderPEM))
	builder, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	operator := "spiffe://wendy.sh/tenant/" + tenant + "/operator/alice"
	op, key := mint(operator, []string{"entitlement:deployment:create:device:" + target.UUID().String() + ":allow"})
	claims := Claims{Version: Version, JobID: uuid.NewString(), Operator: operator, Target: target.Principal(), Builder: builderPrincipal, BuilderSPKI: Fingerprint(builder), Reference: "lab/app:latest", InputSHA256: encode(make([]byte, 32)), IssuedAt: now.Unix(), AdmitBefore: now.Add(time.Minute).Unix(), ValidUntil: now.Add(time.Hour).Unix(), Operations: []string{"stage", "publish"}, MaxBytes: 1 << 30}
	return fixture{now: now, roots: []*x509.Certificate{root}, target: target, builder: builder, builderChain: [][]byte{builder.Raw, rootDER}, claims: claims, op: op, key: key, chain: chain, mint: mint}
}
func (f fixture) sign(t *testing.T, c Claims) string {
	t.Helper()
	j, err := Sign(c, f.op, f.chain, f.key)
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func TestContractOfflineAdmissionAndAuthority(t *testing.T) {
	f := newFixture(t)
	j := f.sign(t, f.claims)
	v, err := Admit(j, f.roots, f.target, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if v.Claims().JobID != f.claims.JobID || v.Compact() != j {
		t.Fatal("lost signed binding")
	}
	for _, tc := range []struct {
		name string
		at   time.Time
	}{{"before issuance", f.now.Add(-time.Second)}, {"admission deadline", time.Unix(f.claims.AdmitBefore, 0)}, {"operator gone", f.now.Add(20 * time.Minute)}} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Admit(j, f.roots, f.target, tc.at); err == nil {
				t.Fatal("new admission accepted outside credential/window")
			}
		})
	}
	if _, err := Admit(j, nil, f.target, f.now); err == nil {
		t.Fatal("JWS roots trusted themselves")
	}
	other, _ := meshid.DeviceFromPrincipal(f.claims.Builder)
	if _, err := Admit(j, f.roots, other, f.now); err == nil {
		t.Fatal("wrong audience accepted")
	}
	parts := strings.Split(j, ".")
	raw, _ := decode(parts[1])
	raw = []byte(strings.Replace(string(raw), "lab/app:latest", "lab/other:latest", 1))
	parts[1] = encode(raw)
	if _, err := Admit(strings.Join(parts, "."), f.roots, f.target, f.now); err == nil {
		t.Fatal("tampered reference accepted")
	}
}
func TestEmbeddedEntitlementsDenyWins(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name   string
		rights []string
		allow  bool
	}{
		{"identity alone", nil, false}, {"unscoped allow", []string{"entitlement:deployment:create:allow"}, true}, {"wildcard permission", []string{"entitlement:deployment:*:allow"}, true}, {"wrong verb", []string{"entitlement:deployment:read:allow"}, false}, {"wrong device", []string{"entitlement:deployment:create:device:33333333-3333-4333-8333-333333333333:allow"}, false}, {"site unproven", []string{"entitlement:deployment:create:site:lab:allow"}, false}, {"deny", []string{"entitlement:deployment:*:allow", "entitlement:deployment:create:deny"}, false}, {"unproven tag deny", []string{"entitlement:deployment:create:allow", "entitlement:deployment:create:tag:prod:deny"}, false}, {"foreign device deny", []string{"entitlement:deployment:create:allow", "entitlement:deployment:create:device:other:deny"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			op, key := f.mint(f.claims.Operator, tc.rights)
			j, err := Sign(f.claims, op, f.chain, key)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Admit(j, f.roots, f.target, f.now)
			if (err == nil) != tc.allow {
				t.Fatalf("admit=%v allow=%v", err, tc.allow)
			}
		})
	}
	device, key := f.mint(f.claims.Builder, []string{"entitlement:deployment:create:allow"})
	c := f.claims
	c.Operator = c.Builder
	if _, err := Sign(c, device, f.chain, key); err == nil {
		t.Fatal("device can issue operator contract")
	}
}
func TestBuilderPKIVerificationBindsExpectedIdentity(t *testing.T) {
	f := newFixture(t)
	expected, _ := meshid.DeviceFromPrincipal(f.claims.Builder)
	leaf, err := VerifyBuilderCertificate(f.builderChain, f.roots, expected, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(leaf) != f.claims.BuilderSPKI {
		t.Fatal("SPKI binding changed")
	}
	if _, err = VerifyBuilderCertificate(f.builderChain, f.roots, f.target, f.now); err == nil {
		t.Fatal("wrong expected builder accepted")
	}
	if _, err = VerifyBuilderCertificate(f.builderChain, nil, expected, f.now); err == nil {
		t.Fatal("untrusted builder accepted")
	}
}
func TestSignedJSONCannotHaveAmbiguousOrExtendedMeaning(t *testing.T) {
	f := newFixture(t)
	good := f.sign(t, f.claims)
	parts := strings.Split(good, ".")
	pairKeyBlock, _ := pem.Decode([]byte(f.key))
	key, err := x509.ParsePKCS8PrivateKey(pairKeyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	signer := key.(crypto.Signer)
	resign := func(h, p []byte) string {
		input := encode(h) + "." + encode(p)
		s, e := signer.Sign(rand.Reader, []byte(input), crypto.Hash(0))
		if e != nil {
			t.Fatal(e)
		}
		return input + "." + encode(s)
	}
	h, _ := decode(parts[0])
	p, _ := decode(parts[1])
	for _, tc := range []struct {
		name string
		h, p []byte
	}{{"aliased claim", h, bytes.Replace(p, []byte(`"iss"`), []byte(`"ISS"`), 1)}, {"null expected digest", h, bytes.Replace(p, []byte(`"expected_digest":""`), []byte(`"expected_digest":null`), 1)}, {"aliased header", bytes.Replace(h, []byte(`"alg"`), []byte(`"ALG"`), 1), p}, {"duplicate claim", h, append([]byte(`{"exp":1,`), p[1:]...)}, {"unknown claim", h, append([]byte(`{"future_authority":true,`), p[1:]...)}, {"duplicate header", append([]byte(`{"alg":"none",`), h[1:]...), p}, {"unknown header", append([]byte(`{"jku":"https://evil.invalid",`), h[1:]...), p}} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Admit(resign(tc.h, tc.p), f.roots, f.target, f.now); err == nil {
				t.Fatal("ambiguous/extended artifact accepted")
			}
		})
	}
}
func TestClaimsRejectExpandedAuthority(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name   string
		change func(*Claims)
	}{
		{"host reference", func(c *Claims) { c.Reference = "registry.invalid/app:latest" }}, {"missing tag", func(c *Claims) { c.Reference = "app" }}, {"unbounded time", func(c *Claims) { c.ValidUntil = c.IssuedAt + int64((MaxLifetime+time.Second)/time.Second) }}, {"no budget", func(c *Claims) { c.MaxBytes = 0 }}, {"extra operation", func(c *Claims) { c.Operations = append(c.Operations, "delete") }}, {"digest uppercase", func(c *Claims) { c.ExpectedDigest = "sha256:" + strings.Repeat("A", 64) }}, {"unknown version", func(c *Claims) { c.Version++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := f.claims
			c.Operations = append([]string(nil), c.Operations...)
			tc.change(&c)
			if err := validate(c); err == nil {
				b, _ := json.Marshal(c)
				t.Fatalf("accepted %s", b)
			}
		})
	}
}

func TestReferencesPreserveWendyAppNames(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"helloworld:latest", "sh.wendy.examples.hellopython:latest", "lab/app:release-1"} {
		c := f.claims
		c.Reference = name
		if err := validate(c); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestBase64URLRejectsIgnoredWhitespace(t *testing.T) {
	good := encode([]byte("public binding"))
	if _, err := decode(good); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{good + "\n", "\r" + good, good[:2] + "\n" + good[2:], good + "="} {
		if _, err := decode(invalid); err == nil {
			t.Fatalf("accepted noncanonical encoding %q", invalid)
		}
	}
}
