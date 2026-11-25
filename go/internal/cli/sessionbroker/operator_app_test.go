//go:build !windows

package sessionbroker

import (
	"bytes"
	"context"
	"crypto"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh"
	"github.com/wendylabsinc/wendy/go/internal/cli/operatormesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshview"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type operatorAppAuthorizer struct {
	port    uint16
	address string
	called  atomic.Bool
}

func TestOperatorDeclaredLiteAppRetainsTargetBeyondPublicInventoryBudget(t *testing.T) {
	r, cc, _, _ := liteManagementFixture(t)
	_, adjacent, target := remoteTopologyFixture(t)
	operator, err := operatorPrincipal(r.certificate)
	if err != nil {
		t.Fatal(err)
	}
	self, err := meshid.PeerFromPrincipal(operator.Principal())
	if err != nil {
		t.Fatal(err)
	}
	self, err = self.WithInstance(uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	snapshot := localmesh.NodeSnapshot{Self: self, CapturedAt: now, Links: []localmesh.PeerLink{{ID: 1, Device: adjacent, Cost: 128}}, RouteAuthorizations: map[localmesh.RouteOrigin]bool{}}
	appendDevice := func(device meshid.Device) {
		origin := localmesh.RouteOrigin{Prefix: netip.PrefixFrom(device.MeshAddress(), 128), RouterID: babel.RouterID(len(snapshot.Devices) + 1), Seqno: 1}
		manifest := localmesh.Manifest{Version: 4, Device: device.UUID(), Revision: 1, Issued: now.UnixMilli(), Expires: now.Add(15 * time.Minute).UnixMilli(), Route: &origin, AgentPort: 5054, AgentTransport: "mtls-v1", Platform: "Lite"}
		for i := range 32 {
			manifest.Services = append(manifest.Services, localmesh.ManifestService{Mesh: "lab", AppID: "sh.wendy.fixture", ServiceID: fmt.Sprintf("service-%d", i), Type: "_echo._tcp", Instance: "Guest", Port: uint16(28000 + i)})
		}
		snapshot.Devices = append(snapshot.Devices, manifest)
		snapshot.Routes = append(snapshot.Routes, babel.Route{Prefix: origin.Prefix, RouterID: origin.RouterID, Seqno: origin.Seqno, Link: 1, Metric: 128})
		snapshot.RouteAuthorizations[origin] = true
	}
	// All 64 unrelated owners sort before the selected target: 2,048 valid
	// unrelated services exceed the public 1,024-service projection budget.
	for i := 1; i <= 64; i++ {
		appendDevice(meshid.NewDevice(target.Tenant(), uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012x", i))))
	}
	appendDevice(target)
	r.localSnapshot = func() localmesh.NodeSnapshot { return snapshot }
	r.topology = func(context.Context) (*pb.LocalMeshTopology, error) {
		return meshview.Topology(self, snapshot, time.Now()), nil
	}
	r.targetTopology = nil
	r.serviceTopology = func(context.Context) (*pb.LocalMeshTopology, error) {
		return meshview.Topology(self, snapshot, time.Now(), true), nil
	}
	public, err := r.serviceTopology(context.Background())
	if err != nil || !public.Truncated {
		t.Fatal("fixture did not reach public inventory bound", err)
	}
	for _, hint := range public.Announcements {
		if hint.Principal == target.Principal() && len(hint.Services) != 0 {
			t.Fatal("selected target unexpectedly fit the unfiltered public view")
		}
	}
	var dials atomic.Int32
	r.appDial = func(_ context.Context, peer meshid.Device, port uint16) (net.Conn, error) {
		if peer != target || port != 28000 {
			t.Fatal("wrong selected service", peer, port)
		}
		dials.Add(1)
		client, server := net.Pipe()
		go func() { defer server.Close(); _, _ = io.Copy(server, server) }()
		return client, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	flow, err := openOperatorApp(ctx, cc, MeshSelector(target), 28000, nil)
	if err != nil {
		t.Fatal("retained target lost behind public truncation", err)
	}
	defer flow.Close()
	payload := []byte("selected retained guest service")
	written := make(chan error, 1)
	go func() { _, err := flow.Write(payload); written <- err }()
	got := make([]byte, len(payload))
	if _, err = io.ReadFull(flow, got); err != nil {
		t.Fatal(err)
	}
	if err = <-written; err != nil || !bytes.Equal(payload, got) || dials.Load() != 1 {
		t.Fatal("selected service resolution", err)
	}
	// Ordinary route loss is still respected by the target-filtered view.
	snapshot.Routes[len(snapshot.Routes)-1].Unreachable = true
	if next, e := openOperatorApp(ctx, cc, MeshSelector(target), 28000, nil); e == nil {
		next.Close()
		t.Fatal("lost selected route remained connectable")
	}
	if dials.Load() != 1 {
		t.Fatal("route loss opened a new app TLS session")
	}
}

type operatorAppObservedListener struct {
	net.Listener
	remote chan string
}

func (l *operatorAppObservedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.remote <- conn.RemoteAddr().String()
	}
	return conn, err
}

func (a *operatorAppAuthorizer) DialAuthorized(port uint16, dial func(string) (net.Conn, error)) (net.Conn, error) {
	if port != a.port {
		return nil, meshsession.ErrDenied
	}
	a.called.Store(true)
	return dial(a.address)
}

// The native C app actor is separately composed with this exact TLS/WAS1
// caller. This fixture joins that production caller to the private broker,
// real userspace source address, destination TLS and running-app authorizer.
func TestOperatorDeclaredLiteAppUsesRetainedIdentityTLSAndWAS1(t *testing.T) {
	for _, algorithm := range []string{"P256", "MLDSA65"} {
		t.Run(algorithm, func(t *testing.T) {
			r, cc, activity, mutate := liteManagementFixture(t)
			_, _, target := remoteTopologyFixture(t)
			root, rootKey := liteManagementCertificate(t, nil, nil, "root", "", true)
			operator, p256 := liteManagementCertificate(t, root, rootKey, "operator", "spiffe://wendy.sh/tenant/"+target.Tenant().String()+"/operator/alice", false)
			var signer crypto.Signer = p256
			if algorithm == "MLDSA65" {
				key, err := mldsa.GenerateKey(mldsa.MLDSA65())
				if err != nil {
					t.Fatal(err)
				}
				operator.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
				der, err := x509.CreateCertificate(rand.Reader, operator, root, key.Public(), rootKey)
				if err != nil {
					t.Fatal(err)
				}
				operator, err = x509.ParseCertificate(der)
				if err != nil {
					t.Fatal(err)
				}
				signer = key
			}
			device, deviceKey := liteManagementCertificate(t, root, rootKey, "Lite", target.Principal(), false)
			encode := func(kind string, der []byte) string {
				return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
			}
			private := func(key crypto.Signer) string {
				t.Helper()
				der, err := x509.MarshalPKCS8PrivateKey(key)
				if err != nil {
					t.Fatal(err)
				}
				return encode("PRIVATE KEY", der)
			}
			chain := encode("CERTIFICATE", root.Raw)
			primary, err := localmesh.NewOperatorCredentials(encode("CERTIFICATE", operator.Raw), chain, private(signer), uuid.New())
			if err != nil {
				t.Fatal(err)
			}
			peer, err := localmesh.NewCredentials(encode("CERTIFICATE", device.Raw), chain, private(deviceKey))
			if err != nil {
				t.Fatal(err)
			}
			r.certificate = config.CertificateInfo{PemCertificate: encode("CERTIFICATE", operator.Raw)}
			var verified atomic.Bool
			verify := peer.Verify
			peer.Verify = func(chain [][]byte, now time.Time) (localmesh.Identity, error) {
				identity, err := verify(chain, now)
				if err == nil && identity.Device.IsOperator() && identity.Device.Principal() == primary.Device.Principal() && bytes.Equal(chain[0], operator.Raw) {
					verified.Store(true)
				}
				return identity, err
			}
			source, err := netmesh.NewNetwork(primary.Device)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			destination, err := netmesh.NewNetwork(target)
			if err != nil {
				t.Fatal(err)
			}
			defer destination.Close()
			left, err := source.AddPeer(1, target)
			if err != nil {
				t.Fatal(err)
			}
			right, err := destination.AddPeer(1, primary.Device)
			if err != nil {
				t.Fatal(err)
			}
			defer left.Close()
			defer right.Close()
			pumps := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(left, right); pumps <- struct{}{} }()
			go func() { _, _ = io.Copy(right, left); pumps <- struct{}{} }()
			defer func() { left.Close(); right.Close(); <-pumps; <-pumps }()
			if err = source.Apply([]babel.Route{{Prefix: netip.PrefixFrom(target.MeshAddress(), 128), Link: 1}}); err != nil {
				t.Fatal(err)
			}
			if err = destination.Apply([]babel.Route{{Prefix: netip.PrefixFrom(primary.Device.MeshAddress(), 128), Link: 1}}); err != nil {
				t.Fatal(err)
			}
			session := &operatormesh.Session{Node: &localmesh.Node{Credentials: primary}, Endpoint: source}
			r.appDial = session.DialAppTLS
			app, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer app.Close()
			appDone := make(chan error, 1)
			go func() {
				c, e := app.Accept()
				if e != nil {
					appDone <- e
					return
				}
				defer c.Close()
				_, e = io.Copy(c, c)
				appDone <- e
			}()
			authorizer := &operatorAppAuthorizer{port: 28000, address: app.Addr().String()}
			server, err := meshsession.NewServer(peer, authorizer)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			listener, err := destination.ListenTCP(meshsession.TLSPort)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			served := make(chan error, 1)
			observed := &operatorAppObservedListener{Listener: listener, remote: make(chan string, 1)}
			go func() { served <- server.ServeTLS(ctx, observed) }()
			defer func() {
				cancel()
				server.Close()
				if err := <-served; err != nil {
					t.Error(err)
				}
			}()
			r.serviceTopology = r.topology
			mutate(func(view *pb.LocalMeshTopology) {
				view.ServicesIncluded = true
				view.Announcements[0].Services = []*pb.LocalMeshService{{Mesh: "lab", AppId: "guest", ServiceId: "echo", Type: "_echo._tcp", Port: 28000}}
			})
			setup, stop := context.WithTimeout(ctx, 5*time.Second)
			conn, err := openOperatorApp(setup, cc, MeshSelector(target), 28000, nil)
			stop() // Setup cancellation must not retire the established flow.
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			payload := bytes.Repeat([]byte("declared Lite guest via real operator TLS/WAS1\n"), 2048)
			written := make(chan error, 1)
			go func() { _, err := conn.Write(payload); written <- err }()
			got := make([]byte, len(payload))
			if _, err = io.ReadFull(conn, got); err != nil {
				t.Fatal(err)
			}
			if err = <-written; err != nil || !bytes.Equal(payload, got) || !verified.Load() || !authorizer.called.Load() {
				t.Fatal("missing actual primary TLS/WAS1 app path", err)
			}
			select {
			case remote := <-observed.remote:
				host, _, err := net.SplitHostPort(remote)
				if err != nil || host != primary.Device.MeshAddress().String() {
					t.Fatal("app lost its actual operator source", remote, err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			conn.Close()
			select {
			case err := <-appDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			for until := time.Now().Add(time.Second); activity.activeRPCs.Load() != 0 && time.Now().Before(until); {
				time.Sleep(time.Millisecond)
			}
			if activity.activeRPCs.Load() != 0 {
				t.Fatal("app flow retained broker lease")
			}
			// Ordinary application shutdown removes its published port; a fresh
			// local connection must refresh the same manifest before starting TLS.
			mutate(func(view *pb.LocalMeshTopology) {
				view.Announcements[0].Services = nil
				view.Announcements[0].Revision++
			})
			if flow, e := openOperatorApp(ctx, cc, MeshSelector(target), 28000, nil); e == nil {
				flow.Close()
				t.Fatal("stopped service remained connectable")
			}
			t.Log(fmt.Sprintf("%s actual primary TLS/WAS1, %d bytes, 5680 packet-network source, setup lifetime and manifest withdrawal PASS", algorithm, len(payload)))
		})
	}
}
