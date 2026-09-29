package mcp

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

type progressEvent struct {
	value   float64
	message string
}

func TestRunProgressThrottlesAndStaysBelowCompletion(t *testing.T) {
	clock := time.Unix(0, 0)
	var got []progressEvent
	p := newRunProgress(func(v float64, m string) { got = append(got, progressEvent{v, m}) })
	p.now = func() time.Time { return clock }

	_, _ = p.Write([]byte("step 1\nstep 2\n")) // sends "step 2"
	_, _ = p.Write([]byte("step 3\n"))         // throttled: same instant
	clock = clock.Add(2 * time.Second)
	_, _ = p.Write([]byte("partial "))      // no newline yet
	_, _ = p.Write([]byte("line\n\n   \n")) // sends "partial line"
	clock = clock.Add(2 * time.Second)
	_, _ = p.Write([]byte(strings.Repeat("é", 300) + "\n"))

	if len(got) != 3 || got[0].message != "step 2" || got[1].message != "partial line" {
		t.Fatalf("events = %+v", got)
	}
	if len(got[2].message) > runProgressMaxMessage || !strings.HasPrefix(got[2].message, "é") {
		t.Fatalf("long line not capped on a rune boundary: %q", got[2].message)
	}
	for i, e := range got {
		if e.value <= 0 || e.value >= 1 || (i > 0 && e.value <= got[i-1].value) {
			t.Fatalf("progress values must increase within (0,1): %+v", got)
		}
	}
}

func TestRunProgressBoundsNewlineFreeOutput(t *testing.T) {
	p := newRunProgress(func(float64, string) {})
	_, _ = p.Write([]byte(strings.Repeat("x", 3*runProgressMaxPartial)))
	if len(p.partial) > runProgressMaxPartial {
		t.Fatalf("partial buffer grew to %d", len(p.partial))
	}
}

func TestRunAttachesProgressSinkOnlyForProgressRequests(t *testing.T) {
	project := runProject(t)
	for _, withToken := range []bool{false, true} {
		s := New(&config.Config{}, nil)
		var sink *runProgress
		s.runCommandFn = func(ctx context.Context, _ []string, _ commandTarget, _ int) (string, bool, error) {
			sink = runProgressFrom(ctx)
			return "ok", false, nil
		}
		req := callToolReq("run", map[string]any{"project_path": project, "device": "vm:test"})
		if withToken {
			req.Params.Meta = &mcpgo.Meta{ProgressToken: "tok"}
		}
		if r, err := s.handleRun(context.Background(), req); err != nil || r.IsError {
			t.Fatalf("run: %v %v", r, err)
		}
		if (sink != nil) != withToken {
			t.Fatalf("withToken=%v: sink attached=%v", withToken, sink != nil)
		}
	}
}

// The test binary doubles as the run child so executeRunCommand is exercised
// end to end: both streams reach the tail and the progress sink.
func TestRunChildHelperProcess(t *testing.T) {
	if os.Getenv("WENDY_MCP_RUN_HELPER") != "1" {
		t.Skip("helper process for TestExecuteRunCommandFeedsProgress")
	}
	fmt.Fprintln(os.Stdout, "building image")
	fmt.Fprintln(os.Stderr, "deploying to device")
	os.Exit(0)
}

func TestExecuteRunCommandFeedsProgress(t *testing.T) {
	t.Setenv("WENDY_MCP_RUN_HELPER", "1")
	var got []progressEvent
	sink := newRunProgress(func(v float64, m string) { got = append(got, progressEvent{v, m}) })
	sink.interval = 0
	ctx := withRunProgress(context.Background(), sink)
	out, truncated, err := executeRunCommand(ctx, []string{"-test.run=^TestRunChildHelperProcess$"}, commandTarget{}, 16384)
	if err != nil || truncated {
		t.Fatalf("executeRunCommand: %q truncated=%v err=%v", out, truncated, err)
	}
	if !strings.Contains(out, "building image") || !strings.Contains(out, "deploying to device") {
		t.Fatalf("tail lost output: %q", out)
	}
	if len(got) == 0 {
		t.Fatal("no progress events from the child's output")
	}
}
