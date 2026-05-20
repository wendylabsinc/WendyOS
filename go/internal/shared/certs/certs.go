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
// PEM-encoded private key and common name. The CSR is returned as a PEM string.
func GenerateCSR(privateKeyPEM string, commonName string) (csrPEM string, err error) {
	key, err := ParseECPrivateKey(privateKeyPEM)
	if err != nil {
		return "", err
	}

	template := &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName: commonName,
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

// ExtractPublicKey extracts the public key from a PEM-encoded EC private key
// and returns it as a PEM-encoded PKIX public key string.
func ExtractPublicKey(privateKeyPEM string) (publicKeyPEM string, err error) {
	key, err := ParseECPrivateKey(privateKeyPEM)
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

// ParseECPrivateKey decodes a PEM-encoded EC private key.
func ParseECPrivateKey(pemData string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block")
	}

	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing EC private key: %w", err)
	}

	return key, nil
}
