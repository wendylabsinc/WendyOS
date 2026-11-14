package mcusource

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type assetIdentitySensor struct {
	agentpbv2.UnimplementedWendySensorServiceServer
	calls atomic.Int32
}

func (s *assetIdentitySensor) GetSensorManifest(context.Context, *agentpbv2.GetSensorManifestRequest) (*sensorlinkpb.SensorManifest, error) {
	s.calls.Add(1)
	return &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{{ChannelId: 42, Name: "authenticated-asset-42"}}}, nil
}

// This is the numeric-asset compatibility path changed by 6acc2f31b, not
// principal-only pairing or an insecure RPC fixture. Both production transports
// must authenticate the source inside the consumer certificate's tenant; the
// legacy pairing OrgID deliberately disagrees. No external PKI or MCU is used.
func TestSensorAssetIdentityOverTLSAndGRPC(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const other = "22222222-2222-4222-8222-222222222222"
	for _, scope := range []struct{ name, own, good, wrongTenant, wrongAsset string }{
		{"legacy", "urn:wendy:org:7:asset:1", "urn:wendy:org:7:asset:42", "urn:wendy:org:9:asset:42", "urn:wendy:org:7:asset:43"},
		{"tenant", "spiffe://wendy.sh/tenant/" + tenant + "/service/asset-1", "spiffe://wendy.sh/tenant/" + tenant + "/service/asset-42", "spiffe://wendy.sh/tenant/" + other + "/service/asset-42", "spiffe://wendy.sh/tenant/" + tenant + "/service/asset-43"},
	} {
		for _, peer := range []struct {
			name, uri string
			allowed   bool
		}{
			{"same-tenant-asset", scope.good, true}, {"wrong-tenant", scope.wrongTenant, false}, {"wrong-asset", scope.wrongAsset, false},
		} {
			for _, protocol := range []string{"tls", "grpc"} {
				t.Run(scope.name+"/"+peer.name+"/"+protocol, func(t *testing.T) {
					root, rootKey := liteCameraCertificate(t, 1, "", nil, nil)
					own, ownKey := liteCameraCertificate(t, 2, scope.own, root, rootKey)
					source, sourceKey := liteCameraCertificate(t, 3, peer.uri, root, rootKey)
					keyDER, err := x509.MarshalECPrivateKey(ownKey)
					if err != nil {
						t.Fatal(err)
					}
					certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: own.Raw}))
					chainPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
					keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
					roots := x509.NewCertPool()
					roots.AddCert(root)
					var authenticated atomic.Bool
					serverTLS := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{source.Raw, root.Raw}, PrivateKey: sourceKey}}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots,
						VerifyConnection: func(state tls.ConnectionState) error {
							if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 || !bytes.Equal(state.PeerCertificates[0].Raw, own.Raw) {
								return fmt.Errorf("unexpected consumer certificate")
							}
							authenticated.Store(true)
							return nil
						}}
					ln, err := net.Listen("tcp4", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					defer ln.Close()
					ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
					defer cancel()
					pairing := SensorPairing{SourceAssetID: 42, OrgID: 9}
					done := make(chan struct{})
					if protocol == "grpc" {
						server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
						sensor := &assetIdentitySensor{}
						agentpbv2.RegisterWendySensorServiceServer(server, sensor)
						go func() { defer close(done); _ = server.Serve(ln) }()
						defer func() {
							server.Stop()
							select {
							case <-done:
							case <-time.After(time.Second):
								t.Error("gRPC source did not drain")
							}
						}()
						transport, err := NewGRPCTransport(zap.NewNop(), certPEM, chainPEM, keyPEM, pairing, ln.Addr().String())
						if err != nil {
							t.Fatal(err)
						}
						defer transport.Close()
						manifest, err := transport.FetchManifest(ctx)
						if peer.allowed {
							if err != nil || len(manifest.GetSensors()) != 1 || manifest.GetSensors()[0].GetName() != "authenticated-asset-42" || sensor.calls.Load() != 1 || !authenticated.Load() {
								t.Fatalf("ASSET_TENANT_RPC: manifest=%v error=%v calls=%d authenticated=%v", manifest, err, sensor.calls.Load(), authenticated.Load())
							}
						} else if err == nil || sensor.calls.Load() != 0 {
							t.Fatalf("rejected identity reached RPC: error=%v calls=%d", err, sensor.calls.Load())
						}
					} else {
						var delivered atomic.Bool
						go func() {
							defer close(done)
							conn, err := ln.Accept()
							if err != nil {
								return
							}
							defer conn.Close()
							_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
							secured := tls.Server(conn, serverTLS)
							if secured.HandshakeContext(ctx) != nil {
								return
							}
							body := make([]byte, 5)
							if _, err = io.ReadFull(secured, body); err == nil && string(body) == "probe" {
								delivered.Store(true)
								_, _ = secured.Write([]byte("reply"))
							}
						}()
						defer func() {
							ln.Close()
							select {
							case <-done:
							case <-time.After(5 * time.Second):
								t.Error("TLS source did not drain")
							}
						}()
						factory := NewMTLSDialer(zap.NewNop(), func() (string, string, string) { return certPEM, chainPEM, keyPEM })
						dialer, err := factory(pairing)
						if err != nil {
							t.Fatal(err)
						}
						conn, err := dialer.Dial(ctx, ln.Addr().String())
						if peer.allowed {
							if err != nil {
								t.Fatalf("ASSET_TENANT_TLS: %v", err)
							}
							defer conn.Close()
							_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
							if _, err = conn.Write([]byte("probe")); err != nil {
								t.Fatal(err)
							}
							body := make([]byte, 5)
							if _, err = io.ReadFull(conn, body); err != nil || string(body) != "reply" || !authenticated.Load() {
								t.Fatalf("authenticated exchange: %q %v", body, err)
							}
						} else {
							if conn != nil {
								conn.Close()
							}
							if err == nil {
								t.Fatal("rejected identity completed TLS")
							}
						}
						select {
						case <-done:
						case <-ctx.Done():
							t.Fatal("source exchange did not finish")
						}
						if delivered.Load() != peer.allowed {
							t.Fatalf("application bytes delivered=%v allowed=%v", delivered.Load(), peer.allowed)
						}
					}
				})
			}
		}
	}
}
