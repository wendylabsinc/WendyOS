// Package cloudenroll adapts CLI sessions and connections to WendyGo enrollment.
package cloudenroll

import (
	"context"
	"fmt"

	wendy "github.com/wendylabsinc/wendy-go"
	"github.com/wendylabsinc/wendy-go/enrollment"
	"github.com/wendylabsinc/wendy/go/internal/agent/acmeenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// ErrDeviceUUIDTaken preserves the enrollment collision sentinel.
var ErrDeviceUUIDTaken = enrollment.ErrDeviceUUIDTaken

// EnrollmentConfig validates the session's public enrollment target in Core.
func EnrollmentConfig(auth *config.AuthConfig, deviceID, directoryURL string) (acmeenroll.Config, error) {
	if auth != nil && len(auth.Certificates) > 0 && auth.Certificates[0].PrincipalURI == "" {
		return acmeenroll.Config{DeviceID: deviceID, DirectoryURL: directoryURL}, fmt.Errorf("session has no valid operator identity; re-run 'wendy auth login'")
	}
	cfg, err := enrollment.EnrollmentConfig(cloudrequest.EnrollmentSession(auth), deviceID, directoryURL)
	return acmeenroll.Config(cfg), err
}

// DeviceUUIDTaken retains grpc-go trailers at the shared retry-policy boundary.
func DeviceUUIDTaken(err error, trailer metadata.MD) bool {
	return enrollment.DeviceUUIDTaken(cloudrequest.EnrollmentError(err, nil, trailer), cloudrequest.EnrollmentMetadata(trailer))
}

// RetryTakenDeviceUUID uses the shared bounded collision policy for compute/Lite.
func RetryTakenDeviceUUID(deviceID string, enroll func(string) error) error {
	return enrollment.RetryTakenDeviceUUID(context.Background(), wendy.CryptoEntropy{}, deviceID, enroll)
}

// NewDeviceID creates an independent permanent identity through host entropy.
func NewDeviceID(ctx context.Context) (string, error) {
	return enrollment.NewDeviceID(ctx, wendy.CryptoEntropy{})
}

// MintEAB borrows an authenticated Cloud connection and host-managed identity.
func MintEAB(ctx context.Context, conn *grpc.ClientConn, auth *config.AuthConfig, cfg acmeenroll.Config, name string, hw *cloudpb.DeviceHardware) (acmeenroll.Config, string, error) {
	client := cloudrequest.EnrollmentClient(ctx, conn, auth)
	var hardware *enrollment.DeviceHardware
	if hw != nil {
		wire, err := proto.Marshal(hw)
		if err != nil {
			return cfg, "", err
		}
		hardware = new(enrollment.DeviceHardware)
		if err := proto.Unmarshal(wire, hardware); err != nil {
			return cfg, "", err
		}
	}
	out, id, err := client.MintEAB(ctx, cloudrequest.EnrollmentSession(auth), enrollment.Config(cfg), name, hardware)
	return acmeenroll.Config(out), id, err
}

// PKISiblingHost retains the existing enrollment endpoint helper.
func PKISiblingHost(endpoint, label string) string { return enrollment.PKISiblingHost(endpoint, label) }

// StartACMEProvisioning retains the CLI's transport boundary and delegates policy.
func StartACMEProvisioning(ctx context.Context, conn grpc.ClientConnInterface, cloudHost string, cfg acmeenroll.Config, assetID string) (string, error) {
	return enrollment.StartACMEProvisioning(ctx, cloudrequest.EnrollmentTransport(conn), cloudHost, enrollment.Config(cfg), assetID)
}
