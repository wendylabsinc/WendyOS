// Package cloudenroll holds the operator-signed device enrollment mint shared
// by the live `wendy device enroll`, the `wendy os install --pre-enroll` bake,
// and the MCP enroll tool. It lives outside cli/commands because cli/mcp cannot
// import cli/commands (commands imports mcp), and all three need the same EAB
// mint against DeviceEnrollmentService.EnrollDevice.
package cloudenroll

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/acmeenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// EnrollmentConfig resolves and validates the ACME enrollment target for an
// operator session and a freshly minted device_id, before Cloud mints a
// once-only credential. deviceID is the permanent identity, not the
// operator-typed name: it becomes the device's SPIFFE SAN and pki-core carries
// it across every renewal. When directoryURL is empty it is derived from the
// session's PKI endpoint.
func EnrollmentConfig(auth *config.AuthConfig, deviceID, directoryURL string) (acmeenroll.Config, error) {
	cfg := acmeenroll.Config{DeviceID: deviceID, DirectoryURL: directoryURL}
	if len(auth.Certificates) == 0 {
		return cfg, fmt.Errorf("session has no operator identity; re-run 'wendy auth login'")
	}
	u, err := url.Parse(auth.Certificates[0].PrincipalURI)
	if err != nil || u.Scheme != "spiffe" || u.Host != "wendy.sh" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return cfg, fmt.Errorf("session has no valid operator identity; re-run 'wendy auth login'")
	}
	parts := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	if len(parts) != 4 || parts[0] != "tenant" || parts[2] != "operator" || parts[3] == "" {
		return cfg, fmt.Errorf("session must carry a tenant operator identity")
	}
	tenant, err := uuid.Parse(parts[1])
	if err != nil || tenant.String() != parts[1] {
		return cfg, fmt.Errorf("session has an invalid tenant UUID")
	}
	if cfg.DirectoryURL == "" {
		host := PKISiblingHost(auth.PKIEndpoint, "acme")
		if host == "" {
			return cfg, fmt.Errorf("no ACME directory configured for this PKI deployment; pass --acme-directory-url")
		}
		cfg.DirectoryURL = "https://" + host + "/" + tenant.String() + "/acme/directory"
	}
	principal, err := cfg.PrincipalURI()
	if err != nil {
		return cfg, err
	}
	if principal != "spiffe://wendy.sh/tenant/"+tenant.String()+"/device/"+deviceID {
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
func DeviceUUIDTaken(err error, trailer metadata.MD) bool {
	field := trailer.Get(cloudrequest.CollidingFieldMetadataKey)
	return status.Code(err) == codes.AlreadyExists && (slices.Contains(field, "device_id") || slices.Contains(field, "mesh_suffix"))
}

// RetryTakenDeviceUUID is the one device UUID collision guard every
// enrollment path shares (compute and Lite). It calls enroll with deviceID and,
// while enroll fails with ErrDeviceUUIDTaken, again with a fresh UUID, at most
// maxMintAttempts times in all; a refused UUID is never reused.
func RetryTakenDeviceUUID(deviceID string, enroll func(deviceID string) error) error {
	for attempt := 1; ; attempt++ {
		err := enroll(deviceID)
		if !errors.Is(err, ErrDeviceUUIDTaken) || attempt == maxMintAttempts {
			return err
		}
		deviceID = uuid.NewString()
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
func MintEAB(tokenCtx context.Context, cloudConn *grpc.ClientConn, auth *config.AuthConfig, cfg acmeenroll.Config, name string, hw *cloudpbv2.DeviceHardware) (out acmeenroll.Config, assetID string, err error) {
	out = cfg
	err = RetryTakenDeviceUUID(cfg.DeviceID, func(deviceID string) error {
		next := cfg
		if deviceID != cfg.DeviceID {
			var err error
			if next, err = EnrollmentConfig(auth, deviceID, cfg.DirectoryURL); err != nil {
				return err
			}
		}
		var err error
		out, assetID, err = mintOnce(tokenCtx, cloudConn, auth, next, name, hw)
		return err
	})
	return out, assetID, err
}

func mintOnce(tokenCtx context.Context, cloudConn *grpc.ClientConn, auth *config.AuthConfig, cfg acmeenroll.Config, name string, hw *cloudpbv2.DeviceHardware) (acmeenroll.Config, string, error) {
	artifact, err := cloudrequest.EnrollmentRequest(auth, cfg.DeviceID)
	if err != nil {
		return cfg, "", fmt.Errorf("signing enrollment request: %w", err)
	}
	credential := &cloudpbv2.EnrollDeviceResponse{}
	var trailer metadata.MD
	err = cloudrequest.InvokeWithTrailer(tokenCtx, cloudConn, auth, cloudpbv2.DeviceEnrollmentService_EnrollDevice_FullMethodName, &cloudpbv2.EnrollDeviceRequest{
		DeviceId: cfg.DeviceID, DeviceClass: cloudpbv2.DeviceClass_DEVICE_CLASS_B,
		EnrollmentRequestJws: artifact, Name: name, Hardware: hw,
	}, credential, &trailer)
	switch status.Code(err) {
	case codes.Unimplemented:
		return cfg, "", fmt.Errorf("this Cloud deployment does not support OIDC device enrollment; it needs DeviceEnrollmentService/EnrollDevice")
	case codes.AlreadyExists:
		if DeviceUUIDTaken(err, trailer) {
			return cfg, "", fmt.Errorf("Cloud refused device UUID %s before minting anything: %w", cfg.DeviceID, ErrDeviceUUIDTaken)
		}
		// Cloud validates the name and its unique indexes BEFORE relaying to
		// pki-core, so nothing was minted. Saying so is the point: the operator
		// otherwise assumes they just spent a single-use credential.
		return cfg, "", fmt.Errorf("Cloud refused this enrollment before minting anything, so no credential was spent: %w", err)
	case codes.InvalidArgument:
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
