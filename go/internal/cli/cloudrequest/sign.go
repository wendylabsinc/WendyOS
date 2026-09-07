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
		resource, required, err := signedResource(method, req)
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
