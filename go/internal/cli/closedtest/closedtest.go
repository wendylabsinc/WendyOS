// Package closedtest runs an agent's access surface for CLI tests: the real
// trusted-key TLS listener of a Closed device on port+1, its plaintext
// listener on port, the role check in front of both, and the real access
// service. Only tests import it.
package closedtest

import (
	"context"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Device is one running device. Restarts counts the access service asking
// the agent to restart; a real agent then serves the new shape, this one only
// after Restart.
type Device struct {
	// Addr is the plaintext port (what discovery advertises); the key port is
	// Addr's port + 1.
	Addr     string
	Keys     *trustedkeys.Store
	Restarts atomic.Int32

	dir      string
	servers  []*grpc.Server
	register []func(*grpc.Server)
}

// Start runs a device whose access file is at dir/trusted-keys.json, Closed
// when that file exists and Open otherwise. register adds agent services
// (behind the same role check) to every listener.
func Start(t testing.TB, dir string, register ...func(*grpc.Server)) *Device {
	t.Helper()
	d := &Device{dir: dir, register: register}
	d.start(t, "127.0.0.1:0")
	return d
}

// Restart stops the device and starts it again on the same ports from its
// config dir, as the agent restarts into a new shape.
func (d *Device) Restart(t testing.TB) {
	t.Helper()
	for _, s := range d.servers {
		s.Stop()
	}
	d.servers = nil
	d.start(t, d.Addr)
}

func (d *Device) start(t testing.TB, addr string) {
	t.Helper()
	keys, err := trustedkeys.Open(filepath.Join(d.dir, trustedkeys.FileName))
	if err != nil {
		t.Fatal(err)
	}
	d.Keys = keys
	access := services.NewAccessService(zap.NewNop(), keys, func() bool { return false }, func() { d.Restarts.Add(1) })

	plainLn, keyLn := listenPair(t, addr)
	d.Addr = plainLn.Addr().String()
	var plainOpts []grpc.ServerOption
	if keys.Closed() {
		plainOpts = authz(keys, interceptor.ClosedKeylessListener)
		certPEM, keyPEM, err := mtls.ClosedServerIdentity(d.dir, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		tlsCfg, err := mtls.NewTrustedKeyTLSConfig(certPEM, keyPEM, keys, trustedkeys.RoleViewer)
		if err != nil {
			t.Fatal(err)
		}
		d.serve(t, keyLn, access, append(authz(keys, interceptor.ClosedKeyListener), grpc.Creds(credentials.NewTLS(tlsCfg)))...)
	} else {
		keyLn.Close() //nolint:errcheck // an Open device has no key port
	}
	d.serve(t, plainLn, access, plainOpts...)
}

func authz(keys *trustedkeys.Store, l interceptor.ClosedListener) []grpc.ServerOption {
	a := &interceptor.ClosedAuthorizer{Logger: zap.NewNop(), Listener: l, Keys: keys}
	return []grpc.ServerOption{grpc.ChainUnaryInterceptor(a.Unary()), grpc.ChainStreamInterceptor(a.Stream())}
}

func (d *Device) serve(t testing.TB, ln net.Listener, access *services.AccessService, opts ...grpc.ServerOption) {
	srv := grpc.NewServer(opts...)
	d.servers = append(d.servers, srv)
	agentpb.RegisterWendyAgentServiceServer(srv, versionService{})
	agentpbv2.RegisterWendyAccessServiceServer(srv, access)
	for _, add := range d.register {
		add(srv)
	}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(srv.Stop)
}

// listenPair listens on addr's port and the next one, as an agent serves
// plaintext on its port and TLS on port + 1. Port 0 picks a free pair.
func listenPair(t testing.TB, addr string) (net.Listener, net.Listener) {
	t.Helper()
	for range 50 {
		a, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		port := a.Addr().(*net.TCPAddr).Port
		b, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port+1)))
		if err == nil {
			return a, b
		}
		a.Close() //nolint:errcheck
		if !strings.HasSuffix(addr, ":0") {
			t.Fatal(err)
		}
	}
	t.Fatal("no two consecutive free loopback ports")
	return nil, nil
}

type versionService struct {
	agentpb.UnimplementedWendyAgentServiceServer
}

func (versionService) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	return &agentpb.GetAgentVersionResponse{Version: "test"}, nil
}
