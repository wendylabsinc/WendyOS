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
		if err != nil {
			return nil, errors.New("Cloud enrollment could not be confirmed. Check the selected organization's device list before trying again")
		}
		defer func() { cfg.Token = "" }()
		out.DeviceID = cfg.DeviceId
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
		if err != nil {
			return nil, errors.New("Cloud enrollment could not be confirmed. Check the selected organization's device list before trying again")
		}
		out.DeviceID = cfg.DeviceID
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
