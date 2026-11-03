package backend

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeUnitreeControl struct {
	mu                   sync.Mutex
	moves, stops, closes int
	fail                 bool
	stopped              chan struct{}
}

func (f *fakeUnitreeControl) Move(context.Context, float64, float64, float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.moves++
	if f.fail {
		return errors.New("lost ack")
	}
	return nil
}
func (f *fakeUnitreeControl) Stop(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	select {
	case f.stopped <- struct{}{}:
	default:
	}
	return nil
}
func (f *fakeUnitreeControl) Close() error    { f.mu.Lock(); defer f.mu.Unlock(); f.closes++; return nil }
func (f *fakeUnitreeControl) Connected() bool { return true }
func newUnitreeControlTest() (*UnitreeController, *fakeUnitreeControl) {
	f := &fakeUnitreeControl{stopped: make(chan struct{}, 10)}
	return &UnitreeController{handle: 1, link: f, lease: 20 * time.Millisecond}, f
}
func TestUnitreeControlLeaseStopsAbandonedMove(t *testing.T) {
	c, f := newUnitreeControlTest()
	defer c.Close(1)
	if err := c.Move(1, .6, 0, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.stopped:
	case <-time.After(time.Second):
		t.Fatal("move did not expire")
	}
}
func TestUnitreeControlStopsOnFailureAndInvalidMotion(t *testing.T) {
	for _, test := range []struct {
		name     string
		velocity float64
		fail     bool
	}{{"failed ack", .6, true}, {"NaN", math.NaN(), false}, {"out of range", 4, false}} {
		t.Run(test.name, func(t *testing.T) {
			c, f := newUnitreeControlTest()
			defer c.Close(1)
			f.fail = test.fail
			if c.Move(1, test.velocity, 0, 0) == nil {
				t.Fatal("expected error")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.stops != 1 {
				t.Fatalf("stops=%d", f.stops)
			}
		})
	}
}
func TestUnitreeControlRejectsStaleHandle(t *testing.T) {
	c, f := newUnitreeControlTest()
	defer c.Close(1)
	if c.Move(2, .6, 0, 0) == nil {
		t.Fatal("stale move accepted")
	}
	if err := c.Close(2); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.moves != 0 || f.stops != 0 || f.closes != 0 {
		t.Fatal("stale caller affected current robot")
	}
}
func TestUnitreeControlReleaseCancelsLease(t *testing.T) {
	c, f := newUnitreeControlTest()
	defer c.Close(1)
	if err := c.Move(1, .6, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.Move(1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	<-f.stopped
	select {
	case <-f.stopped:
		t.Fatal("expired lease sent another stop")
	case <-time.After(60 * time.Millisecond):
	}
}
func TestUnitreeControlCloseStopsAndRejectsMoves(t *testing.T) {
	c, f := newUnitreeControlTest()
	if err := c.Close(1); err != nil {
		t.Fatal(err)
	}
	if c.Move(1, .6, 0, 0) == nil {
		t.Fatal("closed control accepted move")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closes != 1 || f.stops != 1 {
		t.Fatalf("close=%d stop=%d", f.closes, f.stops)
	}
}
func TestUnitreeTargetValidation(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "224.0.0.1", "0.0.0.0", "::1", "robot.local", "192.168.1.1:8080"} {
		if validateUnitreeTarget("GO2-123", address) == nil {
			t.Errorf("accepted %s", address)
		}
	}
	if err := validateUnitreeTarget("GO2-123", "192.168.1.20"); err != nil {
		t.Fatal(err)
	}
	if validateUnitreeTarget("", "192.168.1.20") == nil {
		t.Fatal("accepted empty serial")
	}
}
func TestUnitreePairingRejectsStaleAgentSession(t *testing.T) {
	b := New()
	if _, err := b.UnitreePairings(99); err == nil {
		t.Fatal("stale list accepted")
	}
	if err := b.PairUnitree(99, "GO2-123", "Go2", "192.168.1.20", "global", "", ""); err == nil {
		t.Fatal("stale pair accepted")
	}
	if err := b.ForgetUnitree(99, "GO2-123"); err == nil {
		t.Fatal("stale forget accepted")
	}
}

type desktopUnitreeService struct {
	pb.WendyUnitreeServiceClient
	listed, found             []*pb.UnitreeRobot
	listErr, scanErr, pairErr error
	request                   *pb.PairUnitreeRequest
	known                     []string
}

func (f *desktopUnitreeService) ListUnitreePairings(context.Context, *pb.ListUnitreePairingsRequest, ...grpc.CallOption) (*pb.ListUnitreePairingsResponse, error) {
	return &pb.ListUnitreePairingsResponse{Robots: f.listed}, f.listErr
}
func (f *desktopUnitreeService) DiscoverUnitreeRobots(_ context.Context, r *pb.DiscoverUnitreeRobotsRequest, _ ...grpc.CallOption) (*pb.DiscoverUnitreeRobotsResponse, error) {
	f.known = r.KnownSerials
	return &pb.DiscoverUnitreeRobotsResponse{Robots: f.found}, f.scanErr
}
func (f *desktopUnitreeService) PairUnitree(_ context.Context, r *pb.PairUnitreeRequest, _ ...grpc.CallOption) (*pb.PairUnitreeResponse, error) {
	f.request = r
	return &pb.PairUnitreeResponse{Robot: &pb.UnitreeRobot{Serial: r.Serial, Paired: true}}, f.pairErr
}
func unitreePairingBackend(f *desktopUnitreeService) *Backend {
	b := New()
	b.session = 1
	b.conn = &grpcclient.AgentConnection{UnitreeService: f, IsMTLS: true}
	return b
}
func TestUnitreePairingsMergeUsesSavedConnectionStatus(t *testing.T) {
	f := &desktopUnitreeService{
		listed: []*pb.UnitreeRobot{{Serial: "saved", Name: "My Go2", Address: "192.168.1.2", Paired: true, Connected: false}},
		found:  []*pb.UnitreeRobot{{Serial: "saved", Name: "Unitree Go2", Address: "192.168.1.3", Connected: true}, {Serial: "new", Name: "Unitree Go2", Address: "192.168.1.4"}},
	}
	result, err := unitreePairingBackend(f).UnitreePairings(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Robots) != 2 {
		t.Fatalf("robots: %+v", result.Robots)
	}
	saved := result.Robots[1]
	if saved.Name != "My Go2" || saved.Address != "192.168.1.3" || !saved.Paired || saved.Connected {
		t.Fatalf("saved: %+v", saved)
	}
	if len(f.known) != 1 || f.known[0] != "saved" {
		t.Fatalf("known: %v", f.known)
	}
}
func TestUnitreePairingsRetainSavedRobotsOnScanFailure(t *testing.T) {
	f := &desktopUnitreeService{listed: []*pb.UnitreeRobot{{Serial: "saved", Paired: true}}, scanErr: errors.New("unavailable")}
	result, err := unitreePairingBackend(f).UnitreePairings(1)
	if err != nil || len(result.Robots) != 1 || result.Warning == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	f.listErr = errors.New("unauthenticated")
	if _, err := unitreePairingBackend(f).UnitreePairings(1); err == nil {
		t.Fatal("pairing state failure hidden")
	}
}
func TestUnitreePairRequiresAuthenticatedAgentAndRedactsFailures(t *testing.T) {
	f := &desktopUnitreeService{pairErr: errors.New("secret from server")}
	b := unitreePairingBackend(f)
	b.conn.IsMTLS = false
	if b.PairUnitree(1, "GO2-123", "Go2", "192.168.1.2", "global", "", "") == nil {
		t.Fatal("insecure transport accepted")
	}
	if f.request != nil {
		t.Fatal("sent pairing over insecure transport")
	}
	b.conn.IsMTLS = true
	err := b.PairUnitree(1, "GO2-123", "Go2", "192.168.1.2", "global", "", "")
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe error: %v", err)
	}
	if f.request == nil || f.request.Token != "" || f.request.EncryptionKey != "" {
		t.Fatal("unexpected credential in reconnect request")
	}
}

func TestUnitreeShutdownStopsAndClosesRobot(t *testing.T) {
	c, f := newUnitreeControlTest()
	c.Shutdown()
	if !c.closed || c.link != nil {
		t.Fatal("shutdown left controller open")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stops != 1 || f.closes != 1 {
		t.Fatalf("stop=%d close=%d", f.stops, f.closes)
	}
}
