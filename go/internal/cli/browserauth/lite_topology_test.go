package browserauth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloud "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	lite "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"github.com/wendylabsinc/wendy/go/proto/gen/wcomrelaypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcmetadata "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type topologyRelayOpen struct {
	assetID       string
	authorization []string
}

// topologyRelay transports opaque bytes to a device TLS server.
type topologyRelay struct {
	wcomrelaypb.UnimplementedWendyComRelayServiceServer
	refuse error
	grant  []byte
	opens  chan topologyRelayOpen
	ended  chan struct{}
	device func(net.Conn)
}

func newTopologyRelay() *topologyRelay {
	return &topologyRelay{opens: make(chan topologyRelayOpen, 1), ended: make(chan struct{}, 1)}
}
func (r *topologyRelay) WendyComRelay(stream wcomrelaypb.WendyComRelayService_WendyComRelayServer) error {
	defer func() { r.ended <- struct{}{} }()
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	md, _ := grpcmetadata.FromIncomingContext(stream.Context())
	r.opens <- topologyRelayOpen{assetID: first.GetOpen().GetRequestBody().GetTargetAssetId(), authorization: md.Get("authorization")}
	if r.refuse != nil {
		return r.refuse
	}
	if first.GetOpen().GetPrincipalRequest().GetKind() != "tunnel-principal-request+jws" {
		return status.Error(codes.PermissionDenied, "missing principal proof")
	}
	grant := r.grant
	if grant == nil {
		grant, err = relayGrantFixture()
		if err != nil {
			return err
		}
	}
	if err = stream.Send(&wcomrelaypb.WendyComRelayMessage{Msg: &wcomrelaypb.WendyComRelayMessage_Grant{Grant: &wcomrelaypb.WendyComRelayGrant{Record: grant}}}); err != nil {
		return err
	}
	proxy, device := net.Pipe()
	defer proxy.Close()
	defer device.Close()
	go r.device(device)
	done := make(chan error, 2)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				done <- err
				return
			}
			if msg.GetEstablished() != nil {
				continue
			}
			if _, err = proxy.Write(msg.GetPayload().GetBytes()); err != nil {
				done <- err
				return
			}
		}
	}()
	go func() {
		buffer := make([]byte, 137) // Deliberately split TLS records across relay payloads.
		for {
			n, err := proxy.Read(buffer)
			if err != nil {
				done <- err
				return
			}
			err = stream.Send(&wcomrelaypb.WendyComRelayMessage{Msg: &wcomrelaypb.WendyComRelayMessage_Payload{Payload: &wcomrelaypb.WendyComRelayPayload{Bytes: buffer[:n]}}})
			if err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case err := <-done:
		return err
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
}

func relayGrantFixture() ([]byte, error) {
	raw, err := os.ReadFile("../../../../Proto/conformance/lite-relay/vector.json")
	if err != nil {
		return nil, err
	}
	var vector struct {
		Record string `json:"record_hex"`
	}
	if err = json.Unmarshal(raw, &vector); err != nil {
		return nil, err
	}
	return hex.DecodeString(vector.Record)
}
func topologyCertificate(t *testing.T, serial int64, principal string, ca bool, parent *x509.Certificate, signer *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: fmt.Sprint(serial)}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, IsCA: ca, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	if ca {
		template.KeyUsage |= x509.KeyUsageCertSign
	} else {
		uri, _ := url.Parse(principal)
		template.URIs = []*url.URL{uri}
	}
	if parent == nil {
		parent = template
		signer = key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key
}

type topologyAssetServer struct {
	cloud.UnimplementedAssetServiceServer
	asset *cloud.Asset
}

func (s topologyAssetServer) GetAsset(context.Context, *cloud.GetAssetRequest) (*cloud.Asset, error) {
	return s.asset, nil
}
func topologyReadFrame(conn net.Conn) (*lite.WendyComMessage, error) {
	var header [8]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	body := make([]byte, binary.BigEndian.Uint16(header[6:]))
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, err
	}
	message := new(lite.WendyComMessage)
	err := proto.Unmarshal(body, message)
	return message, err
}
func topologyWriteFrame(conn net.Conn, message *lite.WendyComMessage) error {
	body, err := proto.Marshal(message)
	if err != nil {
		return err
	}
	header := []byte{0xa5, 2, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(header[6:], uint16(len(body)))
	_, err = conn.Write(append(header, body...))
	return err
}
func TestBrowserCloudLiteTopologyEncryptedReporter(t *testing.T) {
	oldNormal, oldSetup, oldHandshake, oldRead := browserTopologyTimeout, browserLiteSetupBudget, browserLiteHandshakeBudget, browserLiteReadBudget
	defer func() {
		browserTopologyTimeout, browserLiteSetupBudget, browserLiteHandshakeBudget, browserLiteReadBudget = oldNormal, oldSetup, oldHandshake, oldRead
	}()
	browserTopologyTimeout = 250 * time.Millisecond
	browserLiteSetupBudget = 800 * time.Millisecond
	browserLiteHandshakeBudget = 700 * time.Millisecond
	browserLiteReadBudget = 1000 * time.Millisecond
	for _, report := range []struct {
		name           string
		enabled, typed bool
	}{
		{"off", false, false}, {"partial", true, false}, {"typed-operators", true, true},
	} {
		t.Run(report.name, func(t *testing.T) {
			enabled := report.enabled
			session, _ := setup(t, nil)
			if _, err := session.Complete(context.Background(), "valid-code", session.state, testIssuer); err != nil {
				t.Fatal(err)
			}
			root, rootKey := topologyCertificate(t, 1, "", true, nil, nil)
			operator, operatorKey := topologyCertificate(t, 2, session.profile.Principal, false, root, rootKey)
			keyDER, err := x509.MarshalPKCS8PrivateKey(operatorKey)
			if err != nil {
				t.Fatal(err)
			}
			rootPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
			session.certificate.PemCertificateChain = rootPEM
			session.liteCertificate = config.CertificateInfo{PrincipalURI: session.profile.Principal, PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: operator.Raw})), PemPrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})), PemCertificateChain: rootPEM}
			const device = "00000000-0000-4000-8000-000000000001"
			principal := "spiffe://wendy.sh/tenant/" + session.profile.Tenant + "/device/" + device
			serverLeaf, serverKey := topologyCertificate(t, 3, principal, false, root, rootKey)
			roots := x509.NewCertPool()
			roots.AddCert(root)
			config := &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, Certificates: []tls.Certificate{{Certificate: [][]byte{serverLeaf.Raw}, PrivateKey: serverKey}}}
			relay := newTopologyRelay()
			deviceDone := make(chan error, 1)
			relay.device = func(raw net.Conn) {
				defer raw.Close()
				raw.SetDeadline(time.Now().Add(5 * time.Second))
				conn := tls.Server(raw, config)
				if enabled {
					time.Sleep(350 * time.Millisecond)
				}
				if err := conn.Handshake(); err != nil {
					deviceDone <- err
					return
				}
				grant, _ := relayGrantFixture()
				record := make([]byte, len(grant))
				if _, err := io.ReadFull(conn, record); err != nil {
					deviceDone <- err
					return
				}
				if !bytes.Equal(grant, record) {
					deviceDone <- fmt.Errorf("grant changed")
					return
				}
				if _, err := conn.Write([]byte("WLA2")); err != nil {
					deviceDone <- err
					return
				}
				handshake, err := topologyReadFrame(conn)
				if err != nil {
					deviceDone <- err
					return
				}
				if err = topologyWriteFrame(conn, handshake); err != nil {
					deviceDone <- err
					return
				}
				request, err := topologyReadFrame(conn)
				if err != nil {
					deviceDone <- err
					return
				}
				tenant := uuid.MustParse(session.profile.Tenant)
				id := uuid.MustParse(device)
				state := lite.WendyComNetState_WENDY_COM_NET_OFF
				if enabled {
					state = lite.WendyComNetState_WENDY_COM_NET_RUNNING
				}
				if enabled {
					time.Sleep(550 * time.Millisecond)
				}
				page := &lite.WendyComNetTopologyPage{Tenant: tenant[:], Device: id[:], RuntimeAvailable: enabled, Partial: enabled && !report.typed, CapturedAtUnixMs: time.Now().UnixMilli(), Status: &lite.WendyComNetStatus{State: state, Desired: &lite.WendyComNetConfig{Enabled: enabled, Ble: true}}}
				if enabled {
					page.Generation = 1
					identity, _ := meshid.DeviceFromPrincipal(principal)
					address := identity.MeshAddress().As16()
					page.Record = &lite.WendyComNetTopologyRecord{Kind: 1, Device: id[:], Address: address[:], LinkId: 1, Transport: 2, Metric: 8192}
				}
				pages := []*lite.WendyComNetTopologyPage{page}
				if report.typed {
					operator, _ := meshoperator.ParsePrincipal("spiffe://wendy.sh/tenant/" + session.profile.Tenant + "/operator/Alice")
					var rows []*lite.WendyComNetTopologyRecord
					gateway := uuid.MustParse("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee")
					for index, text := range []string{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "cccccccc-cccc-4ccc-8ccc-cccccccccccc"} {
						instance := uuid.MustParse(text)
						endpoint := meshoperator.Endpoint{Operator: operator, Instance: meshoperator.InstanceID(instance)}
						address := endpoint.Address().As16()
						row := &lite.WendyComNetTopologyRecord{Kind: 3, OwnerRole: 2, OwnerPrincipal: operator.Principal(), OwnerInstance: instance[:], Address: address[:], RouterId: ^uint64(0) - uint64(index), Revision: ^uint64(0), Authorized: true, ExpiresAtUnixMs: page.CapturedAtUnixMs + 300000}
						rows = append(rows, row)
						route := proto.Clone(row).(*lite.WendyComNetTopologyRecord)
						route.Kind = 2
						route.NextHopDevice = gateway[:]
						route.Metric = 128
						rows = append(rows, route)
					}
					rows = append(rows, &lite.WendyComNetTopologyRecord{Kind: 2, Address: net.ParseIP("fd45:b891:5680::99").To16(), Metric: 65535, Unreachable: true})
					pages = nil
					for index, row := range rows {
						current := proto.Clone(page).(*lite.WendyComNetTopologyPage)
						current.Record = row
						if index+1 < len(rows) {
							current.NextCursor = uint32(index + 1)
						}
						pages = append(pages, current)
					}
				}
				for index, current := range pages {
					if index > 0 {
						request, err = topologyReadFrame(conn)
						if err != nil {
							deviceDone <- err
							return
						}
					}
					if request.GetCommand().GetNetTopology().GetCursor() != uint32(index) {
						deviceDone <- fmt.Errorf("page order changed")
						return
					}
					if err := topologyWriteFrame(conn, &lite.WendyComMessage{Msg: &lite.WendyComMessage_Response{Response: &lite.WendyComResponse{RequestId: request.GetCommand().RequestId, Data: &lite.WendyComResponse_NetTopology{NetTopology: current}}}}); err != nil {
						deviceDone <- err
						return
					}
				}
				deviceDone <- nil
				// Keep the stream alive until the observation closes its private session.
				io.Copy(io.Discard, conn)
			}
			listener := bufconn.Listen(1 << 20)
			defer listener.Close()
			server := grpc.NewServer()
			defer server.Stop()
			cloud.RegisterAssetServiceServer(server, topologyAssetServer{asset: &cloud.Asset{Id: device, OrganizationId: session.profile.Tenant, OsType: proto.String("wendy-lite")}})
			wcomrelaypb.RegisterWendyComRelayServiceServer(server, relay)
			go server.Serve(listener)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/pem-certificate-chain"}}, Body: io.NopCloser(bytes.NewBufferString(rootPEM))}, nil
			})}
			observation := session.MeshTopology(ctx, device, func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }, client)
			if observation.Status != "ok" || observation.Platform != "wendy-lite" {
				t.Fatal(observation)
			}
			var document map[string]any
			if err := json.Unmarshal(observation.Topology, &document); err != nil {
				t.Fatal(err)
			}
			if document["selfPrincipal"] != principal {
				t.Fatal(document)
			}
			if enabled && !report.typed && document["truncated"] != true {
				t.Fatal("partial endpoint view hidden", document)
			}
			if report.typed {
				snapshot := new(pb.LocalMeshTopology)
				if err := protojson.Unmarshal(observation.Topology, snapshot); err != nil {
					t.Fatal(err)
				}
				if len(snapshot.Announcements) != 2 || len(snapshot.Routes) != 3 || snapshot.Truncated {
					t.Fatal(snapshot)
				}
				first, second := snapshot.Announcements[0], snapshot.Announcements[1]
				if first.Principal != second.Principal || first.DeviceId == second.DeviceId || first.Prefix == second.Prefix || first.DeviceId != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" || !bytes.Contains(observation.Topology, []byte("18446744073709551615")) {
					t.Fatal(string(observation.Topology))
				}
				if snapshot.Routes[2].OriginPrincipal != "" || !snapshot.Routes[2].Unreachable {
					t.Fatal(snapshot.Routes[2])
				}
			}
			if err := <-deviceDone; err != nil {
				t.Fatal(err)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBrowserLiteOperatorUsesFreshCSRAndRotatesBackToCloud(t *testing.T) {
	session, calls := setup(t, nil)
	if _, err := session.Complete(context.Background(), "valid-code", session.state, testIssuer); err != nil {
		t.Fatal(err)
	}
	primary := session.certificate.PemCertificate
	original := session.Client
	issued := 0
	root, rootKey := topologyCertificate(t, 1, "", true, nil, nil)
	session.Client = doerFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != IdentityEndpoint {
			return original.Do(request)
		}
		issued++
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(body)
		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok {
			t.Fatal("Lite CSR is not P256")
		}
		uri, _ := url.Parse(session.profile.Principal)
		leaf := &x509.Certificate{SerialNumber: big.NewInt(5), URIs: []*url.URL{uri}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, UnknownExtKeyUsage: []asn1.ObjectIdentifier{{1, 3, 6, 1, 4, 1, 65441, 1, 9}}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, root, csr.PublicKey, rootKey)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))}, nil
	})
	before := *calls
	session.mu.Lock()
	certificate, err := session.liteOperatorLocked(context.Background())
	session.mu.Unlock()
	if err != nil || certificate.PrincipalURI != session.profile.Principal || issued != 1 || *calls != before+2 || session.certificate.PemCertificate != primary || session.tokens.Access == "" {
		t.Fatal(certificate.PrincipalURI, issued, *calls-before, err)
	}
	session.mu.Lock()
	_, err = session.liteOperatorLocked(context.Background())
	session.mu.Unlock()
	if err != nil || issued != 1 {
		t.Fatal(issued, err)
	}
}
