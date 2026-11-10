package commands

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

func TestUnenrollRejectsDirectV2FlagsBeforeCloudOrLiteCleanup(t *testing.T) {
	scoped := cloudDeviceSelector{Endpoint: "cloud.example:443", TenantUUID: liteTestTenant, AssetUUID: liteTestAssetID}.String()
	for _, route := range []struct {
		name, selector          string
		cloudOnly, cloudContext bool
		target                  *SelectedDevice
	}{
		{name: "explicit cloud only", selector: "offline-board", cloudOnly: true},
		{name: "explicit cloud selector", selector: scoped},
		{name: "cloud command", selector: "offline-board", cloudContext: true},
		{name: "selected cloud", target: &SelectedDevice{Selector: scoped}},
		{name: "Lite USB", target: &SelectedDevice{External: &models.ExternalDevice{ProviderKey: "wendy-lite", ConnectionInfo: map[string]string{"type": "USB"}}}},
		{name: "Lite cloud selector", target: &SelectedDevice{Selector: scoped, External: &models.ExternalDevice{ProviderKey: "wendy-lite", ConnectionInfo: map[string]string{"type": "Cloud"}}}},
		{name: "Lite cloud metadata", target: &SelectedDevice{External: &models.ExternalDevice{ProviderKey: "wendy-lite", ConnectionInfo: map[string]string{"type": "Cloud", "cloud": "cloud.example:443", "tenantId": liteTestTenant, "assetId": liteTestAssetID}}}},
	} {
		for _, flag := range []struct{ name, value string }{
			{name: "check", value: "true"},
			{name: "asset-id", value: liteTestAssetID},
			{name: "force", value: "true"},
		} {
			t.Run(route.name+"/"+flag.name, func(t *testing.T) {
				preserveCloudUnenrollmentHooks(t)
				preserveLiteUnenrollmentHooks(t)
				t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
				if err := config.Save(&config.Config{}); err != nil {
					t.Fatal(err)
				}
				deviceFlag = route.selector
				resolved := false
				resolveUnenrollTargetFn = func(context.Context, ...resolveOption) (*SelectedDevice, error) {
					resolved = true
					if route.target == nil {
						t.Fatal("cloud flag validation attempted device resolution")
					}
					return route.target, nil
				}
				resolveCloudUnenrollAuthFn = func(string, string) (*config.AuthConfig, string, error) {
					t.Fatal("unsupported direct-device flag reached cloud cleanup")
					return nil, "", nil
				}
				connectLiteUnenrollmentFn = func(models.ExternalDevice) (liteUnenrollmentDevice, error) {
					t.Fatal("unsupported direct-device flag connected to a Lite board")
					return nil, nil
				}
				cmd := newDeviceUnenrollCmd()
				ctx := context.Background()
				if route.cloudContext {
					ctx = context.WithValue(ctx, cloudDeviceContextKey{}, cloudDeviceConfig{DeviceName: route.selector})
				}
				cmd.SetContext(ctx)
				for name, value := range map[string]string{flag.name: flag.value, "yes": "true"} {
					if err := cmd.Flags().Set(name, value); err != nil {
						t.Fatal(err)
					}
				}
				if route.cloudOnly {
					if err := cmd.Flags().Set("cloud-only", "true"); err != nil {
						t.Fatal(err)
					}
				}
				err := cmd.RunE(cmd, nil)
				if err == nil || !strings.Contains(err.Error(), "--asset-id, --check and --force require a directly connected WendyOS device with v2 UUID enrollment") {
					t.Fatalf("direct-device flag was not rejected: %v", err)
				}
				if resolved != (route.target != nil) {
					t.Fatalf("resolved device = %v, want %v", resolved, route.target != nil)
				}
			})
		}
	}
}

func TestUnenrollResolutionFailureAuthenticatesPriorResetCompletion(t *testing.T) {
	preserveCloudUnenrollmentHooks(t)
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	pin := config.DevicePin{Principal: testUnenrollProgress().Principal, CloudGRPC: testUnenrollProgress().Cloud}
	cfg := &config.Config{DevicePins: map[string]config.DevicePin{"127.0.0.1": pin}}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		calls.Add(1)
		if info.FullMethod != agentpbv2.WendyProvisioningService_IsProvisioned_FullMethodName {
			t.Errorf("unexpected RPC: %s", info.FullMethod)
		}
		return handler(ctx, req)
	}))
	agentpbv2.RegisterWendyProvisioningServiceServer(server, completionProbeServer{})
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	deviceFlag = listener.Addr().String()
	resolveErr := errors.New("device identity refused after reset")
	resolveUnenrollTargetFn = func(context.Context, ...resolveOption) (*SelectedDevice, error) {
		return nil, resolveErr
	}
	cmd := newDeviceUnenrollCmd()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd.SetContext(ctx)
	err = cmd.RunE(cmd, nil)
	if !errors.Is(err, resolveErr) || !strings.Contains(err.Error(), "authenticating prior reset completion") {
		t.Fatalf("resolution failure did not preserve its error and authenticate recovery evidence: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected one public completion status read, got %d", calls.Load())
	}
	after, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.DevicePins, after.DevicePins) {
		t.Fatal("failed completion recovery changed identity pins")
	}
}
