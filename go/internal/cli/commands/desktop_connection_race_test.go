package commands

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

func setCloudTunnelHeadStart(t *testing.T, d time.Duration) {
	t.Helper()
	previous := cloudTunnelHeadStart
	cloudTunnelHeadStart = d
	t.Cleanup(func() { cloudTunnelHeadStart = previous })
}

func raceTestContext(t *testing.T, native bool) context.Context {
	t.Helper()
	if !native {
		return context.Background()
	}
	var session TunnelSession
	session.SetActive(true)
	t.Cleanup(func() { session.SetActive(false) })
	return session.Context(context.Background())
}

func raceRouteName(native bool) string {
	if native {
		return "native"
	}
	return "CLI"
}

// testTunnelChannel is a lazy channel whose transport dials with dial.
func testTunnelChannel(t *testing.T, dial func(context.Context, string) (net.Conn, error)) *grpc.ClientConn {
	t.Helper()
	cc, err := grpc.NewClient("passthrough:///cloud-tunnel", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(dial))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

type blockingVersionClient struct {
	agentpb.WendyAgentServiceClient
}

func (blockingVersionClient) GetAgentVersion(ctx context.Context, _ *agentpb.GetAgentVersionRequest, _ ...grpc.CallOption) (*agentpb.GetAgentVersionResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestCloudTunnelJoinsStalledLANAfterHeadStart(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(raceRouteName(native), func(t *testing.T) {
			auth, asset := desktopConnectionTestSetup(t)
			setCloudTunnelHeadStart(t, 10*time.Millisecond)
			desktopLocalCandidates = func(context.Context, cloudDiscoveryDevice, certs.WendyIdentity) []string {
				// Cloud's private ip_address for a device on another network.
				return []string{"10.9.8.7:50052"}
			}
			lanCanceled := make(chan struct{})
			desktopLocalDial = func(ctx context.Context, _ string, _ config.CertificateInfo, _ certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
				<-ctx.Done()
				close(lanCanceled)
				return nil, ctx.Err()
			}
			client := &desktopConnectionVersionClient{}
			desktopCloudDial = func(context.Context, *config.AuthConfig, cloudDiscoveryDevice, string) (*grpcclient.AgentConnection, error) {
				return &grpcclient.AgentConnection{AgentService: client, IsMTLS: true}, nil
			}
			ctx := raceTestContext(t, native)
			started := time.Now()
			var conn *grpcclient.AgentConnection
			var err error
			_, stderr := captureBoth(t, func() { conn, err = connectCloudDiscoveryDevice(ctx, auth, asset, "") })
			elapsed := time.Since(started)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if conn.AgentService != client || elapsed >= cloudLANBudget/2 {
				t.Fatalf("got tunnel=%v after %v; want the tunnel well before the LAN budget", conn.AgentService == client, elapsed)
			}
			select {
			case <-lanCanceled:
			case <-time.After(time.Second):
				t.Fatal("stalled LAN attempt was not cancelled")
			}
			wantProbes := int32(0)
			if native {
				wantProbes = 1
			}
			if client.probes.Load() != wantProbes || !strings.Contains(stderr, "via cloud tunnel") || strings.Contains(stderr, "via LAN") {
				t.Fatalf("probes=%d, stderr=%q", client.probes.Load(), stderr)
			}
		})
	}
}

func TestCloudLANWinsAfterTunnelJoins(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(raceRouteName(native), func(t *testing.T) {
			auth, asset := desktopConnectionTestSetup(t)
			setCloudTunnelHeadStart(t, 10*time.Millisecond)
			desktopLocalCandidates = func(context.Context, cloudDiscoveryDevice, certs.WendyIdentity) []string {
				return []string{"192.168.1.42:50052"}
			}
			tunnelStarted := make(chan struct{})
			desktopLocalDial = func(ctx context.Context, address string, _ config.CertificateInfo, _ certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
				// A slow LAN handshake completes while the tunnel is still being set up.
				select {
				case <-tunnelStarted:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return &grpcclient.AgentConnection{Addr: address, IsMTLS: true, AgentService: &desktopConnectionVersionClient{}}, nil
			}
			var tunnelCloses atomic.Int32
			desktopCloudDial = func(context.Context, *config.AuthConfig, cloudDiscoveryDevice, string) (*grpcclient.AgentConnection, error) {
				close(tunnelStarted)
				conn := &grpcclient.AgentConnection{IsMTLS: true, ExtraClosers: []io.Closer{desktopConnectionCloser{&tunnelCloses}}}
				if native {
					conn.AgentService = blockingVersionClient{}
				} else {
					conn.AgentService = &desktopConnectionVersionClient{}
					conn.Conn = testTunnelChannel(t, func(ctx context.Context, _ string) (net.Conn, error) {
						<-ctx.Done()
						return nil, ctx.Err()
					})
				}
				return conn, nil
			}
			ctx := raceTestContext(t, native)
			var conn *grpcclient.AgentConnection
			var err error
			_, stderr := captureBoth(t, func() { conn, err = connectCloudDiscoveryDevice(ctx, auth, asset, "") })
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if conn.Addr != "192.168.1.42:50052" || tunnelCloses.Load() != 1 {
				t.Fatalf("route=%q, tunnel closes=%d; want LAN and the unused tunnel closed once", conn.Addr, tunnelCloses.Load())
			}
			if !strings.Contains(stderr, "via cloud tunnel") || !strings.Contains(stderr, "via LAN (192.168.1.42:50052)") {
				t.Fatalf("stderr=%q", stderr)
			}
		})
	}
}

func TestCLICloudTunnelNotReadyStaysLazyUntilLANFails(t *testing.T) {
	auth, asset := desktopConnectionTestSetup(t)
	setCloudTunnelHeadStart(t, 10*time.Millisecond)
	desktopLocalCandidates = func(context.Context, cloudDiscoveryDevice, certs.WendyIdentity) []string {
		return []string{"192.168.1.42:50052"}
	}
	relayErr := errors.New("relay refused the tunnel")
	cc := testTunnelChannel(t, func(context.Context, string) (net.Conn, error) { return nil, relayErr })
	tunnelDialed := make(chan struct{})
	desktopLocalDial = func(ctx context.Context, _ string, _ config.CertificateInfo, _ certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		select {
		case <-tunnelDialed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		// Fail only after the tunnel's first attempt has failed too.
		for state := cc.GetState(); state != connectivity.TransientFailure; state = cc.GetState() {
			if !cc.WaitForStateChange(ctx, state) {
				return nil, ctx.Err()
			}
		}
		return nil, errors.New("LAN device could not be authenticated")
	}
	client := &desktopConnectionVersionClient{}
	desktopCloudDial = func(context.Context, *config.AuthConfig, cloudDiscoveryDevice, string) (*grpcclient.AgentConnection, error) {
		close(tunnelDialed)
		return &grpcclient.AgentConnection{Conn: cc, AgentService: client, IsMTLS: true}, nil
	}
	var conn *grpcclient.AgentConnection
	var err error
	captureBoth(t, func() { conn, err = asset.connect(context.Background(), auth, "") })
	if err != nil || conn == nil || conn.Conn != cc || client.probes.Load() != 0 {
		t.Fatalf("connection = %v, %v; probes=%d; want the lazy tunnel without an extra call", conn, err, client.probes.Load())
	}
	conn.Close()
}

func TestCloudLANCandidatesAddBrowseSightingsToCloudHint(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	auth, _ := desktopConnectionTestSetup(t)
	hint := "10.1.2.3"
	a := &cloudpb.Asset{Id: 42, Name: "camera", IpAddress: &hint}
	asset := cloudDiscoveryDevice{cloudAssetMetadata: a, legacy: a, key: "42"}
	// The browse is still running. Its DHCP-renewed sighting is used without
	// waiting for the browse to finish.
	browse := &cloudLANBrowse{done: make(chan struct{}), updated: make(chan struct{}, 1), devices: []models.LANDevice{
		{ID: "other-device", IsMTLS: true, IPAddress: "192.168.100.51", Port: 50052},
		{ID: "42", IsMTLS: true, IPAddress: "192.168.100.60", Port: 50052},
	}}
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), cloudLANBrowseKey{}, browse), time.Second)
	defer cancel()
	got := localCloudCandidates(ctx, asset, cloudDiscoveryIdentity(auth, asset))
	want := []string{"10.1.2.3:50052", "192.168.100.60:50052", "192.168.100.60:50053"}
	if !reflect.DeepEqual(got, want) || ctx.Err() != nil {
		t.Fatalf("candidates = %v (ctx err %v), want %v without waiting", got, ctx.Err(), want)
	}
}
