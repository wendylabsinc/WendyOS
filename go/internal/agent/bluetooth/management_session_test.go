package bluetooth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/shared/operatorpermission"
	"math/big"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

type managementContainers struct{ stopped atomic.Int32 }

func (*managementContainers) ListContainers(context.Context) ([]*agentpb.AppContainer, error) {
	return nil, nil
}
func (m *managementContainers) StopContainer(context.Context, string) error {
	m.stopped.Add(1)
	return nil
}
func (*managementContainers) DeleteContainer(context.Context, string, bool) error { return nil }

// Exercise the actual TLS configuration and command loop used by the L2CAP
// listener over TCP. Radio framing is unchanged; no hardware is needed here.
func TestManagementStreamRequiresOperatorBeforeDispatch(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const serverPrincipal = "spiffe://wendy.sh/tenant/" + tenant + "/device/33333333-3333-4333-8333-333333333333"
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "management test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, rootKey.Public(), rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	rootPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}))
	var serial int64 = 1
	mint := func(principal, commonName string, claims ...string) (string, string) {
		t.Helper()
		serial++
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(principal)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: commonName},
			NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
			BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{u}}
		if claims != nil {
			leaf.NotAfter = time.Now().Add(4 * time.Minute)
			var values []asn1.RawValue
			for _, claim := range claims {
				values = append(values, asn1.RawValue{Tag: 12, Bytes: []byte(claim)})
			}
			der, _ := asn1.Marshal(values)
			leaf.ExtraExtensions = []pkix.Extension{{Id: operatorpermission.OID, Value: der}}
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, root, key.Public(), rootKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	}
	serverCert, serverKey := mint(serverPrincipal, "target")
	want, err := meshid.DeviceFromPrincipal(serverPrincipal)
	if err != nil {
		t.Fatal(err)
	}
	for _, permissions := range []string{"audit", "enforce"} {
		t.Run(permissions, func(t *testing.T) {
			t.Setenv("WENDY_OPERATOR_PERMISSIONS", permissions)
			for _, mode := range []interceptor.OrgMode{interceptor.OrgModeStrict, interceptor.OrgModeGrace, interceptor.OrgModeOff} {
				for _, tc := range []struct {
					name, principal, cn string
					allowed             bool
				}{
					{"operator", "spiffe://wendy.sh/tenant/" + tenant + "/operator/alice", "operator", true},
					{"entitled operator", "spiffe://wendy.sh/tenant/" + tenant + "/operator/alice", "operator", true},
					{"wrong target", "spiffe://wendy.sh/tenant/" + tenant + "/operator/alice", "operator", true},
					{"device", "spiffe://wendy.sh/tenant/" + tenant + "/device/44444444-4444-4444-8444-444444444444", "device", false},
					{"device with user CN", "spiffe://wendy.sh/tenant/" + tenant + "/device/44444444-4444-4444-8444-444444444444", "wendy/user/alice", false},
					{"signer", "spiffe://wendy.sh/tenant/" + tenant + "/signer/publisher", "signer", false},
					{"foreign operator", "spiffe://wendy.sh/tenant/22222222-2222-4222-8222-222222222222/operator/alice", "operator", false},
				} {
					t.Run(mode.String()+"/"+tc.name, func(t *testing.T) {
						serverConfig, err := mtls.NewOperatorTLSConfig(serverCert, rootPEM, serverKey, zap.NewNop(), time.Time{}, mode)
						if err != nil {
							t.Fatal(err)
						}
						clientCert, clientKey := mint(tc.principal, tc.cn)
						if tc.name == "entitled operator" {
							clientCert, clientKey = mint(tc.principal, tc.cn, "entitlement:deployment:control-container:device:33333333-3333-4333-8333-333333333333:allow")
						}
						if tc.name == "wrong target" {
							clientCert, clientKey = mint(tc.principal, tc.cn, "entitlement:deployment:control-container:device:44444444-4444-4444-8444-444444444444:allow")
						}
						allowed := tc.allowed && (permissions == "audit" || tc.name == "entitled operator")
						// Use the production peer verifier while retaining exact target pinning.
						clientConfig, err := mtls.NewClientTLSConfigExpectingDevice(serverCert, rootPEM, serverKey, zap.NewNop(), want)
						if err != nil {
							t.Fatal(err)
						}
						pair, err := tls.X509KeyPair([]byte(clientCert), []byte(clientKey))
						if err != nil {
							t.Fatal(err)
						}
						clientConfig.Certificates = []tls.Certificate{pair}
						listener, err := net.Listen("tcp", "127.0.0.1:0")
						if err != nil {
							t.Fatal(err)
						}
						defer listener.Close()
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						containers := &managementContainers{}
						done := make(chan struct{})
						go func() {
							defer close(done)
							raw, err := listener.Accept()
							if err != nil {
								return
							}
							serveManagementStream(ctx, raw, zap.NewNop(), NewDispatcher(nil, containers, nil, nil), serverConfig)
						}()
						defer func() { cancel(); listener.Close(); <-done }()
						raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
						if err != nil {
							t.Fatal(err)
						}
						defer raw.Close()
						conn := tls.Client(raw, clientConfig)
						_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
						err = conn.HandshakeContext(ctx)
						if err == nil {
							command := &agentpb.BluetoothCommand{Command: &agentpb.BluetoothCommand_AppsStop{AppsStop: &agentpb.AppsStopCommand{AppName: "test-app"}}}
							payload, marshalErr := proto.Marshal(command)
							if marshalErr != nil {
								t.Fatal(marshalErr)
							}
							err = writeFrameToConn(conn, payload)
							if err == nil {
								var response []byte
								response, err = readMessageFromConn(conn)
								if err == nil {
									var resp agentpb.BluetoothResponse
									err = proto.Unmarshal(response, &resp)
									if err == nil && resp.GetError() != nil {
										err = fmt.Errorf("command denied: %s", resp.GetError().GetMessage())
									}
								}
							}
						}
						if (err == nil) != allowed {
							t.Fatalf("command error=%v, allowed=%v", err, allowed)
						}
						if got := containers.stopped.Load(); got != map[bool]int32{true: 1, false: 0}[allowed] {
							t.Fatalf("stop called %d times", got)
						}
					})
				}
			}
		})
	}
}
