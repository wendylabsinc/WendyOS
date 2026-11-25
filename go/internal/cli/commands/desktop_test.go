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
	"github.com/wendylabsinc/wendy/go/internal/shared/failure"
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
	cfg := &config.Config{CurrentContext: good.Name, Auth: []config.AuthConfig{*other, *good}}
	cfg.SetDevicePin("127.0.0.1", 7, good.CloudGRPC, "42", certs.AssetURN(7, 42))
	setTempConfig(t, cfg)
	setPinCache(t)
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
	cfg, err = config.Load()
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

// A probe marks a row as mTLS from the connection it negotiated, but the row
// keeps the port it was advertised with. While that advertisement still names
// the plaintext port, the agent's mTLS listener is one port above it.
func TestDesktopSecureLANReachesMTLSListenerAbovePlaintextPort(t *testing.T) {
	good, tlsConfig := legacyLiteTestPKI(t, 7, certs.AssetURN(7, 42))
	cfg := &config.Config{CurrentContext: good.Name, Auth: []config.AuthConfig{*good}}
	cfg.SetDevicePin("127.0.0.1", 7, good.CloudGRPC, "42", certs.AssetURN(7, 42))
	setTempConfig(t, cfg)
	setPinCache(t)
	var listener net.Listener
	var plaintextPort int
	for attempt := 0; attempt < 20 && listener == nil; attempt++ {
		first, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		plaintextPort = first.Addr().(*net.TCPAddr).Port
		second, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", plaintextPort+1))
		first.Close() // nothing answers on the plaintext port
		if err == nil {
			listener = second
		}
	}
	if listener == nil {
		t.Fatal("could not reserve two adjacent ports")
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	agentpb.RegisterWendyAgentServiceServer(server, versionOnlyAgent{os: "wendyos"})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	original := plaintextConnectFn
	plaintextConnectFn = func(context.Context, string) (*grpcclient.AgentConnection, error) {
		t.Error("secure target attempted a plaintext connection")
		return nil, errors.New("plaintext is not allowed here")
	}
	t.Cleanup(func() { plaintextConnectFn = original })

	device := desktopNearbyDevice(models.LANDevice{IPAddress: "127.0.0.1", Port: plaintextPort, IsMTLS: true})
	if !device.RequiresMTLS {
		t.Fatal("probed mTLS row does not require mTLS")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	conn, err := DesktopConnectSecureLAN(ctx, device.Selector)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if !conn.IsMTLS {
		t.Fatal("agent answered without mutual TLS")
	}
}

func TestDesktopSecureLANRenewsExpiredSelectedAccount(t *testing.T) {
	for _, offline := range []bool{false, true} {
		t.Run(fmt.Sprintf("offline=%v", offline), func(t *testing.T) {
			resetAutomaticCertReissue(t)
			good, tlsConfig := legacyLiteTestPKI(t, 7, certs.AssetURN(7, 42))
			good.Name = "chosen"
			good.OAuthIssuer = "https://auth.example"
			good.RefreshToken = "original"
			fresh := good.Certificates[0]
			good.Certificates[0].PemCertificate = "expired"
			other, _ := legacyLiteTestPKI(t, 8, certs.AssetURN(8, 42))
			other.Name = "other"
			cfg := &config.Config{CurrentContext: good.Name, Auth: []config.AuthConfig{*other, *good}}
			cfg.SetDevicePin("127.0.0.1", 7, good.CloudGRPC, "42", certs.AssetURN(7, 42))
			setTempConfig(t, cfg)
			setPinCache(t)
			oldExpiry, oldRefresh := leafNotAfterFn, refreshDefaultCloudCertificateFn
			t.Cleanup(func() { leafNotAfterFn, refreshDefaultCloudCertificateFn = oldExpiry, oldRefresh })
			leafNotAfterFn = func(cert string) (time.Time, error) {
				if cert == "expired" {
					return time.Now().Add(-24 * time.Hour), nil
				}
				return time.Now().Add(time.Hour), nil
			}
			networkErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("offline")}
			calls := 0
			refreshDefaultCloudCertificateFn = func(_ context.Context, auth *config.AuthConfig) error {
				calls++
				if auth.Name != good.Name {
					t.Fatalf("renewed wrong account: %s", auth.Name)
				}
				if offline {
					return networkErr
				}
				auth.Certificates[0] = fresh
				auth.RefreshToken = "rotated"
				return nil
			}
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
			if calls != 1 {
				t.Fatalf("refresh calls = %d", calls)
			}
			if offline {
				if !errors.Is(err, networkErr) || conn != nil {
					t.Fatalf("got %v, %v", conn, err)
				}
				if DesktopFailure(err).Remedy == failure.SignIn {
					t.Fatal("offline renewal asked for sign-in")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if !conn.IsMTLS {
				t.Fatal("connection is not authenticated")
			}
			cfg, err = config.Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Auth[1].RefreshToken != "rotated" || cfg.Auth[1].Certificates[0].PemCertificate != fresh.PemCertificate {
				t.Fatal("renewed session was not persisted")
			}
			if cfg.Auth[0].Certificates[0].PemCertificate != other.Certificates[0].PemCertificate {
				t.Fatal("changed unrelated account")
			}
		})
	}
}
