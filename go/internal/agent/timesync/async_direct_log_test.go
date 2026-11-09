package timesync

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/shared/asyncdiag"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"sync"
	"testing"
	"time"
)

type reviewBlockingSink struct {
	entered, release chan struct{}
	once             sync.Once
}

func (s *reviewBlockingSink) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return len(p), nil
}
func (s *reviewBlockingSink) Sync() error { return nil }

func TestDirectDiagnosticsDoNotBlockClockApplyOrOtherSources(t *testing.T) {
	sink := &reviewBlockingSink{entered: make(chan struct{}), release: make(chan struct{})}
	logger := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), sink, zap.DebugLevel))
	m := NewManager(logger, "")
	current := time.Unix(1800000000, 0)
	m.now = func() time.Time { return current }
	m.boot = func() (int64, error) { return int64(time.Second), nil }
	m.applyResult = func(time.Time) error { return nil }
	defer func() {
		close(sink.release)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := asyncdiag.Flush(ctx); err != nil {
			t.Error(err)
		}
	}()
	directDone := make(chan struct{})
	go func() {
		defer close(directDone)
		m.applyDirect(Consensus{Confidence: ConfidenceDegraded, Quorum: 2, LowerOffsetNanos: current.Add(-time.Second).UnixNano(), UpperOffsetNanos: current.Add(time.Second).UnixNano()})
	}()
	select {
	case <-sink.entered:
	case <-time.After(time.Second):
		t.Fatal("diagnostic sink not reached")
	}
	otherDone := make(chan struct{})
	go func() { defer close(otherDone); m.Apply(current) }()
	select {
	case <-otherDone:
	case <-time.After(300 * time.Millisecond):
		t.Error("unrelated verified time source blocked by direct diagnostic sink under clockMu")
	}
	select {
	case <-directDone:
	case <-time.After(time.Second):
		t.Fatal("direct did not drain")
	}
	select {
	case <-otherDone:
	case <-time.After(time.Second):
		t.Fatal("other did not drain")
	}
}
