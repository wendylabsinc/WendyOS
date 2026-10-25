// Package legacycertproof adapts legacy gRPC credentials to WendyGo proofs.
package legacycertproof

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/url"
	"time"

	wendy "github.com/wendylabsinc/wendy-go"
	"github.com/wendylabsinc/wendy-go/enrollment"
	"google.golang.org/grpc/metadata"
)

const (
	IdentityHeader          = enrollment.IdentityHeader
	CertificateSerialHeader = enrollment.CertificateSerialHeader
	TimestampHeader         = enrollment.TimestampHeader
	NonceHeader             = enrollment.NonceHeader
	SignatureHeader         = enrollment.SignatureHeader
)

// Signer retains only a host-owned SDK signer and injectable clock/entropy.
type Signer struct {
	host   *wendy.GoLegacyProofSigner
	now    func() time.Time
	random io.Reader
}

func New(identity, certificate, key string) (*Signer, error) {
	host, err := wendy.NewLegacyProofSigner(identity, certificate, key)
	if err != nil {
		return nil, err
	}
	return &Signer{host: host, now: time.Now, random: rand.Reader}, nil
}

type proofEntropy struct{ io.Reader }

func (e proofEntropy) RandomBytes(ctx context.Context, n int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw := make([]byte, n)
	_, err := io.ReadFull(e.Reader, raw)
	return raw, err
}
func (s *Signer) Metadata(method string) (metadata.MD, error) {
	values, err := enrollment.LegacyMetadata(context.Background(), s.host.IdentityURI(), s.host.CertificateSerial(), method, proofEntropy{s.random}, s.host, s.now())
	if err != nil {
		return nil, err
	}
	md := metadata.MD{}
	for _, entry := range values {
		md.Append(entry.Name, string(entry.Value))
	}
	return md, nil
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
