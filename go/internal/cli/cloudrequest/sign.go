// Package cloudrequest signs privileged Wendy Cloud RPCs with the operator
// certificate obtained from pki-core.
package cloudrequest

import (
	"context"
	"crypto"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

const (
	metadataKey    = "x-wendy-request-signature"
	brokerAudience = "https://cloud.wendy.sh/broker"
	signatureTTL   = 30 * time.Second
)

// Signer creates the JWS request descriptors required by Wendy Cloud for
// operator-privileged mutations. A Signer is safe for concurrent RPCs: it
// holds immutable key material and obtains fresh randomness for each request.
type Signer struct {
	privateKey crypto.Signer
	tenantUUID string
	x5c        []string
	audience   string
	now        func() time.Time
	random     io.Reader
}

// DialOption returns a unary interceptor option for a session that has a
// pki-core operator certificate. Legacy/token-only sessions return nil so
// read-only RPCs continue to work; Cloud will reject their privileged writes.
func DialOption(auth *config.AuthConfig) (grpc.DialOption, error) {
	signer, err := newSigner(auth)
	if err != nil {
		return nil, err
	}
	if signer == nil {
		return nil, nil
	}
	return grpc.WithChainUnaryInterceptor(signer.unaryClientInterceptor()), nil
}

func newSigner(auth *config.AuthConfig) (*Signer, error) {
	if auth == nil || len(auth.Certificates) == 0 {
		return nil, nil
	}
	certInfo := auth.Certificates[0]
	// Operator identity is a property of the certificate, not of the login
	// mechanism that happened to obtain it. In particular, imported/migrated
	// operator credentials may not carry the OAuthIssuer bookkeeping field.
	if certInfo.PrincipalURI == "" {
		return nil, nil
	}
	tenantUUID, err := operatorTenant(certInfo.PrincipalURI)
	if err != nil {
		return nil, fmt.Errorf("loading Cloud request-signing identity: %w", err)
	}
	privateKeyPEM, err := certInfo.PrivateKeyPEM()
	if err != nil {
		return nil, fmt.Errorf("loading Cloud request-signing key: %w", err)
	}
	pair, err := certs.TLSKeyPair(certInfo.PemCertificate, certInfo.PemCertificateChain, privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("loading Cloud request-signing certificate: %w", err)
	}
	// WDY-3032 is a hard cutover: the operator credential is ML-DSA-65, with
	// no ECDSA fallback. A session predating it is refused here rather than
	// signed with, so the failure names the fix instead of surfacing later as
	// a rejected signature from Cloud.
	privateKey, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("Cloud request signing key of type %T cannot sign", pair.PrivateKey)
	}
	if _, ok := privateKey.Public().(*mldsa.PublicKey); !ok {
		return nil, fmt.Errorf("Cloud request signing requires an ML-DSA-65 operator key, but this session holds %T; re-run 'wendy auth login'", privateKey.Public())
	}
	x5c := make([]string, 0, len(pair.Certificate))
	for _, der := range pair.Certificate {
		x5c = append(x5c, base64.StdEncoding.EncodeToString(der))
	}
	return &Signer{
		privateKey: privateKey,
		tenantUUID: tenantUUID,
		x5c:        x5c,
		audience:   brokerAudience,
		now:        time.Now,
		random:     rand.Reader,
	}, nil
}

// operatorTenant reads the tenant a session's principal belongs to.
//
// It used to insist on the kind "operator", which is what the AAA contract
// §5.2 says pki-core's own identity endpoint stamps — but cloud relays its
// leaves through the service-identity profile and stamps "service/user-<id>",
// so a cloud-issued session was refused here for spelling. Both are legitimate
// human-operator identities under the contract (D17 makes a service account a
// normal user behind a different front door), so both are accepted and
// certs.ParsePrincipal is the single place that decides so.
//
// A device or code-signing principal is still refused: neither is an actor
// that may sign a privileged cloud mutation.
func operatorTenant(principal string) (string, error) {
	id, err := certs.ParsePrincipal(principal)
	if err != nil {
		return "", fmt.Errorf("operator certificate has invalid principal URI %q: %w", principal, err)
	}
	if id.EntityType != certs.EntityUser {
		return "", fmt.Errorf("operator certificate principal %q is a %s, not an operator", principal, id.EntityType)
	}
	return id.TenantUUID, nil
}

func (s *Signer) unaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		resource, required, err := signedResource(method, req)
		if method == cloudpbv2.DeviceEnrollmentService_EnrollDevice_FullMethodName {
			in, ok := req.(*cloudpbv2.EnrollDeviceRequest)
			if !ok {
				return requestTypeError(method, req)
			}
			resource = "org/" + s.tenantUUID + "/device/" + in.GetDeviceId()
			required = true
		}
		if err != nil {
			return err
		}
		if required {
			// Sign exactly the bytes that go on the wire. Marshal the request
			// once here, hash those bytes for body_sha256, then force a codec
			// that hands the invoker the same bytes verbatim. The broker hashes
			// the request as received (WDY-3007); letting grpc marshal a second
			// time could put different bytes on the wire than we signed.
			msg, ok := req.(proto.Message)
			if !ok {
				return requestTypeError(method, req)
			}
			raw, err := proto.Marshal(msg)
			if err != nil {
				return fmt.Errorf("marshaling Cloud request %s: %w", method, err)
			}
			sum := sha256.Sum256(raw)
			envelope, err := s.sign(strings.TrimPrefix(method, "/"), resource, base64.RawURLEncoding.EncodeToString(sum[:]))
			if err != nil {
				return fmt.Errorf("signing Cloud request %s: %w", method, err)
			}
			md, ok := metadata.FromOutgoingContext(ctx)
			if ok {
				md = md.Copy()
			} else {
				md = metadata.MD{}
			}
			md.Set(metadataKey, envelope)
			ctx = metadata.NewOutgoingContext(ctx, md)
			opts = append(opts, grpc.ForceCodec(wireCodec{raw: raw}))
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func signedResource(method string, req any) (string, bool, error) {
	switch strings.TrimPrefix(method, "/") {
	case "wendycloud.v1.AssetService/UpdateAsset":
		in, ok := req.(*cloudpb.UpdateAssetRequest)
		if !ok {
			return "", true, requestTypeError(method, req)
		}
		return fmt.Sprintf("asset/%d", in.GetId()), true, nil
	case "wendycloud.v1.AssetService/DeleteAsset":
		in, ok := req.(*cloudpb.DeleteAssetRequest)
		if !ok {
			return "", true, requestTypeError(method, req)
		}
		return fmt.Sprintf("asset/%d", in.GetId()), true, nil
	case "wendycloud.v1.CertificateService/RevokeCertificate":
		in, ok := req.(*cloudpb.RevokeCertificateRequest)
		if !ok {
			return "", true, requestTypeError(method, req)
		}
		return fmt.Sprintf("certificate/%d", in.GetCertificateId()), true, nil
	case "wendycloud.v1.CertificateService/CreateAssetEnrollmentToken":
		in, ok := req.(*cloudpb.CreateAssetEnrollmentTokenRequest)
		if !ok {
			return "", true, requestTypeError(method, req)
		}
		return fmt.Sprintf("org/%d/enroll-asset-name/%s", in.GetOrganizationId(), in.GetName()), true, nil
	default:
		return "", false, nil
	}
}

func requestTypeError(method string, req any) error {
	return fmt.Errorf("cannot sign Cloud request %s with message type %T", method, req)
}

func (s *Signer) sign(operation, resource, bodyDigest string) (string, error) {
	nonceBytes := make([]byte, 32)
	if _, err := io.ReadFull(s.random, nonceBytes); err != nil {
		return "", fmt.Errorf("generating request nonce: %w", err)
	}
	now := s.now().Unix()
	descriptor := map[string]any{
		"aud":         s.audience,
		"body_sha256": bodyDigest,
		"expiry":      now + int64(signatureTTL/time.Second),
		"iat":         now,
		"nonce":       base64.RawURLEncoding.EncodeToString(nonceBytes),
		"operation":   operation,
		"target": map[string]any{
			"resource": resource,
			"tenant":   s.tenantUUID,
		},
	}
	payload, err := canonicalJSON(descriptor)
	if err != nil {
		return "", fmt.Errorf("encoding request descriptor: %w", err)
	}
	// Cloud validates only x5c[0] through PKI, which owns the issuer chain.
	// Sending the ML-DSA intermediates here can exceed the broker's 16 KiB
	// HTTP/2 header limit once the JWS and OAuth bearer are combined.
	return s.signPayload(payload, s.x5c[:1])
}

// EnrollmentRequest signs PKI's enrollment authority separately from the Cloud
// RPC descriptor. PKI verifies this JWS against the tenant's Operator Authority.
func EnrollmentRequest(auth *config.AuthConfig, deviceID string) ([]byte, error) {
	return EnrollmentRequestForClass(auth, deviceID, "B")
}

// EnrollmentRequestForClass binds the operator authorization to a device tier.
func EnrollmentRequestForClass(auth *config.AuthConfig, deviceID, deviceClass string) ([]byte, error) {
	if deviceClass != "A" && deviceClass != "B" && deviceClass != "C" {
		return nil, fmt.Errorf("invalid device class %q", deviceClass)
	}
	s, err := newSigner(auth)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, fmt.Errorf("device enrollment requires an operator certificate")
	}
	now := s.now().Unix()
	payload, err := canonicalJSON(map[string]any{
		"tenant": s.tenantUUID, "device_id": deviceID, "device_class": deviceClass,
		"iat": now, "exp": now + 300, "jti": uuid.NewString(),
	})
	if err != nil {
		return nil, err
	}
	// This artifact is carried in the protobuf body, not HTTP metadata. Keep
	// the full chain for PKI's enrollment signature verifier.
	jws, err := s.signPayload(payload, s.x5c)
	return []byte(jws), err
}

func (s *Signer) signPayload(payload []byte, chain []string) (string, error) {
	header, err := canonicalJSON(map[string]any{"alg": "ML-DSA-65", "x5c": chain})
	if err != nil {
		return "", fmt.Errorf("encoding JWS header: %w", err)
	}
	protected := base64.RawURLEncoding.EncodeToString(header)
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := protected + "." + encodedPayload

	// ML-DSA signs the input bytes directly with empty Options — the same shape
	// pki-core verifies with (reqsig: Verify(pk, signingInput, sig, nil)). No
	// pre-hash, and the JWS signature is the raw FIPS-204 value.
	key, ok := s.privateKey.(*mldsa.PrivateKey)
	if !ok {
		return "", fmt.Errorf("request-signing key is %T, want *mldsa.PrivateKey", s.privateKey)
	}
	signature, err := key.Sign(s.random, []byte(signingInput), &mldsa.Options{})
	if err != nil {
		return "", fmt.Errorf("signing descriptor: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// wireCodec forwards the exact request bytes the signer already hashed for
// body_sha256, so grpc puts those bytes — not a fresh, possibly different
// marshalling — on the wire. Replies decode with the standard proto codec.
// Name is "proto" so the content-subtype and the server's codec are unchanged.
type wireCodec struct{ raw []byte }

func (wireCodec) Name() string { return "proto" }

func (c wireCodec) Marshal(any) ([]byte, error) { return c.raw, nil }

func (wireCodec) Unmarshal(data []byte, v any) error {
	msg, ok := v.(proto.Message)
	if !ok {
		return fmt.Errorf("cloud request codec: reply type %T is not a proto message", v)
	}
	return proto.Unmarshal(data, msg)
}

// canonicalJSON is sufficient for the request descriptor's deliberately
// narrow RFC 8785 subset: string-keyed objects, strings, arrays, and integers.
// encoding/json sorts map keys; disabling HTML escaping preserves JCS strings.
func canonicalJSON(value any) ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(b.String(), "\n")), nil
}
