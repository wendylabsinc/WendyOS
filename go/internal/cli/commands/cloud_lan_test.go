package commands

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func TestCLICloudDevicePrefersLANWithoutNativeSession(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		name := "legacy"
		if v2 {
			name = "v2"
		}
		t.Run(name, func(t *testing.T) {
			auth, asset := desktopConnectionTestSetup(t)
			if v2 {
				pkiName := "enrolled-camera"
				a := &cloudpbv2.Asset{Id: "asset-id", Name: "camera", PkiDeviceName: &pkiName}
				asset = cloudDiscoveryDevice{cloudAssetMetadata: a, v2: a, key: a.GetId()}
			}
			wantIdentity := cloudDiscoveryIdentity(auth, asset)
			desktopLocalCandidates = func(context.Context, cloudDiscoveryDevice, certs.WendyIdentity) []string {
				return []string{"192.168.1.42:50052"}
			}
			var dials, closes atomic.Int32
			desktopLocalDial = func(_ context.Context, address string, _ config.CertificateInfo, expected certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
				dials.Add(1)
				if !expected.SameEntity(wantIdentity) {
					t.Errorf("LAN dial identity = %+v, want %+v", expected, wantIdentity)
				}
				return &grpcclient.AgentConnection{Addr: address, IsMTLS: true, AgentService: &desktopConnectionVersionClient{}, ExtraClosers: []io.Closer{desktopConnectionCloser{&closes}}}, nil
			}
			desktopCloudDial = func(context.Context, *config.AuthConfig, cloudDiscoveryDevice, string) (*grpcclient.AgentConnection, error) {
				t.Error("reachable LAN device opened a Cloud tunnel")
				return nil, errors.New("unexpected Cloud dial")
			}
			stdout, stderr := captureBoth(t, func() {
				// CLI commands have no TunnelSession. Closing their connection
				// must release it, and restart recovery must retain the identity.
				conn, err := asset.connect(context.Background(), auth, "")
				if err != nil {
					t.Error(err)
					return
				}
				conn.Close()
				if closes.Load() != 1 {
					t.Error("CLI connection was retained after Close")
				}
				reconnected, err := conn.Reconnect(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				reconnected.Close()
			})
			if dials.Load() != 2 || closes.Load() != 2 {
				t.Fatalf("dials = %d, closes = %d; want two independent connections", dials.Load(), closes.Load())
			}
			if stdout != "" || strings.Contains(stderr, "cloud tunnel") || !strings.Contains(stderr, "via LAN (192.168.1.42:50052)") {
				t.Fatalf("incorrect route output: stdout=%q stderr=%q", stdout, stderr)
			}
		})
	}
}

func TestCLICloudDeviceFallsBackAfterLANFailure(t *testing.T) {
	auth, asset := desktopConnectionTestSetup(t)
	desktopLocalCandidates = func(context.Context, cloudDiscoveryDevice, certs.WendyIdentity) []string {
		return []string{"192.168.1.42:50052"}
	}
	var localDials, cloudDials atomic.Int32
	desktopLocalDial = func(context.Context, string, config.CertificateInfo, certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		localDials.Add(1)
		return nil, errors.New("LAN device could not be authenticated")
	}
	client := &desktopConnectionVersionClient{}
	desktopCloudDial = func(context.Context, *config.AuthConfig, cloudDiscoveryDevice, string) (*grpcclient.AgentConnection, error) {
		cloudDials.Add(1)
		if localDials.Load() != 1 {
			t.Error("Cloud dial preceded the LAN attempt")
		}
		return &grpcclient.AgentConnection{AgentService: client, IsMTLS: true}, nil
	}
	stdout, stderr := captureBoth(t, func() {
		conn, err := asset.connect(context.Background(), auth, "")
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	})
	if cloudDials.Load() != 1 || client.probes.Load() != 0 {
		t.Fatalf("Cloud dials = %d, probes = %d; want one lazy CLI fallback", cloudDials.Load(), client.probes.Load())
	}
	if stdout != "" || strings.Contains(stderr, "via LAN") || strings.Count(stderr, "via cloud tunnel") != 1 {
		t.Fatalf("incorrect fallback output: stdout=%q stderr=%q", stdout, stderr)
	}
}
