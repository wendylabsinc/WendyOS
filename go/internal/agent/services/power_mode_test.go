package services

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/powermode"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type fakePowerModes struct {
	state  powermode.State
	result powermode.SetResult
	err    error
	asked  []string
	reboot []bool
}

func (f *fakePowerModes) State(context.Context) (powermode.State, error) { return f.state, f.err }
func (f *fakePowerModes) Set(_ context.Context, m string, reboot bool) (powermode.SetResult, error) {
	f.asked = append(f.asked, m)
	f.reboot = append(f.reboot, reboot)
	return f.result, f.err
}

func powerModeService(jetson bool, f *fakePowerModes, reboots *atomic.Int32) *AgentService {
	return &AgentService{
		logger:     zap.NewNop(),
		powerModes: f,
		isJetson:   func() bool { return jetson },
		rebootFn:   func() error { reboots.Add(1); return nil },
	}
}

func orinState() powermode.State {
	maxn := powermode.Mode{ID: 0, Name: "MAXN"}
	w30 := powermode.Mode{ID: 2, Name: "MODE_30W"}
	return powermode.State{Current: w30, Available: []powermode.Mode{maxn, w30}, BootDefault: &w30}
}

func TestPowerModeUnsupportedOffJetson(t *testing.T) {
	var reboots atomic.Int32
	f := &fakePowerModes{state: orinState()}
	s := powerModeService(false, f, &reboots)

	if _, err := s.GetPowerMode(context.Background(), &agentpb.GetPowerModeRequest{}); status.Code(err) != codes.Unavailable {
		t.Errorf("GetPowerMode code = %v, want Unavailable", status.Code(err))
	}
	if _, err := s.SetPowerMode(context.Background(), &agentpb.SetPowerModeRequest{Mode: "MAXN"}); status.Code(err) != codes.Unavailable {
		t.Errorf("SetPowerMode code = %v, want Unavailable", status.Code(err))
	}
	if len(f.asked) != 0 {
		t.Errorf("controller was asked to switch on a non-Jetson: %v", f.asked)
	}
	v, err := s.GetAgentVersion(context.Background(), &agentpb.GetAgentVersionRequest{})
	if err != nil || v.PowerMode != nil {
		t.Errorf("GetAgentVersion power mode = %v (err %v), want absent", v.GetPowerMode(), err)
	}
	// A service with no hooks at all behaves the same.
	if _, err := (&AgentService{logger: zap.NewNop()}).GetPowerMode(context.Background(), &agentpb.GetPowerModeRequest{}); status.Code(err) != codes.Unavailable {
		t.Errorf("bare service code = %v, want Unavailable", status.Code(err))
	}
}

func TestGetPowerMode(t *testing.T) {
	var reboots atomic.Int32
	s := powerModeService(true, &fakePowerModes{state: orinState()}, &reboots)
	resp, err := s.GetPowerMode(context.Background(), &agentpb.GetPowerModeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCurrent().GetName() != "MODE_30W" || resp.GetCurrent().GetId() != 2 {
		t.Errorf("current = %v", resp.GetCurrent())
	}
	if len(resp.GetAvailable()) != 2 || resp.GetBootDefault().GetName() != "MODE_30W" {
		t.Errorf("response = %v", resp)
	}
	v, err := s.GetAgentVersion(context.Background(), &agentpb.GetAgentVersionRequest{})
	if err != nil || v.GetPowerMode().GetName() != "MODE_30W" {
		t.Errorf("GetAgentVersion power mode = %v (err %v)", v.GetPowerMode(), err)
	}
}

func TestGetAgentVersionOmitsUnreadablePowerMode(t *testing.T) {
	var reboots atomic.Int32
	s := powerModeService(true, &fakePowerModes{err: errors.New("boom")}, &reboots)
	v, err := s.GetAgentVersion(context.Background(), &agentpb.GetAgentVersionRequest{})
	if err != nil || v.PowerMode != nil {
		t.Errorf("power mode = %v (err %v), want absent and no error", v.GetPowerMode(), err)
	}
}

func TestSetPowerModeErrorCodes(t *testing.T) {
	for want, err := range map[codes.Code]error{
		codes.InvalidArgument: powermode.ErrUnknownMode,
		codes.Unavailable:     powermode.ErrUnavailable,
		codes.Internal:        errors.New("boom"),
	} {
		var reboots atomic.Int32
		s := powerModeService(true, &fakePowerModes{err: err}, &reboots)
		if _, got := s.SetPowerMode(context.Background(), &agentpb.SetPowerModeRequest{Mode: "x"}); status.Code(got) != want {
			t.Errorf("%v -> %v, want %v", err, status.Code(got), want)
		}
	}
}

func TestSetPowerModeRebootOnlyWhenAskedAndNeeded(t *testing.T) {
	maxn := powermode.Mode{ID: 0, Name: "MAXN"}
	cases := []struct {
		name         string
		needed, ask  bool
		wantReboot   bool
		wantRequired bool
	}{
		{"needed, not asked", true, false, false, true},
		{"needed and asked", true, true, true, true},
		{"not needed, asked", false, true, false, false},
		{"not needed, not asked", false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reboots atomic.Int32
			f := &fakePowerModes{result: powermode.SetResult{Mode: maxn, RebootRequired: tc.needed}}
			s := powerModeService(true, f, &reboots)
			resp, err := s.SetPowerMode(context.Background(), &agentpb.SetPowerModeRequest{Mode: "MAXN", Reboot: tc.ask})
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetRebootRequired() != tc.wantRequired || resp.GetRebootScheduled() != tc.wantReboot || resp.GetMode().GetName() != "MAXN" {
				t.Errorf("response = %v", resp)
			}
			if len(f.reboot) != 1 || f.reboot[0] != tc.ask {
				t.Errorf("controller reboot answers = %v, want [%v]", f.reboot, tc.ask)
			}
			if tc.wantReboot {
				deadline := time.Now().Add(powerModeRebootDelay + 3*time.Second)
				for reboots.Load() == 0 && time.Now().Before(deadline) {
					time.Sleep(20 * time.Millisecond)
				}
			}
			if got := reboots.Load() == 1; got != tc.wantReboot {
				t.Errorf("rebooted = %v, want %v", got, tc.wantReboot)
			}
		})
	}
}
