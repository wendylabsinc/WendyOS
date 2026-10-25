// Package cloud implements portable Wendy Cloud request and inventory behavior.
package cloud

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	cloudpbv2 "github.com/wendylabsinc/WendyOS/go/wendy-core/core/cloud/internal/cloudpb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const signatureTTL = 30 * time.Second

// BrokerAudience is the existing Cloud request-signature audience.
const BrokerAudience = "https://cloud.wendy.sh/broker"

// Channel marks a caller's MCP origin outside the request signature.
type Channel struct {
	Channel                     int32
	MCPClientID, RelaySessionID string
}

// SignerOptions fixes the caller's channel and optional clock at construction.
// Now defaults to time.Now and must be safe for concurrent use when supplied.
type SignerOptions struct {
	Channel Channel
	Now     func() time.Time
}

// SignedRequest is Cloud's exact-body request envelope.
type SignedRequest = cloudpbv2.SignedRequest

// Signer delegates signatures and entropy to the host. The host validates the
// certificate/key binding and supplies its tenant. Instances are immutable and
// safe for concurrent calls when the host capabilities are.
type Signer struct {
	defaultMetadata           platform.Metadata
	key                       platform.Signer
	entropy                   platform.Entropy
	tenantUUID, kid, audience string
	x5c                       []string
	now                       func() time.Time
	channel                   Channel
}

// NewSigner binds an ML-DSA-65 host signer to a canonical operator tenant.
// Hosts validate certificate identity, chain order and key ownership beforehand.
func NewSigner(ctx context.Context, key platform.Signer, tenant string, entropy platform.Entropy, options SignerOptions) (*Signer, error) {
	if key == nil {
		return nil, fmt.Errorf("Cloud request signing requires an operator certificate")
	}
	if key.Algorithm() != platform.MLDSA65 {
		return nil, fmt.Errorf("Cloud request signing requires an ML-DSA-65 operator key; re-run 'wendy auth login'")
	}
	if !canonicalUUID(tenant) {
		return nil, fmt.Errorf("Cloud request signing requires a canonical operator tenant UUID")
	}
	chain, err := key.CertificateChain(ctx)
	if err != nil {
		return nil, err
	}
	if len(chain) == 0 || len(chain[0]) == 0 {
		return nil, fmt.Errorf("Cloud request signing requires an operator certificate")
	}
	x5c := make([]string, len(chain))
	for i, der := range chain {
		x5c[i] = base64.StdEncoding.EncodeToString(der)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Signer{key: key, entropy: entropy, tenantUUID: tenant, kid: leafKID(chain[0]), audience: BrokerAudience, x5c: x5c, now: now, channel: options.Channel}, nil
}

func leafKID(der []byte) string {
	sum := sha256.Sum256(der)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// SignRequest encodes the exact request body and its canonical descriptor.
// byKID is true only after successful leaf registration. Invoke handles that
// negotiation and retry for ordinary callers; adapters may use this encoder
// when they already own the call lifecycle.
func (s *Signer) SignRequest(ctx context.Context, method string, req proto.Message, byKID bool, correlationID string) (*SignedRequest, error) {
	return s.signRequest(ctx, method, req, byKID, correlationID)
}

func (s *Signer) signRequest(ctx context.Context, method string, req proto.Message, byKID bool, correlationID string) (*cloudpbv2.SignedRequest, error) {
	target, ok := signedResource(s.tenantUUID, method, req)
	if !ok {
		return nil, requestTypeError(method, req)
	}
	payload, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling Cloud request %s: %w", method, err)
	}
	sum := sha256.Sum256(payload)
	jws, err := s.sign(ctx, strings.TrimPrefix(method, "/"), target, base64.RawURLEncoding.EncodeToString(sum[:]), byKID, correlationID)
	if err != nil {
		return nil, fmt.Errorf("signing Cloud request %s: %w", method, err)
	}
	signed := &cloudpbv2.SignedRequest{
		Payload:     payload,
		PayloadType: string(req.ProtoReflect().Descriptor().FullName()),
		Signature:   []byte(jws),
	}
	if method == "/wendycloud.v2.AssetService/DeleteAsset" {
		deletion := req.ProtoReflect()
		if deletion.Has(deletion.Descriptor().Fields().ByName("expected_device_id")) {
			management, err := s.unenrollManagementRequest(ctx, stringField(req, "expected_device_id"))
			if err != nil {
				return nil, err
			}
			signed.PkiManagementRequest = management
		}
	}
	signed.Channel = cloudpbv2.RequestChannel(s.channel.Channel)
	signed.McpClientId = s.channel.MCPClientID
	signed.RelaySessionId = s.channel.RelaySessionID
	return signed, nil
}

// Cloud's shared unenrollment path relays this operator authority to PKI.
// The same leaf signs both artifacts; this never calls PKI or revokes locally.
func (s *Signer) unenrollManagementRequest(ctx context.Context, deviceID string) ([]byte, error) {
	if deviceID == "" || len(deviceID) > 64 || strings.Trim(deviceID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") != "" {
		return nil, fmt.Errorf("unenrollment requires an exact safe PKI device identity")
	}
	if uuidLike(deviceID) && !canonicalUUID(deviceID) {
		return nil, fmt.Errorf("unenrollment requires a canonical device UUID")
	}
	return s.principalRevocationManagementRequest(ctx, deviceID)
}

// Principal revocation also supports Lite identities that are not UUIDs.
// RevokePrincipalCertificates validates those IDs before issuing an RPC.
func (s *Signer) principalRevocationManagementRequest(ctx context.Context, deviceID string) ([]byte, error) {
	if !canonicalUUID(s.tenantUUID) {
		return nil, fmt.Errorf("unenrollment requires a canonical operator tenant UUID")
	}
	now := s.now().Unix()
	newJTI, err := randomUUID(ctx, s.entropy)
	if err != nil {
		return nil, err
	}
	payload, err := canonicalJSON(map[string]any{
		"op": "revoke_principal", "tenant": s.tenantUUID,
		"principal": "spiffe://wendy.sh/tenant/" + s.tenantUUID + "/device/" + deviceID,
		"iat":       now, "exp": now + int64(signatureTTL/time.Second), "jti": newJTI,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding unenrollment authority: %w", err)
	}
	jws, err := s.signPayload(ctx, payload, map[string]any{"x5c": s.x5c})
	if err != nil {
		return nil, fmt.Errorf("signing unenrollment authority: %w", err)
	}
	return []byte(jws), nil
}

// signedResource retains the CLI method/type allowlist and resource policy.
// Compare protobuf names rather than Go types so host-generated messages can
// coexist with Core's private generated descriptors.
func signedResource(tenant, method string, req proto.Message) (string, bool) {
	if req == nil {
		return "", false
	}
	name := string(req.ProtoReflect().Descriptor().FullName())
	field := func(key string) string { return stringField(req, protoreflect.Name(key)) }
	switch method {
	case "/wendycloud.v2.OperatorSessionService/RegisterOperatorLeaf":
		return "org/" + field("organization_id") + "/operator-leaf", name == "wendycloud.v2.RegisterOperatorLeafRequest"
	case "/wendycloud.v2.DeviceEnrollmentService/EnrollDevice":
		return "org/" + tenant + "/device/" + field("device_id"), name == "wendycloud.v2.EnrollDeviceRequest"
	case "/wendycloud.v2.AppService/UpsertApp", "/wendycloud.v2.DeploymentService/CreateAppRelease":
		expected, id := "wendycloud.v2.UpsertAppRequest", field("id")
		if strings.HasSuffix(method, "/CreateAppRelease") {
			expected, id = "wendycloud.v2.CreateAppReleaseRequest", field("app_id")
		}
		return "org/" + tenant + "/app/" + id, name == expected && field("organization_id") == tenant && id != ""
	case "/wendycloud.v2.DeploymentService/UpdateAppRelease":
		return "app-release/" + field("id"), name == "wendycloud.v2.UpdateAppReleaseRequest" && canonicalUUID(field("id"))
	case "/wendycloud.v2.RelayMcpService/OpenRelaySession":
		return "org/" + field("organization_id") + "/relay-session", name == "wendycloud.v2.OpenRelaySessionRequest"
	case "/wendycloud.v2.AssetService/DeleteAsset":
		return "asset/" + field("id"), name == "wendycloud.v2.DeleteAssetRequest" && canonicalUUID(field("id"))
	case "/wendycloud.v2.PKIAdminService/RevokePrincipalCertificates":
		return "org/" + tenant + "/pki", name == "wendycloud.v2.RevokePrincipalCertificatesRequest" && field("organization_id") == tenant
	case "/wendycloud.v2.PKIAdminService/RevokeEnrollmentToken":
		return "org/" + tenant + "/pki", name == "wendycloud.v2.RevokeEnrollmentTokenRequest" && field("organization_id") == tenant && field("token_id") != ""
	case "/wendycloud.v2.PKIAdminService/RevokeEABKey":
		return "org/" + tenant + "/pki", name == "wendycloud.v2.RevokeEABKeyRequest" && field("organization_id") == tenant && field("key_id") != ""
	}
	return "", false
}

func stringField(message proto.Message, name protoreflect.Name) string {
	reflected := message.ProtoReflect()
	field := reflected.Descriptor().Fields().ByName(name)
	if field == nil || field.Kind() != protoreflect.StringKind || field.IsList() || field.IsMap() {
		return ""
	}
	return reflected.Get(field).String()
}

func requestTypeError(method string, req proto.Message) error {
	return fmt.Errorf("cannot sign Cloud request %s with message type %T", method, req)
}

func (s *Signer) sign(ctx context.Context, operation, resource, bodyDigest string, byKID bool, correlationID string) (string, error) {
	nonceBytes, err := entropyBytes(ctx, s.entropy, 32)
	if err != nil {
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
		return s.signPayload(ctx, payload, map[string]any{"kid": s.kid})
	}
	return s.signPayload(ctx, payload, map[string]any{"x5c": s.x5c[:1]})
}

// signPayload signs payload under a protected header of alg plus ref, the
// signer's leaf reference (x5c or kid).
func (s *Signer) signPayload(ctx context.Context, payload []byte, ref map[string]any) (string, error) {
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
	signature, err := s.key.Sign(ctx, []byte(signingInput))
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

// WithMetadata returns a signer using an owned copy of metadata when an Invoke
// call supplies nil options.Metadata. It preserves a host operation's original
// correlation ID and authentication context through higher-level workflows.
// Explicit per-call metadata takes precedence. The original signer is unchanged.
func (s *Signer) WithMetadata(metadata platform.Metadata) *Signer {
	out := *s
	out.defaultMetadata = make(platform.Metadata, len(metadata))
	for i, entry := range metadata {
		out.defaultMetadata[i] = platform.MetadataEntry{Name: entry.Name, Value: append([]byte(nil), entry.Value...)}
	}
	return &out
}
