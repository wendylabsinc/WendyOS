// Package enrollment implements portable device enrollment and removal policy.
package enrollment

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/core/cloud"
	cloudpbv2 "github.com/wendylabsinc/WendyOS/go/wendy-core/core/enrollment/internal/cloudpb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
)

//go:generate python3 ../../scripts/generate_enrollment.py

// Session is the public identity of the selected host-managed session.
type Session struct{ Principal, PKIEndpoint string }

// DeviceHardware preserves optional hardware fields from the Agent report.
type DeviceHardware = cloudpbv2.DeviceHardware

// Client uses authenticated Cloud transport supplied by the host.
// Signer supplies Cloud request authority, with keys retained in the host.
type Client struct {
	Cloud    platform.Transport
	Signer   *cloud.Signer
	Entropy  platform.Entropy
	Metadata platform.Metadata
}

// EnrollmentConfig resolves and validates the ACME enrollment target for an
// operator session and a freshly minted device_id, before Cloud mints a
// once-only credential. deviceID is the permanent identity, not the
// operator-typed name: it becomes the device's SPIFFE SAN and pki-core carries
// it across every renewal. When directoryURL is empty it is derived from the
// session's PKI endpoint.
func EnrollmentConfig(session Session, deviceID, directoryURL string) (Config, error) {
	cfg := Config{DeviceID: deviceID, DirectoryURL: directoryURL}
	if session.Principal == "" {
		return cfg, fmt.Errorf("session has no operator identity; re-run 'wendy auth login'")
	}
	u, err := url.Parse(session.Principal)
	if err != nil || u.Scheme != "spiffe" || u.Host != "wendy.sh" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return cfg, fmt.Errorf("session has no valid operator identity; re-run 'wendy auth login'")
	}
	parts := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	if len(parts) != 4 || parts[0] != "tenant" || parts[2] != "operator" || parts[3] == "" {
		return cfg, fmt.Errorf("session must carry a tenant operator identity")
	}
	tenant := parts[1]
	if !canonicalUUID(tenant) {
		return cfg, fmt.Errorf("session has an invalid tenant UUID")
	}
	if cfg.DirectoryURL == "" {
		host := PKISiblingHost(session.PKIEndpoint, "acme")
		if host == "" {
			return cfg, fmt.Errorf("no ACME directory configured for this PKI deployment; pass --acme-directory-url")
		}
		cfg.DirectoryURL = "https://" + host + "/" + tenant + "/acme/directory"
	}
	principal, err := cfg.PrincipalURI()
	if err != nil {
		return cfg, err
	}
	if principal != "spiffe://wendy.sh/tenant/"+tenant+"/device/"+deviceID {
		return cfg, fmt.Errorf("ACME enrollment tenant does not match the selected session")
	}
	return cfg, nil
}

// maxMintAttempts bounds the device UUID collision guard: the first mint plus
// three regenerations. A random UUID colliding even once is already rare.
const maxMintAttempts = 4

// ErrDeviceUUIDTaken is Cloud's pre-mint refusal of a device UUID that is
// already in use, or whose wendy net address another identity claims, in ANY
// tenant. Cloud names only the field, never the holder.
var ErrDeviceUUIDTaken = errors.New("device UUID already in use")

// DeviceUUIDTaken reports whether err is Cloud's EnrollDevice refusal of the
// device UUID itself: AlreadyExists with the colliding-field trailer naming
// device_id or mesh_suffix. A name duplicate carries no such trailer, and a
// fresh UUID cannot clear it.
func DeviceUUIDTaken(err error, trailer platform.Metadata) bool {
	if rpcCode(err) != platform.RPCAlreadyExists {
		return false
	}
	for _, field := range trailer {
		if field.Name == cloud.CollidingFieldMetadataKey && (string(field.Value) == "device_id" || string(field.Value) == "mesh_suffix") {
			return true
		}
	}
	return false
}

// RetryTakenDeviceUUID is the one device UUID collision guard every
// enrollment path shares (compute and Lite). It calls enroll with deviceID and,
// while enroll fails with ErrDeviceUUIDTaken, again with a fresh UUID, at most
// maxMintAttempts times in all; a refused UUID is never reused.
func RetryTakenDeviceUUID(ctx context.Context, entropy platform.Entropy, deviceID string, enroll func(deviceID string) error) error {
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := enroll(deviceID)
		if !errors.Is(err, ErrDeviceUUIDTaken) || attempt == maxMintAttempts {
			return err
		}
		deviceID, err = NewDeviceID(ctx, entropy)
		if err != nil {
			return err
		}
	}
}

// MintEAB signs the operator enrollment request and relays it to Cloud's
// DeviceEnrollmentService.EnrollDevice, returning cfg populated with the
// once-only EAB credential and the registered asset id. The caller supplies
// the cloud connection and its authenticated context, because the CLI and the
// MCP server dial Cloud differently. No live device is involved: this is
// exactly what the live enroll does up to the agent step, so it also serves the
// image pre-enroll bake. hw is the device's own hardware report (ReadHardware);
// nil sends none, as the pre-enroll bake must, having no device to ask.
//
// The device UUID in cfg is the device's one identity: Cloud adopts it as the
// asset id. A UUID Cloud refuses as taken is replaced through
// RetryTakenDeviceUUID. The returned cfg carries the UUID Cloud accepted.
func (c *Client) MintEAB(tokenCtx context.Context, session Session, cfg Config, name string, hw *DeviceHardware) (out Config, assetID string, err error) {
	out = cfg
	if c.Signer == nil {
		return out, "", fmt.Errorf("signing enrollment request: device enrollment requires an operator certificate")
	}
	err = RetryTakenDeviceUUID(tokenCtx, c.Entropy, cfg.DeviceID, func(deviceID string) error {
		next := cfg
		if deviceID != cfg.DeviceID {
			var err error
			if next, err = EnrollmentConfig(session, deviceID, cfg.DirectoryURL); err != nil {
				return err
			}
		}
		var err error
		out, assetID, err = c.mintOnce(tokenCtx, next, name, hw)
		return err
	})
	return out, assetID, err
}

func (c *Client) mintOnce(tokenCtx context.Context, cfg Config, name string, hw *DeviceHardware) (Config, string, error) {
	artifact, err := c.Signer.EnrollmentRequest(tokenCtx, cfg.DeviceID, "B", "")
	if err != nil {
		return cfg, "", fmt.Errorf("signing enrollment request: %w", err)
	}
	credential := &cloudpbv2.EnrollDeviceResponse{}
	response, err := c.signed(tokenCtx, "/wendycloud.v2.DeviceEnrollmentService/EnrollDevice", &cloudpbv2.EnrollDeviceRequest{
		DeviceId: cfg.DeviceID, DeviceClass: cloudpbv2.DeviceClass_DEVICE_CLASS_B,
		EnrollmentRequestJws: artifact, Name: name, Hardware: hw,
	}, credential, "")
	switch rpcCode(err) {
	case platform.RPCUnimplemented:
		return cfg, "", fmt.Errorf("this Cloud deployment does not support OIDC device enrollment; it needs DeviceEnrollmentService/EnrollDevice")
	case platform.RPCAlreadyExists:
		if DeviceUUIDTaken(err, response.TrailingMetadata) {
			return cfg, "", fmt.Errorf("Cloud refused device UUID %s before minting anything: %w", cfg.DeviceID, ErrDeviceUUIDTaken)
		}
		// Cloud validates the name and its unique indexes BEFORE relaying to
		// pki-core, so nothing was minted. Saying so is the point: the operator
		// otherwise assumes they just spent a single-use credential.
		return cfg, "", fmt.Errorf("Cloud refused this enrollment before minting anything, so no credential was spent: %w", err)
	case platform.RPCInvalidArgument:
		return cfg, "", fmt.Errorf("Cloud refused this enrollment before minting anything, so no credential was spent: %w", err)
	}
	if err != nil {
		return cfg, "", fmt.Errorf("creating PKI enrollment credential: %w", err)
	}
	if credential.GetCredentialKind() != "eab" {
		return cfg, "", fmt.Errorf("Cloud returned an unexpected enrollment credential kind")
	}
	cfg.EABKeyID, cfg.EABHMACKey = credential.GetEabKeyId(), credential.GetEabHmacKey()
	if err := cfg.Validate(); err != nil {
		return cfg, "", fmt.Errorf("Cloud returned invalid enrollment credentials: %w", err)
	}
	return cfg, credential.GetAssetId(), nil
}

// PKISiblingHost rewrites a pki-core identity frontend into a sibling frontend
// (e.g. "acme") within the same PKI host family. A URL that is not an https
// identity frontend derives nothing, so the caller reports the deployment as
// unconfigured rather than guessing (WDY-2799).
func PKISiblingHost(identityEndpoint, label string) string {
	u, err := url.Parse(identityEndpoint)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return ""
	}
	rest, ok := strings.CutPrefix(u.Host, "identity.")
	if !ok || rest == "" {
		return ""
	}
	return label + "." + rest
}
