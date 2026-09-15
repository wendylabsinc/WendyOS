package mcusource

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/agent/audioloop"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
	"testing"
	"time"
)

type stoppingTransport struct {
	cleanupTransport
	fetching, closing, finish chan struct{}
}

func (tr *stoppingTransport) FetchManifest(ctx context.Context) (*sensorlinkpb.SensorManifest, error) {
	close(tr.fetching)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (tr *stoppingTransport) Close() error { close(tr.closing); <-tr.finish; return nil }

type releaseAudioLoop struct{ released chan int32 }

func (a releaseAudioLoop) ReleaseSource(id int32)                    { a.released <- id }
func (releaseAudioLoop) Allocate(int32, uint32, string) (int, error) { panic("unexpected allocate") }
func (releaseAudioLoop) OpenWriter(context.Context, int, audioloop.PCMFormat) (audioloop.AudioWriter, error) {
	panic("unexpected writer")
}

func TestUnpairWaitsForTeardownBeforeReleasingSlots(t *testing.T) {
	tr := &stoppingTransport{fetching: make(chan struct{}), closing: make(chan struct{}), finish: make(chan struct{})}
	audio := releaseAudioLoop{released: make(chan int32, 1)}
	sup := NewSupervisor(zap.NewNop(), nil, func(SensorPairing, string) (SensorTransport, error) { return tr, nil }, nil, audio)
	runner := NewRunner(zap.NewNop(), sup)
	runner.Start(SensorPairing{SourceAssetID: 7}, "source")
	select {
	case <-tr.fetching:
	case <-time.After(time.Second):
		t.Fatal("did not start")
	}
	done := make(chan struct{})
	go func() { runner.Stop(7); close(done) }()
	select {
	case <-tr.closing:
	case <-time.After(time.Second):
		t.Fatal("did not cancel")
	}
	select {
	case <-audio.released:
		t.Fatal("released slots before teardown")
	default:
	}
	select {
	case <-done:
		t.Fatal("Stop returned before teardown")
	default:
	}
	close(tr.finish)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not finish")
	}
	select {
	case id := <-audio.released:
		if id != 7 {
			t.Fatalf("released %d", id)
		}
	default:
		t.Fatal("did not release slots")
	}
}

type removableLoopback struct {
	busy  bool
	nodes map[uint32]bool
}

func (l *removableLoopback) EnsureNode(_ context.Context, id uint32, _ string) error {
	l.nodes[id] = true
	return nil
}
func (l *removableLoopback) NodePath(id uint32) (string, bool) { return "camera", l.nodes[id] }
func (l *removableLoopback) RemoveCamera(id uint32) {
	if !l.busy {
		delete(l.nodes, id)
	}
}

func TestUnpairReleasesCameraIDsOnlyAfterNodeRemoval(t *testing.T) {
	for _, busy := range []bool{false, true} {
		lb := &removableLoopback{busy: busy, nodes: make(map[uint32]bool)}
		s := NewSupervisor(zap.NewNop(), lb, nil, nil, nil)
		first, _ := s.nodeID(1, 1)
		_ = lb.EnsureNode(context.Background(), first, "old source")
		s.releaseSource(1)
		next, _ := s.nodeID(2, 1)
		if (first == next) == busy {
			t.Fatalf("busy=%v: first=%d next=%d", busy, first, next)
		}
	}
}
