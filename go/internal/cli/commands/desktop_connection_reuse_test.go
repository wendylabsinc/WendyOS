package commands

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func TestDesktopConnectionReuseSkipsCloudRoster(t *testing.T) {
	_, asset := desktopConnectionTestSetup(t)
	auth, _ := legacyLiteTestPKI(t, 7, certs.AssetURN(7, 42))
	auth.CloudGRPC = "127.0.0.1:1" // No reachable Cloud service is needed to reopen.
	setTempConfig(t, &config.Config{CurrentContext: auth.Name, Auth: []config.AuthConfig{*auth}})
	desktopLocalCandidates = func(context.Context, cloudDiscoveryDevice, certs.WendyIdentity) []string { return nil }
	client := &desktopConnectionVersionClient{}
	desktopCloudDial = func(context.Context, *config.AuthConfig, cloudDiscoveryDevice, string) (*grpcclient.AgentConnection, error) {
		return &grpcclient.AgentConnection{AgentService: client, IsMTLS: true}, nil
	}
	var session TunnelSession
	session.SetActive(true)
	defer session.SetActive(false)
	ctx, cancel := context.WithTimeout(session.Context(context.Background()), 5*time.Second)
	defer cancel()
	one, err := connectCloudDiscoveryDevice(ctx, auth, asset, "")
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	oldFetch := fetchDefaultCloudAssetsFn
	t.Cleanup(func() { fetchDefaultCloudAssetsFn = oldFetch })
	fetchDefaultCloudAssetsFn = func(context.Context, *config.AuthConfig) ([]*cloudpb.Asset, error) {
		t.Error("reopening fetched the Cloud roster")
		return nil, errors.New("Cloud unavailable")
	}
	selector := cloudDeviceSelector{Endpoint: auth.CloudGRPC, OrgID: 7, AssetID: 42}
	picked, err := connectCloudDeviceSelector(ctx, selector)
	if err != nil {
		t.Fatal(err)
	}
	defer picked.Close()
	recording, err := DesktopRecordingConnect(ctx, selector.String(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer recording.Close()
	if picked.Agent.AgentService != client || recording.AgentService != client || client.probes.Load() != 1 {
		t.Fatal("camera and recording selectors did not reuse authenticated channel")
	}
}

func TestDesktopConnectionReuseKeepsCloudPKIBinding(t *testing.T) {
	auth, _ := desktopConnectionTestSetup(t)
	const tenant = "8a53be77-2a69-464f-8f73-83643fe0beaa"
	const assetID = "7791d76e-a942-4a8e-b582-063d063b5623"
	auth.Certificates[0].PrincipalURI = "spiffe://wendy.sh/tenant/" + tenant + "/operator/test"
	pkiName := "camera-identity"
	asset := &cloudv2.Asset{Id: assetID, Name: "camera", PkiDeviceName: &pkiName}
	device := cloudDiscoveryDevice{cloudAssetMetadata: asset, v2: asset, key: assetID}
	desktopLocalCandidates = func(context.Context, cloudDiscoveryDevice, certs.WendyIdentity) []string { return nil }
	client := &desktopConnectionVersionClient{}
	desktopCloudDial = func(context.Context, *config.AuthConfig, cloudDiscoveryDevice, string) (*grpcclient.AgentConnection, error) {
		return &grpcclient.AgentConnection{AgentService: client, IsMTLS: true}, nil
	}
	var session TunnelSession
	session.SetActive(true)
	defer session.SetActive(false)
	ctx := session.Context(context.Background())
	one, err := connectCloudDiscoveryDevice(ctx, auth, device, "")
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	selector, err := cloudDiscoveryDeviceDefault(auth, device)
	if err != nil {
		t.Fatal(err)
	}
	two, err := desktopExistingCloudConnection(ctx, auth, selector)
	if err != nil || two == nil {
		t.Fatalf("Cloud UUID alias missed PKI identity: %v", err)
	}
	two.Close()
	changed := *auth
	changed.Name = "other-account"
	if conn, err := desktopExistingCloudConnection(ctx, &changed, selector); conn != nil || err != nil {
		t.Fatalf("alias crossed account: %v, %v", conn, err)
	}
	session.SetActive(false)
	if conn, err := desktopExistingCloudConnection(ctx, auth, selector); conn != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("alias survived scope teardown: %v, %v", conn, err)
	}
}

func TestDesktopConnectionReuseSavedLANWithoutTransportFlag(t *testing.T) {
	t.Setenv("WENDY_AGENT_SOCKET", "")
	auth, tlsConfig := legacyLiteTestPKI(t, 7, certs.AssetURN(7, 42))
	cfg := &config.Config{CurrentContext: auth.Name, Auth: []config.AuthConfig{*auth}}
	cfg.SetDevicePin("127.0.0.1", 7, auth.CloudGRPC, "42", certs.AssetURN(7, 42))
	setTempConfig(t, cfg)
	setPinCache(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	agentpb.RegisterWendyAgentServiceServer(server, versionOnlyAgent{os: "wendyos"})
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	var session TunnelSession
	session.SetActive(true)
	defer session.SetActive(false)
	ctx, cancel := context.WithTimeout(session.Context(context.Background()), 8*time.Second)
	defer cancel()
	one, err := DesktopConnectSecureLAN(ctx, listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	two, lite, err := DesktopConnectTarget(ctx, listener.Addr().String())
	if err != nil || lite != nil || two == nil {
		t.Fatalf("saved LAN: %v", err)
	}
	defer two.Close()
	three, err := DesktopConnect(ctx, listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer three.Close()
	if !two.IsMTLS || two.Conn != one.Conn || three.Conn != one.Conn {
		t.Fatal("saved LAN selector redialed or lost authenticated transport")
	}
}
