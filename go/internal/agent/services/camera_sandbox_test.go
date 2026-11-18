package services

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/sandbox"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// harmless is a child that starts and does nothing.
func harmless() *exec.Cmd { return exec.Command("true") }

// refuseSandbox makes this host unable to set up a sandbox, and counts how
// often it was asked.
func refuseSandbox(t *testing.T) *int {
	t.Helper()
	orig := sandboxLookup
	t.Cleanup(func() { sandboxLookup = orig })
	asked := 0
	sandboxLookup = func(string, ...string) (*sandbox.Identity, error) {
		asked++
		return nil, errors.New("setgroups: operation not permitted")
	}
	return &asked
}

func TestCameraSandboxFallsBackToRootWithOneWarning(t *testing.T) {
	asked := refuseSandbox(t)
	core, logs := observer.New(zap.WarnLevel)
	for _, mode := range []string{"", "on", "sometimes"} {
		*asked = 0
		logs.TakeAll()
		c := newCameraSandbox(zap.New(core), mode)
		unknown := logs.FilterMessageSnippet("unknown camera sandbox mode").Len()
		if want := map[string]int{"sometimes": 1}[mode]; unknown != want {
			t.Fatalf("mode %q: %d unknown-mode warnings; want %d", mode, unknown, want)
		}
		for range 3 {
			cmd := harmless()
			if err := c.start(cmd, false); err != nil {
				t.Fatalf("mode %q: a host without a sandbox must still stream: %v", mode, err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
		}
		warned := logs.FilterMessageSnippet("run as root")
		if *asked != 1 || warned.Len() != 1 {
			t.Fatalf("mode %q: %d lookups and %d warnings for 3 starts; want 1 and 1", mode, *asked, warned.Len())
		}
		if got := warned.All()[0].ContextMap()["error"]; got != "setgroups: operation not permitted" {
			t.Fatalf("mode %q: warning does not name the cause: %v", mode, got)
		}
	}
}

func TestCameraSandboxRequireRefusesInsteadOfRunningAsRoot(t *testing.T) {
	refuseSandbox(t)
	core, logs := observer.New(zap.WarnLevel)
	c := newCameraSandbox(zap.New(core), "require")
	for _, session := range []bool{false, true} {
		cmd := harmless()
		err := c.start(cmd, session)
		if err == nil || cmd.Process != nil {
			t.Fatalf("session=%v: started without a sandbox: %v", session, err)
		}
		for _, want := range []string{cameraSandboxEnv + "=require", "setgroups: operation not permitted"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("session=%v: error %q does not name %q", session, err, want)
			}
		}
	}
	if logs.FilterMessageSnippet("run as root").Len() != 0 {
		t.Fatalf("require mode claimed a fallback: %v", logs.All())
	}
}

func TestCameraSandboxOffSaysSoAndNeverLooksUp(t *testing.T) {
	asked := refuseSandbox(t)
	core, logs := observer.New(zap.WarnLevel)
	c := newCameraSandbox(zap.New(core), "off")
	cmd := harmless()
	if err := c.start(cmd, false); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if c != nil || *asked != 0 || logs.FilterMessageSnippet("camera sandbox is off").Len() != 1 {
		t.Fatalf("off: sandbox=%v lookups=%d logs=%v", c, *asked, logs.All())
	}
}

// Every launch site must go through the sandbox: with one required and
// unavailable, none of them may start its subprocess.
func TestCameraLaunchSitesAreSandboxed(t *testing.T) {
	refuseSandbox(t)
	svc := &VideoService{logger: zap.NewNop(), camSandbox: newCameraSandbox(zap.NewNop(), "require")}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	want := cameraSandboxEnv + "=require"

	err := svc.runCameraPipeline(ctx, func([]byte, frameTimestamp, agentpb.VideoCodec) bool { return true },
		"/dev/video4", gstEncoderResult{element: "x264enc"}, gstPipelinePlan{args: []string{harmless().Path}}, nil, nil, nil)
	if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), want) {
		t.Fatalf("gst-launch pipeline: %v", err)
	}
	err = svc.gstreamerFrames(ctx, []string{"fakesrc"}, func([]byte) {})
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("IP camera helper: %v", err)
	}
}

func TestCameraSandboxExplainsOnlyItsOwnPermissionFailures(t *testing.T) {
	id := &sandbox.Identity{}
	resolved := func() (*sandbox.Identity, error) { return id, nil }
	sandboxed := &cameraSandbox{device: resolved, session: resolved}
	asRoot := &cameraSandbox{
		device:  func() (*sandbox.Identity, error) { return nil, errors.New("no sandbox") },
		session: resolved,
	}
	failed := status.Error(codes.Internal, "GStreamer pipeline failed.")
	denied := "ERROR: Could not open device '/dev/video0' for reading and writing. system error: Permission denied"

	got := sandboxed.explain(failed, denied, false)
	if status.Code(got) != codes.Internal || !strings.Contains(got.Error(), cameraSandboxEnv+"=off") ||
		!strings.Contains(got.Error(), "GStreamer pipeline failed.") {
		t.Fatalf("sandboxed permission failure does not name the switch: %v", got)
	}
	inUse := errCameraInUse("/dev/video0")
	for _, tc := range []struct {
		name      string
		got, want error
	}{
		{"other failure", sandboxed.explain(failed, "Internal data stream error.", false), failed},
		{"fell back to root", asRoot.explain(failed, denied, false), failed},
		{"sandbox off", (*cameraSandbox)(nil).explain(failed, denied, false), failed},
		{"success", sandboxed.explain(nil, denied, false), nil},
		{"error carrying a reason", sandboxed.explain(inUse, denied, false), inUse},
	} {
		// Identity, not equivalence: an untouched error is the same value.
		if tc.got != tc.want {
			t.Errorf("%s: error was rewritten: %v", tc.name, tc.got)
		}
	}
}
