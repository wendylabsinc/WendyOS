package appservice

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc"
)

type watcherPin struct {
	checks      atomic.Int32
	closes      atomic.Int32
	err         error
	observation pinObservation
	onClose     func()
}

func (p *watcherPin) Alive() error { err, _ := p.aliveDiagnostic(); return err }
func (p *watcherPin) aliveDiagnostic() (error, pinObservation) {
	p.checks.Add(1)
	return p.err, p.observation
}
func (p *watcherPin) Close() error {
	p.closes.Add(1)
	if p.onClose != nil {
		p.onClose()
	}
	return nil
}

func watcherDirectory(t *testing.T) string {
	t.Helper()
	// t.TempDir includes the test and subtest names, which can exceed the
	// Unix socket path limit on macOS. Match the broker fixture's short path.
	root, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, "ww-watch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func watcherConnection(t *testing.T, s *service, pin peerPin) *trackedConn {
	t.Helper()
	path := filepath.Join(watcherDirectory(t), "watch.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	server, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	conn := &trackedConn{UnixConn: server, service: s, pin: pin}
	s.connections = map[*trackedConn]struct{}{conn: {}}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestRefusalPeerWatcherReportsAfterCloseAndOnlyWhenNeeded(t *testing.T) {
	for _, mode := range []string{"healthy", "recovered-interruption", "dead", "interrupt-limit"} {
		for _, enabled := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "-off", true: "-on"}[enabled], func(t *testing.T) {
				sink := traceSink(t, 8)
				s := &service{manager: &Manager{}, ctx: context.Background(), scope: serviceorigin.Scope{App: "app", Service: "sensor", DeploymentGeneration: 1}}
				if enabled {
					s.manager.cfg.Trace = sink
				}
				pin := &watcherPin{}
				if mode == "recovered-interruption" {
					pin.observation.interruptions = 2
				}
				if mode == "dead" {
					pin.err = ErrPeer
					pin.observation.reason = rpctrace.ReasonPinDead
				}
				if mode == "interrupt-limit" {
					pin.err = ErrPeer
					pin.observation = pinObservation{reason: rpctrace.ReasonPinInterruptLimit, interruptions: 8}
				}
				conn := watcherConnection(t, s, pin)
				pin.onClose = func() {
					if sink.Stats().Enqueued != 0 {
						t.Error("watch event emitted before connection close completed")
					}
					if enabled && conn.watcherReason.Load() != uint32(pin.observation.reason) {
						t.Error("watch reason was not retained before close")
					}
				}
				s.closeDepartedPeers()
				pin.onClose = nil
				if pin.checks.Load() != 1 || pin.closes.Load() != map[bool]int32{false: 0, true: 1}[pin.err != nil] {
					t.Fatal("watcher changed liveness or close calls")
				}
				if !enabled && conn.watcherReason.Load() != 0 {
					t.Fatal("nil trace wrote diagnostic history")
				}
				rows := traceRows(t, sink)
				wantEvent := enabled && mode != "healthy"
				if !wantEvent {
					if len(rows) != 0 {
						t.Fatal("uneventful/nil watcher emitted")
					}
					return
				}
				if len(rows) != 1 || rows[0].Method != "PeerWatch" || len(rows[0].Stages) != 1 || rows[0].Stages[0].Stage != "total" {
					t.Fatal("watch event fabricated measured work")
				}
				if pin.err == nil {
					if rows[0].Code != "OK" || rows[0].Refusal != nil {
						t.Fatal("recovered interruption treated as refusal")
					}
				} else if rows[0].Code != "Unavailable" || rows[0].Refusal == nil || rows[0].Refusal.Point != "peer_watcher" {
					t.Fatal("watcher refusal missing")
				}
				if pin.observation.interruptions != 0 && (len(rows[0].PinInterruptions) != 1 || rows[0].PinInterruptions[0].Count != uint64(pin.observation.interruptions)) {
					t.Fatal("watcher interruption count lost")
				}
			})
		}
	}
}

func TestRefusalPeerWatcherCloseRaceRetainsFirstObservation(t *testing.T) {
	sink := traceSink(t, 8)
	s := &service{manager: &Manager{cfg: Config{Trace: sink}}, ctx: context.Background(), scope: serviceorigin.Scope{App: "app", Service: "sensor", DeploymentGeneration: 1}}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	pin := &watcherPin{err: ErrPeer, observation: pinObservation{reason: rpctrace.ReasonPinDead}}
	conn := watcherConnection(t, s, pin)
	t.Cleanup(open)
	pin.onClose = func() { close(entered); <-release }
	done := make(chan struct{})
	go func() { s.closeDepartedPeers(); close(done) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("watcher close did not enter")
	}
	if conn.watcherReason.Load() != uint32(rpctrace.ReasonPinDead) || sink.Stats().Enqueued != 0 {
		t.Fatal("watcher observation ordering changed")
	}
	other := make(chan struct{})
	go func() { _ = conn.Close(); close(other) }()
	open()
	for _, ch := range []<-chan struct{}{done, other} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("concurrent close did not finish")
		}
	}
	if pin.closes.Load() != 1 || conn.watcherReason.Load() != uint32(rpctrace.ReasonPinDead) {
		t.Fatal("racing close changed retained observation or repeated close")
	}
	if rows := traceRows(t, sink); len(rows) != 1 {
		t.Fatal("watcher close race lost its bounded event")
	}
}

type watcherPublisher struct {
	Publisher
	done                chan struct{}
	validations, closes atomic.Int32
	validate            func(context.Context) error
	onClose             func()
}

func (p *watcherPublisher) Done() <-chan struct{} { return p.done }
func (p *watcherPublisher) Validate(ctx context.Context) error {
	p.validations.Add(1)
	return p.validate(ctx)
}
func (p *watcherPublisher) Close() error {
	p.closes.Add(1)
	if p.onClose != nil {
		p.onClose()
	}
	return nil
}

func TestRefusalAuthorityWatcherCapturesContextBeforeCancel(t *testing.T) {
	for _, mode := range []string{"done", "refused", "deadline"} {
		for _, enabled := range []bool{false, true} {
			t.Run(mode+map[bool]string{false: "-off", true: "-on"}[enabled], func(t *testing.T) {
				sink := traceSink(t, 8)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				dir := filepath.Join(watcherDirectory(t), "service")
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "s.sock"), Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				p := &watcherPublisher{done: make(chan struct{}), validate: func(context.Context) error { return errors.New("unexported publisher error") }}
				reason := "publisher_refused"
				if mode == "done" {
					close(p.done)
					reason = "publisher_done"
				}
				if mode == "deadline" {
					p.validate = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
					reason = "context_deadline"
				}
				s := &service{manager: &Manager{cfg: Config{Limits: Limits{AuthorityCheckInterval: time.Millisecond, RequestTimeout: 5 * time.Millisecond}}}, ctx: ctx, cancel: cancel, publisher: p, listener: listener, server: grpc.NewServer(), mount: Mount{Directory: dir}, connections: map[*trackedConn]struct{}{}, scope: serviceorigin.Scope{App: "app", Service: "reader", DeploymentGeneration: 1}}
				if mode == "done" {
					// Keep a delayed test goroutine from making both select cases
					// ready before it observes the already-closed Done channel.
					s.manager.cfg.Limits.AuthorityCheckInterval = time.Hour
				}
				if enabled {
					s.manager.cfg.Trace = sink
				}
				p.onClose = func() {
					if sink.Stats().Enqueued != 0 {
						t.Error("watch event emitted inside stop")
					}
					if enabled && s.watcherReason.Load() == 0 {
						t.Error("watch reason absent before stop")
					}
				}
				done := make(chan struct{})
				go func() { s.watchAuthority(); close(done) }()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("authority watcher failed to stop")
				}
				wantValidations := int32(1)
				if mode == "done" {
					wantValidations = 0
				}
				if p.closes.Load() != 1 || p.validations.Load() != wantValidations {
					t.Fatal("watcher changed publisher/close calls")
				}
				rows := traceRows(t, sink)
				if !enabled {
					if len(rows) != 0 || s.watcherReason.Load() != 0 {
						t.Fatal("nil trace wrote watcher diagnostics")
					}
					return
				}
				if len(rows) != 1 || rows[0].Method != "AuthorityWatch" || rows[0].Refusal == nil || rows[0].Refusal.Reason != reason {
					t.Fatalf("wrong authority watch cause: %+v", rows)
				}
			})
		}
	}
}
