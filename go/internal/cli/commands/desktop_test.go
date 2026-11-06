package commands

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func TestDesktopSecureLANRequiresAuthenticationWithoutTryingPlaintext(t *testing.T) {
	device := models.LANDevice{IPAddress: "127.0.0.1", Port: 50052, IsMTLS: true}
	secure := desktopNearbyDevice(device)
	device.IsMTLS = false
	if !secure.RequiresMTLS || desktopNearbyDevice(device).RequiresMTLS {
		t.Fatal("discovered transport requirement lost")
	}
	original := plaintextConnectFn
	plaintextConnectFn = func(context.Context, string) (*grpcclient.AgentConnection, error) {
		t.Fatal("secure target attempted a plaintext connection")
		return nil, nil
	}
	t.Cleanup(func() { plaintextConnectFn = original })
	for _, tt := range []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"signed out", &config.Config{}, "sign in to Wendy Cloud to connect to this enrolled device"},
		{"account without operator certificate", &config.Config{Auth: []config.AuthConfig{{Name: "test", RefreshToken: "test-token"}}}, "this account has no device connection certificate"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setTempConfig(t, tt.cfg)
			if _, err := DesktopConnectSecureLAN(context.Background(), secure.Selector); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want %q", err, tt.want)
			}
		})
	}
	t.Run("unreachable with current operator credentials", func(t *testing.T) {
		auth, _ := legacyLiteTestPKI(t, 7, certs.AssetURN(7, 42))
		setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		if conn, err := DesktopConnectSecureLAN(ctx, deadAgentAddr(t)); err == nil || conn != nil {
			t.Fatal("secure target unexpectedly connected")
		}
	})
}

func TestDesktopSecureLANAuthenticatesSelectedAccountAtAdvertisedPort(t *testing.T) {
	good, tlsConfig := legacyLiteTestPKI(t, 7, certs.AssetURN(7, 42))
	other, _ := legacyLiteTestPKI(t, 8, certs.AssetURN(8, 42))
	setTempConfig(t, &config.Config{CurrentContext: good.Name, Auth: []config.AuthConfig{*other, *good}})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	agentpb.RegisterWendyAgentServiceServer(server, versionOnlyAgent{os: "wendyos"})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	conn, err := DesktopConnectSecureLAN(ctx, listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if !conn.IsMTLS {
		t.Fatal("agent answered without mutual TLS")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.CurrentContext = other.Name
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if wrong, err := DesktopConnectSecureLAN(ctx, listener.Addr().String()); err == nil || wrong != nil {
		t.Fatal("used another account's certificate after switching accounts")
	}
}

func TestDesktopPinRecoveryRequiresTypedRefusal(t *testing.T) {
	refusal := refuseDevicePin(devicePinDiagnostic{hostname: "ci-iq8.local", heading: "identity changed"})
	if got := DesktopPinRecovery(fmt.Errorf("connect: %w", refusal)); got != "ci-iq8.local" {
		t.Fatal(got)
	}
	mismatch := spkiRefusal("ci-iq8.local", &devicepin.PinMismatchError{Key: "urn:wendy:org:77:asset:537"})
	if got := DesktopPinRecovery(mismatch); got != "urn:wendy:org:77:asset:537" {
		t.Fatal(got)
	}
	for _, err := range []error{nil, errors.New(refusal.Error()), refuseIdentity("unrelated refusal"), errors.New("no route to host")} {
		if got := DesktopPinRecovery(err); got != "" {
			t.Fatalf("unexpected recovery: %q", got)
		}
	}
}

func TestDesktopUnpinClearsOnlySelectedDevice(t *testing.T) {
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"ci-iq8": {OrgID: 77, CloudGRPC: "grpc.a.sh:443", AssetID: "537"},
		"other":  {OrgID: 77, CloudGRPC: "grpc.a.sh:443", AssetID: "538"},
	})
	seedSPKIPins(t, "urn:wendy:org:77:asset:537", "urn:wendy:org:77:asset:538")
	if err := DesktopUnpin("ci-iq8.local"); err != nil {
		t.Fatal(err)
	}
	if _, ok := readPins()["ci-iq8"]; ok {
		t.Fatal("selected pin survived")
	}
	if _, ok := readPins()["other"]; !ok {
		t.Fatal("unrelated pin removed")
	}
	keys := readSPKIPins(t)
	if _, ok := keys["urn:wendy:org:77:asset:537"]; ok {
		t.Fatal("selected key survived")
	}
	if _, ok := keys["urn:wendy:org:77:asset:538"]; !ok {
		t.Fatal("unrelated key removed")
	}
}
