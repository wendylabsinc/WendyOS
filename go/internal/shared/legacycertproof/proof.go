// Package legacycertproof signs replay-protected gRPC metadata for the legacy
// Wendy Cloud API, whose Cloud Run ingress cannot forward a TLS client certificate.
package legacycertproof

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/metadata"
)

const (
	IdentityHeader          = "x-wendy-certificate-uri"
	CertificateSerialHeader = "x-wendy-certificate-serial"
	TimestampHeader         = "x-wendy-certificate-timestamp"
	NonceHeader             = "x-wendy-certificate-nonce"
	SignatureHeader         = "x-wendy-certificate-signature"
	domain                  = "wendy-legacy-certificate-proof/v1"
)

// Signer proves possession of one enrolled ECDSA P-256 certificate key.
type Signer struct {
	identityURI       string
	certificateSerial string
	privateKey        *ecdsa.PrivateKey
	now               func() time.Time
	random            io.Reader
}

// New validates that the certificate and private key are the same P-256 pair.
func New(identityURI, certificatePEM, privateKeyPEM string) (*Signer, error) {
	certificate, err := parseCertificate(certificatePEM)
	if err != nil {
		return nil, err
	}
	privateKey, err := parsePrivateKey(privateKeyPEM)
	if err != nil {
		return nil, err
	}
	publicKey, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve != elliptic.P256() ||
		publicKey.X.Cmp(privateKey.PublicKey.X) != 0 || publicKey.Y.Cmp(privateKey.PublicKey.Y) != 0 {
		return nil, fmt.Errorf("legacy certificate proof key does not match the P-256 certificate")
	}
	if certificate.SerialNumber == nil || certificate.SerialNumber.Sign() <= 0 {
		return nil, fmt.Errorf("legacy certificate proof certificate serial must be positive")
	}
	serial := hex.EncodeToString(certificate.SerialNumber.Bytes())
	if len(serial) < 2 || len(serial) > 40 || len(serial)%2 != 0 || strings.HasPrefix(serial, "00") {
		return nil, fmt.Errorf("legacy certificate proof certificate serial is not canonical")
	}
	if identityURI == "" {
		return nil, fmt.Errorf("legacy certificate proof identity URI is empty")
	}
	return &Signer{
		identityURI:       identityURI,
		certificateSerial: serial,
		privateKey:        privateKey,
		now:               time.Now,
		random:            rand.Reader,
	}, nil
}

// Metadata creates a fresh proof bound to one fully-qualified gRPC method.
func (s *Signer) Metadata(fullMethod string) (metadata.MD, error) {
	method := strings.TrimPrefix(fullMethod, "/")
	if method == "" || len(method) > 256 {
		return nil, fmt.Errorf("legacy certificate proof method is invalid")
	}
	nonceBytes := make([]byte, 16)
	if _, err := io.ReadFull(s.random, nonceBytes); err != nil {
		return nil, fmt.Errorf("generate legacy certificate proof nonce: %w", err)
	}
	timestamp := strconv.FormatInt(s.now().Unix(), 10)
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	canonical, err := canonicalBytes(
		method,
		s.identityURI,
		s.certificateSerial,
		timestamp,
		nonce,
	)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(canonical)
	signature, err := ecdsa.SignASN1(s.random, s.privateKey, digest[:])
	if err != nil {
		return nil, fmt.Errorf("sign legacy certificate proof: %w", err)
	}
	return metadata.Pairs(
		IdentityHeader, s.identityURI,
		CertificateSerialHeader, s.certificateSerial,
		TimestampHeader, timestamp,
		NonceHeader, nonce,
		SignatureHeader, base64.RawURLEncoding.EncodeToString(signature),
	), nil
}

// GetRequestMetadata lets a Signer be installed as gRPC PerRPCCredentials.
func (s *Signer) GetRequestMetadata(_ context.Context, requestURI ...string) (map[string]string, error) {
	if len(requestURI) == 0 {
		return nil, fmt.Errorf("legacy certificate proof request URI is missing")
	}
	method := requestURI[0]
	if parsed, err := url.Parse(method); err == nil && parsed.Path != "" {
		method = parsed.Path
	}
	md, err := s.Metadata(method)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(md))
	for key, values := range md {
		if len(values) == 1 {
			result[key] = values[0]
		}
	}
	return result, nil
}

// RequireTransportSecurity prevents proofs from being sent over plaintext.
func (s *Signer) RequireTransportSecurity() bool { return true }

func canonicalBytes(components ...string) ([]byte, error) {
	result := make([]byte, 0, 256)
	for _, component := range append([]string{domain}, components...) {
		if uint64(len(component)) > uint64(^uint32(0)) {
			return nil, fmt.Errorf("legacy certificate proof component is too large")
		}
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(component)))
		result = append(result, length[:]...)
		result = append(result, component...)
	}
	return result, nil
}

func parseCertificate(value string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(value))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("decode legacy certificate proof leaf certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse legacy certificate proof leaf certificate: %w", err)
	}
	return certificate, nil
}

func parsePrivateKey(value string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(value))
	if block == nil {
		return nil, fmt.Errorf("decode legacy certificate proof private key")
	}
	var key *ecdsa.PrivateKey
	switch block.Type {
	case "EC PRIVATE KEY":
		parsed, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse legacy certificate proof EC private key: %w", err)
		}
		key = parsed
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse legacy certificate proof PKCS#8 private key: %w", err)
		}
		var ok bool
		key, ok = parsed.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("legacy certificate proof private key is not ECDSA")
		}
	default:
		return nil, fmt.Errorf("unsupported legacy certificate proof private key PEM type %q", block.Type)
	}
	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("legacy certificate proof private key must use ECDSA P-256")
	}
	return key, nil
}
