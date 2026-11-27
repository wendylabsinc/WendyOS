package appservice

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

type testClock struct {
	mu     sync.Mutex
	window trustedtime.Interval
	err    error
}

type testPeerPin struct{}

func (testPeerPin) Alive() error { return nil }
func (testPeerPin) Close() error { return nil }

func (c *testClock) Now() (trustedtime.Interval, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.window, c.err
}
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.window.Earliest = c.window.Earliest.Add(d)
	c.window.Latest = c.window.Latest.Add(d)
	c.window.Monotonic += d
}

type fixture struct {
	manager    *Manager
	scope      serviceorigin.Scope
	clock      *testClock
	mu         sync.Mutex
	publishers map[string]*serviceorigin.Publisher
	accepted   []serviceorigin.VerifiedRecord
	denied     map[string]bool
	epoch      uint64
	config     Config
}

func setup(t *testing.T, change func(*Config)) *fixture {
	t.Helper()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	identity := testidentity.NewWith(t, now, "spiffe://wendy.sh/tenant/"+testidentity.Tenant+"/device/22222222-2222-4222-8222-222222222222")
	certs, err := wendycert.NewDevice(wendycert.Config{Tenant: identity.Tenant, Roots: [][]byte{identity.RootDER}})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{clock: &testClock{window: trustedtime.Interval{Earliest: now, Latest: now.Add(time.Millisecond), BootID: "test-boot", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-authority"}}, publishers: map[string]*serviceorigin.Publisher{}, denied: map[string]bool{}}
	f.scope = serviceorigin.Scope{Tenant: identity.Tenant, Site: "office", Device: identity.Principal, App: "com.example.camera", Service: "detector-a", DeploymentGeneration: 1, ExecutionDomain: "simulation", Publish: []serviceorigin.PublishScope{{ComponentType: "wendy.pose.v1", Stream: "people"}}}
	root, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, "ww-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cfg := Config{Directory: dir, UID: os.Geteuid(), GID: os.Getegid(), Limits: Limits{AuthorityCheckInterval: 10 * time.Millisecond}}
	cfg.Factory = func(ctx context.Context, scope serviceorigin.Scope) (Publisher, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.epoch++
		p, err := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: scope, Epoch: f.epoch, Lifetime: 10 * time.Minute, Clock: f.clock, Certificates: certs, ChainDER: identity.ChainDER, DeviceKey: identity.Key})
		if err == nil {
			f.publishers[scope.Service] = p
		}
		return p, err
	}
	cfg.PeerCheck = func(ctx context.Context, p Peer, scope serviceorigin.Scope) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if p.PID != int32(os.Getpid()) || f.denied[scope.Service] {
			return ErrPeer
		}
		return nil
	}
	cfg.Accept = func(ctx context.Context, scope serviceorigin.Scope, signed serviceorigin.SignedRecord, fence *Fence) error {
		verifier, err := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: scope.Tenant, Site: scope.Site, ExecutionDomain: scope.ExecutionDomain, Certificates: certs, Clock: f.clock, Authorize: func(_ context.Context, claims serviceorigin.DelegationClaims, _ wendycert.DeviceIdentity) error {
			if !reflect.DeepEqual(claims.Scope, scope) {
				return serviceorigin.ErrDenied
			}
			return nil
		}})
		if err != nil {
			return err
		}
		verified, err := verifier.Verify(ctx, signed)
		if err != nil {
			return err
		}
		return fence.Commit(ctx, func(ctx context.Context) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.accepted = append(f.accepted, verified)
			return nil
		})
	}
	if change != nil {
		change(&cfg)
	}
	f.config = cfg
	f.manager, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "linux" {
		// Explicit non-production test seam. Linux uses real SO_PEERCRED.
		f.manager.peerCredentials = func(*net.UnixConn) (Peer, error) {
			return Peer{PID: int32(os.Getpid()), UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}, nil
		}
		f.manager.peerPin = func(*net.UnixConn) (peerPin, error) { return testPeerPin{}, nil }
	}
	t.Cleanup(func() { _ = f.manager.Close() })
	return f
}

func (f *fixture) mount(t *testing.T, scope serviceorigin.Scope) Mount {
	t.Helper()
	mount, err := f.manager.Ensure(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	return mount
}
func dial(t *testing.T, path string) (appv1.WorldAppServiceClient, *grpc.ClientConn) {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///private-world", grpc.WithTransportCredentials(ClientCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return appv1.NewWorldAppServiceClient(conn), conn
}
func request() *appv1.PublishRequest {
	return &appv1.PublishRequest{Entity: "person-1", ComponentType: "wendy.pose.v1", Stream: "people", Frame: &appv1.FrameRef{Id: "floor", Revision: "v1"}, Timing: &appv1.Timing{SampleUnixNs: 10_000_000_123, ClockDomain: "simulation:run", ErrorKnown: true}, TtlNs: uint64(2 * time.Second), Provenance: &appv1.Provenance{Origin: appv1.Origin_ORIGIN_SIMULATED, AppVersion: "detector-v1", Simulation: &appv1.SimulationContext{RunId: "run", ScenarioVersion: "v1", Seed: 42, ClockDomain: "simulation:run"}}, PayloadJson: []byte("{ \"position\" : [1,0,2] }\n")}
}
func publish(t *testing.T, c appv1.WorldAppServiceClient) *appv1.PublishReply {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	reply, err := c.Publish(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	if !reply.Accepted {
		t.Fatal("unaccepted publication")
	}
	return reply
}
func (f *fixture) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.accepted) }

func TestTwoSameAppServicesStampExactSourcesAndPreservePayload(t *testing.T) {
	f := setup(t, nil)
	a := f.mount(t, f.scope)
	bScope := f.scope
	bScope.Service = "detector-b"
	b := f.mount(t, bScope)
	if a.Directory == b.Directory {
		t.Fatal("services share identity mount")
	}
	if again := f.mount(t, f.scope); again != a {
		t.Fatal("unchanged Ensure replaced mount")
	}
	ca, _ := dial(t, a.SocketPath)
	cb, _ := dial(t, b.SocketPath)
	for _, c := range []appv1.WorldAppServiceClient{ca, cb} {
		reply := publish(t, c)
		var signed serviceorigin.SignedRecord
		if json.Unmarshal(reply.SignedRecordJson, &signed) != nil {
			t.Fatal("invalid signed response")
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.accepted) != 2 {
		t.Fatal("missing accepted publications")
	}
	for i, name := range []string{"detector-a", "detector-b"} {
		component := f.accepted[i].Component()
		if component.Key.Source.Service != name || component.Key.Source.App != f.scope.App || component.Key.Source.Device != f.scope.Device || string(component.Payload) != string(request().PayloadJson) {
			t.Fatalf("source or payload changed: %+v", component)
		}
	}
}

func TestUnaryAndStreamRejectForgeryAndUnknownFields(t *testing.T) {
	f := setup(t, nil)
	mount := f.mount(t, f.scope)
	client, _ := dial(t, mount.SocketPath)
	bad := request()
	bad.Stream = "another-service"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.Publish(ctx, bad); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("forged stream accepted: %v", err)
	}
	unknown := request()
	unknown.ProtoReflect().SetUnknown(protowire.AppendString(protowire.AppendTag(nil, 20, protowire.BytesType), "forged-source"))
	if _, err := client.Publish(ctx, unknown); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown identity field accepted: %v", err)
	}
	stream, err := client.PublishStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = stream.Send(request()); err != nil {
		t.Fatal(err)
	}
	if reply, err := stream.Recv(); err != nil || !reply.Accepted {
		t.Fatal("valid stream input failed", err)
	}
	if err = stream.Send(bad); err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("stream forgery accepted: %v", err)
	}
	if f.count() != 1 {
		t.Fatal("forged record reached sink")
	}
}

func TestRevocationAndReplacementCloseActiveStreams(t *testing.T) {
	for _, action := range []string{"release", "replace", "release-app", "signing-invalidated", "clock-expired"} {
		t.Run(action, func(t *testing.T) {
			f := setup(t, nil)
			old := f.mount(t, f.scope)
			client, _ := dial(t, old.SocketPath)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			stream, err := client.PublishStream(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = stream.Send(request()); err != nil {
				t.Fatal(err)
			}
			if _, err = stream.Recv(); err != nil {
				t.Fatal(err)
			}
			switch action {
			case "release":
				f.manager.Release(f.scope.App, f.scope.Service)
			case "replace":
				replacement := f.scope
				replacement.DeploymentGeneration++
				mount := f.mount(t, replacement)
				if mount.Directory == old.Directory {
					t.Fatal("replacement reused old mount")
				}
				fresh, _ := dial(t, mount.SocketPath)
				publish(t, fresh)
			case "release-app":
				f.manager.ReleaseApp(f.scope.App)
			case "signing-invalidated":
				f.mu.Lock()
				p := f.publishers[f.scope.Service]
				f.mu.Unlock()
				_ = p.Close()
			case "clock-expired":
				f.clock.advance(11 * time.Minute)
			}
			if _, err = stream.Recv(); err == nil || status.Code(err) == codes.DeadlineExceeded {
				t.Fatalf("active stream survived invalidation until caller deadline: %v", err)
			}
			if _, err = os.Stat(old.SocketPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("old socket remains: %v", err)
			}
		})
	}
}

func TestEveryStreamMessageRechecksExactPeer(t *testing.T) {
	f := setup(t, nil)
	mount := f.mount(t, f.scope)
	client, _ := dial(t, mount.SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := client.PublishStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = stream.Send(request()); err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.denied[f.scope.Service] = true
	f.mu.Unlock()
	if err = stream.Send(request()); err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("changed service peer retained stream authority: %v", err)
	}
	if f.count() != 1 {
		t.Fatal("unauthorized second message admitted")
	}
}

func TestRestartUsesNewSocketAndPublisherEpoch(t *testing.T) {
	f := setup(t, nil)
	old := f.mount(t, f.scope)
	client, _ := dial(t, old.SocketPath)
	publish(t, client)
	f.manager.Release(f.scope.App, f.scope.Service)
	fresh := f.mount(t, f.scope)
	if fresh == old {
		t.Fatal("restart reused socket")
	}
	next, _ := dial(t, fresh.SocketPath)
	publish(t, next)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.accepted[1].Record.Epoch <= f.accepted[0].Record.Epoch || f.accepted[1].Record.Sequence != 1 {
		t.Fatal("factory restart epoch was not respected")
	}
}

func TestBudgetsAndFailClosedDependencies(t *testing.T) {
	t.Run("rate", func(t *testing.T) {
		f := setup(t, func(cfg *Config) { cfg.Limits.MessagesPerSecond = .0001; cfg.Limits.MessageBurst = 2 })
		client, _ := dial(t, f.mount(t, f.scope).SocketPath)
		publish(t, client)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := client.Publish(ctx, request()); status.Code(err) != codes.ResourceExhausted {
			t.Fatal(err)
		}
		if f.count() != 1 {
			t.Fatal("over-budget admission")
		}
	})
	t.Run("response", func(t *testing.T) {
		f := setup(t, func(cfg *Config) { cfg.Limits.MaxResponseBytes = 1024 })
		client, _ := dial(t, f.mount(t, f.scope).SocketPath)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := client.Publish(ctx, request()); status.Code(err) != codes.ResourceExhausted {
			t.Fatal(err)
		}
		if f.count() != 0 {
			t.Fatal("oversized response committed before budget check")
		}
	})
	t.Run("request", func(t *testing.T) {
		f := setup(t, func(cfg *Config) { cfg.Limits.MaxRequestBytes = 1024 })
		client, _ := dial(t, f.mount(t, f.scope).SocketPath)
		large := proto.Clone(request()).(*appv1.PublishRequest)
		large.PayloadJson = make([]byte, 2048)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := client.Publish(ctx, large); status.Code(err) != codes.ResourceExhausted {
			t.Fatal(err)
		}
		if f.count() != 0 {
			t.Fatal("oversized input admitted")
		}
	})
	t.Run("clock", func(t *testing.T) {
		f := setup(t, nil)
		f.clock.mu.Lock()
		f.clock.err = trustedtime.ErrUntrusted
		f.clock.mu.Unlock()
		if _, err := f.manager.Ensure(context.Background(), f.scope); err == nil {
			t.Fatal("untrusted physical clock replaced by local wall time")
		}
	})
	t.Run("missing-peer", func(t *testing.T) {
		f := setup(t, nil)
		cfg := f.config
		cfg.PeerCheck = nil
		if _, err := New(cfg); err == nil {
			t.Fatal("missing peer checker accepted")
		}
	})
	t.Run("unsupported-peer", func(t *testing.T) {
		f := setup(t, nil)
		f.manager.peerCredentials = func(*net.UnixConn) (Peer, error) { return Peer{}, ErrPeer }
		client, _ := dial(t, f.mount(t, f.scope).SocketPath)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if _, err := client.Publish(ctx, request()); err == nil {
			t.Fatal("failed kernel identity check bypassed")
		}
		if f.count() != 0 {
			t.Fatal("unknown peer admitted")
		}
	})
}
