package mcusource

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// wholeFrame is a frame sent as a single chunk.
func wholeFrame(channelID, seq uint32) *sensorlinkpb.SensorData {
	return &sensorlinkpb.SensorData{ChannelId: channelID, FrameSeq: seq, Flags: sensorlink.FlagLastChunk}
}

// fakeWendycomClient stands in for a Wendy Lite board's WendyCom connection.
// send plays the client's read loop: it hands a chunk to every listener
// synchronously, so a listener that blocks would hang the test.
type fakeWendycomClient struct {
	manifest *sensorlinkpb.SensorManifest

	mu           sync.Mutex
	listeners    map[int]func(*sensorlinkpb.SensorData)
	nextID       int
	lastListener func(*sensorlinkpb.SensorData) // kept after removal
	subscribed   [][]uint32
	unsubscribed [][]uint32
	subscribeErr error
	// onSubscribe runs inside SensorLinkSubscribe, before it returns: the
	// window in which a board may already be sending frames.
	onSubscribe func()
	closes      int

	done     chan struct{}
	dropOnce sync.Once
}

func newFakeWendycomClient() *fakeWendycomClient {
	return &fakeWendycomClient{
		manifest:  &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{{ChannelId: 1, Name: "cam0"}}},
		listeners: make(map[int]func(*sensorlinkpb.SensorData)),
		done:      make(chan struct{}),
	}
}

func (c *fakeWendycomClient) GetSensorManifest(time.Duration) (*sensorlinkpb.SensorManifest, error) {
	return c.manifest, nil
}

func (c *fakeWendycomClient) SensorLinkSubscribe(ids []uint32, _ time.Duration) error {
	c.mu.Lock()
	c.subscribed = append(c.subscribed, ids)
	hook, err := c.onSubscribe, c.subscribeErr
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (c *fakeWendycomClient) SensorLinkUnsubscribe(ids []uint32, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unsubscribed = append(c.unsubscribed, ids)
	return nil
}

func (c *fakeWendycomClient) AddSensorDataListener(fn func(*sensorlinkpb.SensorData)) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextID
	c.nextID++
	c.listeners[id] = fn
	c.lastListener = fn
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.listeners, id)
	}
}

func (c *fakeWendycomClient) send(d *sensorlinkpb.SensorData) {
	c.mu.Lock()
	fns := make([]func(*sensorlinkpb.SensorData), 0, len(c.listeners))
	for _, fn := range c.listeners {
		fns = append(fns, fn)
	}
	c.mu.Unlock()
	for _, fn := range fns {
		fn(d)
	}
}

func (c *fakeWendycomClient) listenerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.listeners)
}

func (c *fakeWendycomClient) unsubscribes() [][]uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.unsubscribed)
}

func (c *fakeWendycomClient) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

// dropOff simulates the board disappearing: the client's read loop dies.
func (c *fakeWendycomClient) dropOff() { c.dropOnce.Do(func() { close(c.done) }) }

func (c *fakeWendycomClient) Done() <-chan struct{} { return c.done }

func (c *fakeWendycomClient) Close() error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
	c.dropOff()
	return nil
}

func TestNewWendyComTransportRejectsMissingIdentity(t *testing.T) {
	for _, tc := range []struct{ name, cert, key string }{
		{"no certificate", "", "key"},
		{"no key", "cert", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewWendyComTransport(zap.NewNop(), tc.cert, "chain", tc.key, SensorPairing{SourceAssetID: 7}, "board:5054"); err == nil {
				t.Fatal("expected an error when the agent has no mTLS identity")
			}
		})
	}
}

func TestNewWendyComTransportRejectsMissingChain(t *testing.T) {
	if _, err := NewWendyComTransport(zap.NewNop(), "cert", "", "key", SensorPairing{SourceAssetID: 7}, "board:5054"); err == nil {
		t.Fatal("expected an error when the agent has no CA chain to verify the board against")
	}
}

// Credentials are parsed at connect time, as mtlsDialer does, so a broken
// identity surfaces from FetchManifest before anything is dialed.
func TestWendyComTransportReportsBadClientCertificate(t *testing.T) {
	tr, err := NewWendyComTransport(zap.NewNop(), "cert", "chain", "key", SensorPairing{SourceAssetID: 7}, "board.invalid:5054")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := tr.FetchManifest(ctx); err == nil || !strings.Contains(err.Error(), "wendycom client certificate") {
		t.Fatalf("FetchManifest: got %v, want a client certificate error", err)
	}
}

func newTestWendycomTransport(logger *zap.Logger, c *fakeWendycomClient) (*wendycomTransport, *atomic.Int32) {
	var connects atomic.Int32
	return &wendycomTransport{logger: logger, connect: func() (wendycomClient, error) {
		connects.Add(1)
		return c, nil
	}}, &connects
}

// requireClosed fails unless frames is closed without yielding a frame.
func requireClosed(t *testing.T, frames <-chan *sensorlink.SensorFrame) {
	t.Helper()
	select {
	case f, ok := <-frames:
		if ok {
			t.Fatalf("got frame %v, want a closed stream", f)
		}
	case <-time.After(time.Second):
		t.Fatal("frames not closed")
	}
}

func TestWendycomTransportSharesOneConnection(t *testing.T) {
	c := newFakeWendycomClient()
	tr, connects := newTestWendycomTransport(zap.NewNop(), c)
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m, err := tr.FetchManifest(ctx)
	if err != nil || len(m.GetSensors()) != 1 {
		t.Fatalf("FetchManifest: got %v, %v", m, err)
	}
	frames, closeStream, err := tr.Stream(context.Background(), []uint32{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStream()
	if n := connects.Load(); n != 1 {
		t.Fatalf("connected %d times, want 1", n)
	}
	if !slices.EqualFunc(c.subscribed, [][]uint32{{1, 2}}, slices.Equal) {
		t.Fatalf("subscribed %v, want [[1 2]]", c.subscribed)
	}

	c.send(wholeFrame(2, 5))
	select {
	case f := <-frames:
		if f.ChannelID != 2 || f.Seq != 5 {
			t.Fatalf("got frame channel=%d seq=%d, want channel=2 seq=5", f.ChannelID, f.Seq)
		}
	default:
		t.Fatal("frame not delivered")
	}
}

func TestWendycomTransportListensBeforeSubscribing(t *testing.T) {
	c := newFakeWendycomClient()
	c.onSubscribe = func() { c.send(wholeFrame(1, 1)) }
	tr, _ := newTestWendycomTransport(zap.NewNop(), c)
	defer tr.Close()

	frames, closeStream, err := tr.Stream(context.Background(), []uint32{1})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStream()
	select {
	case f := <-frames:
		if f.Seq != 1 {
			t.Fatalf("got seq %d, want 1", f.Seq)
		}
	default:
		t.Fatal("a frame sent while Subscribe was in flight was lost")
	}
}

// A Wendy Lite board splits a frame into chunks; the stream yields it whole.
func TestWendycomTransportAssemblesChunks(t *testing.T) {
	c := newFakeWendycomClient()
	tr, _ := newTestWendycomTransport(zap.NewNop(), c)
	defer tr.Close()

	frames, closeStream, err := tr.Stream(context.Background(), []uint32{1})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStream()
	c.send(&sensorlinkpb.SensorData{ChannelId: 1, FrameSeq: 4, ChunkSeq: 0, TsUs: 99, Flags: sensorlink.FlagKeyframe, Payload: []byte("ab")})
	c.send(&sensorlinkpb.SensorData{ChannelId: 1, FrameSeq: 4, ChunkSeq: 1, TsUs: 99, Payload: []byte("cd")})
	select {
	case f := <-frames:
		t.Fatalf("got frame %+v before its last chunk", f)
	default:
	}
	c.send(&sensorlinkpb.SensorData{ChannelId: 1, FrameSeq: 4, ChunkSeq: 2, TsUs: 99, Flags: sensorlink.FlagLastChunk, Payload: []byte("ef")})
	select {
	case f := <-frames:
		if f.ChannelID != 1 || f.Seq != 4 || f.TsUs != 99 || f.Flags != sensorlink.FlagKeyframe || string(f.Payload) != "abcdef" {
			t.Fatalf("got frame %+v, want channel 1 seq 4 keyframe payload abcdef", f)
		}
	default:
		t.Fatal("frame not delivered after its last chunk")
	}
}

func TestWendycomTransportSubscribeErrorRemovesListener(t *testing.T) {
	c := newFakeWendycomClient()
	c.subscribeErr = errors.New("device busy")
	tr, _ := newTestWendycomTransport(zap.NewNop(), c)
	defer tr.Close()

	if _, _, err := tr.Stream(context.Background(), []uint32{1}); err == nil {
		t.Fatal("Stream succeeded despite a failed Subscribe")
	}
	if n := c.listenerCount(); n != 0 {
		t.Fatalf("%d listeners left after a failed Subscribe", n)
	}
}

func TestWendycomTransportEndsStreamWhenBoardDropsOff(t *testing.T) {
	c := newFakeWendycomClient()
	tr, _ := newTestWendycomTransport(zap.NewNop(), c)
	defer tr.Close()

	frames, closeStream, err := tr.Stream(context.Background(), []uint32{1})
	if err != nil {
		t.Fatal(err)
	}
	c.dropOff()
	requireClosed(t, frames)

	// Nothing to unsubscribe from once the connection is gone.
	closeStream()
	if u := c.unsubscribes(); len(u) != 0 {
		t.Fatalf("unsubscribed %v over a dead connection", u)
	}
}

func TestWendycomTransportEndsStreamOnContextCancel(t *testing.T) {
	c := newFakeWendycomClient()
	tr, _ := newTestWendycomTransport(zap.NewNop(), c)
	defer tr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	frames, closeStream, err := tr.Stream(ctx, []uint32{1})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStream()
	cancel()
	requireClosed(t, frames)
}

func TestWendycomTransportCloseStream(t *testing.T) {
	c := newFakeWendycomClient()
	tr, _ := newTestWendycomTransport(zap.NewNop(), c)
	defer tr.Close()

	frames, closeStream, err := tr.Stream(context.Background(), []uint32{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	closeStream()
	requireClosed(t, frames)
	if n := c.listenerCount(); n != 0 {
		t.Fatalf("%d listeners left after closeStream", n)
	}
	closeStream()
	if u := c.unsubscribes(); !slices.EqualFunc(u, [][]uint32{{1, 2}}, slices.Equal) {
		t.Fatalf("unsubscribed %v, want exactly [[1 2]]", u)
	}

	// The client may still be running a listener from a snapshot taken
	// before it was removed; that late frame must be dropped, not panic.
	c.lastListener(wholeFrame(1, 0))
}

func TestWendycomTransportBackpressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		core, logs := observer.New(zap.WarnLevel)
		c := newFakeWendycomClient()
		tr, _ := newTestWendycomTransport(zap.New(core), c)
		defer tr.Close()

		frames, closeStream, err := tr.Stream(context.Background(), []uint32{1})
		if err != nil {
			t.Fatal(err)
		}
		defer closeStream()

		// A stalled consumer must not block the client's read loop. The first
		// eight frames are queued; three more are dropped with one warning.
		for seq := range uint32(11) {
			c.send(wholeFrame(1, seq))
		}
		assertDropLogs(t, logs, [][2]uint64{{1, 1}})

		// Continued congestion produces an aggregate warning after five
		// seconds, including the drops suppressed since the first warning.
		time.Sleep(5 * time.Second)
		c.send(wholeFrame(1, 11))
		assertDropLogs(t, logs, [][2]uint64{{1, 1}, {3, 4}})

		for seq := range uint32(8) {
			if f := <-frames; f.Seq != seq {
				t.Fatalf("queued frame seq = %d, want %d", f.Seq, seq)
			}
		}
		c.send(wholeFrame(1, 12))
		if f := <-frames; f.Seq != 12 {
			t.Fatalf("stream did not recover: got seq %d", f.Seq)
		}
	})
}

func TestWendycomTransportCloseClosesClientOnce(t *testing.T) {
	c := newFakeWendycomClient()
	tr, connects := newTestWendycomTransport(zap.NewNop(), c)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := tr.FetchManifest(ctx); err != nil {
		t.Fatal(err)
	}
	tr.Close()
	tr.Close()
	if n := c.closeCount(); n != 1 {
		t.Fatalf("client closed %d times, want 1", n)
	}
	if _, err := tr.FetchManifest(ctx); err == nil {
		t.Fatal("FetchManifest succeeded on a closed transport")
	}
	if n := connects.Load(); n != 1 {
		t.Fatalf("connected %d times, want 1: a closed transport must not reconnect", n)
	}
}

func TestWendycomTransportConnectBoundedByContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newFakeWendycomClient()
		release := make(chan struct{})
		tr := &wendycomTransport{logger: zap.NewNop(), connect: func() (wendycomClient, error) {
			<-release // a TCP dial to an address that never answers
			return c, nil
		}}
		defer tr.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := tr.FetchManifest(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("FetchManifest: got %v, want a deadline error", err)
		}

		// A connect that completes after the caller gave up is not leaked.
		close(release)
		synctest.Wait()
		if n := c.closeCount(); n != 1 {
			t.Fatalf("late connection closed %d times, want 1", n)
		}
	})
}
