package cloudrelay

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"net"
	"time"

	"github.com/cloudflare/circl/sign"
	"github.com/cloudflare/circl/sign/mldsa/mldsa44"
	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	"github.com/cloudflare/circl/sign/mldsa/mldsa87"
	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	authpb "github.com/wendylabsinc/wendy/go/proto/gen/wendyauthpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// PrincipalSigner builds the cert-bound request required by Cloud/pki-core.
// TLS-compatible EC operator certificates cannot sign this ML-DSA-only profile;
// report that capability gap rather than silently downgrade the request.
func PrincipalSigner(certPEM string, keyPEM []byte) (func([]byte) ([]byte, error), error) {
	leaves, err := certs.ParseCertsFromPEM([]byte(certPEM))
	if err != nil || len(leaves) != 1 {
		return nil, fmt.Errorf("expected one operator signing certificate")
	}
	var spki struct {
		Algorithm pkix.AlgorithmIdentifier
		PublicKey asn1.BitString
	}
	rest, err := asn1.Unmarshal(leaves[0].RawSubjectPublicKeyInfo, &spki)
	if err != nil || len(rest) != 0 {
		return nil, fmt.Errorf("invalid operator public key")
	}
	var scheme sign.Scheme
	switch spki.Algorithm.Algorithm.String() {
	case "2.16.840.1.101.3.4.3.17":
		scheme = mldsa44.Scheme()
	case "2.16.840.1.101.3.4.3.18":
		scheme = mldsa65.Scheme()
	case "2.16.840.1.101.3.4.3.19":
		scheme = mldsa87.Scheme()
	default:
		return nil, fmt.Errorf("Cloud tunnels require an ML-DSA operator request-signing certificate; this login has a TLS-only certificate (Cloud/PKI must issue the tunnel signing credential)")
	}
	if len(spki.Algorithm.Parameters.FullBytes) != 0 {
		return nil, fmt.Errorf("unsupported operator signing-key parameters")
	}
	block, restPEM := pem.Decode(keyPEM)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(restPEM)) != 0 {
		return nil, fmt.Errorf("expected PKCS#8 operator signing key")
	}
	var p8 struct {
		Version    int
		Algorithm  pkix.AlgorithmIdentifier
		PrivateKey []byte
	}
	rest, err = asn1.Unmarshal(block.Bytes, &p8)
	if err != nil || len(rest) != 0 || !p8.Algorithm.Algorithm.Equal(spki.Algorithm.Algorithm) || len(p8.Algorithm.Parameters.FullBytes) != 0 {
		return nil, fmt.Errorf("operator signing key algorithm mismatch")
	}
	// RFC 9881: seed [0] IMPLICIT OCTET STRING, expanded OCTET STRING, or
	// SEQUENCE {seed, expandedKey}. Verify the derived public key against x5c.
	var value asn1.RawValue
	rest, err = asn1.Unmarshal(p8.PrivateKey, &value)
	if err != nil || len(rest) != 0 {
		return nil, fmt.Errorf("invalid ML-DSA private key encoding")
	}
	var sk sign.PrivateKey
	switch {
	case value.Class == 2 && value.Tag == 0 && !value.IsCompound && len(value.Bytes) == scheme.SeedSize():
		_, sk = scheme.DeriveKey(value.Bytes)
	case value.Class == 0 && value.Tag == 4 && !value.IsCompound:
		sk, err = scheme.UnmarshalBinaryPrivateKey(value.Bytes)
	case value.Class == 0 && value.Tag == 16 && value.IsCompound:
		var both struct{ Seed, Expanded []byte }
		_, err = asn1.Unmarshal(p8.PrivateKey, &both)
		if err == nil && len(both.Seed) == scheme.SeedSize() {
			_, sk = scheme.DeriveKey(both.Seed)
			expanded, _ := sk.MarshalBinary()
			if !bytes.Equal(expanded, both.Expanded) {
				err = fmt.Errorf("ML-DSA seed/expanded key mismatch")
			}
		}
	}
	if err != nil || sk == nil {
		return nil, fmt.Errorf("invalid ML-DSA private key")
	}
	pk, err := scheme.UnmarshalBinaryPublicKey(spki.PublicKey.Bytes)
	if err != nil {
		return nil, err
	}
	alg := scheme.Name()
	header, err := canonicalJSON(map[string]any{"alg": alg, "typ": "tunnel-principal-request+jws", "x5c": [][]byte{leaves[0].Raw}})
	if err != nil {
		return nil, err
	}
	return func(payload []byte) ([]byte, error) {
		input := []byte(b64.EncodeToString(header) + "." + b64.EncodeToString(payload))
		sig := scheme.Sign(sk, input, &sign.SignatureOpts{Context: ""})
		if !scheme.Verify(pk, input, sig, &sign.SignatureOpts{Context: ""}) {
			return nil, fmt.Errorf("operator signing key does not match certificate")
		}
		return []byte(string(input) + "." + b64.EncodeToString(sig)), nil
	}, nil
}

// LitePrincipalSigner uses the explicit P-256 profile for constrained devices.
// Other tunnel profiles retain their existing signing policy.
func LitePrincipalSigner(certPEM string, keyPEM []byte) (func([]byte) ([]byte, error), error) {
	leaves, err := certs.ParseCertsFromPEM([]byte(certPEM))
	if err != nil || len(leaves) != 1 {
		return nil, fmt.Errorf("expected one Lite operator signing certificate")
	}
	pub, err := x509.ParsePKIXPublicKey(leaves[0].RawSubjectPublicKeyInfo)
	if err != nil {
		return nil, err
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, fmt.Errorf("Lite requires a P-256 operator certificate")
	}
	block, rest := pem.Decode(keyPEM)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("invalid Lite signing key PEM")
	}
	var key *ecdsa.PrivateKey
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		key, _ = parsed.(*ecdsa.PrivateKey)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("unsupported Lite signing key encoding")
	}
	if err != nil || key == nil || key.Curve != elliptic.P256() || !key.PublicKey.Equal(ec) {
		return nil, fmt.Errorf("Lite signing key does not match its P-256 certificate")
	}
	header, err := canonicalJSON(map[string]any{"alg": "ES256", "typ": "tunnel-principal-request+jws", "x5c": [][]byte{leaves[0].Raw}})
	if err != nil {
		return nil, err
	}
	return func(payload []byte) ([]byte, error) {
		input := []byte(b64.EncodeToString(header) + "." + b64.EncodeToString(payload))
		digest := sha256.Sum256(input)
		r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
		if err != nil {
			return nil, err
		}
		signature := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
		return []byte(string(input) + "." + b64.EncodeToString(signature)), nil
	}, nil
}

// OpenTCP asks Cloud to authorize a symbolic service, then joins the selected
// broker with an ephemeral caller key. authCtx's identity metadata is used only
// on Cloud's authorization RPC, and is never forwarded to the relay.
func OpenTCP(ctx, authCtx context.Context, cloudConn *grpc.ClientConn, verifier *Verifier, assetID, service string, signRequest func([]byte) ([]byte, error)) (net.Conn, error) {
	s, err := openPortSession(ctx, authCtx, cloudConn, verifier, assetID, service, pb.DialTransport_DIAL_TRANSPORT_TCP, signRequest)
	if err != nil {
		return nil, err
	}
	return s.Conn(), nil
}

func openPortSession(ctx, authCtx context.Context, cloudConn *grpc.ClientConn, verifier *Verifier, assetID, service string, transport pb.DialTransport, signRequest func([]byte) ([]byte, error)) (*Session, error) {
	if _, err := uuid.Parse(assetID); err != nil {
		return nil, fmt.Errorf("invalid Cloud asset UUID")
	}
	if actual, _, err := serviceTarget(service); err != nil || actual != transport {
		return nil, fmt.Errorf("Cloud has no supported tunnel service %q", service)
	}
	request, key, descriptor, err := PrincipalRequest(assetID, service, signRequest)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(authCtx, 30*time.Second)
	defer cancel()
	response, err := pb.NewTunnelAuthorizationServiceClient(cloudConn).RequestTunnel(requestCtx, request)
	if err != nil {
		// A Cloud before WDY-3955 (cloud #895) knows only ssh and wendy-agent.
		if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound && st.Message() == "unknown service" {
			return nil, fmt.Errorf("Cloud does not offer tunnel service %q; port tunnels need a Cloud release with WDY-3955: %w", service, err)
		}
		return nil, fmt.Errorf("authorizing Cloud tunnel (%s): %w", pb.TunnelAuthorizationService_RequestTunnel_FullMethodName, err)
	}
	if response.Broker == nil {
		return nil, fmt.Errorf("missing Cloud tunnel broker")
	}
	destinations := response.AuthorizedDatagramDestinations
	if transport == pb.DialTransport_DIAL_TRANSPORT_TCP {
		if len(destinations) != 0 {
			return nil, fmt.Errorf("invalid Cloud TCP authorization response")
		}
	} else if len(destinations) != 1 || destinations[0].GetDestinationId() != 1 {
		return nil, fmt.Errorf("invalid Cloud UDP authorization response")
	}
	c, err := verifier.verify(ctx, response.SessionGrantJws, grantType, response.Broker.Audience, time.Now())
	if err != nil {
		return nil, err
	}
	if c.Request != binding(descriptor) || c.Caller != binding(publicDER(key)) {
		return nil, fmt.Errorf("Cloud tunnel grant request/key mismatch")
	}
	s, err := join(ctx, response.Broker, response.SessionGrantJws, c, key, pb.JoinRole_JOIN_ROLE_CALLER, verifier.dialRelay)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// PrincipalRequest binds a tunnel-v2 request to a fresh caller key.
func PrincipalRequest(assetID, service string, signRequest func([]byte) ([]byte, error)) (*pb.RequestTunnelRequest, *ecdsa.PrivateKey, []byte, error) {
	if signRequest == nil {
		return nil, nil, nil, fmt.Errorf("operator request signer is required")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	request, descriptor, err := principalRequest(assetID, service, "create_tunnel", publicDER(key), signRequest)
	if err != nil {
		return nil, nil, nil, err
	}
	return request, key, descriptor, nil
}

// LitePrincipalRequest binds the Lite grant to the operator key presented in
// inner mTLS. Lite uses its own operation and never an ephemeral caller key.
func LitePrincipalRequest(assetID string, identity config.CertificateInfo, signRequest func([]byte) ([]byte, error)) (*pb.RequestTunnelRequest, error) {
	if signRequest == nil {
		return nil, fmt.Errorf("operator request signer is required")
	}
	if _, err := uuid.Parse(assetID); err != nil {
		return nil, fmt.Errorf("invalid Cloud asset UUID")
	}
	leaves, err := certs.ParseCertsFromPEM([]byte(identity.PemCertificate))
	if err != nil || len(leaves) != 1 {
		return nil, fmt.Errorf("expected one Lite operator signing certificate")
	}
	key, ok := leaves[0].PublicKey.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("Lite requires a P-256 operator certificate")
	}
	request, _, err := principalRequest(assetID, "wendycom", "create_lite_tunnel", leaves[0].RawSubjectPublicKeyInfo, signRequest)
	return request, err
}

func principalRequest(assetID, service, operation string, callerSPKI []byte, signRequest func([]byte) ([]byte, error)) (*pb.RequestTunnelRequest, []byte, error) {
	requestID := make([]byte, 16)
	if _, err := rand.Read(requestID); err != nil {
		return nil, nil, err
	}
	body := &pb.TunnelPrincipalRequestBody{TargetAssetId: assetID, Service: service, CallerSigningPublicKeySpkiDer: callerSPKI, RequestId: requestID}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().Unix()
	descriptor, err := canonicalJSON(map[string]any{"aud": "pki-core/tunnel-attestation", "body_sha256": binding(raw), "expiry": now + 60, "iat": now, "nonce": uuid.NewString(), "operation": operation, "target": map[string]any{"asset_id": assetID, "service": service}})
	if err != nil {
		return nil, nil, err
	}
	signed, err := signRequest(descriptor)
	if err != nil {
		return nil, nil, err
	}
	return &pb.RequestTunnelRequest{RequestBody: body, PrincipalRequest: &authpb.SignedArtifact{Kind: "tunnel-principal-request+jws", Value: signed}}, descriptor, nil
}
