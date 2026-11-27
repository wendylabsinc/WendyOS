package wendycert_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"math/big"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

var now = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func verifier(t *testing.T, f *testidentity.Fixture) *wendycert.Verifier {
	t.Helper()
	v, err := wendycert.New(wendycert.Config{Tenant: f.Tenant, Roots: [][]byte{f.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestOperatorChainAndRawSignature(t *testing.T) {
	f := testidentity.New(t, now)
	id, err := verifier(t, f).Verify(f.ChainDER, now.Add(-time.Second), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if id.Principal != f.Principal || id.Tenant != f.Tenant || len(id.SigningKeySHA256) != 64 || len(id.CertificateSHA256) != 64 || !id.ValidUntil.Equal(f.NotAfter) || !id.ValidFrom.Equal(now.Add(-time.Second)) {
		t.Fatal("identity or path bounds differ", id)
	}
	message := []byte("test-only world proof")
	sig, err := f.Key.Sign(rand.Reader, message, &mldsa.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = id.VerifySignature(message, sig); err != nil {
		t.Fatal(err)
	}
	if id.VerifySignature([]byte("changed"), sig) == nil {
		t.Fatal("modified message accepted")
	}
	sig[0] ^= 1
	if id.VerifySignature(message, sig) == nil {
		t.Fatal("modified signature accepted")
	}
	if (wendycert.Identity{}).VerifySignature(message, sig) == nil {
		t.Fatal("unverified zero identity accepted signature")
	}
}

func TestLeafAndIdentityRefusals(t *testing.T) {
	f := testidentity.New(t, now)
	for name, change := range map[string]func(*x509.Certificate){
		"device": func(c *x509.Certificate) {
			c.URIs = []*url.URL{mustURL(t, "spiffe://wendy.sh/tenant/"+f.Tenant+"/device/test")}
		},
		"service-user": func(c *x509.Certificate) {
			c.URIs = []*url.URL{mustURL(t, "spiffe://wendy.sh/tenant/"+f.Tenant+"/service/user-test")}
		},
		"signer": func(c *x509.Certificate) {
			c.URIs = []*url.URL{mustURL(t, "spiffe://wendy.sh/tenant/"+f.Tenant+"/signer/test")}
		},
		"foreign-tenant": func(c *x509.Certificate) {
			c.URIs = []*url.URL{mustURL(t, "spiffe://wendy.sh/tenant/22222222-2222-4222-8222-222222222222/operator/test")}
		},
		"duplicate-principal": func(c *x509.Certificate) { c.URIs = append(c.URIs, c.URIs[0]) },
		"query-principal":     func(c *x509.Certificate) { c.URIs = []*url.URL{mustURL(t, f.Principal+"?other=true")} },
		"missing-principal":   func(c *x509.Certificate) { c.URIs = nil },
		"CA-leaf": func(c *x509.Certificate) {
			c.IsCA = true
			c.BasicConstraintsValid = true
			c.KeyUsage |= x509.KeyUsageCertSign
		},
		"missing-EKU": func(c *x509.Certificate) { c.ExtKeyUsage = nil },
		"any-EKU":     func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageAny} },
		"server-EKU":  func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth} },
		"expired":     func(c *x509.Certificate) { c.NotAfter = now.Add(-time.Second) },
		"future":      func(c *x509.Certificate) { c.NotBefore = now.Add(time.Second) },
		"lifetime":    func(c *x509.Certificate) { c.NotBefore = now.Add(-733 * 24 * time.Hour) },
		"critical-extension": func(c *x509.Certificate) {
			c.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 2, 3, 4}, Critical: true, Value: []byte{5, 0}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			leaf := *f.Leaf
			leaf.URIs = append([]*url.URL(nil), f.Leaf.URIs...)
			leaf.ExtraExtensions = nil
			change(&leaf)
			c := testidentity.Issue(t, &leaf, f.Issuer, f.Key.Public(), f.IssuerKey)
			if _, err := verifier(t, f).Verify([][]byte{c.Raw, f.Issuer.Raw}, now, now); err == nil {
				t.Fatal("invalid identity accepted")
			}
		})
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, e := url.Parse(s)
	if e != nil {
		t.Fatal(e)
	}
	return u
}

func TestWrongSigningAlgorithmAndUnknownRoot(t *testing.T) {
	f := testidentity.New(t, now)
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := testidentity.Issue(t, f.Leaf, f.Issuer, k.Public(), f.IssuerKey)
	if _, err = verifier(t, f).Verify([][]byte{c.Raw, f.Issuer.Raw}, now, now); err == nil {
		t.Fatal("ECDSA request signer accepted")
	}
	other := testidentity.New(t, now)
	if _, err = verifier(t, other).Verify(append(f.ChainDER, f.RootDER), now, now); err == nil {
		t.Fatal("peer root promoted to trust")
	}
}

func TestPathValidityConstraintsAndUnrelatedIssuer(t *testing.T) {
	f := testidentity.New(t, now)
	for name, change := range map[string]func(*x509.Certificate){
		"expired-issuer":      func(c *x509.Certificate) { c.NotAfter = now.Add(-time.Second) },
		"interval-end-issuer": func(c *x509.Certificate) { c.NotAfter = now.Add(time.Second) },
		"constraint": func(c *x509.Certificate) {
			c.PermittedDNSDomainsCritical = true
			c.PermittedURIDomains = []string{"other.invalid"}
		},
		"critical": func(c *x509.Certificate) {
			c.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 2, 3, 4}, Critical: true, Value: []byte{5, 0}}}
		},
		"not-CA": func(c *x509.Certificate) { c.IsCA = false },
	} {
		t.Run(name, func(t *testing.T) {
			issuer := *f.Issuer
			issuer.ExtraExtensions = nil
			change(&issuer)
			c := testidentity.Issue(t, &issuer, f.Root, f.IssuerKey.Public(), f.RootKey)
			if _, err := verifier(t, f).Verify([][]byte{f.Leaf.Raw, c.Raw}, now, now.Add(2*time.Second)); err == nil {
				t.Fatal("invalid issuer path accepted")
			}
		})
	}
	// An unrelated early-expiring candidate must not shorten the selected path.
	unused := *f.Issuer
	unused.Subject = pkix.Name{CommonName: "unused issuer"}
	unused.NotAfter = now.Add(time.Minute)
	other := testidentity.Issue(t, &unused, f.Root, f.IssuerKey.Public(), f.RootKey)
	id, err := verifier(t, f).Verify([][]byte{f.Leaf.Raw, f.Issuer.Raw, other.Raw}, now, now.Add(time.Second))
	if err != nil || !id.ValidUntil.Equal(f.NotAfter) {
		t.Fatal("unused issuer affected expiry", id.ValidUntil, err)
	}
}

func TestIntervalCannotCombineDifferentSuccessfulPaths(t *testing.T) {
	f := testidentity.New(t, now)
	left, right := *f.Issuer, *f.Issuer
	left.NotAfter = now
	right.NotBefore = now
	left.SerialNumber = big.NewInt(10)
	right.SerialNumber = big.NewInt(11)
	a := testidentity.Issue(t, &left, f.Root, f.IssuerKey.Public(), f.RootKey)
	b := testidentity.Issue(t, &right, f.Root, f.IssuerKey.Public(), f.RootKey)
	chain := [][]byte{f.Leaf.Raw, a.Raw, b.Raw}
	v := verifier(t, f)
	earliest, latest := now.Add(-time.Second), now.Add(time.Second)
	for _, at := range []time.Time{earliest, latest} {
		if _, err := v.Verify(chain, at, at); err != nil {
			t.Fatal("fixture endpoint must have a valid path", err)
		}
	}
	if _, err := v.Verify(chain, earliest, latest); err == nil {
		t.Fatal("combined alternate paths across interval")
	}
}

func TestRenewalCopiesInputsAndBounds(t *testing.T) {
	f := testidentity.New(t, now)
	root := append([]byte(nil), f.RootDER...)
	v, err := wendycert.New(wendycert.Config{Tenant: f.Tenant, Roots: [][]byte{root}})
	if err != nil {
		t.Fatal(err)
	}
	clear(root)
	first, err := v.Verify(f.ChainDER, now, now)
	if err != nil {
		t.Fatal("caller mutated retained root", err)
	}
	renew := *f.Leaf
	renew.SerialNumber = big.NewInt(99)
	c := testidentity.Issue(t, &renew, f.Issuer, f.Key.Public(), f.IssuerKey)
	second, err := v.Verify([][]byte{c.Raw, f.Issuer.Raw}, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.SigningKeySHA256 != second.SigningKeySHA256 || first.CertificateSHA256 == second.CertificateSHA256 {
		t.Fatal("renewal identity hashes incorrect")
	}
	for _, bad := range [][][]byte{nil, {bytes.Repeat([]byte{1}, wendycert.MaxCertificateBytes+1)}, {append(append([]byte(nil), f.Leaf.Raw...), 0)}, make([][]byte, wendycert.MaxCertificates+1)} {
		if _, err = v.Verify(bad, now, now); err == nil {
			t.Fatal("unbounded or malformed chain accepted")
		}
	}
	for _, window := range [][2]time.Time{{{}, now}, {now, {}}, {now.Add(time.Second), now}} {
		if _, err = v.Verify(f.ChainDER, window[0], window[1]); err == nil {
			t.Fatal("invalid clock accepted")
		}
	}
	if _, err = wendycert.New(wendycert.Config{Tenant: f.Tenant, Roots: [][]byte{f.Leaf.Raw}}); err == nil {
		t.Fatal("configured leaf accepted as anchor")
	}
}

func TestCandidateRevocationSetIncludesUnusedAndRootAndReturnsCopies(t *testing.T) {
	f := testidentity.New(t, now)
	other := testidentity.New(t, now)
	chain := append(append([][]byte(nil), f.ChainDER...), other.Issuer.Raw)
	id, err := verifier(t, f).Verify(chain, now, now)
	if err != nil {
		t.Fatal(err)
	}
	certHash := func(c *x509.Certificate) string { h := sha256.Sum256(c.Raw); return hex.EncodeToString(h[:]) }
	keyHash := func(c *x509.Certificate) string {
		h := sha256.Sum256(c.RawSubjectPublicKeyInfo)
		return hex.EncodeToString(h[:])
	}
	for _, c := range []*x509.Certificate{f.Leaf, f.Issuer, f.Root, other.Issuer} {
		if !slices.Contains(id.CandidateCertificateSHA256(), certHash(c)) || !slices.Contains(id.CandidateSigningKeySHA256(), keyHash(c)) {
			t.Fatal("revocation candidate omitted")
		}
	}
	certs, keys := id.CandidateCertificateSHA256(), id.CandidateSigningKeySHA256()
	certs[0], keys[0] = "changed", "changed"
	if slices.Contains(id.CandidateCertificateSHA256(), "changed") || slices.Contains(id.CandidateSigningKeySHA256(), "changed") {
		t.Fatal("caller mutated revocation candidates")
	}
}

// A re-signed alternate SPKI must preserve the key used for replay/revocation.
func TestAlternateSPKIEncodingKeepsKeyIdentity(t *testing.T) {
	f := testidentity.New(t, now)
	v := verifier(t, f)
	original, err := v.Verify(f.ChainDER, now, now)
	if err != nil {
		t.Fatal(err)
	}
	outer := sequenceChildren(t, f.Leaf.Raw)
	tbs := sequenceChildren(t, outer[0])
	spki := sequenceChildren(t, tbs[6])
	tbs[6] = rawSequence(t, append(spki, asn1.NullBytes))
	outer[0] = rawSequence(t, tbs)
	sig, err := f.IssuerKey.Sign(rand.Reader, outer[0], &mldsa.Options{})
	if err != nil {
		t.Fatal(err)
	}
	outer[2], err = asn1.Marshal(asn1.BitString{Bytes: sig, BitLength: len(sig) * 8})
	if err != nil {
		t.Fatal(err)
	}
	alternateDER := rawSequence(t, outer)
	alternate, err := x509.ParseCertificate(alternateDER)
	if err != nil {
		t.Fatalf("alternate parser behavior changed: %v", err)
	}
	if bytes.Equal(alternate.RawSubjectPublicKeyInfo, f.Leaf.RawSubjectPublicKeyInfo) {
		t.Fatal("SPKI was not altered")
	}
	canonical, err := x509.MarshalPKIXPublicKey(alternate.PublicKey)
	if err != nil || !bytes.Equal(canonical, f.Leaf.RawSubjectPublicKeyInfo) {
		t.Fatal("alternate encoding changed actual key", err)
	}
	got, err := v.Verify([][]byte{alternateDER, f.Issuer.Raw}, now, now)
	if err != nil {
		t.Fatal("re-signed alternate chain rejected", err)
	}
	if got.SigningKeySHA256 != original.SigningKeySHA256 {
		t.Fatal("same public key acquired a different replay/revocation identity")
	}
	if got.CertificateSHA256 == original.CertificateSHA256 {
		t.Fatal("certificate fingerprint must retain exact DER identity")
	}
	if !slices.Equal(got.CandidateSigningKeySHA256(), original.CandidateSigningKeySHA256()) {
		t.Fatal("candidate key identities changed for the same parsed keys")
	}
	// NULL algorithm parameters are rejected, unlike trailing SPKI elements.
	spki = sequenceChildren(t, f.Leaf.RawSubjectPublicKeyInfo)
	algorithm := sequenceChildren(t, spki[0])
	spki[0] = rawSequence(t, append(algorithm, asn1.NullBytes))
	tbs[6] = rawSequence(t, spki)
	outer[0] = rawSequence(t, tbs)
	if _, err = x509.ParseCertificate(rawSequence(t, outer)); err == nil {
		t.Fatal("ML-DSA NULL parameters accepted")
	}
}

func sequenceChildren(t *testing.T, der []byte) [][]byte {
	t.Helper()
	var seq asn1.RawValue
	rest, err := asn1.Unmarshal(der, &seq)
	if err != nil || len(rest) != 0 || seq.Tag != asn1.TagSequence {
		t.Fatal("bad test sequence", err)
	}
	var out [][]byte
	for contents := seq.Bytes; len(contents) > 0; {
		var child asn1.RawValue
		contents, err = asn1.Unmarshal(contents, &child)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, child.FullBytes)
	}
	return out
}
func rawSequence(t *testing.T, children [][]byte) []byte {
	t.Helper()
	der, err := asn1.Marshal(asn1.RawValue{Class: 0, Tag: asn1.TagSequence, IsCompound: true, Bytes: bytes.Join(children, nil)})
	if err != nil {
		t.Fatal(err)
	}
	return der
}
