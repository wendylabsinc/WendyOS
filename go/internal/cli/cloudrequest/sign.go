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
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
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
	kid        string // base64url(SHA-256(leaf DER)): the RFC 7515 x5t#S256 value
	audience   string
	now        func() time.Time
	random     io.Reader
}

// Invoke calls an operator-signed method with req wrapped in the
// wendycloud.v2.SignedRequest envelope it takes (WDY-3458): payload is req
// serialized once, payload_type its message name, and signature a JWS whose
// body_sha256 binds exactly those payload bytes. Nothing is added to the call's
// metadata.
//
// The JWS names the operator leaf by kid (WDY-3463) once the leaf is
// registered, which happens once per process with RegisterOperatorLeaf, signed
// with x5c. A Cloud without that RPC (Unimplemented) gets x5c instead, since
// registration is optional by contract. Cloud refuses an unknown, revoked or
// expired kid with a generic PERMISSION_DENIED, and a leaf pki-core holds no
// DER for with FAILED_PRECONDITION "leaf not stored"; either is retried exactly
// once signed with x5c under the same correlation_id, and the registration is
// dropped so the next call registers again. The refusal cannot tell a stale
// kid from a missing permission, so a second refusal is final.
func Invoke(ctx context.Context, conn grpc.ClientConnInterface, auth *config.AuthConfig, method string, req, reply proto.Message) error {
	s, err := newSigner(auth)
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("%s requires an operator certificate; run 'wendy auth login'", method)
	}
	// AAA §11.2: adopt an existing flow id unchanged. Registration and retries
	// remain messages in that flow, not new origins. Match Cloud's existing
	// CorrelationContext alphabet/64-byte bound; reject unsafe supplied values
	// rather than sending a header that disagrees with the signed descriptor.
	correlationID := ""
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		for _, value := range md.Get("x-correlation-id") {
			if value != "" {
				correlationID = value
				break
			}
		}
	}
	if correlationID != "" {
		if len(correlationID) > 64 || strings.IndexFunc(correlationID, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-')
		}) >= 0 {
			return fmt.Errorf("invalid supplied Cloud correlation id")
		}
	} else {
		correlationID = uuid.NewString()
	}
	call := func(byKID bool) error {
		signed, err := s.signRequest(method, req, byKID, correlationID)
		if err != nil {
			return err
		}
		return conn.Invoke(ctx, method, signed, reply)
	}
	if x5cOnly[method] {
		return call(false)
	}
	registered, err := s.register(ctx, conn, correlationID)
	if err != nil {
		return err
	}
	if !registered {
		return call(false)
	}
	err = call(true)
	if !kidRefused(err) {
		return err
	}
	registeredKIDs.Delete(s.kid)
	if retryErr := call(false); retryErr != nil {
		return fmt.Errorf("Cloud refused %s by key id and again with the operator certificate attached: %w", method, retryErr)
	}
	return nil
}

// x5cOnly lists the methods always signed with x5c, never kid: the leaf
// registration itself, and the two whose JWS Cloud forwards verbatim to
// devices (PostboxEntry.request_jws), which cannot resolve a kid.
var x5cOnly = map[string]bool{
	cloudpbv2.OperatorSessionService_RegisterOperatorLeaf_FullMethodName: true,
	cloudpbv2.DeploymentService_CreateDeployment_FullMethodName:          true,
	cloudpbv2.DeploymentService_ControlContainer_FullMethodName:          true,
}

// registeredKIDs holds the leaves this process has registered.
// ponytail: per process, not persisted; a CLI run registers once per signed
// command, which is one call today. Persist in the auth session if that grows.
var registeredKIDs sync.Map

// kidRefused reports the two refusals a kid-signed call may get for its key
// reference: a generic PERMISSION_DENIED, or the explicit "leaf not stored".
// Any other FAILED_PRECONDITION comes from the handler and is not retried.
func kidRefused(err error) bool {
	st, _ := status.FromError(err)
	switch st.Code() {
	case codes.PermissionDenied:
		return true
	case codes.FailedPrecondition:
		return strings.Contains(st.Message(), "leaf not stored")
	}
	return false
}

// register reports whether the leaf is registered and may be named by kid. A
// Cloud that does not serve RegisterOperatorLeaf yields false, not an error.
func (s *Signer) register(ctx context.Context, conn grpc.ClientConnInterface, correlationID string) (bool, error) {
	if _, ok := registeredKIDs.Load(s.kid); ok {
		return true, nil
	}
	method := cloudpbv2.OperatorSessionService_RegisterOperatorLeaf_FullMethodName
	signed, err := s.signRequest(method, &cloudpbv2.RegisterOperatorLeafRequest{OrganizationId: s.tenantUUID}, false, correlationID)
	if err != nil {
		return false, err
	}
	var resp cloudpbv2.RegisterOperatorLeafResponse
	if err := conn.Invoke(ctx, method, signed, &resp); err != nil {
		if status.Code(err) == codes.Unimplemented {
			return false, nil
		}
		return false, fmt.Errorf("registering the operator certificate with Cloud: %w", err)
	}
	if resp.GetKid() != s.kid {
		return false, fmt.Errorf("Cloud registered operator certificate %q, not this session's %q", resp.GetKid(), s.kid)
	}
	registeredKIDs.Store(s.kid, struct{}{})
	return true, nil
}

func leafKID(der []byte) string {
	sum := sha256.Sum256(der)
	return base64.RawURLEncoding.EncodeToString(sum[:])
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
		kid:        leafKID(pair.Certificate[0]),
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

func (s *Signer) signRequest(method string, req proto.Message, byKID bool, correlationID string) (*cloudpbv2.SignedRequest, error) {
	resource, ok := signedResources[method]
	if !ok {
		return nil, requestTypeError(method, req)
	}
	target, ok := resource(s.tenantUUID, req)
	if !ok {
		return nil, requestTypeError(method, req)
	}
	payload, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling Cloud request %s: %w", method, err)
	}
	sum := sha256.Sum256(payload)
	jws, err := s.sign(strings.TrimPrefix(method, "/"), target, base64.RawURLEncoding.EncodeToString(sum[:]), byKID, correlationID)
	if err != nil {
		return nil, fmt.Errorf("signing Cloud request %s: %w", method, err)
	}
	signed := &cloudpbv2.SignedRequest{
		Payload:     payload,
		PayloadType: string(req.ProtoReflect().Descriptor().FullName()),
		Signature:   []byte(jws),
	}
	if method == cloudpbv2.AssetService_DeleteAsset_FullMethodName {
		deletion := req.(*cloudpbv2.DeleteAssetRequest) // checked by signedResources
		if deletion.ExpectedDeviceId != nil {
			management, err := s.unenrollManagementRequest(deletion.GetExpectedDeviceId())
			if err != nil {
				return nil, err
			}
			signed.PkiManagementRequest = management
		}
	}
	return signed, nil
}

// Cloud's shared unenrollment path relays this operator authority to PKI.
// The same leaf signs both artifacts; this never calls PKI or revokes locally.
func (s *Signer) unenrollManagementRequest(deviceID string) ([]byte, error) {
	device, err := uuid.Parse(deviceID)
	if err != nil || device.String() != deviceID {
		return nil, fmt.Errorf("unenrollment requires a canonical device UUID")
	}
	tenant, err := uuid.Parse(s.tenantUUID)
	if err != nil || tenant.String() != s.tenantUUID {
		return nil, fmt.Errorf("unenrollment requires a canonical operator tenant UUID")
	}
	now := s.now().Unix()
	payload, err := canonicalJSON(map[string]any{
		"op": "revoke_principal", "tenant": s.tenantUUID,
		"principal": "spiffe://wendy.sh/tenant/" + s.tenantUUID + "/device/" + deviceID,
		"iat":       now, "exp": now + int64(signatureTTL/time.Second), "jti": uuid.NewString(),
	})
	if err != nil {
		return nil, fmt.Errorf("encoding unenrollment authority: %w", err)
	}
	jws, err := s.signPayload(payload, map[string]any{"x5c": s.x5c})
	if err != nil {
		return nil, fmt.Errorf("signing unenrollment authority: %w", err)
	}
	return []byte(jws), nil
}

// signedResources maps each operator-signed method the CLI calls to its
// target.resource (cloud RequestSigning.md). The type assertion is what keeps
// payload_type equal to the method's request message.
var signedResources = map[string]func(tenant string, req proto.Message) (string, bool){
	cloudpbv2.OperatorSessionService_RegisterOperatorLeaf_FullMethodName: func(tenant string, req proto.Message) (string, bool) {
		in, ok := req.(*cloudpbv2.RegisterOperatorLeafRequest)
		return "org/" + in.GetOrganizationId() + "/operator-leaf", ok
	},
	cloudpbv2.DeviceEnrollmentService_EnrollDevice_FullMethodName: func(tenant string, req proto.Message) (string, bool) {
		in, ok := req.(*cloudpbv2.EnrollDeviceRequest)
		return "org/" + tenant + "/device/" + in.GetDeviceId(), ok
	},
	cloudpbv2.AssetService_DeleteAsset_FullMethodName: func(_ string, req proto.Message) (string, bool) {
		in, ok := req.(*cloudpbv2.DeleteAssetRequest)
		if !ok {
			return "", false
		}
		id, err := uuid.Parse(in.GetId())
		if err != nil || id.String() != in.GetId() {
			return "", false
		}
		return "asset/" + in.GetId(), true
	},
}

func requestTypeError(method string, req proto.Message) error {
	return fmt.Errorf("cannot sign Cloud request %s with message type %T", method, req)
}

func (s *Signer) sign(operation, resource, bodyDigest string, byKID bool, correlationID string) (string, error) {
	nonceBytes := make([]byte, 32)
	if _, err := io.ReadFull(s.random, nonceBytes); err != nil {
		return "", fmt.Errorf("generating request nonce: %w", err)
	}
	now := s.now().Unix()
	descriptor := map[string]any{
		"aud":            s.audience,
		"body_sha256":    bodyDigest,
		"correlation_id": correlationID,
		"expiry":         now + int64(signatureTTL/time.Second),
		"iat":            now,
		"nonce":          base64.RawURLEncoding.EncodeToString(nonceBytes),
		"operation":      operation,
		"target": map[string]any{
			"resource": resource,
			"tenant":   s.tenantUUID,
		},
	}
	payload, err := canonicalJSON(descriptor)
	if err != nil {
		return "", fmt.Errorf("encoding request descriptor: %w", err)
	}
	// Exactly one leaf reference: its kid, or an x5c of the leaf alone (Cloud
	// validates only the leaf through PKI, which owns the issuer chain).
	if byKID {
		return s.signPayload(payload, map[string]any{"kid": s.kid})
	}
	return s.signPayload(payload, map[string]any{"x5c": s.x5c[:1]})
}

// EnrollmentRequest signs PKI's enrollment authority separately from the Cloud
// RPC descriptor. PKI verifies this JWS against the tenant's Operator Authority.
func EnrollmentRequest(auth *config.AuthConfig, deviceID string) ([]byte, error) {
	s, err := newSigner(auth)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, fmt.Errorf("device enrollment requires an operator certificate")
	}
	now := s.now().Unix()
	payload, err := canonicalJSON(map[string]any{
		"tenant": s.tenantUUID, "device_id": deviceID, "device_class": "B",
		"iat": now, "exp": now + 300, "jti": uuid.NewString(),
	})
	if err != nil {
		return nil, err
	}
	// This artifact is carried in the protobuf body, not HTTP metadata. Keep
	// the full chain for PKI's enrollment signature verifier.
	jws, err := s.signPayload(payload, map[string]any{"x5c": s.x5c})
	return []byte(jws), err
}

// signPayload signs payload under a protected header of alg plus ref, the
// signer's leaf reference (x5c or kid).
func (s *Signer) signPayload(payload []byte, ref map[string]any) (string, error) {
	ref["alg"] = "ML-DSA-65"
	header, err := canonicalJSON(ref)
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
