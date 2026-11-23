package commands

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type desktopConnectionVersionClient struct {
	agentpb.WendyAgentServiceClient
	probes atomic.Int32
	err    error
}

func (c *desktopConnectionVersionClient) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest, ...grpc.CallOption) (*agentpb.GetAgentVersionResponse, error) {
	c.probes.Add(1)
	return &agentpb.GetAgentVersionResponse{}, c.err
}

type desktopConnectionCloser struct{ closes *atomic.Int32 }

func (c desktopConnectionCloser) Close() error { c.closes.Add(1); return nil }

func desktopConnectionTestSetup(t *testing.T) (*config.AuthConfig, cloudDiscoveryDevice) {
	t.Helper()
	oldCandidates, oldLocal, oldCloud := desktopLocalCandidates, desktopLocalDial, desktopCloudDial
	t.Cleanup(func() { desktopLocalCandidates, desktopLocalDial, desktopCloudDial = oldCandidates, oldLocal, oldCloud })
	auth := &config.AuthConfig{Name: "operator", CloudGRPC: "cloud.test:443", Certificates: []config.CertificateInfo{{OrganizationID: 7}}}
	asset := &cloudpb.Asset{Id: 42, Name: "camera"}
	return auth, cloudDiscoveryDevice{cloudAssetMetadata: asset, legacy: asset, key: "42"}
}

func TestDesktopConnectionsPreferLocalAndReuseAcrossRoutes(t *testing.T) {
	auth, asset := desktopConnectionTestSetup(t)
	desktopLocalCandidates = func(context.Context, cloudDiscoveryDevice, certs.WendyIdentity) []string {
		return []string{"192.168.1.42:50052"}
	}
	var dials, closes atomic.Int32
	transport := &grpcclient.AgentConnection{IsMTLS: true, Addr: "192.168.1.42:50052", ExtraClosers: []io.Closer{desktopConnectionCloser{&closes}}}
	desktopLocalDial = func(_ context.Context, address string, _ config.CertificateInfo, expected certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		dials.Add(1)
		if address != transport.Addr || expected.EntityID != "42" || expected.OrgID != 7 {
			t.Errorf("wrong address or expected identity: %s %+v", address, expected)
		}
		return transport, nil
	}
	desktopCloudDial = func(context.Context, *config.AuthConfig, cloudDiscoveryDevice, string) (*grpcclient.AgentConnection, error) {
		t.Error("reachable LAN camera opened a Cloud tunnel")
		return nil, errors.New("unexpected Cloud dial")
	}
	var session TunnelSession
	session.SetActive(true)
	t.Cleanup(func() { session.SetActive(false) })
	ctx := session.Context(context.Background())
	one, err := connectCloudDiscoveryDevice(ctx, auth, asset, "")
	if err != nil {
		t.Fatal(err)
	}
	two, err := connectCloudDiscoveryDevice(ctx, auth, asset, "")
	if err != nil {
		t.Fatal(err)
	}
	// A secure LAN selector with the same saved identity must find the channel
	// created for its Cloud selector rather than open a second transport.
	three, err := desktopSessionConnection(ctx, auth, cloudDiscoveryIdentity(auth, asset).IdentityKey(), func(context.Context) (*grpcclient.AgentConnection, error) {
		t.Error("LAN alias did not find the existing channel")
		return nil, errors.New("unexpected alias dial")
	})
	if err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 1 || one.Addr != transport.Addr || two.Addr != transport.Addr || three.Addr != transport.Addr {
		t.Fatal("local transport was not reused")
	}
	one.Close()
	two.Close()
	if closes.Load() != 0 {
		t.Fatal("closing consumers closed the shared channel")
	}
	three.Close()
	if closes.Load() != 0 {
		t.Fatal("idle channel was not retained for reopening")
	}
	session.SetActive(false)
	if closes.Load() != 1 {
		t.Fatal("account teardown did not close the shared channel exactly once")
	}
}

func TestDesktopConnectionsFallbackAndReuseCloud(t *testing.T) {
	auth, asset := desktopConnectionTestSetup(t)
	desktopLocalCandidates = func(context.Context, cloudDiscoveryDevice, certs.WendyIdentity) []string {
		return []string{"192.168.1.42:50052"}
	}
	var localDials, cloudDials atomic.Int32
	desktopLocalDial = func(context.Context, string, config.CertificateInfo, certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		localDials.Add(1)
		return nil, errors.New("stale LAN address")
	}
	client := &desktopConnectionVersionClient{}
	desktopCloudDial = func(context.Context, *config.AuthConfig, cloudDiscoveryDevice, string) (*grpcclient.AgentConnection, error) {
		cloudDials.Add(1)
		return &grpcclient.AgentConnection{AgentService: client, IsMTLS: true, IsCloudV2: true}, nil
	}
	var session TunnelSession
	session.SetActive(true)
	t.Cleanup(func() { session.SetActive(false) })
	ctx := session.Context(context.Background())
	one, err := connectCloudDiscoveryDevice(ctx, auth, asset, "")
	if err != nil {
		t.Fatal(err)
	}
	one.Close()
	two, err := connectCloudDiscoveryDevice(ctx, auth, asset, "")
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	if !two.IsCloudV2 || localDials.Load() != 1 || cloudDials.Load() != 1 || client.probes.Load() != 1 {
		t.Fatal("reopening repeated setup or skipped authentication")
	}
}

func TestDesktopConnectionsCancellationDoesNotFallBack(t *testing.T) {
	auth, asset := desktopConnectionTestSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	desktopLocalCandidates = func(context.Context, cloudDiscoveryDevice, certs.WendyIdentity) []string {
		return []string{"192.168.1.42:50052"}
	}
	desktopLocalDial = func(context.Context, string, config.CertificateInfo, certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		cancel()
		return nil, context.Canceled
	}
	desktopCloudDial = func(context.Context, *config.AuthConfig, cloudDiscoveryDevice, string) (*grpcclient.AgentConnection, error) {
		t.Error("cancelled local attempt started a Cloud connection")
		return nil, errors.New("unexpected fallback")
	}
	if conn, err := connectCloudDiscoveryDevice(ctx, auth, asset, ""); conn != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, %v", conn, err)
	}
}

func TestDesktopConnectionsFailedCloudAuthenticationIsNotReused(t *testing.T) {
	auth, asset := desktopConnectionTestSetup(t)
	desktopLocalCandidates = func(context.Context, cloudDiscoveryDevice, certs.WendyIdentity) []string { return nil }
	var dials, closes atomic.Int32
	want := errors.New("agent authentication failed")
	desktopCloudDial = func(context.Context, *config.AuthConfig, cloudDiscoveryDevice, string) (*grpcclient.AgentConnection, error) {
		client := &desktopConnectionVersionClient{}
		if dials.Add(1) == 1 {
			client.err = want
		}
		return &grpcclient.AgentConnection{AgentService: client, ExtraClosers: []io.Closer{desktopConnectionCloser{&closes}}}, nil
	}
	var session TunnelSession
	session.SetActive(true)
	t.Cleanup(func() { session.SetActive(false) })
	ctx := session.Context(context.Background())
	if conn, err := connectCloudDiscoveryDevice(ctx, auth, asset, ""); conn != nil || !errors.Is(err, want) {
		t.Fatalf("got %v, %v", conn, err)
	}
	if closes.Load() != 1 {
		t.Fatal("failed authentication left its channel open")
	}
	conn, err := connectCloudDiscoveryDevice(ctx, auth, asset, "")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if dials.Load() != 2 {
		t.Fatal("retry reused the unauthenticated channel")
	}
}

func TestDesktopConnectionsIsolateCredentialsDevicesAndAccountGenerations(t *testing.T) {
	auth, asset := desktopConnectionTestSetup(t)
	var dials, closes atomic.Int32
	desktopLocalCandidates = func(context.Context, cloudDiscoveryDevice, certs.WendyIdentity) []string {
		return []string{"192.168.1.42:50052"}
	}
	desktopLocalDial = func(context.Context, string, config.CertificateInfo, certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		dials.Add(1)
		return &grpcclient.AgentConnection{ExtraClosers: []io.Closer{desktopConnectionCloser{&closes}}}, nil
	}
	var session TunnelSession
	session.SetActive(true)
	t.Cleanup(func() { session.SetActive(false) })
	oldCtx := session.Context(context.Background())
	open := func(auth *config.AuthConfig, asset cloudDiscoveryDevice) {
		t.Helper()
		conn, err := connectCloudDiscoveryDevice(oldCtx, auth, asset, "")
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}
	open(auth, asset)
	otherAccount := *auth
	otherAccount.Name = "other-operator"
	open(&otherAccount, asset)
	otherServer := *auth
	otherServer.CloudGRPC = "other.test:443"
	open(&otherServer, asset)
	renewed := *auth
	renewed.Certificates = append([]config.CertificateInfo(nil), auth.Certificates...)
	renewed.Certificates[0].PemCertificate = "renewed certificate"
	open(&renewed, asset)
	otherAsset := asset
	otherAsset.key = "43"
	open(auth, otherAsset)
	if dials.Load() != 5 {
		t.Fatal("different device or credential scopes shared a channel")
	}
	session.SetActive(false)
	if closes.Load() != 5 {
		t.Fatal("old account transports survived teardown")
	}
	session.SetActive(true)
	if conn, err := connectCloudDiscoveryDevice(oldCtx, auth, asset, ""); conn != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("retired handle acquired a new generation")
	}
	conn, err := connectCloudDiscoveryDevice(session.Context(context.Background()), auth, asset, "")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if dials.Load() != 6 {
		t.Fatal("new account generation reused old transport")
	}
}

func TestDesktopConnectionsLocalTLSRequiresExactSelectedDevice(t *testing.T) {
	auth, tlsConfig := legacyLiteTestPKI(t, 7, certs.AssetURN(7, 42))
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
	setPinCache(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	agentpb.RegisterWendyAgentServiceServer(server, versionOnlyAgent{os: "wendyos"})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	expected := certs.WendyIdentity{OrgID: 7, EntityType: certs.EntityAsset, EntityID: "42"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialDesktopLocal(ctx, listener.Addr().String(), auth.Certificates[0], expected)
	if err != nil {
		t.Fatal(err)
	}
	if !conn.IsMTLS {
		t.Fatal("local connection was not authenticated")
	}
	conn.Close()
	wrong := expected
	wrong.EntityID = "43"
	wrongCtx, stop := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer stop()
	if conn, err := dialDesktopLocal(wrongCtx, listener.Addr().String(), auth.Certificates[0], wrong); err == nil || conn != nil {
		t.Fatal("same-CA device with the wrong identity was accepted")
	}
}

func TestDesktopConnectionsExplicitReconnectRedialsSameDevice(t *testing.T) {
	for _, route := range []string{"LAN", "Cloud"} {
		t.Run(route, func(t *testing.T) {
			auth, asset := desktopConnectionTestSetup(t)
			var dials, closes atomic.Int32
			newChannel := func() *grpcclient.AgentConnection {
				dials.Add(1)
				return &grpcclient.AgentConnection{AgentService: &desktopConnectionVersionClient{}, IsMTLS: true, ExtraClosers: []io.Closer{desktopConnectionCloser{&closes}}}
			}
			desktopLocalCandidates = func(_ context.Context, got cloudDiscoveryDevice, expected certs.WendyIdentity) []string {
				if got.key != asset.key || expected.EntityID != "42" || expected.OrgID != 7 {
					t.Errorf("reconnect lost selected device identity: %+v %+v", got, expected)
				}
				if route == "LAN" {
					return []string{"192.168.1.42:50052"}
				}
				return nil
			}
			desktopLocalDial = func(_ context.Context, address string, cert config.CertificateInfo, expected certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
				if route != "LAN" || address != "192.168.1.42:50052" || cert.OrganizationID != 7 || expected.EntityID != "42" || expected.OrgID != 7 {
					t.Errorf("reconnect changed local credentials or target: %s %+v", address, expected)
				}
				return newChannel(), nil
			}
			desktopCloudDial = func(_ context.Context, gotAuth *config.AuthConfig, gotAsset cloudDiscoveryDevice, _ string) (*grpcclient.AgentConnection, error) {
				if route != "Cloud" || gotAuth.Name != auth.Name || gotAsset.key != asset.key {
					t.Error("reconnect changed Cloud credentials or target")
				}
				return newChannel(), nil
			}
			var session TunnelSession
			session.SetActive(true)
			t.Cleanup(func() { session.SetActive(false) })
			ctx := session.Context(context.Background())
			original, err := connectCloudDiscoveryDevice(ctx, auth, asset, "")
			if err != nil {
				t.Fatal(err)
			}
			defer original.Close()
			if original.Reconnect == nil {
				t.Fatal("Cloud-selected connection lost its identity-preserving reconnect")
			}
			// The original is still available in the pool. Explicit recovery must
			// establish a new channel even before gRPC marks that channel failed.
			next, err := original.Reconnect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			if next.Reconnect == nil || dials.Load() != 2 {
				t.Fatal("explicit reconnect reused the old channel or lost future recovery")
			}
			third, err := next.Reconnect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			third.Close()
			ordinary, err := connectCloudDiscoveryDevice(ctx, auth, asset, "")
			if err != nil {
				t.Fatal(err)
			}
			defer ordinary.Close()
			if ordinary.AgentService != third.AgentService || dials.Load() != 3 {
				t.Fatal("ordinary open did not reuse the recovered channel")
			}
			if closes.Load() != 0 {
				t.Fatal("repeated recovery failed to redial or closed another consumer")
			}
			session.SetActive(false)
			if closes.Load() != 3 {
				t.Fatal("scope reset did not close all recovery channels")
			}
			if reopened, err := original.Reconnect(context.Background()); reopened != nil || !errors.Is(err, context.Canceled) {
				if reopened != nil {
					reopened.Close()
				}
				t.Fatal("retired account could reconnect with its former credentials")
			}
		})
	}
}
