//go:build !windows

package sessionbroker

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestOperatorAuxiliaryRegistryTLSUsesSelectedTargetAndSurvivesSetupCancel(t *testing.T) {
	r, control, activity, mutate := liteManagementFixture(t)
	_, _, target := remoteTopologyFixture(t)
	mutate(func(view *pb.LocalMeshTopology) {
		view.Announcements[0].Platform = "WendyOS"
		view.Announcements[0].AgentPort = 50052
	})
	root, rootKey := liteManagementCertificate(t, nil, nil, "root", "", true)
	operator, operatorKey := liteManagementCertificate(t, root, rootKey, "operator", "spiffe://wendy.sh/tenant/"+target.Tenant().String()+"/operator/alice", false)
	device, deviceKey := liteManagementCertificate(t, root, rootKey, "registry device", target.Principal(), false)
	r.certificate = config.CertificateInfo{PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: operator.Raw}))}
	fixtureLocalOperatorReporter(t, r, mutate, 0)
	pool := x509.NewCertPool()
	pool.AddCert(root)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	payload := make([]byte, 64*1024)
	if _, err = rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	completed := make(chan error, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			completed <- err
			return
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
		secure := tls.Server(raw, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{device.Raw}, PrivateKey: deviceKey}}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert})
		if err = secure.Handshake(); err == nil {
			if got := secure.ConnectionState().PeerCertificates[0].URIs[0].String(); got != operator.URIs[0].String() {
				err = fmt.Errorf("registry saw operator %s", got)
			}
		}
		buf := make([]byte, len(payload))
		if err == nil {
			_, err = io.ReadFull(secure, buf)
		}
		if err == nil && !bytes.Equal(buf, payload) {
			err = fmt.Errorf("registry input mismatch")
		}
		if err == nil {
			_, err = secure.Write(buf)
		}
		completed <- err
	}()
	var dials atomic.Int32
	r.packetDial = func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		want := net.JoinHostPort(target.MeshAddress().String(), "5000")
		if network != "tcp" || address != want {
			return nil, fmt.Errorf("registry dial %s %s, want %s", network, address, want)
		}
		return (&net.Dialer{}).DialContext(ctx, network, listener.Addr().String())
	}
	// Normal remote AgentConnection carries this interceptor. Opening a second
	// connection must keep one exact target field, rather than duplicate it.
	remote, err := grpc.NewClient(control.Target(), append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}, operatorTargetOptions(MeshSelector(target))...)...)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	setup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	conn, err := openOperatorTCP(setup, remote, MeshSelector(target), 5000, nil)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	secure := tls.Client(conn, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{operator.Raw}, PrivateKey: operatorKey}}, InsecureSkipVerify: true, VerifyConnection: func(state tls.ConnectionState) error {
		leaf := state.PeerCertificates[0]
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			return err
		}
		if len(leaf.URIs) != 1 || leaf.URIs[0].String() != target.Principal() {
			return fmt.Errorf("registry destination differs")
		}
		return nil
	}})
	_ = secure.SetDeadline(time.Now().Add(10 * time.Second))
	if err = secure.Handshake(); err != nil {
		t.Fatal(err)
	}
	if _, err = secure.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err = io.ReadFull(secure, got); err != nil || !bytes.Equal(got, payload) {
		t.Fatal("registry roundtrip", err)
	}
	if err = <-completed; err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	until := time.Now().Add(2 * time.Second)
	for activity.activeRPCs.Load() != 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if activity.activeRPCs.Load() != 0 || dials.Load() != 1 {
		t.Fatal("auxiliary stream owner not released", activity.activeRPCs.Load(), dials.Load())
	}
	// A normal route outage applies to the next registry socket too. No first-
	// hop LAN address or stale target hint may substitute for that route.
	mutate(func(view *pb.LocalMeshTopology) { view.Routes[0].Unreachable = true })
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if next, err := openOperatorTCP(ctx, remote, MeshSelector(target), 5000, nil); err == nil {
		next.Close()
		t.Fatal("unavailable route was dialed")
	}
	if dials.Load() != 1 {
		t.Fatal("socket dialed after route withdrawal")
	}
}

func TestOperatorAuxiliaryAndLiteStreamsShareBoundedAdmission(t *testing.T) {
	r, cc, activity, mutate := liteManagementFixture(t)
	_, _, target := remoteTopologyFixture(t)
	r.serviceTopology = r.topology
	mutate(func(view *pb.LocalMeshTopology) {
		view.ServicesIncluded = true
		view.Announcements[0].Services = []*pb.LocalMeshService{{Mesh: "lab", AppId: "guest", ServiceId: "echo", Type: "_echo._tcp", Port: 28000}}
	})
	var remotes []net.Conn
	r.packetDial = func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		remotes = append(remotes, server)
		return client, nil
	}
	r.appDial = func(ctx context.Context, _ meshid.Device, _ uint16) (net.Conn, error) {
		return r.packetDial(ctx, "tcp", "fixture")
	}
	defer func() {
		for _, conn := range remotes {
			conn.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var streams []net.Conn
	defer func() {
		for _, conn := range streams {
			conn.Close()
		}
	}()
	for i := 0; i < operatorLiteStreams; i++ {
		var conn net.Conn
		var err error
		if i%3 == 0 {
			conn, err = openOperatorLite(ctx, cc, MeshSelector(target), nil)
		} else if i%3 == 1 {
			conn, err = openOperatorTCP(ctx, cc, MeshSelector(target), 43022, nil)
		} else {
			conn, err = openOperatorApp(ctx, cc, MeshSelector(target), 28000, nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		streams = append(streams, conn)
	}
	if next, err := openOperatorTCP(ctx, cc, MeshSelector(target), 43022, nil); err == nil || !strings.Contains(err.Error(), "capacity") {
		if next != nil {
			next.Close()
		}
		t.Fatal("bounded stream admission", err)
	}
	for _, conn := range streams {
		conn.Close()
	}
	until := time.Now().Add(2 * time.Second)
	for activity.activeRPCs.Load() != 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if activity.activeRPCs.Load() != 0 {
		t.Fatal("active auxiliary leases leaked")
	}
}
