package commands

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestContinuedReadinessWithDeterministicTicks(t *testing.T) {
	ticks := make(chan time.Time, 4)
	for range 4 {
		ticks <- time.Time{}
	}
	probes, states, warnings := 0, 0, 0
	err := continueReadiness(context.Background(), ticks, func(context.Context) bool { probes++; return probes == 3 }, func(context.Context) (bool, error) {
		states++
		if states == 2 {
			return false, errors.New("status temporarily unavailable")
		}
		return true, nil
	}, func(error) { warnings++ })
	if err != nil || probes != 3 || states != 4 || warnings != 1 {
		t.Fatalf("result=%v probes=%d states=%d warnings=%d", err, probes, states, warnings)
	}
}

func TestContinuedReadinessCancelsObsoleteProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	ticks <- time.Time{}
	err := continueReadiness(ctx, ticks, func(context.Context) bool { cancel(); return true }, func(context.Context) (bool, error) { return true, nil }, func(error) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stale successful probe survived cancellation: %v", err)
	}
}

func TestContinuedReadinessStopsOnTerminalService(t *testing.T) {
	ticks := make(chan time.Time, 1)
	ticks <- time.Time{}
	err := continueReadiness(context.Background(), ticks, func(context.Context) bool { t.Fatal("probed a stopped service"); return true }, func(context.Context) (bool, error) { return false, nil }, func(error) {})
	if err == nil {
		t.Fatal("stopped service reported ready")
	}
}

func TestReadinessStateUsesServiceNotGroupAggregate(t *testing.T) {
	fake := &lifecycleFakeContainerClient{container: &agentpb.AppContainer{AppName: "app", RunningState: agentpb.AppRunningState_RUNNING,
		Services: []*agentpb.ServiceEntry{{Name: "api", RunningState: agentpb.AppRunningState_RUNNING}, {Name: "model", RunningState: agentpb.AppRunningState_STOPPED}},
	}}
	conn := newLifecycleTestConn("127.0.0.1", fake)
	for _, tc := range []struct {
		service string
		running bool
	}{{"api", true}, {"model", false}, {"", false}} {
		running, err := readinessState(context.Background(), conn, &appconfig.AppConfig{AppID: "app", ServiceName: tc.service})
		if err != nil || running != tc.running {
			t.Fatalf("%q: %v, %v", tc.service, running, err)
		}
	}
}
