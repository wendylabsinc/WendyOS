package enrollment

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/core/cloud"
	agentpb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/enrollment/internal/agentpb"
	cloudpb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/enrollment/internal/cloudpb"
	pkipb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/enrollment/internal/pkipb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"google.golang.org/protobuf/proto"
	"strings"
)

func rpcCode(err error) platform.RPCStatusCode {
	if err == nil {
		return platform.RPCOK
	}
	var status *platform.RPCStatusError
	if errors.As(err, &status) {
		return status.Code
	}
	return platform.RPCUnknown
}

func unary(ctx context.Context, transport platform.Transport, method string, request, reply proto.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if transport == nil {
		return fmt.Errorf("host RPC transport is required")
	}
	wire, err := proto.Marshal(request)
	if err != nil {
		return err
	}
	response, err := transport.Unary(ctx, platform.RPCRequest{Method: method, Message: wire})
	if err != nil {
		return err
	}
	return proto.Unmarshal(response.Message, reply)
}

func (c *Client) signed(ctx context.Context, method string, request, reply proto.Message, revokeDeviceID string) (platform.RPCResponse, error) {
	if c.Signer == nil {
		return platform.RPCResponse{}, fmt.Errorf("%s requires an operator certificate; run 'wendy auth login'", method)
	}
	response, err := c.Signer.Invoke(ctx, c.Cloud, method, request, cloud.InvokeOptions{Metadata: c.Metadata, RevokeDeviceID: revokeDeviceID})
	if err != nil {
		return response, err
	}
	return response, proto.Unmarshal(response.Message, reply)
}

// NewDeviceID generates an unpredictable RFC 4122 version-4 permanent identity.
// The device name remains independent and can be changed without changing it.
func NewDeviceID(ctx context.Context, entropy platform.Entropy) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if entropy == nil {
		return "", fmt.Errorf("host entropy source is required")
	}
	raw, err := entropy.RandomBytes(ctx, 16)
	if err != nil {
		return "", err
	}
	if len(raw) != 16 {
		return "", fmt.Errorf("host entropy source returned the wrong number of bytes")
	}
	raw[6] = raw[6]&15 | 64
	raw[8] = raw[8]&63 | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:]), nil
}

func canonicalUUID(value string) bool {
	if len(value) != 36 || value != strings.ToLower(value) {
		return false
	}
	if value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	_, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	return err == nil
}

func operatorTenant(principal string) string {
	id, err := ParsePrincipal(principal)
	if err != nil || id.EntityType != "user" {
		return ""
	}
	return id.TenantUUID
}

func (c *Client) revokePrincipal(ctx context.Context, tenant, deviceID string) (int, error) {
	if deviceID == "" || len(deviceID) > 64 || strings.Trim(deviceID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") != "" {
		return 0, fmt.Errorf("device ID must contain 1–64 letters, digits, dots, underscores or hyphens")
	}
	reply := new(pkipb.RevokeByPrincipalResponse)
	_, err := c.signed(ctx, "/wendycloud.v2.PKIAdminService/RevokePrincipalCertificates", &cloudpb.RevokePrincipalCertificatesRequest{OrganizationId: tenant}, reply, deviceID)
	if err != nil {
		return 0, err
	}
	if reply.GetRevokedCount() < 0 {
		return 0, fmt.Errorf("Cloud returned an invalid revoked-certificate count")
	}
	return int(reply.GetRevokedCount()), nil
}

// StartACMEProvisioning sends the accepted EAB to the device and verifies the
// returned permanent identity. An error preserves the Cloud reservation warning.
func StartACMEProvisioning(ctx context.Context, transport platform.Transport, cloudHost string, cfg Config, assetID string) (string, error) {
	reply := new(agentpb.StartACMEProvisioningResponse)
	err := unary(ctx, transport, "/wendy.agent.services.v2.WendyProvisioningService/StartACMEProvisioning", &agentpb.StartACMEProvisioningRequest{CloudHost: cloudHost, DirectoryUrl: cfg.DirectoryURL, DeviceId: cfg.DeviceID, EabKeyId: cfg.EABKeyID, EabHmacKey: cfg.EABHMACKey}, reply)
	if rpcCode(err) == platform.RPCUnimplemented {
		return "", fmt.Errorf("Cloud registered asset %s, but this agent does not support direct PKI enrollment; update the agent (the device name is now reserved in Cloud)", assetID)
	}
	if err != nil {
		return "", fmt.Errorf("Cloud registered asset %s, but device PKI enrollment failed (the device name is now reserved in Cloud): %w", assetID, err)
	}
	expected, _ := cfg.PrincipalURI()
	if reply.GetPrincipalUri() != expected {
		return "", fmt.Errorf("agent returned an unexpected enrollment identity")
	}
	return reply.GetPrincipalUri(), nil
}
