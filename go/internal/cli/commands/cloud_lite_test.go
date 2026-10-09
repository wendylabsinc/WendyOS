package commands

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	wendypb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"github.com/wendylabsinc/wendy/go/proto/gen/wcomrelaypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

const (
	liteTestTenant  = "8a53be77-2a69-464f-8f73-83643fe0beaa"
	liteTestAssetID = "7791d76e-a942-4a8e-b582-063d063b5623"
)

func liteTestAuth(cloud string) *config.AuthConfig {
	return &config.AuthConfig{CloudGRPC: cloud, Certificates: []config.CertificateInfo{{
		PrincipalURI: "spiffe://wendy.sh/tenant/" + liteTestTenant + "/operator/test",
	}}}
}

// liteCloudAsset is a Lite board as Cloud lists it once its control connection is up.
func liteCloudAsset() *cloudpbv2.Asset {
	osType, osVersion, target, deviceName := "wendy-lite", "0.9.1", "esp32c6", "lite-00aa11bb"
	return &cloudpbv2.Asset{
		Id:            liteTestAssetID,
		Name:          "bench-board",
		OsType:        &osType,
		OsVersion:     &osVersion,
		DeviceType:    &target,
		PkiDeviceName: &deviceName,
	}
}

func TestCloudLiteSelectorTargetsTheLiteProvider(t *testing.T) {
	auth := liteTestAuth("cloud.example:443")
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
	oldFetch, oldConnect := fetchDefaultCloudAssetsV2Fn, connectDefaultCloudAssetV2Fn
	t.Cleanup(func() { fetchDefaultCloudAssetsV2Fn, connectDefaultCloudAssetV2Fn = oldFetch, oldConnect })
	fetchDefaultCloudAssetsV2Fn = func(context.Context, *config.AuthConfig, bool) ([]*cloudpbv2.Asset, error) {
		return []*cloudpbv2.Asset{liteCloudAsset()}, nil
	}
	connectDefaultCloudAssetV2Fn = func(context.Context, *config.AuthConfig, *cloudpbv2.Asset, string) (*grpcclient.AgentConnection, error) {
		t.Error("opened a WendyOS agent tunnel to a Wendy Lite asset")
		return nil, errors.New("unreachable")
	}

	selector := cloudDeviceSelector{Endpoint: auth.CloudGRPC, TenantUUID: liteTestTenant, AssetUUID: liteTestAssetID}
	selected, err := connectCloudDeviceSelector(context.Background(), selector)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Agent != nil || selected.External == nil {
		t.Fatalf("selected %+v, want an external Wendy Lite device", selected)
	}
	if _, ok := selected.Provider.(*providers.MicroWendyProvider); !ok {
		t.Errorf("provider = %T, want the Wendy Lite provider", selected.Provider)
	}
	if selected.External.ProviderKey != "wendy-lite" {
		t.Errorf("provider key = %q, want wendy-lite", selected.External.ProviderKey)
	}
	want := map[string]string{
		"type":     "Cloud",
		"assetId":  liteTestAssetID,
		"cloud":    auth.CloudGRPC,
		"tenantId": liteTestTenant,
		"deviceId": "lite-00aa11bb",
	}
	for key, value := range want {
		if got := selected.External.ConnectionInfo[key]; got != value {
			t.Errorf("connection info %s = %q, want %q", key, got, value)
		}
	}
	if selected.DefaultSelector != selector.String() {
		t.Errorf("default selector = %q, want %q", selected.DefaultSelector, selector.String())
	}
}

// Callers that need a WendyOS agent get a refusal for a Lite board, never a
// nil connection; resolveTarget hands the board to the Lite provider.
func TestAgentOnlyPathsRefuseACloudLiteDevice(t *testing.T) {
	auth := liteTestAuth("cloud.example:443")
	key := cloudDeviceSelector{Endpoint: auth.CloudGRPC, TenantUUID: liteTestTenant, AssetUUID: liteTestAssetID}.String()
	setTempConfig(t, &config.Config{DefaultDevice: key})
	t.Setenv("WENDY_AGENT_SOCKET", "")
	oldFlag, oldJSON, oldConnect := deviceFlag, jsonOutput, connectCloudDeviceSelectorFn
	t.Cleanup(func() { deviceFlag, jsonOutput, connectCloudDeviceSelectorFn = oldFlag, oldJSON, oldConnect })
	deviceFlag, jsonOutput = "", true
	connectCloudDeviceSelectorFn = func(context.Context, cloudDeviceSelector) (*SelectedDevice, error) {
		return cloudLiteSelectedDevice(auth, liteCloudAsset(), key)
	}

	if conn, err := connectToAgent(context.Background(), NonInteractive()); conn != nil || err == nil || !strings.Contains(err.Error(), "Wendy Lite") {
		t.Errorf("connectToAgent = %v, %v; want a Wendy Lite refusal", conn, err)
	}
	if conn, err := connectMCPDevice(context.Background(), mcpStartupAddress(key)); conn != nil || err == nil || !strings.Contains(err.Error(), "Wendy Lite") {
		t.Errorf("connectMCPDevice = %v, %v; want a Wendy Lite refusal", conn, err)
	}
	target, err := resolveTargetInner(context.Background(), NonInteractive())
	if err != nil || target.External == nil || target.External.ConnectionType() != "Cloud" {
		t.Errorf("resolveTargetInner = %+v, %v; want the Cloud Lite device", target, err)
	}
}

// liteRelayStub forwards bytes to an mTLS device fixture.
type liteRelayStub struct {
	wcomrelaypb.UnimplementedWendyComRelayServiceServer
	opened    chan relayStubOpen
	tlsConfig *tls.Config
}

type relayStubOpen struct {
	assetID       string
	authorization []string
}

func (r *liteRelayStub) WendyComRelay(stream wcomrelaypb.WendyComRelayService_WendyComRelayServer) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	md, _ := metadata.FromIncomingContext(stream.Context())
	r.opened <- relayStubOpen{assetID: first.GetOpen().GetAssetId(), authorization: md.Get("authorization")}
	proxy, device := net.Pipe()
	defer proxy.Close()
	defer device.Close()
	go func() {
		defer proxy.Close()
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			if _, err = proxy.Write(msg.GetPayload().GetBytes()); err != nil {
				return
			}
		}
	}()
	go func() {
		defer proxy.Close()
		buffer := make([]byte, 4096)
		for {
			n, err := proxy.Read(buffer)
			if err != nil {
				return
			}
			if err = stream.Send(&wcomrelaypb.WendyComRelayMessage{Msg: &wcomrelaypb.WendyComRelayMessage_Payload{Payload: &wcomrelaypb.WendyComRelayPayload{Bytes: buffer[:n]}}}); err != nil {
				return
			}
		}
	}()
	stop := context.AfterFunc(stream.Context(), func() { proxy.Close() })
	defer stop()
	conn := tls.Server(device, r.tlsConfig)
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := conn.Handshake(); err != nil {
		return err
	}
	for {
		header := make([]byte, 8)
		if _, err := io.ReadFull(conn, header); err != nil {
			return err
		}
		body := make([]byte, binary.BigEndian.Uint16(header[6:8]))
		if _, err := io.ReadFull(conn, body); err != nil {
			return err
		}
		req := &wendypb.WendyComMessage{}
		if err := proto.Unmarshal(body, req); err != nil {
			return err
		}

		var reply *wendypb.WendyComMessage
		switch {
		case req.GetHandshake() != nil:
			reply = req
		case req.GetCommand().GetGetDeviceInfo() != nil:
			reply = &wendypb.WendyComMessage{Msg: &wendypb.WendyComMessage_Response{Response: &wendypb.WendyComResponse{
				RequestId: req.GetCommand().GetRequestId(),
				Data: &wendypb.WendyComResponse_DeviceInfo{DeviceInfo: &wendypb.WendyComDeviceInfo{
					Os: "wendy-lite", OsVersion: "0.9.1", CpuArchitecture: "riscv32", Target: "esp32c6",
				}},
			}}}
		default:
			continue
		}
		body, err = proto.Marshal(reply)
		if err != nil {
			return err
		}
		binary.BigEndian.PutUint16(header[6:8], uint16(len(body)))
		if _, err = conn.Write(append(header, body...)); err != nil {
			return err
		}

	}
}

// The whole path a picked Cloud Lite board takes: the Lite provider dials the
// relay through this package's Cloud session and runs WendyCom end-to-end.
func TestCloudLiteDeviceInfoGoesThroughTheRelayWithTheSession(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relay := &liteRelayStub{opened: make(chan relayStubOpen, 1)}
	srv := grpc.NewServer()
	wcomrelaypb.RegisterWendyComRelayServiceServer(srv, relay)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	// A non-:443 cloud is dialled without TLS, like a local Cloud.
	auth := liteTestAuth(lis.Addr().String())
	auth.APIKey = "api-key-123"
	auth.Certificates, relay.tlsConfig = liteRelayTestPKI(t)
	t.Setenv("WENDY_SECRET_STORE", "file")
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})

	selected, err := cloudLiteSelectedDevice(auth, liteCloudAsset(), "")
	if err != nil {
		t.Fatal(err)
	}
	info, err := selected.Provider.GetDeviceInfo(context.Background(), *selected.External)
	if err != nil {
		t.Fatalf("GetDeviceInfo: %v", err)
	}
	if info.OS != "wendy-lite" || info.OSVersion != "0.9.1" || info.DeviceType != "esp32c6" {
		t.Errorf("device info = %+v, want the board's", info)
	}
	open := <-relay.opened
	if open.assetID != liteTestAssetID {
		t.Errorf("relay opened asset %q, want %q", open.assetID, liteTestAssetID)
	}
	if len(open.authorization) != 1 || open.authorization[0] != "Bearer api-key-123" {
		t.Errorf("relay saw authorization %q, want the Cloud session's", open.authorization)
	}
}

func TestCloudTabShowsLiteFirmwareWithoutAnAgentTunnel(t *testing.T) {
	asset := liteCloudAsset()
	device := cloudDiscoveryDevice{cloudAssetMetadata: asset, v2: asset, key: asset.GetId()}

	rows := cloudDiscoveryTableRows([]cloudDiscoveryDevice{device}, nil)
	if got := rows[0][2]; got != "esp32c6 (Lite)" {
		t.Errorf("type column = %q, want esp32c6 (Lite)", got)
	}
	if got := rows[0][3]; got != "0.9.1" {
		t.Errorf("version column = %q, want the firmware version", got)
	}
	info, ok := device.info(nil).(cloudDiscoverV2Info)
	if !ok || info.Type != "esp32c6 (Lite)" || info.Version != "0.9.1" {
		t.Errorf("info = %+v, want the Lite type and firmware version", info)
	}
	if _, err := device.connect(context.Background(), liteTestAuth("cloud.example:443"), ""); err == nil || !strings.Contains(err.Error(), "Wendy Lite") {
		t.Errorf("connect = %v, want a refusal before any agent tunnel", err)
	}
}

func liteRelayTestPKI(t *testing.T) ([]config.CertificateInfo, *tls.Config) {
	t.Helper()
	issue := func(serial int64, principal string, parent *x509.Certificate, signer *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
		if parent == nil {
			template.IsCA = true
			template.KeyUsage |= x509.KeyUsageCertSign
			parent = template
			signer = key
		}
		if principal != "" {
			u, err := url.Parse(principal)
			if err != nil {
				t.Fatal(err)
			}
			template.URIs = []*url.URL{u}
		}
		der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, signer)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return cert, key
	}
	root, rk := issue(1, "", nil, nil)
	principal := "spiffe://wendy.sh/tenant/" + liteTestTenant + "/operator/test"
	operator, ok := issue(2, principal, root, rk)
	device, dk := issue(3, "spiffe://wendy.sh/tenant/"+liteTestTenant+"/device/lite-00aa11bb", root, rk)
	key, err := x509.MarshalECPrivateKey(ok)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	return []config.CertificateInfo{{PrincipalURI: principal, PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: operator.Raw})), PemCertificateChain: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw})), PemPrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: key}))}}, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{device.Raw}, PrivateKey: dk}}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
}
