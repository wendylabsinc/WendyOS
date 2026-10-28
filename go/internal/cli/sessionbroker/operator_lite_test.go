//go:build !windows

package sessionbroker

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

func liteManagementFixture(t *testing.T) (*operatorTargets, *grpc.ClientConn, *activity, func(func(*pb.LocalMeshTopology))) {
	t.Helper()
	view, adjacent, target := remoteTopologyFixture(t)
	view.Announcements[0].Platform = "Lite"
	view.Announcements[0].AgentPort = 5054
	view.Announcements[0].Name = "test C5"
	view.Announcements[0].Caps = []string{"sensors", "future-optional"}
	cert, _ := liteManagementCertificate(t, nil, nil, "operator", "spiffe://wendy.sh/tenant/"+target.Tenant().String()+"/operator/alice", false)
	r := newOperatorTargets(nil, nil, config.CertificateInfo{PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}))}, t.TempDir(), Spec{Host: MeshSelector(adjacent), Operator: &OperatorSpec{RouterUUID: adjacent.UUID().String()}})
	var mu sync.Mutex
	r.targetTopology = nil // This fixture supplies coherent observations in-process.
	r.topology = func(context.Context) (*pb.LocalMeshTopology, error) {
		mu.Lock()
		defer mu.Unlock()
		copy := proto.Clone(view).(*pb.LocalMeshTopology)
		copy.CapturedAtUnixMs = time.Now().UnixMilli()
		return copy, nil
	}
	mutate := func(f func(*pb.LocalMeshTopology)) { mu.Lock(); defer mu.Unlock(); f(view) }
	a := newActivity()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.ForceServerCodec(rawCodec{}), grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error { _, err := r.localManagement(stream, a); return err }))
	go server.Serve(listener)
	cc, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close(); server.Stop(); listener.Close(); r.Close() })
	return r, cc, a, mutate
}

func liteManagementCertificate(t *testing.T, parent *x509.Certificate, signer *ecdsa.PrivateKey, name, principal string, ca bool) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, IsCA: ca, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	if ca {
		template.KeyUsage |= x509.KeyUsageCertSign
	}
	if principal != "" {
		uri, _ := url.Parse(principal)
		template.URIs = []*url.URL{uri}
	}
	if parent == nil {
		parent = template
		signer = key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, key.Public(), signer)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func describeLiteFixture(ctx context.Context, cc *grpc.ClientConn, selector string) (OperatorTargetInfo, error) {
	ctx = metadata.AppendToOutgoingContext(ctx, operatorTargetHeader, selector)
	request, reply := rawMessage{}, rawMessage{}
	err := cc.Invoke(ctx, operatorDescribeMethod, &request, &reply, grpc.ForceCodec(rawCodec{}))
	var info OperatorTargetInfo
	if err == nil {
		err = json.Unmarshal(reply, &info)
	}
	return info, err
}

func TestOperatorLiteSignedDescriptionPortMoveAndReturn(t *testing.T) {
	_, cc, _, mutate := liteManagementFixture(t)
	_, _, target := remoteTopologyFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	selector := MeshSelector(target)
	info, err := describeLiteFixture(ctx, cc, selector)
	if err != nil {
		t.Fatal(err)
	}
	if info.Platform != "Lite" || info.Device != target.UUID().String() || info.OperatorPrincipal != "spiffe://wendy.sh/tenant/"+target.Tenant().String()+"/operator/alice" || len(info.Caps) != 2 {
		t.Fatalf("description %#v", info)
	}
	mutate(func(v *pb.LocalMeshTopology) { v.Announcements[0].AgentPort = 5055; v.Announcements[0].Revision++ })
	info, err = describeLiteFixture(ctx, cc, selector)
	if err != nil || info.Address != net.JoinHostPort(target.MeshAddress().String(), "5055") {
		t.Fatal(info, err)
	}
	mutate(func(v *pb.LocalMeshTopology) { v.Routes[0].Unreachable = true })
	if _, err = describeLiteFixture(ctx, cc, selector); err == nil {
		t.Fatal("normal unavailable route must defer selection")
	}
	mutate(func(v *pb.LocalMeshTopology) { v.Routes[0].Unreachable = false })
	if _, err = describeLiteFixture(ctx, cc, selector); err != nil {
		t.Fatal("route returned", err)
	}
}

func TestOperatorLiteRoutedTLSWendyComAndSetupCancellation(t *testing.T) {
	r, cc, a, _ := liteManagementFixture(t)
	_, _, target := remoteTopologyFixture(t)
	root, rootKey := liteManagementCertificate(t, nil, nil, "root", "", true)
	operator, operatorKey := liteManagementCertificate(t, root, rootKey, "operator", "spiffe://wendy.sh/tenant/"+target.Tenant().String()+"/operator/alice", false)
	liteCA, liteCAKey := liteManagementCertificate(t, root, rootKey, "Lite Device CA", "", true)
	device, deviceKey := liteManagementCertificate(t, liteCA, liteCAKey, "C5", target.Principal(), false)
	pool := x509.NewCertPool()
	pool.AddCert(root)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	completed := make(chan error, 1)
	var tlsCount atomic.Int32
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			completed <- err
			return
		}
		defer raw.Close()
		raw.SetDeadline(time.Now().Add(10 * time.Second))
		secure := tls.Server(raw, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{device.Raw}, PrivateKey: deviceKey}}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert})
		if err = secure.Handshake(); err != nil {
			completed <- err
			return
		}
		tlsCount.Add(1)
		if secure.ConnectionState().PeerCertificates[0].URIs[0].String() != operator.URIs[0].String() {
			completed <- io.ErrUnexpectedEOF
			return
		}
		for i := 0; i < 3; i++ {
			var header [8]byte
			if _, err = io.ReadFull(secure, header[:]); err != nil {
				completed <- err
				return
			}
			body := make([]byte, binary.BigEndian.Uint16(header[6:]))
			if _, err = io.ReadFull(secure, body); err != nil {
				completed <- err
				return
			}
			message := new(litepb.WendyComMessage)
			if err = proto.Unmarshal(body, message); err != nil {
				completed <- err
				return
			}
			if command := message.GetCommand(); command != nil {
				response := &litepb.WendyComResponse{RequestId: command.RequestId}
				if command.GetGetDeviceInfo() != nil {
					response.Data = &litepb.WendyComResponse_DeviceInfo{DeviceInfo: &litepb.WendyComDeviceInfo{Os: "Wendy Lite", Target: "esp32c5", WasmAppSupport: true}}
				}
				message = &litepb.WendyComMessage{Msg: &litepb.WendyComMessage_Response{Response: response}}
			}
			body, err = proto.Marshal(message)
			if err != nil {
				completed <- err
				return
			}
			header = [8]byte{0xa5, 2}
			binary.BigEndian.PutUint16(header[6:], uint16(len(body)))
			if _, err = secure.Write(append(header[:], body...)); err != nil {
				completed <- err
				return
			}
		}
		completed <- nil
	}()
	var dials atomic.Int32
	r.packetDial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != net.JoinHostPort(target.MeshAddress().String(), "5054") {
			t.Errorf("routed destination %s %s", network, address)
		}
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}
	pins, err := devicepin.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	setup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	client := liteclient.NewWendyLiteClient()
	err = client.ConnectWithPKIDialerContext(setup, "signed mesh endpoint", tls.Certificate{Certificate: [][]byte{operator.Raw}, PrivateKey: operatorKey}, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw})), target.UUID().String(), func(context.Context) (string, error) {
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: liteCA.Raw})), nil
	}, pins,
		func(ctx context.Context, _, _ string) (net.Conn, error) {
			return openOperatorLite(ctx, cc, MeshSelector(target), nil)
		})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer client.Close()
	cancel()
	if a.activeRPCs.Load() != 1 {
		t.Fatal("live routed stream did not retain attachment lease")
	}
	if err = client.Ping(); err != nil {
		t.Fatal("WendyCom after setup cancellation", err)
	}
	info, err := client.GetDeviceInfo(3 * time.Second)
	if err != nil || info.Target != "esp32c5" {
		t.Fatal(info, err)
	}
	if err = <-completed; err != nil {
		t.Fatal(err)
	}
	if tlsCount.Load() != 1 || dials.Load() != 1 {
		t.Fatal("expected one routed socket with one end-to-end TLS", tlsCount.Load(), dials.Load())
	}
	if !pins.Has(target.Principal()) {
		t.Fatal("verified Lite-CA device was not pinned")
	}
	if client.PeerCertificate() == nil || !bytes.Equal(client.PeerCertificate().Raw, device.Raw) {
		t.Fatal("device verification unavailable")
	}
}

func TestOperatorLiteSlowReaderFlowControlHalfCloseAndClose(t *testing.T) {
	r, cc, a, _ := liteManagementFixture(t)
	_, _, target := remoteTopologyFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	begin := make(chan struct{})
	done := make(chan error, 1)
	payload := bytes.Repeat([]byte("bounded-routed-byte-stream"), 65536)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer raw.Close()
		raw.(*net.TCPConn).SetReadBuffer(1024)
		raw.SetDeadline(time.Now().Add(8 * time.Second))
		<-begin
		hash := make([]byte, len(payload))
		_, err = io.ReadFull(raw, hash)
		if err == nil && !bytes.Equal(hash, payload) {
			err = io.ErrUnexpectedEOF
		}
		if err == nil {
			var one [1]byte
			_, err = raw.Read(one[:])
			if err == io.EOF {
				err = nil
			}
		}
		if err == nil {
			_, err = raw.Write([]byte("complete after request EOF"))
		}
		done <- err
	}()
	r.packetDial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
		if err == nil {
			raw.(*net.TCPConn).SetWriteBuffer(1024)
		}
		return raw, err
	}
	conn, err := openOperatorLite(ctx, cc, MeshSelector(target), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(8 * time.Second))
	writes := make(chan error, 1)
	go func() {
		_, err := conn.Write(payload)
		if err == nil {
			err = conn.(interface{ CloseWrite() error }).CloseWrite()
		}
		writes <- err
	}()
	select {
	case err := <-writes:
		t.Fatalf("slow peer did not exert bounded backpressure: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if a.activeRPCs.Load() != 1 {
		t.Fatal("blocked stream lost broker lease")
	}
	close(begin)
	if err = <-writes; err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(conn)
	if err != nil || string(reply) != "complete after request EOF" {
		t.Fatal(string(reply), err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for a.activeRPCs.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if a.activeRPCs.Load() != 0 {
		t.Fatal("stream retained broker lease after completion")
	}
	// A second ordinary connection can be canceled while both relay directions
	// are blocked, without waiting for a remote peer to consume more bytes.
	blocked, err := openOperatorLite(ctx, cc, MeshSelector(target), nil)
	if err != nil {
		t.Fatal(err)
	}
	reading := make(chan error, 1)
	go func() { var one [1]byte; _, err := blocked.Read(one[:]); reading <- err }()
	if err = blocked.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reading:
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock routed read")
	}
}

func TestOperatorLiteDialCancellationReleasesAdmission(t *testing.T) {
	r, cc, a, _ := liteManagementFixture(t)
	_, _, target := remoteTopologyFixture(t)
	entered := make(chan struct{})
	r.packetDial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	completed := make(chan error, 1)
	go func() {
		conn, err := openOperatorLite(ctx, cc, MeshSelector(target), nil)
		if conn != nil {
			conn.Close()
		}
		completed <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("dial was not scheduled")
	}
	cancel()
	select {
	case err := <-completed:
		if err == nil {
			t.Fatal("canceled open succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("pending open did not observe cancellation")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		active := r.liteStreams
		r.mu.Unlock()
		if active == 0 && a.activeRPCs.Load() == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("canceled dial retained stream admission or broker lease")
}
