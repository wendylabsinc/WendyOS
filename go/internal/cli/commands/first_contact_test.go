package commands

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// serveTLSAgent serves a TLS agent with serverTLS and returns its address.
func serveTLSAgent(t *testing.T, serverTLS *tls.Config) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	agentpb.RegisterWendyAgentServiceServer(server, &selectionPowerModeProbe{})
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

func signedInConfig(t *testing.T) {
	t.Helper()
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{{Certificates: []config.CertificateInfo{selfSignedCLICert(t, 2)}}}})
	setPinCache(t)
}

// A signed-in CLI with no saved identity reaches a freshly installed device:
// `wendy device info --device <addr>` resolves its target this way.
func TestSignedInDeviceInfoReachesUnenrolledDevice(t *testing.T) {
	restoreDeviceGlobals(t)
	jsonOutput = false
	signedInConfig(t)
	deviceFlag = serveSetupProbe(t, "unprovisioned")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	target, err := resolveTarget(ctx, IncludeBluetooth(), DisableSessionBroker(), SuppressUpdateCheck())
	if err != nil {
		t.Fatalf("signed-in CLI refused an unenrolled device: %v", err)
	}
	defer target.Close()
	if target.Agent == nil || target.Agent.IsMTLS || !target.Agent.NoSavedIdentity {
		t.Fatalf("want an unverified plaintext connection, got %+v", target.Agent)
	}
	if _, err := target.Agent.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
		t.Fatal(err)
	}
}

// An enrolled device with no saved identity is dialled with the operator's
// certificate; the device decides, and its refusal is reported plainly.
func TestFirstContactWithEnrolledDevice(t *testing.T) {
	t.Run("device accepts", func(t *testing.T) {
		auth, serverTLS := legacyLiteTestPKI(t, 2, certs.AssetURN(2, 606))
		setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
		setPinCache(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, _, err := dialAgentLadderWithCerts(ctx, newDialTarget("robot.local", serveTLSAgent(t, serverTLS)), auth.Certificates)
		if err != nil {
			t.Fatalf("first contact with an enrolled device: %v", err)
		}
		defer conn.Close()
		if !conn.IsMTLS || !conn.NoSavedIdentity {
			t.Fatalf("want an mTLS first-contact connection, got IsMTLS=%v NoSavedIdentity=%v", conn.IsMTLS, conn.NoSavedIdentity)
		}
	})
	t.Run("device refuses the user", func(t *testing.T) {
		auth, serverTLS := legacyLiteTestPKI(t, 2, certs.AssetURN(2, 606))
		setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
		setPinCache(t)
		serverTLS.ClientCAs = x509.NewCertPool() // trusts no operator
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, _, err := dialAgentLadderWithCerts(ctx, newDialTarget("robot.local", serveTLSAgent(t, serverTLS)), auth.Certificates)
		if conn != nil {
			conn.Close()
			t.Fatal("connected although the device refused the user")
		}
		want := "Device \"robot.local\" is enrolled and refused your sign-in certificate.\n  " + deviceRefusedUserStep
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v (cause %v), want %q", err, errors.Unwrap(err), want)
		}
		if !errors.Is(err, errTLSHandshakeRejected) {
			t.Fatalf("refusal lost its rejection type: %v", err)
		}
	})
}

// changedIdentityFixture is a device saved as asset 606 that now presents 607.
func changedIdentityFixture(t *testing.T) {
	t.Helper()
	restoreDeviceGlobals(t)
	jsonOutput = false
	auth, serverTLS := legacyLiteTestPKI(t, 2, certs.AssetURN(2, 607))
	auth.CloudGRPC = "cloud.test:443"
	cfg := &config.Config{Auth: []config.AuthConfig{*auth}}
	cfg.SetDevicePin("127.0.0.1", 2, auth.CloudGRPC, "606", "")
	setTempConfig(t, cfg)
	setPinCache(t)
	deviceFlag = serveTLSAgent(t, serverTLS)
	origDiscover := discoverLANDevices
	discoverLANDevices = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
	origConfirm := confirmChangedIdentityFn
	t.Cleanup(func() { discoverLANDevices, confirmChangedIdentityFn = origDiscover, origConfirm })
}

func savedAsset(t *testing.T) string {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	pin, _ := cfg.DevicePinFor("127.0.0.1")
	return pin.AssetID
}

func TestChangedDeviceIdentity(t *testing.T) {
	t.Run("without a terminal it refuses with the next step", func(t *testing.T) {
		changedIdentityFixture(t)
		confirmChangedIdentityFn = func() (bool, error) { t.Fatal("prompted without a terminal"); return false, nil }
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := connectToAgent(ctx, SuppressProvisioningHint(), SuppressUpdateCheck(), NonInteractive(), DisableSessionBroker())
		if conn != nil {
			conn.Close()
			t.Fatal("connected to a device with a different identity")
		}
		if !errors.Is(err, errDeviceIdentityRefused) || !strings.Contains(err.Error(), changedIdentityStep) ||
			!strings.Contains(err.Error(), "asset 606") || !strings.Contains(err.Error(), "asset 607") {
			t.Fatalf("err = %v, want the changed-identity refusal with both identities and the next step", err)
		}
		if got := savedAsset(t); got != "606" {
			t.Fatalf("saved asset = %q, want 606 unchanged", got)
		}
	})
	t.Run("yes connects and saves the new identity", func(t *testing.T) {
		changedIdentityFixture(t)
		stubInteractive(t)
		confirmChangedIdentityFn = func() (bool, error) { return true, nil }
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := connectToAgent(ctx, SuppressProvisioningHint(), SuppressUpdateCheck(), DisableSessionBroker())
		if err != nil {
			t.Fatalf("accepted identity did not connect: %v", err)
		}
		conn.Close()
		if got := savedAsset(t); got != "607" {
			t.Fatalf("saved asset = %q, want the accepted 607", got)
		}
	})
	t.Run("no stops and changes nothing", func(t *testing.T) {
		changedIdentityFixture(t)
		stubInteractive(t)
		confirmChangedIdentityFn = func() (bool, error) { return false, nil }
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := connectToAgent(ctx, SuppressProvisioningHint(), SuppressUpdateCheck(), DisableSessionBroker())
		if conn != nil {
			conn.Close()
			t.Fatal("connected after the user declined")
		}
		if !errors.Is(err, ErrUserCancelled) {
			t.Fatalf("err = %v, want a cancellation", err)
		}
		if got := savedAsset(t); got != "606" {
			t.Fatalf("saved asset = %q, want 606 unchanged", got)
		}
	})
}

// discover's probe shows an unenrolled device that answered as reachable.
func TestDiscoverProbeReportsUnenrolledDeviceReachable(t *testing.T) {
	signedInConfig(t)
	addr := serveSetupProbe(t, "unprovisioned")
	host, portText, _ := net.SplitHostPort(addr)
	port, _ := net.LookupPort("tcp", portText)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dev, err := lanProber(ctx, models.LANDevice{Hostname: "new-robot.local", IPAddress: host, Port: port})
	if err != nil || dev.ProbeFailure != nil {
		t.Fatalf("probe failed: err=%v failure=%+v", err, dev.ProbeFailure)
	}
	if dev.IsMTLS || dev.AgentVersion != "setup-test" || lanProvisionedDisplay(&dev) != "Unprovisioned" {
		t.Fatalf("want a reachable unenrolled device, got %+v", dev)
	}
}

// reinstalledDeviceFixture is a device saved as enrolled asset 606 that now
// answers as unenrolled, as it does after a reinstall or reset.
func reinstalledDeviceFixture(t *testing.T) {
	t.Helper()
	restoreDeviceGlobals(t)
	jsonOutput = false
	auth, _ := legacyLiteTestPKI(t, 2, certs.AssetURN(2, 606))
	auth.CloudGRPC = "cloud.test:443"
	cfg := &config.Config{Auth: []config.AuthConfig{*auth}}
	cfg.SetDevicePin("127.0.0.1", 2, auth.CloudGRPC, "606", "")
	setTempConfig(t, cfg)
	setPinCache(t)
	deviceFlag = serveSetupProbe(t, "unprovisioned")
	origDiscover := discoverLANDevices
	discoverLANDevices = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
	origConfirm := confirmChangedIdentityFn
	t.Cleanup(func() { discoverLANDevices, confirmChangedIdentityFn = origDiscover, origConfirm })
}

func TestSavedDeviceNowUnenrolled(t *testing.T) {
	t.Run("without a terminal it refuses with the next step", func(t *testing.T) {
		reinstalledDeviceFixture(t)
		confirmChangedIdentityFn = func() (bool, error) { t.Fatal("prompted without a terminal"); return false, nil }
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		conn, err := connectToAgent(ctx, SuppressProvisioningHint(), SuppressUpdateCheck(), NonInteractive(), DisableSessionBroker())
		if conn != nil {
			conn.Close()
			t.Fatal("connected without confirmation")
		}
		if err == nil || !strings.Contains(err.Error(), "now answers as a device that is not enrolled") || !strings.Contains(err.Error(), changedIdentityStep) {
			t.Fatalf("err = %v, want the not-enrolled warning and the next step", err)
		}
		if got := savedAsset(t); got != "606" {
			t.Fatalf("saved asset = %q, want 606 unchanged", got)
		}
	})
	t.Run("yes connects unverified and forgets the saved identity", func(t *testing.T) {
		reinstalledDeviceFixture(t)
		stubInteractive(t)
		confirmChangedIdentityFn = func() (bool, error) { return true, nil }
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		conn, err := connectToAgent(ctx, SuppressProvisioningHint(), SuppressUpdateCheck(), DisableSessionBroker())
		if err != nil {
			t.Fatalf("confirmed unenrolled device did not connect: %v", err)
		}
		defer conn.Close()
		if conn.IsMTLS || !conn.NoSavedIdentity {
			t.Fatalf("want an unverified plaintext connection, got IsMTLS=%v NoSavedIdentity=%v", conn.IsMTLS, conn.NoSavedIdentity)
		}
		if got := savedAsset(t); got != "" {
			t.Fatalf("saved asset = %q, want the stale identity forgotten", got)
		}
	})
	t.Run("no stops and changes nothing", func(t *testing.T) {
		reinstalledDeviceFixture(t)
		stubInteractive(t)
		confirmChangedIdentityFn = func() (bool, error) { return false, nil }
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		conn, err := connectToAgent(ctx, SuppressProvisioningHint(), SuppressUpdateCheck(), DisableSessionBroker())
		if conn != nil {
			conn.Close()
			t.Fatal("connected after the user declined")
		}
		if !errors.Is(err, ErrUserCancelled) {
			t.Fatalf("err = %v, want a cancellation", err)
		}
		if got := savedAsset(t); got != "606" {
			t.Fatalf("saved asset = %q, want 606 unchanged", got)
		}
	})
}
