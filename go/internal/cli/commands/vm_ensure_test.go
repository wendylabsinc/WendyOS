package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestResolveVMAliasIgnoresAnythingThatIsNotAVM(t *testing.T) {
	// A hostname or address must fall through untouched, and must not pay for
	// a store read or a VM start.
	for _, device := range []string{"rpi5.local", "127.0.0.1:50051", "docker", "192.168.2.253", ""} {
		addr, matched, err := resolveVMAlias(context.Background(), device)
		if matched || err != nil || addr != "" {
			t.Errorf("resolveVMAlias(%q) = (%q, %v, %v), want it left alone", device, addr, matched, err)
		}
	}
}

func TestResolveVMAliasRejectsAnInvalidVMName(t *testing.T) {
	if _, _, err := resolveVMAlias(context.Background(), "vm:../escape"); err == nil {
		t.Error("resolveVMAlias() accepted a traversal in the VM name")
	}
}

func TestSimulatorFailureNeverBecomesACloudDeploy(t *testing.T) {
	// resolveWithCloudFallback answers most errors with a cloud tunnel. If it
	// did that here the app would land on a different machine than the user
	// picked, and report success.
	err := errors.New("boom")
	wrapped := errors.Join(errSimulatorUnavailable, err)
	if !errors.Is(wrapped, errSimulatorUnavailable) {
		t.Fatal("errSimulatorUnavailable does not survive wrapping")
	}
}

func TestConnectSimulatorChoiceRefusesToDownloadNonInteractively(t *testing.T) {
	stubTerminal(t, false)
	savedYes := vmAssumeYes
	vmAssumeYes = false
	t.Cleanup(func() { vmAssumeYes = savedYes })

	savedQEMU := ensureQEMUFn
	ensureQEMUFn = func(context.Context) error { return nil }
	t.Cleanup(func() { ensureQEMUFn = savedQEMU })

	_, err := connectSimulatorChoice(context.Background(),
		&simulatorChoice{Name: "sim", Create: true}, true)
	if err == nil {
		t.Fatal("connectSimulatorChoice() = nil, want a refusal")
	}
	if !errors.Is(err, errSimulatorUnavailable) {
		t.Errorf("err = %v, want errSimulatorUnavailable", err)
	}
	if !strings.Contains(err.Error(), "wendy vm create") {
		t.Errorf("err = %v, want it to name the manual command", err)
	}
}

func TestConnectSimulatorChoiceSurfacesAMissingQEMU(t *testing.T) {
	saved := ensureQEMUFn
	ensureQEMUFn = func(context.Context) error { return errors.New("qemu-system-aarch64 not found") }
	t.Cleanup(func() { ensureQEMUFn = saved })

	_, err := connectSimulatorChoice(context.Background(), &simulatorChoice{Name: "sim"}, true)
	if !errors.Is(err, errSimulatorUnavailable) {
		t.Errorf("err = %v, want errSimulatorUnavailable so it never falls back to cloud", err)
	}
}

func TestWaitForSimulatorAgentGivesUpAtTheBudget(t *testing.T) {
	// Nothing is listening, so this must hit the deadline rather than block.
	start := time.Now()
	_, err := waitForSimulatorAgent(context.Background(), "dev", "127.0.0.1:59998", 50*time.Millisecond)
	if err == nil {
		t.Fatal("waitForSimulatorAgent() = nil, want a timeout")
	}
	if !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("err = %v, want it to say the simulator never answered", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("waited %v, far past the budget", elapsed)
	}
}

func TestWaitForSimulatorAgentStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := waitForSimulatorAgent(ctx, "dev", "127.0.0.1:59998", time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("waitForSimulatorAgent() = %v, want context.Canceled", err)
	}
}

func TestSimulatorErrorsAreMarkedOnceNotTwice(t *testing.T) {
	// ensureSimulatorRunning marks its own failures, so re-wrapping at the call
	// site printed "simulator unavailable: simulator unavailable: ...".
	already := fmt.Errorf("%w: no VM named %q", errSimulatorUnavailable, "sim")
	got := markSimulatorUnavailable(already).Error()
	if strings.Count(got, "simulator unavailable") != 1 {
		t.Errorf("markSimulatorUnavailable() = %q, want the prefix exactly once", got)
	}
	plain := markSimulatorUnavailable(errors.New("boom"))
	if !errors.Is(plain, errSimulatorUnavailable) {
		t.Error("an unmarked error lost the marker; cloud fallback would take over")
	}
}

func TestConsoleTailStripsGuestControlSequences(t *testing.T) {
	// The console is whatever the guest printed. An escape sequence in it would
	// be executed by the user's terminal rather than shown.
	got := sanitizeConsoleLine("boot \x1b[2Jok\x07\ttab")
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Errorf("sanitizeConsoleLine() = %q, want no control characters", got)
	}
	if !strings.Contains(got, "\t") {
		t.Errorf("sanitizeConsoleLine() = %q, want the tab kept", got)
	}
	if !strings.Contains(got, "boot") || !strings.Contains(got, "ok") {
		t.Errorf("sanitizeConsoleLine() = %q, want the printable text kept", got)
	}
}

func TestWaitForSimulatorAgentRetriesPinnedEndpointDuringBoot(t *testing.T) {
	setTempConfig(t, &config.Config{})
	cfg := &config.Config{}
	cfg.SetDevicePin("vm:booting", 1, "cloud", "booting", "")
	oldLoad, oldDial, oldRecord := loadConfigForPinFn, dialAgentLadderFn, vmRecordHostnameFn
	t.Cleanup(func() { loadConfigForPinFn, dialAgentLadderFn, vmRecordHostnameFn = oldLoad, oldDial, oldRecord })
	loadConfigForPinFn = func() (*config.Config, error) { return cfg, nil }
	vmRecordHostnameFn = func(string, string) error { return nil }
	calls := 0
	dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
		calls++
		if target.PinKey != "vm:booting" || target.Addr != "127.0.0.1:50103" || target.Expected == nil || target.Expected.EntityID != "booting" {
			t.Fatalf("boot retry lost pinned target: %+v", target)
		}
		if calls == 1 {
			return nil, nil, fmt.Errorf("booting: %w", errNoAuthenticatedEndpoint)
		}
		return &grpcclient.AgentConnection{AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{}}}, nil, nil
	}
	conn, err := waitForSimulatorAgent(context.Background(), "booting", "127.0.0.1:50103", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if calls != 2 || conn.SimulatorName != "booting" {
		t.Fatalf("calls = %d, simulator = %q", calls, conn.SimulatorName)
	}
}

func TestWaitForSimulatorAgentConnectionFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
		timeout bool
	}{
		{"unreachable", fmt.Errorf("booting: %w", errNoAuthenticatedEndpoint), true},
		{"identity mismatch", fmt.Errorf("wrong device: %w", errDeviceIdentityRefused), false},
		{"TLS rejection", newTLSHandshakeRejectedError(errors.New("remote error: tls: bad certificate")), false},
		{"certificate algorithms", newTLSHandshakeRejectedError(errors.New("tls: peer doesn't support any of the certificate's signature algorithms")), false},
		{"wrong organization", fmt.Errorf("connect: %w", orgMismatchDeviceError{deviceOrg: 42, userOrgs: []int32{1}}), false},
		{"unauthenticated", status.Error(codes.Unauthenticated, "certificate expired"), false},
		{"permission denied", status.Error(codes.PermissionDenied, "access denied"), false},
		{"agent restarting", status.Error(codes.Unavailable, "connection refused"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setTempConfig(t, &config.Config{})
			oldDial := dialAgentLadderFn
			t.Cleanup(func() { dialAgentLadderFn = oldDial })
			calls := 0
			dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
				calls++
				return nil, nil, tc.failure
			}
			start := time.Now()
			_, err := waitForSimulatorAgent(context.Background(), "booting", "127.0.0.1:50103", 50*time.Millisecond)
			if !errors.Is(err, tc.failure) {
				t.Fatalf("error = %v, want %v", err, tc.failure)
			}
			if got := strings.Contains(err.Error(), "did not answer"); got != tc.timeout {
				t.Fatalf("error = %v, want timeout %v", err, tc.timeout)
			}
			if calls != 1 {
				t.Fatalf("calls = %d, want 1", calls)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("wait exceeded boot budget: %v", elapsed)
			}
		})
	}
}

func TestAwaitSimulatorKeepsRecoveryErrorWithoutBootLog(t *testing.T) {
	setTempConfig(t, &config.Config{})
	stubTerminal(t, false)
	store, err := vm.NewStore()
	if err != nil {
		t.Fatal(err)
	}
	logPath := store.LogPath("dev")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("boot diagnostic\nlogin banner\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rejection := newTLSHandshakeRejectedError(errors.New("remote error: tls: bad certificate"))
	for _, tc := range []struct {
		name        string
		cause       error
		wantConsole bool
		unavailable bool
	}{
		{"TLS rejection", rejection, false, true},
		{"identity mismatch", errDeviceIdentityRefused, false, true},
		{"wrong organization", fmt.Errorf("connect: %w", orgMismatchDeviceError{deviceOrg: 42}), false, true},
		{"timeout", fmt.Errorf("simulator did not answer: %w", context.DeadlineExceeded), true, true},
		{"cancelled", context.Canceled, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := awaitSimulatorWith(context.Background(), "dev", "127.0.0.1:50103", false,
				func(context.Context) (*grpcclient.AgentConnection, error) { return nil, tc.cause })
			if !errors.Is(err, tc.cause) || errors.Is(err, errSimulatorUnavailable) != tc.unavailable {
				t.Fatalf("error = %v, lost cause or simulator classification", err)
			}
			if got := strings.Contains(err.Error(), "login banner"); got != tc.wantConsole {
				t.Fatalf("error = %v, want console output %v", err, tc.wantConsole)
			}
			if errors.Is(err, errTLSHandshakeRejected) {
				if !strings.Contains(err.Error(), "wendy auth refresh-certs") || !strings.Contains(err.Error(), "retry this command") {
					t.Fatalf("TLS rejection missing recovery instructions: %v", err)
				}
			}
		})
	}
}
