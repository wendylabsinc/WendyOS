// Package certs provides certificate and key utilities for mTLS authentication.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"net/url"
)

// GenerateKeyPair generates a new P-256 EC private key and returns it as a PEM-encoded string.
func GenerateKeyPair() (privateKeyPEM string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generating EC key: %w", err)
	}

	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("marshaling EC private key: %w", err)
	}

	block := &pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: der,
	}

	return string(pem.EncodeToMemory(block)), nil
}

// GenerateCSR creates a PKCS#10 certificate signing request using the provided
// PEM-encoded private key (as bytes, so callers can zero the slice after use),
// common name, and identity URIs. The CSR is returned as a PEM string.
//
// identityURIs are placed in the CSR as URI Subject Alternative Names; empty
// strings are skipped, so passing none (or only "") omits the SAN entirely
// (e.g. test fixtures).
//
// Callers pass the tenant SPIFFE principal — AssetSPIFFEURI/UserSPIFFEURI —
// which cloud requires the CSR to carry before it will sign a relay grant
// (WDY-2498) and which is the identity IdentityFromCert resolves first.
//
// A legacy urn:wendy URN (UserURN/AssetURN) still travels alongside it on the
// cloud-relayed path, so a peer running an older agent — one that reads nothing
// but the URN — can still identify the holder. It is inert on anything pki-core
// renews, which strips it; nothing should be built to depend on it.
//
// Only URI SANs are ever emitted. Never add a dNSName here: "service-identity"
// is the one pki-core profile that consults the tenant domain allow-list, and
// any DNS SAN outside it fails the mint with ErrDNSSANNotAuthorized.
//
// The CSR requests digitalSignature key usage and the supplied extended key
// usages so that CAs honoring CSR extensions issue certs the wendy-agent mTLS
// interceptor accepts (it requires an explicit clientAuth EKU). When no EKUs
// are supplied it defaults to clientAuth, keeping user/CLI certs scoped to
// client authentication. Device-provisioning callers (device enroll, os
// install) pass both clientAuth and serverAuth so the device identity can act
// as a TLS client to the cloud and a TLS server for the agent's gRPC and tunnel
// endpoints. The Wendy cloud backends set key usages server-side and ignore
// these, so this only matters for CAs that derive extensions from the CSR.
func GenerateCSR(privateKeyPEM []byte, commonName string, identityURIs []string, extKeyUsages ...x509.ExtKeyUsage) (csrPEM string, err error) {
	key, err := parseECPrivateKey(privateKeyPEM)
	if err != nil {
		return "", err
	}

	if len(extKeyUsages) == 0 {
		extKeyUsages = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	ekuOIDs := make([]asn1.ObjectIdentifier, 0, len(extKeyUsages))
	for _, eku := range extKeyUsages {
		oid, ok := extKeyUsageOID(eku)
		if !ok {
			return "", fmt.Errorf("unsupported extended key usage: %d", eku)
		}
		ekuOIDs = append(ekuOIDs, oid)
	}

	// KeyUsage is a BIT STRING; digitalSignature is bit 0 (RFC 5280 4.2.1.3).
	keyUsageValue, err := asn1.Marshal(asn1.BitString{Bytes: []byte{0x80}, BitLength: 1})
	if err != nil {
		return "", fmt.Errorf("marshaling key usage: %w", err)
	}
	ekuValue, err := asn1.Marshal(ekuOIDs)
	if err != nil {
		return "", fmt.Errorf("marshaling extended key usage: %w", err)
	}

	var uris []*url.URL
	for _, raw := range identityURIs {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("parsing identity URI %q: %w", raw, err)
		}
		uris = append(uris, u)
	}

	template := &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName: commonName,
		},
		// URIs populates the subjectAltName extension (id-ce-subjectAltName,
		// 2.5.29.17), which is distinct from the key-usage extensions below, so
		// there is no extension collision.
		URIs: uris,
		ExtraExtensions: []pkix.Extension{
			{
				Id:       asn1.ObjectIdentifier{2, 5, 29, 15}, // id-ce-keyUsage
				Critical: true,
				Value:    keyUsageValue,
			},
			{
				Id:    asn1.ObjectIdentifier{2, 5, 29, 37}, // id-ce-extKeyUsage
				Value: ekuValue,
			},
		},
	}

	csrDER, err := x509.CreateCertificateRequest(rand.Reader, template, key)
	if err != nil {
		return "", fmt.Errorf("creating CSR: %w", err)
	}

	block := &pem.Block{
		Type:  "CERTIFICATE REQUEST",
		Bytes: csrDER,
	}

	return string(pem.EncodeToMemory(block)), nil
}

// extKeyUsageOID maps the extended key usages this package supports in CSRs to
// their RFC 5280 object identifiers. Only the usages relevant to Wendy mTLS are
// handled; unsupported values report ok=false so callers can fail loudly.
func extKeyUsageOID(eku x509.ExtKeyUsage) (oid asn1.ObjectIdentifier, ok bool) {
	switch eku {
	case x509.ExtKeyUsageServerAuth:
		return asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 1}, true // id-kp-serverAuth
	case x509.ExtKeyUsageClientAuth:
		return asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 2}, true // id-kp-clientAuth
	default:
		return nil, false
	}
}

// ExtractPublicKey extracts the public key from a PEM-encoded EC private key
// and returns it as a PEM-encoded PKIX public key string.
func ExtractPublicKey(privateKeyPEM string) (publicKeyPEM string, err error) {
	key, err := parseECPrivateKey([]byte(privateKeyPEM))
	if err != nil {
		return "", err
	}

	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", fmt.Errorf("marshaling public key: %w", err)
	}

	block := &pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: pubDER,
	}

	return string(pem.EncodeToMemory(block)), nil
}

// LoadTLSConfig builds a tls.Config for mTLS using the provided PEM-encoded
// certificate, certificate chain, private key, and optional CA bundle.
//
// The certPEM and chainPEM are concatenated to form the full client certificate chain.
// If caBundlePEM is non-empty it is used as the root CA pool; otherwise the system
// roots are used.
func LoadTLSConfig(certPEM, chainPEM, keyPEM, caBundlePEM string) (*tls.Config, error) {
	// Build the full certificate chain PEM.
	fullChain := certPEM
	if chainPEM != "" {
		fullChain = certPEM + "\n" + chainPEM
	}

	cert, err := tls.X509KeyPair([]byte(fullChain), []byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("loading X509 key pair: %w", err)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}

	if caBundlePEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(caBundlePEM)) {
			return nil, fmt.Errorf("failed to parse CA bundle PEM")
		}
		tlsCfg.RootCAs = pool
	}

	return tlsCfg, nil
}

// LeafCertificatePEM returns only the first CERTIFICATE block from a PEM bundle.
// Some pki-core certificates include trailing bytes after the outer ASN.1
// certificate SEQUENCE; re-encoding only that first ASN.1 element keeps the
// certificate acceptable to Go TLS clients.
func LeafCertificatePEM(certPEM string) (string, error) {
	rest := []byte(certPEM)
	for len(rest) > 0 {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		var raw asn1.RawValue
		if trailing, err := asn1.Unmarshal(block.Bytes, &raw); err == nil && len(trailing) > 0 {
			block = &pem.Block{
				Type:    block.Type,
				Headers: block.Headers,
				Bytes:   raw.FullBytes,
			}
		}
		return string(pem.EncodeToMemory(block)), nil
	}
	return "", fmt.Errorf("no CERTIFICATE block found")
}

// parseECPrivateKey decodes a PEM-encoded EC private key from a byte slice.
func parseECPrivateKey(pemData []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block")
	}

	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing EC private key: %w", err)
	}

	return key, nil
}
