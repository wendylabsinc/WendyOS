package cloud

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	pb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/cloud/internal/cloudpb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"google.golang.org/protobuf/proto"
)

//go:generate python3 ../../scripts/generate_cloud.py

// CollidingFieldMetadataKey identifies the Cloud collision-detail trailer.
const CollidingFieldMetadataKey = "wendy-colliding-field"

var registeredKIDs sync.Map

// InvokeOptions retains the caller's metadata and exact revocation authority.
// RevokeDeviceID is used only for RevokePrincipalCertificates; ordinary callers
// leave it empty. Metadata is borrowed and never mutated.
type InvokeOptions struct {
	Metadata       platform.Metadata
	RevokeDeviceID string
}

// Invoke wraps the exact serialized request in Cloud's SignedRequest. A cached
// kid refusal gets one x5c retry with a fresh signature and the same flow ID.
// It returns response metadata even when the RPC fails.
func (s *Signer) Invoke(ctx context.Context, transport platform.Transport, method string, request proto.Message, options InvokeOptions) (platform.RPCResponse, error) {
	if err := ctx.Err(); err != nil {
		return platform.RPCResponse{}, err
	}
	if _, ok := signedResource(s.tenantUUID, method, request); !ok {
		return platform.RPCResponse{}, requestTypeError(method, request)
	}
	if options.RevokeDeviceID != "" {
		if method != "/wendycloud.v2.PKIAdminService/RevokePrincipalCertificates" {
			return platform.RPCResponse{}, fmt.Errorf("revocation authority requires RevokePrincipalCertificates")
		}
		if err := validateDeviceID(options.RevokeDeviceID); err != nil {
			return platform.RPCResponse{}, err
		}
	}
	if transport == nil {
		return platform.RPCResponse{}, fmt.Errorf("host Cloud transport is required")
	}
	correlation, err := CorrelationID(ctx, options.Metadata, s.entropy)
	if err != nil {
		return platform.RPCResponse{}, err
	}
	call := func(byKID bool) (platform.RPCResponse, error) {
		signed, err := s.signRequest(ctx, method, request, byKID, correlation)
		if err != nil {
			return platform.RPCResponse{}, err
		}
		if options.RevokeDeviceID != "" {
			signed.PkiManagementRequest, err = s.principalRevocationManagementRequest(ctx, options.RevokeDeviceID)
			if err != nil {
				return platform.RPCResponse{}, err
			}
		}
		wire, err := proto.Marshal(signed)
		if err != nil {
			return platform.RPCResponse{}, err
		}
		return transport.Unary(ctx, platform.RPCRequest{Method: method, Message: wire, Metadata: options.Metadata})
	}
	if x5cOnly[method] {
		return call(false)
	}
	registered, err := s.register(ctx, transport, correlation, options.Metadata)
	if err != nil {
		return platform.RPCResponse{}, err
	}
	if !registered {
		return call(false)
	}
	response, err := call(true)
	if !kidRefused(err) {
		return response, err
	}
	registeredKIDs.Delete(s.kid)
	response, err = call(false)
	if err != nil {
		return response, fmt.Errorf("Cloud refused %s by key id and again with the operator certificate attached: %w", method, err)
	}
	return response, nil
}

var x5cOnly = map[string]bool{
	"/wendycloud.v2.OperatorSessionService/RegisterOperatorLeaf": true,
	"/wendycloud.v2.DeploymentService/CreateDeployment":          true,
	"/wendycloud.v2.DeploymentService/ControlContainer":          true,
}

func kidRefused(err error) bool {
	var status *platform.RPCStatusError
	if !errors.As(err, &status) {
		return false
	}
	return status.Code == platform.RPCPermissionDenied || status.Code == platform.RPCFailedPrecondition && strings.Contains(status.Message, "leaf not stored")
}

func (s *Signer) register(ctx context.Context, transport platform.Transport, correlation string, metadata platform.Metadata) (bool, error) {
	if _, ok := registeredKIDs.Load(s.kid); ok {
		return true, nil
	}
	const method = "/wendycloud.v2.OperatorSessionService/RegisterOperatorLeaf"
	signed, err := s.signRequest(ctx, method, &pb.RegisterOperatorLeafRequest{OrganizationId: s.tenantUUID}, false, correlation)
	if err != nil {
		return false, err
	}
	wire, err := proto.Marshal(signed)
	if err != nil {
		return false, err
	}
	response, err := transport.Unary(ctx, platform.RPCRequest{Method: method, Message: wire, Metadata: metadata})
	if err != nil {
		var status *platform.RPCStatusError
		if errors.As(err, &status) && status.Code == platform.RPCUnimplemented {
			return false, nil
		}
		return false, fmt.Errorf("registering the operator certificate with Cloud: %w", err)
	}
	var reply pb.RegisterOperatorLeafResponse
	if err := proto.Unmarshal(response.Message, &reply); err != nil {
		return false, err
	}
	if reply.GetKid() != s.kid {
		return false, fmt.Errorf("Cloud registered operator certificate %q, not this session's %q", reply.GetKid(), s.kid)
	}
	registeredKIDs.Store(s.kid, struct{}{})
	return true, nil
}

// CorrelationID adopts a safe caller flow ID or creates a host-random UUID.
func CorrelationID(ctx context.Context, metadata platform.Metadata, entropy platform.Entropy) (string, error) {
	for _, entry := range metadata {
		if entry.Name != "x-correlation-id" || len(entry.Value) == 0 {
			continue
		}
		id := string(entry.Value)
		if len(id) > 64 || strings.IndexFunc(id, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-')
		}) >= 0 {
			return "", fmt.Errorf("invalid supplied Cloud correlation id")
		}
		return id, nil
	}
	return randomUUID(ctx, entropy)
}

func entropyBytes(ctx context.Context, entropy platform.Entropy, size int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if entropy == nil {
		return nil, fmt.Errorf("host entropy source is required")
	}
	raw, err := entropy.RandomBytes(ctx, size)
	if err != nil {
		return nil, err
	}
	if len(raw) != size {
		return nil, fmt.Errorf("host entropy source returned the wrong number of bytes")
	}
	return raw, nil
}

func randomUUID(ctx context.Context, entropy platform.Entropy) (string, error) {
	raw, err := entropyBytes(ctx, entropy, 16)
	if err != nil {
		return "", err
	}
	raw[6] = raw[6]&15 | 64
	raw[8] = raw[8]&63 | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:]), nil
}

func canonicalUUID(s string) bool {
	if len(s) != 36 || s != strings.ToLower(s) {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// uuidLike matches the UUID spellings previously accepted by uuid.Parse.
func uuidLike(s string) bool {
	if len(s) == 45 && strings.EqualFold(s[:9], "urn:uuid:") {
		s = s[9:]
	}
	if len(s) == 38 && s[0] == '{' && s[37] == '}' {
		s = s[1:37]
	}
	if len(s) == 36 {
		if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
			return false
		}
		s = strings.ReplaceAll(s, "-", "")
	}
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func validateDeviceID(id string) error {
	if id == "" || len(id) > 64 || strings.Trim(id, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") != "" {
		return fmt.Errorf("device ID must contain 1–64 letters, digits, dots, underscores or hyphens")
	}
	return nil
}
