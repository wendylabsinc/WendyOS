package mtls

import (
	"bytes"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"strings"
	"testing"
	"time"

	circlSign "github.com/cloudflare/circl/sign"
	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
)

// tbsCertificate is the ASN.1 structure for the to-be-signed portion of an
// X.509 certificate. We build it manually because Go's crypto/x509 does not
// support ML-DSA as a signature algorithm.
type tbsCertificate struct {
	Version              int `asn1:"optional,explicit,tag:0,default:0"`
	SerialNumber         *big.Int
	Signature            algID
	Issuer               asn1.RawValue
	Validity             validity
	Subject              asn1.RawValue
	SubjectPublicKeyInfo spkiOuter
	Extensions           []pkix.Extension `asn1:"optional,explicit,tag:3"`
}

type validity struct {
	NotBefore time.Time
	NotAfter  time.Time
}

// reverseBitsInAByte reverses the bit order in a byte, matching the
// encoding used by Go's crypto/x509 for ASN.1 BIT STRING KeyUsage extensions.
func reverseBitsInAByte(in byte) byte {
	b1 := in>>4 | in<<4
	b2 := b1>>2&0x33 | b1<<2&0xcc
	b3 := b2>>1&0x55 | b2<<1&0xaa
	return b3
}

// asn1BitLength returns the number of significant bits in a bit string,
// counting from the MSB of the first byte (as per ASN.1 convention).
func asn1BitLength(bitString []byte) int {
	bitLen := len(bitString) * 8
	for i := range bitString {
		b := bitString[len(bitString)-i-1]
		for bit := uint(0); bit < 8; bit++ {
			if (b>>bit)&1 == 1 {
				return bitLen
			}
			bitLen--
		}
	}
	return 0
}

// buildMLDSACACert creates a self-signed CA certificate using ML-DSA-65.
// It sets BasicConstraints (isCA=true) and optionally KeyUsageCertSign.
// Pass withCertSign=false to create a CA cert that is missing the CertSign
// KeyUsage, so that verifyMLDSAClientCert rejects it before reaching
// signature verification.
func buildMLDSACACert(t *testing.T, subject pkix.Name, withCertSign bool) (*x509.Certificate, circlSign.PrivateKey) {
	t.Helper()

	scheme := mldsa65.Scheme()
	pub, priv, err := scheme.GenerateKey()
	if err != nil {
		t.Fatalf("generating ML-DSA key: %v", err)
	}

	pubBytes, err := pub.MarshalBinary()
	if err != nil {
		t.Fatalf("marshaling public key: %v", err)
	}

	subjectRDN, err := asn1.Marshal(subject.ToRDNSequence())
	if err != nil {
		t.Fatalf("marshaling subject: %v", err)
	}

	var keyUsageBits x509.KeyUsage
	if withCertSign {
		keyUsageBits = x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign
	} else {
		// Only DigitalSignature — no CertSign. This causes the verifier to
		// reject this CA before it reaches signature verification.
		keyUsageBits = x509.KeyUsageDigitalSignature
	}
	keyUsageExt, err := buildKeyUsageExt(keyUsageBits)
	if err != nil {
		t.Fatalf("building key usage extension: %v", err)
	}

	basicConstraintsExt, err := buildBasicConstraintsExt(true)
	if err != nil {
		t.Fatalf("building basic constraints extension: %v", err)
	}

	ekuExt, err := buildEKUExt([]asn1.ObjectIdentifier{
		{1, 3, 6, 1, 5, 5, 7, 3, 2}, // id-kp-clientAuth
	})
	if err != nil {
		t.Fatalf("building EKU extension: %v", err)
	}

	spki := spkiOuter{
		Algorithm: algID{Algorithm: oidMLDSA65},
		PublicKey: asn1.BitString{Bytes: pubBytes, BitLength: len(pubBytes) * 8},
	}

	now := time.Now()
	tbs := tbsCertificate{
		Version:      2, // X.509v3
		SerialNumber: big.NewInt(now.UnixNano()),
		Signature:    algID{Algorithm: oidMLDSA65},
		Issuer:       asn1.RawValue{FullBytes: subjectRDN},
		Validity: validity{
			NotBefore: now.Add(-time.Hour),
			NotAfter:  now.Add(24 * time.Hour),
		},
		Subject:              asn1.RawValue{FullBytes: subjectRDN},
		SubjectPublicKeyInfo: spki,
		Extensions:           []pkix.Extension{basicConstraintsExt, keyUsageExt, ekuExt},
	}

	tbsDER, err := asn1.Marshal(tbs)
	if err != nil {
		t.Fatalf("marshaling TBSCertificate: %v", err)
	}

	opts := &circlSign.SignatureOpts{Context: ""}
	sig := scheme.Sign(priv, tbsDER, opts)

	outer := certOuter{
		TBSCertificate:     asn1.RawValue{FullBytes: tbsDER},
		SignatureAlgorithm: algID{Algorithm: oidMLDSA65},
		Signature:          asn1.BitString{Bytes: sig, BitLength: len(sig) * 8},
	}

	certDER, err := asn1.Marshal(outer)
	if err != nil {
		t.Fatalf("marshaling certificate: %v", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		// ML-DSA certs may produce "trailing data" errors; strip to exact ASN.1 element.
		var raw asn1.RawValue
		if _, asn1Err := asn1.Unmarshal(certDER, &raw); asn1Err != nil {
			t.Fatalf("parsing ML-DSA CA certificate: %v (asn1 err: %v)", err, asn1Err)
		}
		cert, err = x509.ParseCertificate(raw.FullBytes)
		if err != nil {
			t.Fatalf("parsing ML-DSA CA certificate after ASN.1 trim: %v", err)
		}
	}

	return cert, priv
}

// buildMLDSACACertExpired creates a self-signed CA certificate that is already
// expired (NotBefore and NotAfter are both in the past). This causes
// verifyMLDSAClientCert to reject it with "not valid at current time".
func buildMLDSACACertExpired(t *testing.T, subject pkix.Name) (*x509.Certificate, circlSign.PrivateKey) {
	t.Helper()

	scheme := mldsa65.Scheme()
	pub, priv, err := scheme.GenerateKey()
	if err != nil {
		t.Fatalf("generating ML-DSA key: %v", err)
	}

	pubBytes, err := pub.MarshalBinary()
	if err != nil {
		t.Fatalf("marshaling public key: %v", err)
	}

	subjectRDN, err := asn1.Marshal(subject.ToRDNSequence())
	if err != nil {
		t.Fatalf("marshaling subject: %v", err)
	}

	keyUsageExt, err := buildKeyUsageExt(x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign)
	if err != nil {
		t.Fatalf("building key usage extension: %v", err)
	}

	basicConstraintsExt, err := buildBasicConstraintsExt(true)
	if err != nil {
		t.Fatalf("building basic constraints extension: %v", err)
	}

	ekuExt, err := buildEKUExt([]asn1.ObjectIdentifier{
		{1, 3, 6, 1, 5, 5, 7, 3, 2}, // id-kp-clientAuth
	})
	if err != nil {
		t.Fatalf("building EKU extension: %v", err)
	}

	spki := spkiOuter{
		Algorithm: algID{Algorithm: oidMLDSA65},
		PublicKey: asn1.BitString{Bytes: pubBytes, BitLength: len(pubBytes) * 8},
	}

	// Place both NotBefore and NotAfter firmly in the past so the CA is expired.
	past := time.Now().Add(-48 * time.Hour)
	tbs := tbsCertificate{
		Version:      2, // X.509v3
		SerialNumber: big.NewInt(past.UnixNano()),
		Signature:    algID{Algorithm: oidMLDSA65},
		Issuer:       asn1.RawValue{FullBytes: subjectRDN},
		Validity: validity{
			NotBefore: past.Add(-time.Hour),
			NotAfter:  past, // expired 48 h ago
		},
		Subject:              asn1.RawValue{FullBytes: subjectRDN},
		SubjectPublicKeyInfo: spki,
		Extensions:           []pkix.Extension{basicConstraintsExt, keyUsageExt, ekuExt},
	}

	tbsDER, err := asn1.Marshal(tbs)
	if err != nil {
		t.Fatalf("marshaling TBSCertificate: %v", err)
	}

	opts := &circlSign.SignatureOpts{Context: ""}
	sig := scheme.Sign(priv, tbsDER, opts)

	outer := certOuter{
		TBSCertificate:     asn1.RawValue{FullBytes: tbsDER},
		SignatureAlgorithm: algID{Algorithm: oidMLDSA65},
		Signature:          asn1.BitString{Bytes: sig, BitLength: len(sig) * 8},
	}

	certDER, err := asn1.Marshal(outer)
	if err != nil {
		t.Fatalf("marshaling certificate: %v", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		var raw asn1.RawValue
		if _, asn1Err := asn1.Unmarshal(certDER, &raw); asn1Err != nil {
			t.Fatalf("parsing expired ML-DSA CA certificate: %v (asn1 err: %v)", err, asn1Err)
		}
		cert, err = x509.ParseCertificate(raw.FullBytes)
		if err != nil {
			t.Fatalf("parsing expired ML-DSA CA certificate after ASN.1 trim: %v", err)
		}
	}

	return cert, priv
}

// buildMLDSALeafCert creates a leaf certificate (not a CA) signed by the given
// issuer using ML-DSA-65. The leaf has the clientAuth EKU and no CertSign KeyUsage.
func buildMLDSALeafCert(t *testing.T, issuerCert *x509.Certificate, issuerPriv circlSign.PrivateKey) *x509.Certificate {
	t.Helper()

	scheme := mldsa65.Scheme()
	pub, _, err := scheme.GenerateKey()
	if err != nil {
		t.Fatalf("generating leaf ML-DSA key: %v", err)
	}

	pubBytes, err := pub.MarshalBinary()
	if err != nil {
		t.Fatalf("marshaling leaf public key: %v", err)
	}

	leafSubject := pkix.Name{CommonName: "test-leaf", Organization: []string{"WendyTest"}}
	subjectRDN, err := asn1.Marshal(leafSubject.ToRDNSequence())
	if err != nil {
		t.Fatalf("marshaling leaf subject: %v", err)
	}

	ekuExt, err := buildEKUExt([]asn1.ObjectIdentifier{
		{1, 3, 6, 1, 5, 5, 7, 3, 2}, // id-kp-clientAuth
	})
	if err != nil {
		t.Fatalf("building EKU extension: %v", err)
	}

	spki := spkiOuter{
		Algorithm: algID{Algorithm: oidMLDSA65},
		PublicKey: asn1.BitString{Bytes: pubBytes, BitLength: len(pubBytes) * 8},
	}

	now := time.Now()
	tbs := tbsCertificate{
		Version:      2,
		SerialNumber: big.NewInt(now.UnixNano() + 1),
		Signature:    algID{Algorithm: oidMLDSA65},
		// Issuer bytes come directly from the issuer's RawSubject so that
		// bytes.Equal(ca.RawSubject, leaf.RawIssuer) is guaranteed to hold.
		Issuer: asn1.RawValue{FullBytes: issuerCert.RawSubject},
		Validity: validity{
			NotBefore: now.Add(-time.Hour),
			NotAfter:  now.Add(24 * time.Hour),
		},
		Subject:              asn1.RawValue{FullBytes: subjectRDN},
		SubjectPublicKeyInfo: spki,
		Extensions:           []pkix.Extension{ekuExt},
	}

	tbsDER, err := asn1.Marshal(tbs)
	if err != nil {
		t.Fatalf("marshaling leaf TBSCertificate: %v", err)
	}

	opts := &circlSign.SignatureOpts{Context: ""}
	sig := scheme.Sign(issuerPriv, tbsDER, opts)

	outer := certOuter{
		TBSCertificate:     asn1.RawValue{FullBytes: tbsDER},
		SignatureAlgorithm: algID{Algorithm: oidMLDSA65},
		Signature:          asn1.BitString{Bytes: sig, BitLength: len(sig) * 8},
	}

	certDER, err := asn1.Marshal(outer)
	if err != nil {
		t.Fatalf("marshaling leaf certificate: %v", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		var raw asn1.RawValue
		if _, asn1Err := asn1.Unmarshal(certDER, &raw); asn1Err != nil {
			t.Fatalf("parsing ML-DSA leaf certificate: %v (asn1 err: %v)", err, asn1Err)
		}
		cert, err = x509.ParseCertificate(raw.FullBytes)
		if err != nil {
			t.Fatalf("parsing ML-DSA leaf certificate after ASN.1 trim: %v", err)
		}
	}

	return cert
}

// buildKeyUsageExt encodes a KeyUsage value as an X.509 extension (OID 2.5.29.15).
// The bit encoding matches Go's crypto/x509 marshalKeyUsage implementation.
func buildKeyUsageExt(usage x509.KeyUsage) (pkix.Extension, error) {
	var a [2]byte
	a[0] = reverseBitsInAByte(byte(usage))
	a[1] = reverseBitsInAByte(byte(usage >> 8))
	l := 1
	if a[1] != 0 {
		l = 2
	}
	bitString := a[:l]
	encoded, err := asn1.Marshal(asn1.BitString{Bytes: bitString, BitLength: asn1BitLength(bitString)})
	if err != nil {
		return pkix.Extension{}, err
	}
	return pkix.Extension{
		Id:       asn1.ObjectIdentifier{2, 5, 29, 15},
		Critical: true,
		Value:    encoded,
	}, nil
}

// buildBasicConstraintsExt encodes a BasicConstraints extension (OID 2.5.29.19).
func buildBasicConstraintsExt(isCA bool) (pkix.Extension, error) {
	type basicConstraints struct {
		IsCA bool `asn1:"optional"`
	}
	val, err := asn1.Marshal(basicConstraints{IsCA: isCA})
	if err != nil {
		return pkix.Extension{}, err
	}
	return pkix.Extension{
		Id:       asn1.ObjectIdentifier{2, 5, 29, 19},
		Critical: true,
		Value:    val,
	}, nil
}

// buildEKUExt encodes an ExtendedKeyUsage extension (OID 2.5.29.37).
func buildEKUExt(oids []asn1.ObjectIdentifier) (pkix.Extension, error) {
	val, err := asn1.Marshal(oids)
	if err != nil {
		return pkix.Extension{}, err
	}
	return pkix.Extension{
		Id:    asn1.ObjectIdentifier{2, 5, 29, 37},
		Value: val,
	}, nil
}

// sameSubjectName returns a pkix.Name so that two independently-built
// certificates share identical RawSubject DER bytes.
func sameSubjectName() pkix.Name {
	return pkix.Name{
		CommonName:   "Wendy Root CA",
		Organization: []string{"Wendy Labs Inc"},
		Country:      []string{"US"},
	}
}

// TestVerifyMLDSAClientCert_MultipleCAsSameSubject_SecondCASucceeds verifies
// that when two trusted CAs share the same subject DN, the verifier does not
// stop at the first failing CA but continues and succeeds against the second.
//
// Setup:
//   - CA1: valid CA, same DN as CA2, but KeyUsage has no CertSign => rejected
//   - CA2: valid CA, same DN as CA1, correctly signed the leaf => should succeed
//   - Leaf: ML-DSA certificate signed by CA2
//
// Expected: verifyMLDSAClientCert returns nil (no error).
func TestVerifyMLDSAClientCert_MultipleCAsSameSubject_SecondCASucceeds(t *testing.T) {
	subject := sameSubjectName()

	// CA1 is missing CertSign — verifyMLDSAClientCert will reject it with
	// "not permitted to sign certificates" and continue to CA2.
	ca1, _ := buildMLDSACACert(t, subject, false /* withCertSign=false */)

	// CA2 has full KeyUsage and is the actual signer of the leaf.
	ca2, ca2Priv := buildMLDSACACert(t, subject, true)

	// Sanity check: both CAs share the same RawSubject bytes.
	if !bytes.Equal(ca1.RawSubject, ca2.RawSubject) {
		t.Fatalf("CA1 and CA2 RawSubject differ — test setup is broken\nCA1: %x\nCA2: %x",
			ca1.RawSubject, ca2.RawSubject)
	}

	// Leaf is signed by CA2.
	leaf := buildMLDSALeafCert(t, ca2, ca2Priv)

	// Sanity check: leaf's RawIssuer matches the CA subject.
	if !bytes.Equal(leaf.RawIssuer, ca2.RawSubject) {
		t.Fatalf("leaf RawIssuer does not match CA2 RawSubject — test setup is broken")
	}

	// Trusted pool has CA1 first, CA2 second.
	trustedCAs := []*x509.Certificate{ca1, ca2}

	err := verifyMLDSAClientCert(leaf, trustedCAs)
	if err != nil {
		t.Errorf("verifyMLDSAClientCert() = %v; want nil (second CA should succeed)", err)
	}
}

// TestVerifyMLDSAClientCert_MultipleCAsSameSubject_AllFail verifies that when
// all CAs with the matching subject DN fail, the error returned is from the
// last attempted CA — not the generic "issuer not found" message and not the
// error from the first CA.
//
// Setup:
//   - CA1: same DN as CA2, but expired (NotAfter in the past) => "not valid at current time"
//   - CA2: same DN as CA1, valid time but missing CertSign KeyUsage => "not permitted to sign"
//   - Leaf: ML-DSA certificate whose RawIssuer matches both CA subjects
//
// Expected: error is non-nil, mentions CA2's specific failure ("not permitted
// to sign"), and does NOT mention CA1's failure ("not valid at current time").
// This proves the loop continued past CA1 and returned the last (CA2) error.
func TestVerifyMLDSAClientCert_MultipleCAsSameSubject_AllFail(t *testing.T) {
	subject := sameSubjectName()

	// CA1 is expired — verifyMLDSAClientCert rejects it with "not valid at current time".
	ca1, _ := buildMLDSACACertExpired(t, subject)

	// CA2 has valid time but is missing CertSign — rejected with "not permitted to sign".
	ca2, ca2Priv := buildMLDSACACert(t, subject, false /* withCertSign=false */)

	// Sanity check: same RawSubject.
	if !bytes.Equal(ca1.RawSubject, ca2.RawSubject) {
		t.Fatalf("CA1 and CA2 RawSubject differ — test setup is broken")
	}

	// Leaf is signed by CA2 (the signer doesn't matter here; what matters is
	// that leaf.RawIssuer matches both CA subjects so both are tried).
	leaf := buildMLDSALeafCert(t, ca2, ca2Priv)

	trustedCAs := []*x509.Certificate{ca1, ca2}

	err := verifyMLDSAClientCert(leaf, trustedCAs)
	if err == nil {
		t.Fatal("verifyMLDSAClientCert() = nil; want an error when all CAs fail")
	}

	errMsg := err.Error()

	// The returned error must be from CA2 (the last attempted CA): "not permitted to sign".
	if !strings.Contains(errMsg, "not permitted to sign") {
		t.Errorf("error %q does not contain %q; want the CA2-specific failure", errMsg, "not permitted to sign")
	}

	// If the loop bailed out at CA1, we'd see CA1's error instead — guard against that.
	if strings.Contains(errMsg, "not valid at current time") {
		t.Errorf("error %q contains %q; the loop must not have stopped at CA1", errMsg, "not valid at current time")
	}

	// The error must not be the generic "issuer not found" fallback.
	if strings.Contains(errMsg, "issuer not found") {
		t.Errorf("error %q contains %q; want a CA-specific error from the last failing CA", errMsg, "issuer not found")
	}
}

// TestVerifyMLDSAClientCert_IssuerNotFound verifies that when no CA in the
// trusted pool has a matching subject DN, the "issuer not found" error is returned.
func TestVerifyMLDSAClientCert_IssuerNotFound(t *testing.T) {
	subject := sameSubjectName()
	differentSubject := pkix.Name{
		CommonName:   "Different CA",
		Organization: []string{"Other Org"},
	}

	// Build a real CA with 'subject' and a leaf signed by it.
	realCA, realCAPriv := buildMLDSACACert(t, subject, true)
	leaf := buildMLDSALeafCert(t, realCA, realCAPriv)

	// Trusted pool only contains a CA with a different subject.
	fakeCA, _ := buildMLDSACACert(t, differentSubject, true)
	trustedCAs := []*x509.Certificate{fakeCA}

	err := verifyMLDSAClientCert(leaf, trustedCAs)
	if err == nil {
		t.Fatal("verifyMLDSAClientCert() = nil; want an error when issuer is not found")
	}
	if !strings.Contains(err.Error(), "issuer not found") {
		t.Errorf("error %q does not contain %q", err.Error(), "issuer not found")
	}
}
