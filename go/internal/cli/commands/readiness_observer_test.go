package commands

import (
	"context"
	"errors"
	"net"
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
	err := continueReadiness(context.Background(), ticks, nil, func(context.Context) bool { probes++; return probes == 3 }, func(context.Context) (bool, error) {
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
	err := continueReadiness(ctx, ticks, nil, func(context.Context) bool { cancel(); return true }, func(context.Context) (bool, error) { return true, nil }, func(error) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stale successful probe survived cancellation: %v", err)
	}
}

func TestContinuedReadinessStopsOnTerminalService(t *testing.T) {
	ticks := make(chan time.Time, 1)
	ticks <- time.Time{}
	err := continueReadiness(context.Background(), ticks, nil, func(context.Context) bool { t.Fatal("probed a stopped service"); return true }, func(context.Context) (bool, error) { return false, nil }, func(error) {})
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

func TestContinuedReadinessStopsAtDeadline(t *testing.T) {
	deadline := make(chan time.Time, 1)
	deadline <- time.Time{}
	err := continueReadiness(context.Background(), make(chan time.Time), deadline, func(context.Context) bool {
		t.Fatal("probed after the deadline")
		return false
	}, func(context.Context) (bool, error) { return true, nil }, func(error) {})
	if !errors.Is(err, errReadinessObserveLimit) {
		t.Fatalf("err = %v, want errReadinessObserveLimit", err)
	}
}

func TestReadinessProbeTimeout(t *testing.T) {
	for _, tc := range []struct {
		cfg      *appconfig.ReadinessConfig
		override time.Duration
		want     time.Duration
	}{
		{nil, 0, 30 * time.Second},
		{&appconfig.ReadinessConfig{}, 0, 30 * time.Second},
		{&appconfig.ReadinessConfig{TimeoutSeconds: 180}, 0, 180 * time.Second},
		{&appconfig.ReadinessConfig{TimeoutSeconds: 180}, 5 * time.Second, 5 * time.Second},
	} {
		if got := readinessProbeTimeout(tc.cfg, tc.override); got != tc.want {
			t.Errorf("readinessProbeTimeout(%+v, %v) = %v, want %v", tc.cfg, tc.override, got, tc.want)
		}
	}
}

// --readiness-timeout is the whole deadline for an attached run's host-side
// readiness wait: no extended observation afterwards.
func TestWaitForAttachedReadinessOverrideIsTheWholeDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := testPort(t, ln)
	ln.Close()
	cfg := &appconfig.AppConfig{AppID: "slow", Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: port}, TimeoutSeconds: 30}}
	fake := &lifecycleFakeContainerClient{container: &agentpb.AppContainer{AppName: "slow", RunningState: agentpb.AppRunningState_RUNNING}}
	start := time.Now()
	err = waitForAttachedReadiness(context.Background(), newLifecycleTestConn("127.0.0.1", fake), cfg, "127.0.0.1", time.Second)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("took %v, want about the 1s override", elapsed)
	}
	if ErrorClass(err) != "readiness_timeout" {
		t.Fatalf("err = %v, want readiness_timeout", err)
	}
	if fake.listContainersCalls != 0 {
		t.Fatal("entered extended observation despite --readiness-timeout")
	}
}
