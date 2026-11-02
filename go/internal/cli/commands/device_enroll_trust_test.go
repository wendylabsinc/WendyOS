package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

type enrollmentTrustVersionClient struct {
	agentpb.WendyAgentServiceClient
	err         error
	calls       int
	beforeProbe func()
}

func (c *enrollmentTrustVersionClient) GetAgentVersion(ctx context.Context, _ *agentpb.GetAgentVersionRequest, _ ...grpc.CallOption) (*agentpb.GetAgentVersionResponse, error) {
	c.calls++
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("version probe has no deadline")
	}
	if c.beforeProbe != nil {
		c.beforeProbe()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.err != nil {
		return nil, c.err
	}
	return &agentpb.GetAgentVersionResponse{}, nil
}

type enrollmentTrustCloser struct{ calls int }

func (c *enrollmentTrustCloser) Close() error {
	c.calls++
	return nil
}

func enrollmentTrustConnection(probeErr error) (*grpcclient.AgentConnection, *enrollmentTrustVersionClient, *enrollmentTrustCloser) {
	client := &enrollmentTrustVersionClient{err: probeErr}
	closer := &enrollmentTrustCloser{}
	return &grpcclient.AgentConnection{AgentService: client, ExtraClosers: []io.Closer{closer}}, client, closer
}

func stubEnrollmentTrust(t *testing.T) {
	t.Helper()
	interactive, json, confirm, connect := isInteractiveTerminalFn, jsonOutput, confirmEnrollmentTrustFn, plaintextConnectFn
	t.Cleanup(func() {
		isInteractiveTerminalFn, jsonOutput, confirmEnrollmentTrustFn, plaintextConnectFn = interactive, json, confirm, connect
	})
	isInteractiveTerminalFn = func() bool { return true }
	jsonOutput = false
	confirmEnrollmentTrustFn = func() (bool, error) { return true, nil }
	plaintextConnectFn = func(context.Context, string) (*grpcclient.AgentConnection, error) {
		t.Fatal("unexpected plaintext connection")
		return nil, nil
	}
}

func enrollmentTrustPins() map[string]config.DevicePin {
	return map[string]config.DevicePin{
		"thor":  {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
		"other": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "99"},
	}
}

func TestEnrollmentTrustAcceptUsesCapturedCandidates(t *testing.T) {
	stubEnrollmentTrust(t)
	pins := enrollmentTrustPins()
	readPins := writePinTestConfig(t, pins)
	seedSPKIPins(t, "urn:wendy:org:7:asset:42", "urn:wendy:org:7:asset:99")
	spkiBefore := readSPKIPins(t)
	candidates := []string{"192.168.100.113:50051", "thor.local:50051"}
	failure := pinnedHostNoAuthenticatedEndpointError("thor", candidates, nil, false)
	// The user's selection can change while the prompt is open. Consent must
	// still apply only to the addresses captured by the failed dial.
	candidates[0] = "other.local:50051"
	confirmed := false
	confirmEnrollmentTrustFn = func() (bool, error) {
		confirmed = true
		return true, nil
	}
	failed, failedProbe, failedClose := enrollmentTrustConnection(errors.New("connection refused"))
	live, liveProbe, liveClose := enrollmentTrustConnection(nil)
	var dialed []string
	plaintextConnectFn = func(_ context.Context, addr string) (*grpcclient.AgentConnection, error) {
		if !confirmed {
			t.Fatal("plaintext connection attempted before consent")
		}
		dialed = append(dialed, addr)
		switch addr {
		case "192.168.100.113:50051":
			return failed, nil
		case "thor.local:50051":
			if failedClose.calls != 1 {
				t.Fatal("failed probe connection was not closed before the next candidate")
			}
			return live, nil
		default:
			t.Fatalf("dialed unrelated endpoint %q", addr)
			return nil, nil
		}
	}
	connectCalls, enrollCalls := 0, 0
	err := runWithEnrollmentTarget(context.Background(), func() (*SelectedDevice, error) {
		connectCalls++
		return nil, fmt.Errorf("selecting device: %w", failure)
	}, func(target *SelectedDevice) error {
		enrollCalls++
		if target.Agent != live || target.PinKey != "" {
			t.Fatalf("enrollment target = %+v, want the consented plaintext connection without the old pin", target)
		}
		if got := readPins(); !reflect.DeepEqual(got, pins) {
			t.Fatalf("pins cleared before enrollment succeeded: %+v", got)
		}
		if got := readSPKIPins(t); !reflect.DeepEqual(got, spkiBefore) {
			t.Fatalf("SPKI pins cleared before enrollment succeeded: %+v", got)
		}
		if _, ok := target.Agent.CachedAgentVersion(); !ok {
			t.Fatal("live version probe was not cached")
		}
		if liveClose.calls != 0 {
			t.Fatal("live connection was closed before enrollment")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if connectCalls != 1 || enrollCalls != 1 || failedProbe.calls != 1 || liveProbe.calls != 1 || failedClose.calls != 1 || liveClose.calls != 1 {
		t.Fatalf("connect=%d enroll=%d failed probe/close=%d/%d live probe/close=%d/%d", connectCalls, enrollCalls, failedProbe.calls, failedClose.calls, liveProbe.calls, liveClose.calls)
	}
	if want := []string{"192.168.100.113:50051", "thor.local:50051"}; !reflect.DeepEqual(dialed, want) {
		t.Fatalf("dialed %v, want %v", dialed, want)
	}
	if got, want := readPins(), map[string]config.DevicePin{"other": pins["other"]}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pins after enrollment = %+v, want %+v", got, want)
	}
	spkiAfter := readSPKIPins(t)
	if len(spkiAfter) != 1 || spkiAfter["urn:wendy:org:7:asset:99"] != spkiBefore["urn:wendy:org:7:asset:99"] {
		t.Fatalf("SPKI pins after enrollment = %+v, want only the unchanged unrelated pin", spkiAfter)
	}
}

func TestEnrollmentTrustRequiresConsent(t *testing.T) {
	promptErr := errors.New("prompt failed")
	for _, tt := range []struct {
		name        string
		interactive bool
		json        bool
		certSeen    bool
		wrongError  bool
		identityErr bool
		accept      bool
		promptErr   error
		wantPrompt  bool
	}{
		{name: "declined", interactive: true, wantPrompt: true},
		{name: "cancelled", interactive: true, promptErr: tui.ErrCancelled, wantPrompt: true},
		{name: "prompt error", interactive: true, promptErr: promptErr, wantPrompt: true},
		{name: "JSON", interactive: true, json: true, accept: true},
		{name: "noninteractive", accept: true},
		{name: "certificate seen", interactive: true, certSeen: true, accept: true},
		{name: "unrelated failure", interactive: true, wrongError: true, accept: true},
		{name: "identity refused", interactive: true, identityErr: true, accept: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stubEnrollmentTrust(t)
			pins := enrollmentTrustPins()
			readPins := writePinTestConfig(t, pins)
			seedSPKIPins(t, "urn:wendy:org:7:asset:42", "urn:wendy:org:7:asset:99")
			spkiBefore := readSPKIPins(t)
			isInteractiveTerminalFn = func() bool { return tt.interactive }
			jsonOutput = tt.json
			promptCalls := 0
			confirmEnrollmentTrustFn = func() (bool, error) {
				promptCalls++
				return tt.accept, tt.promptErr
			}
			var cause error = pinnedHostNoAuthenticatedEndpointError("thor", []string{"thor.local:50051"}, nil, tt.certSeen)
			if tt.wrongError {
				cause = errors.New("different identity or transport failure")
			}
			if tt.identityErr {
				cause = fmt.Errorf("device presented another enrolled identity: %w", errDeviceIdentityRefused)
			}
			wantErr := cause
			if tt.promptErr != nil {
				wantErr = tt.promptErr
				if errors.Is(tt.promptErr, tui.ErrCancelled) {
					wantErr = ErrUserCancelled
				}
			}
			err := runWithEnrollmentTarget(context.Background(), func() (*SelectedDevice, error) {
				return nil, cause
			}, func(*SelectedDevice) error {
				t.Fatal("enrollment attempted without consent")
				return nil
			})
			if !errors.Is(err, wantErr) {
				t.Fatalf("error = %v, want %v", err, wantErr)
			}
			wantCalls := 0
			if tt.wantPrompt {
				wantCalls = 1
			}
			if promptCalls != wantCalls {
				t.Fatalf("prompt called %d times, want %d", promptCalls, wantCalls)
			}
			if got := readPins(); !reflect.DeepEqual(got, pins) {
				t.Fatalf("pins changed without consent: %+v", got)
			}
			if got := readSPKIPins(t); !reflect.DeepEqual(got, spkiBefore) {
				t.Fatalf("SPKI pins changed without consent: %+v", got)
			}
		})
	}
}

func TestEnrollmentTrustFailedEnrollmentPreservesPins(t *testing.T) {
	stubEnrollmentTrust(t)
	pins := enrollmentTrustPins()
	readPins := writePinTestConfig(t, pins)
	seedSPKIPins(t, "urn:wendy:org:7:asset:42", "urn:wendy:org:7:asset:99")
	spkiBefore := readSPKIPins(t)
	conn, _, closed := enrollmentTrustConnection(nil)
	plaintextConnectFn = func(context.Context, string) (*grpcclient.AgentConnection, error) { return conn, nil }
	enrollErr := errors.New("cloud enrollment failed")
	err := runWithEnrollmentTarget(context.Background(), func() (*SelectedDevice, error) {
		return nil, pinnedHostNoAuthenticatedEndpointError("thor", []string{"thor.local:50051"}, nil, false)
	}, func(*SelectedDevice) error { return enrollErr })
	if !errors.Is(err, enrollErr) || closed.calls != 1 {
		t.Fatalf("error=%v close calls=%d, want enrollment error and one close", err, closed.calls)
	}
	if got := readPins(); !reflect.DeepEqual(got, pins) {
		t.Fatalf("pins changed after enrollment failed: %+v", got)
	}
	if got := readSPKIPins(t); !reflect.DeepEqual(got, spkiBefore) {
		t.Fatalf("SPKI pins changed after enrollment failed: %+v", got)
	}
}

func TestEnrollmentTrustConcurrentPinChangeIsPreserved(t *testing.T) {
	stubEnrollmentTrust(t)
	pins := enrollmentTrustPins()
	readPins := writePinTestConfig(t, pins)
	seedSPKIPins(t, "urn:wendy:org:7:asset:42", "urn:wendy:org:7:asset:99", "urn:wendy:org:7:asset:100")
	spkiBefore := readSPKIPins(t)
	conn, _, closed := enrollmentTrustConnection(nil)
	plaintextConnectFn = func(context.Context, string) (*grpcclient.AgentConnection, error) { return conn, nil }
	replacement := config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "100"}
	err := runWithEnrollmentTarget(context.Background(), func() (*SelectedDevice, error) {
		return nil, pinnedHostNoAuthenticatedEndpointError("thor", []string{"thor.local:50051"}, nil, false)
	}, func(*SelectedDevice) error {
		return config.Update(func(cfg *config.Config) (bool, error) {
			cfg.DevicePins["thor"] = replacement
			return true, nil
		})
	})
	if err == nil || closed.calls != 1 {
		t.Fatalf("error=%v close calls=%d, want changed-pin error and one close", err, closed.calls)
	}
	pins["thor"] = replacement
	if got := readPins(); !reflect.DeepEqual(got, pins) {
		t.Fatalf("concurrent pin update was lost: %+v", got)
	}
	if got := readSPKIPins(t); !reflect.DeepEqual(got, spkiBefore) {
		t.Fatalf("SPKI pins changed after concurrent update: %+v", got)
	}
}

func TestEnrollmentTrustUnreachablePreservesPins(t *testing.T) {
	stubEnrollmentTrust(t)
	pins := enrollmentTrustPins()
	readPins := writePinTestConfig(t, pins)
	seedSPKIPins(t, "urn:wendy:org:7:asset:42", "urn:wendy:org:7:asset:99")
	spkiBefore := readSPKIPins(t)
	dialErr, probeErr := errors.New("dial refused"), errors.New("probe refused")
	conn, probe, closed := enrollmentTrustConnection(probeErr)
	plaintextConnectFn = func(_ context.Context, addr string) (*grpcclient.AgentConnection, error) {
		if addr == "192.168.100.113:50051" {
			return nil, dialErr
		}
		return conn, nil
	}
	err := runWithEnrollmentTarget(context.Background(), func() (*SelectedDevice, error) {
		return nil, pinnedHostNoAuthenticatedEndpointError("thor", []string{"192.168.100.113:50051", "thor.local:50051"}, nil, false)
	}, func(*SelectedDevice) error {
		t.Fatal("enrollment attempted without a live agent")
		return nil
	})
	if !errors.Is(err, dialErr) || !errors.Is(err, probeErr) || probe.calls != 1 || closed.calls != 1 {
		t.Fatalf("error=%v probe=%d close=%d, want both failures, one probe, and one close", err, probe.calls, closed.calls)
	}
	if got := readPins(); !reflect.DeepEqual(got, pins) {
		t.Fatalf("pins changed when every endpoint was unreachable: %+v", got)
	}
	if got := readSPKIPins(t); !reflect.DeepEqual(got, spkiBefore) {
		t.Fatalf("SPKI pins changed when every endpoint was unreachable: %+v", got)
	}
}

func TestEnrollmentTrustCancellationPreservesPins(t *testing.T) {
	for _, phase := range []string{"before recovery", "confirmation", "candidate probe"} {
		t.Run(phase, func(t *testing.T) {
			stubEnrollmentTrust(t)
			pins := enrollmentTrustPins()
			readPins := writePinTestConfig(t, pins)
			seedSPKIPins(t, "urn:wendy:org:7:asset:42", "urn:wendy:org:7:asset:99")
			spkiBefore := readSPKIPins(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "before recovery" {
				cancel()
			}
			promptCalls := 0
			confirmEnrollmentTrustFn = func() (bool, error) {
				promptCalls++
				if phase == "confirmation" {
					cancel()
				}
				return true, nil
			}
			conn, probe, closed := enrollmentTrustConnection(nil)
			probe.beforeProbe = cancel
			dialCalls := 0
			plaintextConnectFn = func(_ context.Context, addr string) (*grpcclient.AgentConnection, error) {
				dialCalls++
				if phase != "candidate probe" || dialCalls != 1 || addr != "192.168.100.113:50051" {
					t.Fatalf("unexpected dial %q after cancellation at %s", addr, phase)
				}
				return conn, nil
			}
			err := runWithEnrollmentTarget(ctx, func() (*SelectedDevice, error) {
				return nil, pinnedHostNoAuthenticatedEndpointError("thor", []string{"192.168.100.113:50051", "thor.local:50051"}, nil, false)
			}, func(*SelectedDevice) error {
				t.Fatal("enrollment attempted after cancellation")
				return nil
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
			wantPrompts, wantDials := 1, 0
			if phase == "before recovery" {
				wantPrompts = 0
			}
			if phase == "candidate probe" {
				wantDials = 1
			}
			if promptCalls != wantPrompts || dialCalls != wantDials || probe.calls != wantDials || closed.calls != wantDials {
				t.Fatalf("prompt/dial/probe/close calls = %d/%d/%d/%d, want %d/%d/%d/%d", promptCalls, dialCalls, probe.calls, closed.calls, wantPrompts, wantDials, wantDials, wantDials)
			}
			if got := readPins(); !reflect.DeepEqual(got, pins) {
				t.Fatalf("pins changed after cancellation: %+v", got)
			}
			if got := readSPKIPins(t); !reflect.DeepEqual(got, spkiBefore) {
				t.Fatalf("SPKI pins changed after cancellation: %+v", got)
			}
		})
	}
}
