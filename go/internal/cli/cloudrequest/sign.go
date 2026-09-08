// Package cloudrequest signs privileged Wendy Cloud RPCs with the operator
// certificate obtained from pki-core.
package cloudrequest

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const (
	metadataKey     = "x-wendy-request-signature"
	brokerAudience  = "https://cloud.wendy.sh/broker"
	emptyBodyDigest = "47DEQpj8HBSa-_TImW-5JCeuQeRkm5NMpJWZG3hSuFU"
	signatureTTL    = 30 * time.Second
)

// Signer creates the JWS request descriptors required by Wendy Cloud for
// operator-privileged mutations. A Signer is safe for concurrent RPCs: it
// holds immutable key material and obtains fresh randomness for each request.
type Signer struct {
	privateKey *ecdsa.PrivateKey
	tenantUUID string
	x5c        []string
	audience   string
	now        func() time.Time
	random     io.Reader
}

// DialOption returns a unary interceptor option for an OIDC session that has
// a pki-core operator certificate. Legacy/token-only sessions return nil so
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
	if auth == nil || auth.OAuthIssuer == "" || len(auth.Certificates) == 0 {
		return nil, nil
	}
	certInfo := auth.Certificates[0]
	tenantUUID, err := operatorTenant(certInfo.PrincipalURI)
	if err != nil {
		return nil, fmt.Errorf("loading Cloud request-signing identity: %w", err)
	}
	privateKeyPEM, err := certInfo.PrivateKeyPEM()
	if err != nil {
		return nil, fmt.Errorf("loading Cloud request-signing key: %w", err)
	}
	pair, err := tls.X509KeyPair(
		[]byte(certInfo.PemCertificate+"\n"+certInfo.PemCertificateChain),
		[]byte(privateKeyPEM),
	)
	if err != nil {
		return nil, fmt.Errorf("loading Cloud request-signing certificate: %w", err)
	}
	privateKey, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok || privateKey.Curve.Params().Name != "P-256" {
		return nil, fmt.Errorf("Cloud request signing requires an ECDSA P-256 operator key")
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
		resource, required, err := s.signedResource(method, req)
		if err != nil {
			return err
		}
		if required {
			envelope, err := s.sign(strings.TrimPrefix(method, "/"), resource)
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
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func (s *Signer) signedResource(method string, req any) (string, bool, error) {
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
	case "wendycloud.v2.DeviceEnrollmentService/EnrollDevice":
		in, ok := req.(*cloudpbv2.EnrollDeviceRequest)
		if !ok {
			return "", true, requestTypeError(method, req)
		}
		// Cloud compares this byte for byte against
		// "org/<tenant>/device/<device_id>" with the tenant lower-cased and the
		// device id exactly as it sits in the protobuf field — separators and
		// all, so a multi-segment id keeps its slashes.
		return fmt.Sprintf("org/%s/device/%s", strings.ToLower(s.tenantUUID), in.GetDeviceId()), true, nil
	default:
		return "", false, nil
	}
}

func requestTypeError(method string, req any) error {
	return fmt.Errorf("cannot sign Cloud request %s with message type %T", method, req)
}

func (s *Signer) sign(operation, resource string) (string, error) {
	nonceBytes := make([]byte, 32)
	if _, err := io.ReadFull(s.random, nonceBytes); err != nil {
		return "", fmt.Errorf("generating request nonce: %w", err)
	}
	now := s.now().Unix()
	descriptor := map[string]any{
		"aud":         s.audience,
		"body_sha256": emptyBodyDigest,
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
	header, err := canonicalJSON(map[string]any{"alg": "ES256", "x5c": s.x5c})
	if err != nil {
		return "", fmt.Errorf("encoding JWS header: %w", err)
	}
	protected := base64.RawURLEncoding.EncodeToString(header)
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := protected + "." + encodedPayload
	digest := sha256.Sum256([]byte(signingInput))
	r, ss, err := ecdsa.Sign(s.random, s.privateKey, digest[:])
	if err != nil {
		return "", fmt.Errorf("signing descriptor: %w", err)
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	ss.FillBytes(signature[32:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
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

// enrollmentRequestTTL is the lifetime stamped on an enrollment request. It is
// the operator's window to get the request relayed, not the credential's own
// lifetime. pki-core refuses a window wider than 24h and treats exp as a hard
// deadline with no skew, so this stays short.
const enrollmentRequestTTL = 10 * time.Minute

// NewEnrollmentSigner returns the signer for an operator-signed enrollment
// request. Unlike the request-signature interceptor, which stays silent for
// sessions that cannot sign, enrolling a device REQUIRES an operator identity:
// the operator's signature is the whole authority pki-core verifies, so a
// session that cannot produce one has to say so rather than send something
// cloud will refuse.
func NewEnrollmentSigner(auth *config.AuthConfig) (*Signer, error) {
	signer, err := newSigner(auth)
	if err != nil {
		return nil, err
	}
	if signer == nil {
		return nil, fmt.Errorf("this session has no pki-core operator certificate; run 'wendy auth login' with an OIDC issuer")
	}
	return signer, nil
}

// Tenant is the operator's tenant UUID, read from its certificate's SPIFFE
// principal.
func (s *Signer) Tenant() string { return s.tenantUUID }

// SignEnrollmentRequest builds the compact JWS that authorizes enrolling one
// device, as pki-core's fabric relay expects it under the artifact kind
// "enrollment-request+jws".
//
// This is deliberately a SECOND signature, not a reuse of the request
// signature the interceptor attaches. Same operator, same key, two scopes: the
// request signature authorizes the RPC, this one authorizes the enrollment
// itself, and collapsing them would either widen the request signature into an
// enrollment authority or narrow the enrollment artifact into a transport
// detail.
//
// The returned bytes are the artifact. They must reach pki-core BYTE-IDENTICAL
// — every hop relays them unchanged, because any re-serialization invalidates
// the signature.
func (s *Signer) SignEnrollmentRequest(deviceID string, class DeviceClass) ([]byte, error) {
	if deviceID == "" {
		return nil, fmt.Errorf("enrollment request needs a device id")
	}
	letter, err := class.letter()
	if err != nil {
		return nil, err
	}
	now := s.now().Unix()
	// csr_key_binding and attestation_ref are deliberately absent: pki-core
	// refuses an enrollment request carrying either, rather than minting a
	// credential it cannot yet bind.
	payload, err := canonicalJSON(map[string]any{
		"device_class": letter,
		"device_id":    deviceID,
		"exp":          now + int64(enrollmentRequestTTL/time.Second),
		"iat":          now,
		// Single-use at the far end: pki-core burns the jti before it mints,
		// so a failed mint cannot be retried with this request.
		"jti": uuid.NewString(),
		// Must string-equal the tenant cloud puts on the relay envelope.
		"tenant": s.tenantUUID,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding enrollment request: %w", err)
	}
	// x5c entries are base64-STANDARD per RFC 7515 4.1.6, leaf first — which
	// is what the request-signature header already carries.
	header, err := canonicalJSON(map[string]any{"alg": "ES256", "x5c": s.x5c})
	if err != nil {
		return nil, fmt.Errorf("encoding enrollment JWS header: %w", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signingInput))
	r, ss, err := ecdsa.Sign(s.random, s.privateKey, digest[:])
	if err != nil {
		return nil, fmt.Errorf("signing enrollment request: %w", err)
	}
	// Raw fixed-width r||s, not DER: JWS ECDSA signature encoding.
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	ss.FillBytes(signature[32:])
	return []byte(signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)), nil
}

// DeviceClass is the device tier pki-core maps to a credential kind. Cloud
// sends the class and pki-core derives the profile; nothing here picks one.
type DeviceClass int

const (
	// DeviceClassA demands hardware attestation at redemption.
	DeviceClassA DeviceClass = 1
	// DeviceClassB is an EAB with no attestation challenge.
	DeviceClassB DeviceClass = 2
	// DeviceClassC is an EST enrollment token for hardware that cannot run an
	// ACME client.
	DeviceClassC DeviceClass = 3
)

func (c DeviceClass) letter() (string, error) {
	switch c {
	case DeviceClassA:
		return "A", nil
	case DeviceClassB:
		return "B", nil
	case DeviceClassC:
		return "C", nil
	default:
		return "", fmt.Errorf("unknown device class %d", int(c))
	}
}
