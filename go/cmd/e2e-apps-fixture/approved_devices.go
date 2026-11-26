package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudsession"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
)

// Bind approved LAN routes to identities read from the authenticated Cloud.
// This updates public selector metadata in the normal config only. No credential
// is exported, copied, resolved for serialization or written to a fixture.
func pinApprovedDevices(o options) (any, error) {
	if os.Getenv("WENDY_E2E_APPS_DEPLOYMENTS") != "true" || !strings.Contains(o.cloud, ".dev.") || !strings.HasSuffix(o.cloud, ":443") || os.Getenv("WENDY_CLOUD_INSECURE") != "" {
		return nil, errors.New("explicit Dev fixture required")
	}
	a, err := cloudsession.LoadAccount(o.account, o.tenant)
	if err != nil || a.CloudGRPC != o.cloud {
		return nil, errors.New("selected saved Dev account unavailable")
	}
	data, err := os.ReadFile(o.devices)
	if err != nil {
		return nil, errors.New("approved device resource file unavailable")
	}
	var devices []fleetDevice
	if json.Unmarshal(data, &devices) != nil || len(devices) == 0 {
		return nil, errors.New("approved devices required")
	}
	pins := map[string]config.DevicePin{}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	err = cloudsession.With(ctx, a, heldToken, func(ctx context.Context, c grpc.ClientConnInterface) error {
		for _, d := range devices {
			asset, err := pb.NewAssetServiceClient(c).GetAsset(ctx, &pb.GetAssetRequest{Id: d.Asset})
			if err != nil {
				return err
			}
			if asset.Id != d.Asset || asset.OrganizationId != o.tenant || asset.GetPkiDeviceName() != d.Asset || d.Host == "" {
				return errors.New("approved asset is not enrolled in selected tenant")
			}
			pins[d.Host] = config.DevicePin{CloudGRPC: o.cloud, AssetID: d.Asset, Principal: "spiffe://wendy.sh/tenant/" + o.tenant + "/device/" + d.Asset, Source: config.PinSourceCloud}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = config.Update(func(cfg *config.Config) (bool, error) {
		// Validate all old constraints before making any update. Complete an
		// incomplete same-Cloud pin, but never replace a different identity.
		for host, want := range pins {
			if old, ok := cfg.DevicePinFor(host); ok && (old.CloudGRPC != want.CloudGRPC || old.AssetID != "" && old.AssetID != want.AssetID || old.Principal != "" && old.Principal != want.Principal || old.OrgID != 0) {
				return false, errors.New("existing device pin conflicts with approved Cloud identity")
			}
		}
		changed := false
		for host, want := range pins {
			if old, ok := cfg.DevicePinFor(host); !ok || old != want {
				cfg.SetDevicePinFrom(host, 0, want.CloudGRPC, want.AssetID, want.Principal, want.Source)
				changed = true
			}
		}
		return changed, nil
	})
	return map[string]any{"verified": err == nil, "devices": devices, "tenant": o.tenant}, err
}

// The normal CLI's device-info JSON omits GetAgentVersion.hostname. Observe it
// through the existing authenticated client, with both expected identity and
// the normal SPKI store enforced. This is read-only and exports no credentials.
func approvedDeviceProfile(a *config.AuthConfig, o options) (any, error) {
	raw, err := os.ReadFile(os.Getenv("WENDY_E2E_APPS_DEVICES_PATH"))
	if err != nil {
		return nil, errors.New("approved device resource file required")
	}
	var devices []fleetDevice
	if json.Unmarshal(raw, &devices) != nil {
		return nil, errors.New("invalid device resource file")
	}
	var host string
	for _, d := range devices {
		if d.Asset == o.asset {
			if host != "" {
				return nil, errors.New("ambiguous approved asset")
			}
			host = d.Host
		}
	}
	if host == "" {
		return nil, errors.New("asset outside approved fixture")
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	pin, ok := cfg.DevicePinFor(host)
	principal := "spiffe://wendy.sh/tenant/" + o.tenant + "/device/" + o.asset
	if !ok || pin.AssetID != o.asset || pin.CloudGRPC != o.cloud || pin.Principal != principal {
		return nil, errors.New("exact saved device binding required")
	}
	expected, err := certs.ParsePrincipal(principal)
	if err != nil {
		return nil, err
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return nil, err
	}
	pins, err := devicepin.Open(dir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := grpcclient.ConnectWithTLSExpecting(ctx, net.JoinHostPort(host, "50052"), &a.Certificates[0], pins, &expected)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	version, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return nil, err
	}
	identity, ok := conn.ObservedServerIdentity()
	if !ok || identity.Principal != principal {
		return nil, errors.New("exact signed device identity not observed")
	}
	return map[string]any{"hostname": version.Hostname, "principal": identity.Principal, "os": version.Os, "architecture": version.CpuArchitecture}, nil
}
