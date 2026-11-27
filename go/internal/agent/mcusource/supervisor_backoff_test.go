package mcusource

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestBackoffDelayCapsSourcesWithNothingToMountLater(t *testing.T) {
	for level := 0; level <= maxBackoffLevel; level++ {
		if d := backoffDelay(level, false); d > backoffCap {
			t.Fatalf("level %d: %v exceeds the reconnect cap", level, d)
		}
	}
	if got := backoffDelay(0, true); got != backoffBase {
		t.Fatalf("first idle retry = %v, want %v so a booting camera is picked up quickly", got, backoffBase)
	}
	if got := backoffDelay(maxBackoffLevel, true); got != idleBackoffCap {
		t.Fatalf("idle cap = %v, want %v", got, idleBackoffCap)
	}
	for level := 1; level <= maxBackoffLevel; level++ {
		if backoffDelay(level, true) < backoffDelay(level-1, true) {
			t.Fatalf("idle backoff shrinks at level %d", level)
		}
	}
}

// A source that was unreachable for a while and then answers before its camera
// is up must be checked again soon, not after the longest idle delay.
func TestRetryScheduleStartsIdleBackoffAfresh(t *testing.T) {
	var r retrySchedule
	for range maxBackoffLevel + 2 {
		r.next(false)
	}
	if got := r.next(true); got != backoffBase {
		t.Fatalf("first idle delay after failures = %v, want %v", got, backoffBase)
	}
	var last time.Duration
	for range maxBackoffLevel + 1 {
		last = r.next(true)
	}
	if last != idleBackoffCap {
		t.Fatalf("idle delay after a long idle streak = %v, want %v", last, idleBackoffCap)
	}
	if got := r.next(false); got != backoffCap {
		t.Fatalf("a failure after idling waited %v, want the failure schedule's %v", got, backoffCap)
	}
	if got := r.next(true); got != backoffBase {
		t.Fatalf("a new idle streak starts at %v, want %v", got, backoffBase)
	}
	r.reset()
	if got := r.next(false); got != backoffBase {
		t.Fatalf("after a healthy stream the next delay = %v, want %v", got, backoffBase)
	}
}

// manifestOnly answers the manifest and nothing else.
type manifestOnly struct{ m *sensorlinkpb.SensorManifest }

func (manifestOnly) Close() error { return nil }
func (t manifestOnly) FetchManifest(context.Context) (*sensorlinkpb.SensorManifest, error) {
	return t.m, nil
}
func (manifestOnly) Stream(context.Context, []uint32) (<-chan *sensorlink.SensorFrame, func() error, error) {
	return nil, nil, errors.New("nothing was subscribed")
}

func TestStreamOnceReportsASourceWithNothingToMount(t *testing.T) {
	thermalOnly := &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{{ChannelId: 1, Name: "thermal 0x66"}}}
	sup := NewSupervisor(zap.NewNop(), nil, func(SensorPairing, string) (SensorTransport, error) {
		return manifestOnly{thermalOnly}, nil
	}, nil, nil)
	delivered, err := sup.streamOnce(context.Background(), SensorPairing{SourceAssetID: 7}, "board:5054")
	if delivered || !errors.Is(err, errNothingToMount) {
		t.Fatalf("delivered = %v, err = %v; want errNothingToMount", delivered, err)
	}
}

// A source with only generic readings, such as a thermal board, is forwarded
// once a generic sink is set, so it is not idle.
func TestStreamOnceForwardsAGenericOnlySource(t *testing.T) {
	thermal := &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{{ChannelId: 1, Name: "thermal 0x66",
		Format: &sensorlinkpb.SensorDescriptor_Sensor{Sensor: &sensorlinkpb.SensorFormat{Schema: "dev.wendy.mlx90642.celsius-times50.v1", SampleBytes: 2}}}}}
	sup := NewSupervisor(zap.NewNop(), nil, func(SensorPairing, string) (SensorTransport, error) {
		return manifestOnly{thermal}, nil
	}, nil, nil)
	if _, err := sup.streamOnce(context.Background(), SensorPairing{SourceAssetID: 7}, "board:5054"); !errors.Is(err, errNothingToMount) {
		t.Fatalf("without a generic sink: err = %v, want errNothingToMount", err)
	}
	sup.SetGenericSink(&recordingGeneric{})
	if _, err := sup.streamOnce(context.Background(), SensorPairing{SourceAssetID: 7}, "board:5054"); err == nil || errors.Is(err, errNothingToMount) {
		t.Fatalf("with a generic sink: err = %v, want the stream to be tried", err)
	}
}

// A source that answered with nothing to mount was reached. Its other address
// would cost it one more handshake, and that dial's error must not hide the
// answer and put the source back on the fast schedule.
func TestRunPairingStopsDialingASourceWithNothingToMount(t *testing.T) {
	thermalOnly := &sensorlinkpb.SensorManifest{Sensors: []*sensorlinkpb.SensorDescriptor{{ChannelId: 1, Name: "thermal 0x66"}}}
	dials := make(chan string, 8)
	core, logs := observer.New(zapcore.InfoLevel)
	sup := NewSupervisor(zap.New(core), nil, func(_ SensorPairing, addr string) (SensorTransport, error) {
		dials <- addr
		if addr == "lan:1" {
			return manifestOnly{thermalOnly}, nil
		}
		return nil, errors.New("mesh route unreachable")
	}, nil, nil)
	sup.SetSourceResolver(func(context.Context, SensorPairing) ([]string, bool) { return []string{"lan:1", "mesh:1"}, true })
	// The first idle retry waits backoffBase, so only the first attempt runs.
	ctx, cancel := context.WithTimeout(context.Background(), backoffBase/2)
	defer cancel()
	_ = sup.runPairing(ctx, SensorPairing{SourceAssetID: 7}, "", false)
	close(dials)
	var dialed []string
	for addr := range dials {
		dialed = append(dialed, addr)
	}
	if len(dialed) != 1 || dialed[0] != "lan:1" {
		t.Fatalf("dialed %v, want only the address that answered", dialed)
	}
	if logs.FilterMessage("sensor source offers nothing to mount; checking again later").Len() != 1 {
		t.Fatalf("the attempt was not treated as idle: %v", logs.All())
	}
}

// Keep the test honest about the schedule it implies: about ten retries in the
// first hour instead of more than a hundred.
func TestIdleSourceRetriesPerHour(t *testing.T) {
	var elapsed time.Duration
	retries := 0
	for level := 0; elapsed < time.Hour; retries++ {
		elapsed += backoffDelay(level, true)
		if level < maxBackoffLevel {
			level++
		}
	}
	if retries > 20 {
		t.Fatalf("an idle source is retried %d times an hour", retries)
	}
}
