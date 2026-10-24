// Package cloudrequest adapts CLI credentials and grpc-go calls to WendyGo.
package cloudrequest

import (
	"context"
	"crypto"
	"crypto/mldsa"
	"fmt"
	"os"
	"strings"
	"time"

	wendy "github.com/wendylabsinc/wendy-go"
	"github.com/wendylabsinc/wendy-go/cloud"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"github.com/wendylabsinc/wendy/go/proto/gen/pkicorepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// Signer retains only host-owned key and checked certificate material.
// WendyGo owns the request authority, canonical encoding and retry protocol.
type Signer struct {
	privateKey crypto.Signer
	chain      [][]byte
	tenantUUID string
	now        func() time.Time
}

// CollidingFieldMetadataKey names Cloud's exact collision-field trailer.
const CollidingFieldMetadataKey = cloud.CollidingFieldMetadataKey

// Invoke sends an exact-body operator-signed request through WendyGo.
func Invoke(ctx context.Context, conn grpc.ClientConnInterface, auth *config.AuthConfig, method string, req, reply proto.Message) error {
	return invoke(ctx, conn, auth, method, req, reply, "", nil)
}

// InvokeWithTrailer retains the call's response trailers for collision handling.
func InvokeWithTrailer(ctx context.Context, conn grpc.ClientConnInterface, auth *config.AuthConfig, method string, req, reply proto.Message, trailer *metadata.MD) error {
	return invoke(ctx, conn, auth, method, req, reply, "", trailer)
}

func invoke(ctx context.Context, conn grpc.ClientConnInterface, auth *config.AuthConfig, method string, req, reply proto.Message, revokeDeviceID string, trailer *metadata.MD) error {
	signer, err := newSigner(auth)
	if err != nil {
		return err
	}
	if signer == nil {
		return fmt.Errorf("%s requires an operator certificate; run 'wendy auth login'", method)
	}
	shared, err := signer.sdk(ctx)
	if err != nil {
		return err
	}
	md, _ := metadata.FromOutgoingContext(ctx)
	response, err := shared.Invoke(ctx, invokeTransport{conn: conn, reply: reply}, method, req, cloud.InvokeOptions{Metadata: coreMetadata(md), RevokeDeviceID: revokeDeviceID})
	if trailer != nil {
		*trailer = grpcMetadata(response.TrailingMetadata)
	}
	return err
}

func (s *Signer) sdk(ctx context.Context) (*cloud.Signer, error) {
	host, err := wendy.NewMLDSA65Signer(s.privateKey.(*mldsa.PrivateKey), s.chain)
	if err != nil {
		return nil, err
	}
	return cloud.NewSigner(ctx, host, s.tenantUUID, wendy.CryptoEntropy{}, cloud.SignerOptions{Channel: requestChannel(), Now: s.now})
}

// RevokePrincipalCertificates revokes the selected tenant's live device
// certificates through Cloud's PKI relay. Both request signatures use the
// same operator leaf. Each retry carries fresh PKI replay authority.
func RevokePrincipalCertificates(ctx context.Context, conn grpc.ClientConnInterface, auth *config.AuthConfig, deviceID string) (int, error) {
	if deviceID == "" || len(deviceID) > 64 || strings.Trim(deviceID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") != "" {
		return 0, fmt.Errorf("device ID must contain 1–64 letters, digits, dots, underscores or hyphens")
	}
	if auth == nil || len(auth.Certificates) == 0 {
		return 0, fmt.Errorf("device revocation requires an operator certificate")
	}
	tenant, err := operatorTenant(auth.Certificates[0].PrincipalURI)
	if err != nil {
		return 0, err
	}
	reply := new(pkicorepb.RevokeByPrincipalResponse)
	err = invoke(ctx, conn, auth, cloudpbv2.PKIAdminService_RevokePrincipalCertificates_FullMethodName,
		&cloudpbv2.RevokePrincipalCertificatesRequest{OrganizationId: tenant}, reply, deviceID, nil)
	if err != nil {
		return 0, err
	}
	if reply.GetRevokedCount() < 0 {
		return 0, fmt.Errorf("Cloud returned an invalid revoked-certificate count")
	}
	return int(reply.GetRevokedCount()), nil
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
	return &Signer{privateKey: privateKey, chain: pair.Certificate, tenantUUID: tenantUUID, now: time.Now}, nil
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

// The MCP channel marker of this process and of the CLI children its tools
// start, set by SetChannel and read by markChannel.
const (
	channelEnv        = "WENDY_MCP_CHANNEL"
	mcpClientIDEnv    = "WENDY_MCP_CLIENT_ID"
	relaySessionIDEnv = "WENDY_MCP_RELAY_SESSION_ID"
)

// SetChannel marks every later signed Cloud call of this process, and of the
// CLI children it starts, as made through an MCP channel (Relay MCP contract,
// docs/relay-mcp.md): MCP_LOCAL for `wendy mcp serve`, HOSTED_MCP with the web
// client's OAuth client_id and the relay session for `wendy cloud mcp`. The
// marker is outside the signature; a modified client can omit it.
func SetChannel(channel cloudpbv2.RequestChannel, mcpClientID, relaySessionID string) error {
	for _, kv := range [][2]string{{mcpClientIDEnv, mcpClientID}, {relaySessionIDEnv, relaySessionID}, {channelEnv, channel.String()}} {
		if err := os.Setenv(kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

func markChannel(signed *cloudpbv2.SignedRequest) {
	signed.Channel = cloudpbv2.RequestChannel(cloudpbv2.RequestChannel_value[os.Getenv(channelEnv)])
	if signed.Channel == cloudpbv2.RequestChannel_REQUEST_CHANNEL_HOSTED_MCP {
		signed.McpClientId = os.Getenv(mcpClientIDEnv)
		signed.RelaySessionId = os.Getenv(relaySessionIDEnv)
	}
}

func requestChannel() cloud.Channel {
	signed := new(cloudpbv2.SignedRequest)
	markChannel(signed)
	return cloud.Channel{Channel: int32(signed.Channel), MCPClientID: signed.McpClientId, RelaySessionID: signed.RelaySessionId}
}

// EnrollmentRequest signs PKI's enrollment authority separately from the Cloud
// RPC descriptor. PKI verifies this JWS against the tenant's Operator Authority.
func EnrollmentRequest(auth *config.AuthConfig, deviceID string) ([]byte, error) {
	return EnrollmentRequestForClass(auth, deviceID, "B")
}

// EnrollmentRequestForClass binds the operator authorization to a device tier.
func EnrollmentRequestForClass(auth *config.AuthConfig, deviceID, deviceClass string) ([]byte, error) {
	return EnrollmentRequestForChain(auth, deviceID, deviceClass, "")
}

// EnrollmentChainP256 selects the PKI's P-256 device authority for constrained
// devices. The operator signature binds this choice alongside the device tier.
const EnrollmentChainP256 = cloud.EnrollmentChainP256

func EnrollmentRequestForChain(auth *config.AuthConfig, deviceID, deviceClass, chainAlgorithm string) ([]byte, error) {
	if deviceClass != "A" && deviceClass != "B" && deviceClass != "C" {
		return nil, fmt.Errorf("invalid device class %q", deviceClass)
	}
	if chainAlgorithm != "" && (deviceClass != "C" || chainAlgorithm != EnrollmentChainP256) {
		return nil, fmt.Errorf("P-256 certificate chains are supported only for Class C enrollment")
	}
	s, err := newSigner(auth)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, fmt.Errorf("device enrollment requires an operator certificate")
	}
	shared, err := s.sdk(context.Background())
	if err != nil {
		return nil, err
	}
	return shared.EnrollmentRequest(context.Background(), deviceID, deviceClass, chainAlgorithm)
}
