//go:build !windows

package sessionbroker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh/carriers"
	"github.com/wendylabsinc/wendy/go/internal/cli/operatormesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// Real native Go client/broker process restart, with the production embedded
// host used by mobile. This does not claim a GUI, radio or OS reboot test.
// Only discovery is supplied by the loopback router fixture; profile loading,
// intent persistence, broker ownership, client IPC, authenticated routing and
// management RPC all run through production code in independent processes.
func TestOperatorNativeProcessRestartPreservesExplicitDisconnect(t *testing.T) {
	if os.Getenv("WENDY_E2E_OPERATOR_RESTART") != "1" {
		t.Skip("run through test-operator-native-restart.sh in the existing network job")
	}
	operatorBrokerNormal(t, "P256-native-restart", false)
}

type nativeRestartResult struct {
	PID      int    `json:"pid"`
	Endpoint string `json:"endpoint"`
}

func operatorNativeRestartJourney(t *testing.T, ctx context.Context, directory, principal string, router meshid.Device, address string, service *operatorBrokerAgent, admitted, withdrawn *atomic.Int32) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var endpoint string
	var expectedAdmissions int32
	for _, phase := range []string{"enable-exit", "passive-disconnect", "blocked", "explicit-resume", "passive-cleanup"} {
		before := service.calls.Load()
		report := filepath.Join(directory, phase+".result.json")
		child, cancel := context.WithTimeout(ctx, 20*time.Second)
		cmd := exec.CommandContext(child, executable, "-test.run=^TestOperatorNativeRestartChild$", "-test.v", "-test.count=1", "-test.timeout=19s")
		cmd.Env = append(os.Environ(), "WENDY_OPERATOR_NATIVE_PHASE="+phase,
			"WENDY_OPERATOR_NATIVE_REPORT="+report, "WENDY_OPERATOR_BROKER_ADDRESS="+address,
			"WENDY_OPERATOR_NATIVE_SELECTOR="+MeshSelector(router))
		output, err := cmd.CombinedOutput()
		cancel() // CombinedOutput has reaped the exact child before continuing.
		if err != nil {
			t.Fatalf("native process phase %s: %v\n%s", phase, err, output)
		}
		if !strings.Contains(string(output), "--- PASS: TestOperatorNativeRestartChild") {
			t.Fatalf("native process phase %s did not run its assertions: %s", phase, output)
		}
		data, err := os.ReadFile(report)
		if err != nil {
			t.Fatal(err)
		}
		var result nativeRestartResult
		if err := json.Unmarshal(data, &result); err != nil || result.PID == os.Getpid() || result.PID == 0 {
			t.Fatalf("phase did not execute in an independent native process: %+v %v", result, err)
		}
		if phase == "blocked" {
			if service.calls.Load() != before || admitted.Load() != expectedAdmissions {
				t.Fatal("restarted disconnected client emitted authenticated traffic")
			}
		} else {
			expectedAdmissions++
			if service.calls.Load() <= before || admitted.Load() != expectedAdmissions {
				t.Fatalf("phase %s did not perform a new authenticated attachment and management RPC: admissions=%d calls=%d", phase, admitted.Load(), service.calls.Load())
			}
			if endpoint == "" {
				endpoint = result.Endpoint
			}
			if result.Endpoint != endpoint || endpoint == "" || endpoint == router.UUID().String() {
				t.Fatalf("process restart changed the persisted operator endpoint: %q -> %q", endpoint, result.Endpoint)
			}
		}
		// Process exit must release the old real router attachment before the
		// next process is allowed to open a fresh one. No reused PID is signaled.
		deadline := time.NewTimer(3 * time.Second)
		for withdrawn.Load() != expectedAdmissions {
			select {
			case <-deadline.C:
				t.Fatalf("phase %s retained a router attachment after process exit: %d/%d", phase, withdrawn.Load(), expectedAdmissions)
			case <-time.After(10 * time.Millisecond):
			}
		}
		deadline.Stop()
		t.Logf("fresh native process completed %s; authenticated attachments=%d withdrawn=%d", phase, admitted.Load(), withdrawn.Load())
	}
	recipe, err := readOperatorRecipe(operatorRecipePath(directory, principal))
	if err != nil || !recipe.Disconnected {
		t.Fatalf("final explicit disconnect was not retained on disk: %+v %v", recipe, err)
	}
}

func TestOperatorNativeRestartChild(t *testing.T) {
	phase := os.Getenv("WENDY_OPERATOR_NATIVE_PHASE")
	if phase == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
	defer cancel()
	selector := os.Getenv("WENDY_OPERATOR_NATIVE_SELECTOR")
	router, _, err := ParseMeshSelector(selector)
	if err != nil {
		t.Fatal(err)
	}
	var starts atomic.Int32
	openOperatorSession = func(ctx context.Context, options operatormesh.Options) (*operatormesh.Session, error) {
		starts.Add(1)
		options.CarrierSources = []carriers.Source{func(ctx context.Context, emit func([]carriers.Candidate)) error {
			emit([]carriers.Candidate{{Device: router, Transport: carriers.TCP, Address: os.Getenv("WENDY_OPERATOR_BROKER_ADDRESS"), Cost: 128}})
			<-ctx.Done()
			return ctx.Err()
		}}
		return operatormesh.Open(ctx, options)
	}
	EnableEmbeddedHost()
	cfg, err := config.Load() // Fresh process reads the real persisted profile.
	if err != nil {
		t.Fatal(err)
	}
	auth, err := config.ResolveAuth(cfg, "", nil)
	if err != nil || len(auth.Certificates) != 1 {
		t.Fatalf("read isolated native profile: %v", err)
	}
	certificate := auth.Certificates[0]
	result := nativeRestartResult{PID: os.Getpid()}
	if phase == "blocked" {
		checks := []struct {
			name string
			run  func() error
		}{
			{"passive automatic", func() error {
				c, e := PrepareOperatorAutomatic(ctx, certificate)
				if c != nil {
					c.Close()
				}
				return e
			}},
			{"mesh target", func() error {
				c, e := ConnectOperator(ctx, selector)
				if c != nil {
					c.Close()
				}
				return e
			}},
			{"nearby discovery", func() error { _, e := ReadOperatorDirectoryAutomatic(ctx); return e }},
		}
		for _, check := range checks {
			if err := check.run(); !errors.Is(err, errOperatorDisconnected) {
				t.Fatalf("fresh native client lost persisted disconnect at %s: %v", check.name, err)
			}
		}
		if starts.Load() != 0 {
			t.Fatal("disconnected restart opened an operator session")
		}
	} else {
		prepare := PrepareOperatorAutomatic
		if phase == "enable-exit" || phase == "explicit-resume" {
			prepare = EnableOperatorAutomatic
		} else if phase != "passive-disconnect" && phase != "passive-cleanup" {
			t.Fatalf("unknown native restart phase %q", phase)
		}
		ready, err := prepare(ctx, certificate)
		if err != nil {
			t.Fatal(err)
		}
		ready.Close()
		client, err := ConnectOperator(ctx, selector)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
		if err != nil || response.GetVersion() != "retained-operator" || !client.IsSessionProxy {
			t.Fatalf("restarted native broker did not carry authenticated RPC: %v %v", response, err)
		}
		view, err := ReadOperatorTopology(ctx)
		if err != nil || view.SelfPrincipal != certificate.PrincipalURI {
			t.Fatalf("restarted native topology: %v %v", view, err)
		}
		result.Endpoint = view.SelfDeviceId
		client.Close()
		if phase == "passive-disconnect" || phase == "passive-cleanup" {
			if err := DisconnectOperator(ctx); err != nil {
				t.Fatal("explicit native disconnect did not drain its broker", err)
			}
		}
		if starts.Load() != 1 {
			t.Fatalf("fresh native app created %d broker sessions", starts.Load())
		}
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("WENDY_OPERATOR_NATIVE_REPORT"), data, 0600); err != nil {
		t.Fatal(err)
	}
	// No SetEmbeddedActive(false) or recipe rewrite on exit: the OS process
	// ending is the actual app-restart boundary, not an in-memory reset helper.
}
