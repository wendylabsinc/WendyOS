package commands

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
)

// allOfflineV2Server enrolls devices and reports none of them online.
type allOfflineV2Server struct{ discoveryV2Server }

func (s *allOfflineV2Server) ListAssets(req *cloudpbv2.ListAssetsRequest, stream grpc.ServerStreamingServer[cloudpbv2.ListAssetsResponse]) error {
	if req.GetOnlineOnly() {
		return nil
	}
	return s.discoveryV2Server.ListAssets(req, stream)
}

func allOfflineV2Auth(t *testing.T, count int32) *config.AuthConfig {
	t.Helper()
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	cloudpbv2.RegisterAssetServiceServer(srv, &allOfflineV2Server{discoveryV2Server{t: t, count: count, organization: testOperatorTenant}})
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = lis.Addr().String()
	auth.Certificates[0].OrganizationID = 0
	return auth
}

// An enrolled device that Cloud reports offline must come with steps the user
// can act on, on both Cloud generations: once in the text a person reads, and
// as next_steps for JSON mode (WDY-4295). No command reads "presence logs".
func TestCloudOfflineErrorsCarryActionableNextSteps(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const listStep = "List all enrolled devices with 'wendy cloud discover --all --json'."
	legacyRoster := func() ([]*cloudpb.Asset, error) {
		return []*cloudpb.Asset{cloudAssetFixture(41, "offline-device"), cloudAssetFixture(42, "b"), cloudAssetFixture(43, "c")}, nil
	}
	for _, tc := range []struct {
		name     string
		err      func(t *testing.T) error
		headline string
		steps    []string
	}{
		{
			name: "v2 named device",
			err: func(t *testing.T) error {
				_, err := pickCloudDiscoveryDevice(ctx, discoveryV2Auth(t, 1, false), "offline-device", "")
				return err
			},
			headline: `device "offline-device" is enrolled but currently reported offline`,
			steps:    []string{"Check the device's power and network connection, then retry.", listStep},
		},
		{
			name: "v2 every device",
			err: func(t *testing.T) error {
				_, err := pickCloudDiscoveryDevice(ctx, allOfflineV2Auth(t, 3), "", "")
				return err
			},
			headline: "all 3 enrolled devices are currently reported offline",
			steps:    []string{"Check their power and network connections, then retry.", listStep},
		},
		{
			name: "legacy named device",
			err: func(*testing.T) error {
				return upgradeOfflineResolveErr(&errCloudDeviceNotFound{name: "offline-device"}, "offline-device", legacyRoster)
			},
			headline: `device "offline-device" is enrolled but currently reported offline`,
			steps:    []string{"Check the device's power and network connection, then retry.", listStep},
		},
		{
			name: "legacy every device",
			err: func(*testing.T) error {
				return upgradeOfflineResolveErr(errNoCloudDevicesEnrolled, "", legacyRoster)
			},
			headline: "all 3 enrolled devices are currently reported offline",
			steps:    []string{"Check their power and network connections, then retry.", listStep},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.err(t)
			if !errors.Is(err, errDeviceOffline) || ErrorClass(err) != "device_offline" || ExecutionErrorClass(err) != "device_offline" {
				t.Fatalf("class = %q, want device_offline; error = %v", ErrorClass(err), err)
			}
			if strings.Contains(err.Error(), "presence logs") {
				t.Errorf("error sends the user to logs no command reads: %q", err.Error())
			}
			if got := NextSteps(err); !reflect.DeepEqual(got, tc.steps) {
				t.Errorf("NextSteps = %q, want %q", got, tc.steps)
			}
			// What a person reads: the headline, then each step once.
			if want := tc.headline + "\n  " + strings.Join(tc.steps, "\n  "); err.Error() != want {
				t.Errorf("Error() = %q, want %q", err.Error(), want)
			}
			// What JSON mode keeps as the message once the steps are listed.
			if got := messageWithoutNextSteps(err); got != tc.headline {
				t.Errorf("message without steps = %q, want %q", got, tc.headline)
			}
		})
	}
}
