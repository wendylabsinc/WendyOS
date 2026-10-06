// Package cloudenroll holds the operator-signed device enrollment mint shared
// by the live `wendy device enroll`, the `wendy os install --pre-enroll` bake,
// and the MCP enroll tool. It lives outside cli/commands because cli/mcp cannot
// import cli/commands (commands imports mcp), and all three need the same EAB
// mint against DeviceEnrollmentService.EnrollDevice.
package cloudenroll

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/acmeenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
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

// MintEAB signs the operator enrollment request and relays it to Cloud's
// DeviceEnrollmentService.EnrollDevice, returning cfg populated with the
// once-only EAB credential and the registered asset id. The caller supplies
// the cloud connection and its authenticated context, because the CLI and the
// MCP server dial Cloud differently. No live device is involved: this is
// exactly what the live enroll does up to the agent step, so it also serves the
// image pre-enroll bake.
func MintEAB(tokenCtx context.Context, cloudConn *grpc.ClientConn, auth *config.AuthConfig, cfg acmeenroll.Config, name string) (acmeenroll.Config, string, error) {
	artifact, err := cloudrequest.EnrollmentRequest(auth, cfg.DeviceID)
	if err != nil {
		return cfg, "", fmt.Errorf("signing enrollment request: %w", err)
	}
	credential := &cloudpbv2.EnrollDeviceResponse{}
	err = cloudrequest.Invoke(tokenCtx, cloudConn, auth, cloudpbv2.DeviceEnrollmentService_EnrollDevice_FullMethodName, &cloudpbv2.EnrollDeviceRequest{
		DeviceId: cfg.DeviceID, DeviceClass: cloudpbv2.DeviceClass_DEVICE_CLASS_B,
		EnrollmentRequestJws: artifact, Name: name,
	}, credential)
	switch status.Code(err) {
	case codes.Unimplemented:
		return cfg, "", fmt.Errorf("this Cloud deployment does not support OIDC device enrollment; it needs DeviceEnrollmentService/EnrollDevice")
	case codes.AlreadyExists, codes.InvalidArgument:
		// Cloud validates the name and its unique indexes BEFORE relaying to
		// pki-core, so nothing was minted. Saying so is the point: the operator
		// otherwise assumes they just spent a single-use credential.
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
