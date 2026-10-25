package enrollment

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"strconv"
	"strings"
	"time"
)

const (
	IdentityHeader          = "x-wendy-certificate-uri"
	CertificateSerialHeader = "x-wendy-certificate-serial"
	TimestampHeader         = "x-wendy-certificate-timestamp"
	NonceHeader             = "x-wendy-certificate-nonce"
	SignatureHeader         = "x-wendy-certificate-signature"
	domain                  = "wendy-legacy-certificate-proof/v1"
)

// LegacyMetadata creates a fresh method-bound proof with a host-validated P-256
// certificate/key pair. The host's ES256LegacySigner signs SHA-256(message) as
// ASN.1 ECDSA, matching the existing Cloud ingress proof rather than JWS ES256.
func LegacyMetadata(ctx context.Context, identityURI, serial, fullMethod string, entropy platform.Entropy, sign platform.LegacyProofSigner, now time.Time) (platform.Metadata, error) {
	method := strings.TrimPrefix(fullMethod, "/")
	if method == "" || len(method) > 256 {
		return nil, fmt.Errorf("legacy certificate proof method is invalid")
	}
	if entropy == nil || sign == nil {
		return nil, fmt.Errorf("host legacy proof signer and entropy are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	nonceBytes, err := entropy.RandomBytes(ctx, 16)
	if err != nil {
		return nil, fmt.Errorf("generate legacy certificate proof nonce: %w", err)
	}
	if len(nonceBytes) != 16 {
		return nil, fmt.Errorf("host entropy source returned the wrong number of bytes")
	}
	timestamp := strconv.FormatInt(now.Unix(), 10)
	nonce := base64.RawURLEncoding.EncodeToString(nonceBytes)
	canonical, err := canonicalBytes(method, identityURI, serial, timestamp, nonce)
	if err != nil {
		return nil, err
	}
	signature, err := sign.SignLegacyProof(ctx, canonical)
	if err != nil {
		return nil, fmt.Errorf("sign legacy certificate proof: %w", err)
	}
	return platform.Metadata{
		{Name: IdentityHeader, Value: []byte(identityURI)}, {Name: CertificateSerialHeader, Value: []byte(serial)},
		{Name: TimestampHeader, Value: []byte(timestamp)}, {Name: NonceHeader, Value: []byte(nonce)},
		{Name: SignatureHeader, Value: []byte(base64.RawURLEncoding.EncodeToString(signature))},
	}, nil
}
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
