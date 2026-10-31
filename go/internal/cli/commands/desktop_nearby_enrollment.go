package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteenroll"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Kept in Go memory for this setup attempt. Never returned through the UI ABI.
type DesktopNearbyCredential struct {
	Request                                    *pb.SetupEnrollCommand
	DeviceID, Principal, Account, Organization string
	Lite                                       bool
	RequestedAt                                time.Time
}

func NearbyEnrollmentNameValid(name string) bool {
	if len(name) < 1 || len(name) > 63 || name[0] < 'a' || name[0] > 'z' || strings.HasSuffix(name, "-") {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// beforeMint is called only at the first potentially mutating Cloud request.
// The caller must retain that fact even when the response is lost.
func DesktopPrepareNearbyEnrollment(ctx context.Context, account, organization, name string, state *pb.SetupStatusResponse, beforeMint func()) (*DesktopNearbyCredential, error) {
	if !NearbyEnrollmentNameValid(name) {
		return nil, errors.New("Use a name with lowercase letters, numbers and hyphens")
	}
	if state == nil || state.GetEnrolled() || !(state.GetDeviceClass() == "C" && state.GetLiteSupported() || state.GetDeviceClass() != "C" && state.GetAcmeSupported()) {
		return nil, errors.New("Device is not available for enrollment")
	}
	if account == "" || organization == "" {
		return nil, errors.New("Choose an organization first")
	}
	auth, err := desktopCloudAuth(account, organization)
	if err != nil {
		return nil, err
	}
	if err = ensureFreshCertificateFn(ctx, auth); err != nil {
		return nil, errors.New("Sign in to the selected organization again")
	}
	if err = validateEnrollmentCertificate(auth); err != nil {
		return nil, err
	}
	out := &DesktopNearbyCredential{DeviceID: uuid.NewString(), Account: account, Organization: organization, Lite: state.GetDeviceClass() == "C", RequestedAt: time.Now()}
	cloud, err := dialCloudGRPC(auth)
	if err != nil {
		return nil, err
	}
	defer cloud.Close()
	cloudCtx, err := cloudContext(ctx, auth)
	if err != nil {
		return nil, err
	}
	if out.Lite {
		limit := int(state.GetMaxConfigSize())
		if limit < 1 || limit > 60000 {
			return nil, errors.New("Unsupported Lite setup configuration limit")
		}
		cfg, err := liteenroll.Config(auth, out.DeviceID, "", "", "", 5055)
		if err != nil {
			return nil, err
		}
		client, err := liteenroll.DiscoverTrust(ctx, cfg, "")
		if err != nil {
			return nil, fmt.Errorf("Prepare Lite trust: %w", err)
		}
		defer client.CloseIdleConnections()
		cfg.SignedTime, err = liteenroll.SignedTime(ctx, client, cfg.TimeUrl, state.GetNonce())
		if err != nil {
			return nil, fmt.Errorf("Prepare Lite time: %w", err)
		}
		conf := &litepb.WendyConf{DeviceName: proto.String(name), Enrollment: cfg}
		cfg.Token = strings.Repeat("x", 512)
		size := proto.Size(conf)
		cfg.Token = ""
		if size > limit {
			return nil, errors.New("Enrollment configuration exceeds the device limit")
		}
		beforeMint()
		mintCtx, cancel := context.WithTimeout(cloudCtx, 30*time.Second)
		defer cancel()
		_, err = liteenroll.Mint(mintCtx, cloud, auth, cfg, name, &cloudpb.DeviceHardware{SerialNumber: proto.String(state.GetHardwareId())}, liteenroll.TrustProfile{})
		out.DeviceID = cfg.DeviceId
		if err != nil {
			return nearbyEnrollmentFailure(out, err)
		}
		defer func() { cfg.Token = "" }()
		raw, err := proto.Marshal(conf)
		if err != nil {
			return nil, err
		}
		out.Request = &pb.SetupEnrollCommand{LiteConfig: raw}
	} else {
		cfg, err := cloudenroll.EnrollmentConfig(auth, out.DeviceID, "")
		if err != nil {
			return nil, err
		}
		beforeMint()
		mintCtx, cancel := context.WithTimeout(cloudCtx, 30*time.Second)
		defer cancel()
		cfg, _, err = cloudenroll.MintEAB(mintCtx, cloud, auth, cfg, name, nil)
		out.DeviceID = cfg.DeviceID
		if err != nil {
			return nearbyEnrollmentFailure(out, err)
		}
		out.Request = &pb.SetupEnrollCommand{CloudHost: auth.CloudGRPC, DirectoryUrl: cfg.DirectoryURL, DeviceId: cfg.DeviceID, EabKeyId: cfg.EABKeyID, EabHmacKey: cfg.EABHMACKey}
	}
	out.Principal = "spiffe://wendy.sh/tenant/" + organization + "/device/" + out.DeviceID
	return out, nil
}

func DesktopCheckNearbyEnrollment(ctx context.Context, credential *DesktopNearbyCredential) (bool, error) {
	auth, err := desktopCloudAuth(credential.Account, credential.Organization)
	if err != nil {
		return false, err
	}
	devices, err := fetchCloudAssetsV2(ctx, auth, true)
	if err != nil {
		return false, err
	}
	for _, device := range devices {
		if nearbyEnrollmentPresent(device, credential) {
			return true, nil
		}
	}
	return false, nil
}
func nearbyEnrollmentPresent(device *cloudpb.Asset, c *DesktopNearbyCredential) bool {
	if device == nil || c == nil {
		return false
	}
	stamp := device.GetLastHeartbeat()
	return device.GetId() == c.DeviceID && device.GetOrganizationId() == c.Organization &&
		(!c.Lite || device.GetOsType() == "wendy-lite") && stamp != nil && stamp.IsValid() &&
		!stamp.AsTime().Before(c.RequestedAt.Add(-5*time.Second)) &&
		!stamp.AsTime().Before(time.Now().Add(-2*time.Minute)) && stamp.AsTime().Before(time.Now().Add(time.Minute)) &&
		(device.GetPkiDeviceName() == "" || device.GetPkiDeviceName() == c.DeviceID)
}

// NearbyEnrollmentRejectedError means Cloud refused before issuing a credential.
// Only these replies permit another explicit enrollment attempt.
type NearbyEnrollmentRejectedError struct{ Message string }

func (e *NearbyEnrollmentRejectedError) Error() string { return e.Message }

func nearbyEnrollmentFailure(c *DesktopNearbyCredential, err error) (*DesktopNearbyCredential, error) {
	var message string
	switch status.Code(err) {
	case codes.AlreadyExists:
		message = "That device name is already in use. Choose another name."
	case codes.InvalidArgument:
		message = "Cloud rejected the enrollment details. Check the device name and organization."
	case codes.Unauthenticated:
		message = "Sign in to the selected organization again."
	case codes.PermissionDenied:
		message = "Your account does not have permission to add devices to this organization."
	case codes.FailedPrecondition, codes.NotFound, codes.Unimplemented:
		message = "Enrollment is not available for this organization. Contact your administrator."
	}
	if message != "" {
		return nil, &NearbyEnrollmentRejectedError{Message: message}
	}
	// Retain only the identity needed for read-only reconciliation. Never repeat
	// a mint after a timeout or a lost reply, and never expose remote error text
	// which may contain credential material.
	c.Request = nil
	return c, fmt.Errorf("Cloud enrollment reply was not received (%s). Checking the device automatically", status.Code(err))
}
