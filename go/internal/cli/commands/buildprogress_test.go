package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	containerdlog "github.com/containerd/log"
	"github.com/sirupsen/logrus"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

type buildLibraryLogModel struct {
	finished *atomic.Bool
}

func (m buildLibraryLogModel) Init() tea.Cmd {
	return func() tea.Msg {
		var wg sync.WaitGroup
		for i := 0; i < 5; i++ {
			wg.Go(func() {
				containerdlog.L.Warn("failed check for fsverity support")
			})
		}
		wg.Wait()
		return tui.BuildAllDoneMsg{}
	}
}

func (m buildLibraryLogModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(tui.BuildAllDoneMsg); ok {
		m.finished.Store(true)
		return m, tea.Quit
	}
	return m, nil
}

func (m buildLibraryLogModel) View() string { return "Building 5 service(s)...\n" }

type buildLibraryLogOutput struct {
	bytes.Buffer
	finished *atomic.Bool
	leaked   atomic.Bool
}

func (w *buildLibraryLogOutput) Write(p []byte) (int, error) {
	if !w.finished.Load() {
		w.leaked.Store(true)
	}
	return w.Buffer.Write(p)
}

func TestBuildProgressDefersLibraryLogsUntilViewCloses(t *testing.T) {
	logger := logrus.StandardLogger()
	original := logger.Out
	t.Cleanup(func() { logger.SetOutput(original) })
	finished := &atomic.Bool{}
	out := &buildLibraryLogOutput{finished: finished}
	logger.SetOutput(out)
	prog := tui.NewProgressProgram(buildLibraryLogModel{finished: finished},
		tea.WithInput(nil), tea.WithOutput(io.Discard))
	if _, err := runBuildProgressProgram(prog); err != nil {
		t.Fatal(err)
	}
	if out.leaked.Load() {
		t.Fatal("library warning reached terminal while progress view was active")
	}
	if got := strings.Count(out.String(), "failed check for fsverity support"); got != 5 {
		t.Fatalf("got %d warnings after build, want 5: %q", got, out.String())
	}
	if logger.Out != out {
		t.Fatal("library logger output was not restored")
	}
}

func TestRunBuildWithProgressCtrlCCancelsAndJoinsBuilder(t *testing.T) {
	restoreInteractive := forceBuildProgressInteractive(true)
	defer restoreInteractive()
	originalProgram := buildProgressProgram
	defer func() { buildProgressProgram = originalProgram }()
	buildProgressProgram = func(model tea.Model) *tea.Program {
		return tui.NewProgressProgram(model,
			tea.WithInput(strings.NewReader("\x03")),
			tea.WithOutput(io.Discard),
		)
	}

	started := make(chan struct{})
	exited := make(chan struct{})
	err := runBuildWithProgress(context.Background(), "Building image...", dumpRawAlways, func(ctx context.Context, _, _ io.Writer) error {
		close(started)
		<-ctx.Done()
		close(exited)
		return ctx.Err()
	})
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("err = %v, want ErrUserCancelled", err)
	}
	select {
	case <-started:
	default:
		t.Fatal("builder never started")
	}
	select {
	case <-exited:
	default:
		t.Fatal("runBuildWithProgress returned before the cancelled builder exited")
	}
}

func TestRunBuildWithProgressPlainSuccess(t *testing.T) {
	// Force non-interactive rendering and capture stdout via the package sink.
	restore := forceBuildProgressInteractive(false)
	defer restore()
	var out strings.Builder
	restoreOut := setBuildProgressOut(&out)
	defer restoreOut()

	err := runBuildWithProgress(context.Background(), "Building image...", dumpRawAlways, func(_ context.Context, stream, logw io.Writer) error {
		io.WriteString(stream, "#9 [4/6] RUN pip install\n#9 DONE 4.3s\n")
		io.WriteString(stream, "#6 [1/6] FROM python\n#6 CACHED\n")
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "cached") || !strings.Contains(got, "4.3s") {
		t.Errorf("missing step lines:\n%s", got)
	}
	if !strings.Contains(got, "1 cached") || !strings.Contains(got, "1 rebuilt") {
		t.Errorf("missing summary tally:\n%s", got)
	}
}

// TestBuildSetupStepWriterEmitsRunningDetailAndDone exercises
// newBuildSetupStepWriter directly: partial lines split across Write calls
// must still buffer into whole-line Detail updates, a trailing line with no
// final '\n' must be flushed by finish() rather than silently dropped from
// the live view, finish() must report a positive elapsed Dur, a second
// finish() call must be a no-op, and every byte written must reach tee
// untouched (the setupLog failure-replay contract).
func TestBuildSetupStepWriterEmitsRunningDetailAndDone(t *testing.T) {
	cases := []struct {
		name        string
		writes      []string // concatenated and written to w, one Write call each
		wantRunning []string // expected Running-event Details, in order
	}{
		{
			name: "partial line split across writes",
			writes: []string{
				"[buildx] boot",
				"strapping builder \"wendy\"\n",
				"[buildx] pulling image\n",
			},
			wantRunning: []string{
				`[buildx] bootstrapping builder "wendy"`,
				"[buildx] pulling image",
			},
		},
		{
			name: "trailing unterminated line is flushed by finish",
			writes: []string{
				"[buildx] bootstrapping builder \"wendy\"\n",
				"ERROR: no space left on device", // no trailing '\n'
			},
			wantRunning: []string{
				`[buildx] bootstrapping builder "wendy"`,
				"ERROR: no space left on device",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var events []tui.BuildStepEvent
			emit := func(e tui.BuildStepEvent) { events = append(events, e) }
			var tee bytes.Buffer

			w, finish := newBuildSetupStepWriter(emit, &tee)

			var wantTee strings.Builder
			for _, chunk := range tc.writes {
				io.WriteString(w, chunk)
				wantTee.WriteString(chunk)
			}

			time.Sleep(time.Millisecond)
			finish()
			finish() // idempotent: must not emit a second Done or re-flush

			if got := tee.String(); got != wantTee.String() {
				t.Errorf("tee = %q, want %q (every byte must reach the failure-replay buffer)", got, wantTee.String())
			}

			var running []string
			var done int
			for _, e := range events {
				if e.ID != buildSetupStepID {
					t.Fatalf("event ID = %q, want %q", e.ID, buildSetupStepID)
				}
				if e.Kind != tui.BuildVertexSetup {
					t.Fatalf("event Kind = %v, want tui.BuildVertexSetup", e.Kind)
				}
				if e.Display != "preparing buildx builder" {
					t.Fatalf("event Display = %q, want %q", e.Display, "preparing buildx builder")
				}
				switch e.Status {
				case tui.BuildStepRunning:
					running = append(running, e.Detail)
				case tui.BuildStepDone:
					done++
					if e.Dur <= 0 {
						t.Errorf("Done Dur = %v, want > 0", e.Dur)
					}
				default:
					t.Fatalf("unexpected status %v", e.Status)
				}
			}
			if !reflect.DeepEqual(running, tc.wantRunning) {
				t.Errorf("running Details = %#v, want %#v", running, tc.wantRunning)
			}
			if done != 1 {
				t.Errorf("done events = %d, want 1 (double-finish must be idempotent)", done)
			}
		})
	}
}

// TestRunBuildWithProgressSurfacesBuilderSetupStep confirms the builder-setup
// chatter written to logw (WDY-2432 / A5's streamed bootstrapOCIBuilder
// output) is now surfaced as a synthetic, completed build step in the plain
// renderer instead of only appearing on failure — while the existing
// summary/tally line remains driven solely by real Dockerfile steps.
func TestRunBuildWithProgressSurfacesBuilderSetupStep(t *testing.T) {
	restore := forceBuildProgressInteractive(false)
	defer restore()
	var out strings.Builder
	restoreOut := setBuildProgressOut(&out)
	defer restoreOut()

	err := runBuildWithProgress(context.Background(), "Building image...", dumpRawAlways, func(_ context.Context, stream, logw io.Writer) error {
		io.WriteString(logw, "[buildx] bootstrapping builder \"wendy\" (a cold start pulls the BuildKit image; this can take a few minutes)\n")
		io.WriteString(stream, "#9 [4/6] RUN pip install\n#9 DONE 4.3s\n")
		io.WriteString(stream, "#6 [1/6] FROM python\n#6 CACHED\n")
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out.String()

	if !strings.Contains(got, "preparing buildx builder") {
		t.Errorf("missing builder-setup step line:\n%s", got)
	}
	if !strings.Contains(got, "done    preparing buildx builder") {
		t.Errorf("builder-setup step not marked done:\n%s", got)
	}

	// Existing summary/tally behavior is unchanged: the synthetic setup step
	// (Kind BuildVertexSetup) does not enter the cached/rebuilt tally.
	if !strings.Contains(got, "cached") || !strings.Contains(got, "4.3s") {
		t.Errorf("missing step lines:\n%s", got)
	}
	if !strings.Contains(got, "1 cached") || !strings.Contains(got, "1 rebuilt") {
		t.Errorf("missing summary tally:\n%s", got)
	}
}

func TestRunBuildWithProgressPrintsRawOnFailure(t *testing.T) {
	restore := forceBuildProgressInteractive(false)
	defer restore()
	var out strings.Builder
	restoreOut := setBuildProgressOut(&out)
	defer restoreOut()
	originalPersist := persistBuildFailureLog
	defer func() { persistBuildFailureLog = originalPersist }()
	var saved string
	persistBuildFailureLog = func(_ string, raw string) (string, error) {
		saved = raw
		return "/tmp/wendy-build-image-test.log", nil
	}

	wantErr := errors.New("docker buildx build failed")
	err := runBuildWithProgress(context.Background(), "Building image...", dumpRawAlways, func(_ context.Context, stream, logw io.Writer) error {
		io.WriteString(stream, "#9 [4/6] RUN pip install\n")
		io.WriteString(stream, "#9 12.34 ERROR: could not find a version\n")
		io.WriteString(logw, "[buildx] bootstrapping builder\n")
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	got := out.String()
	// The terminal gets the useful cause and a pointer to the retained full log.
	if !strings.Contains(got, "could not find a version") {
		t.Errorf("build cause not summarized:\n%s", got)
	}
	if !strings.Contains(got, "Build log: /tmp/wendy-build-image-test.log") {
		t.Errorf("full log path not surfaced:\n%s", got)
	}
	if !strings.Contains(saved, "bootstrapping builder") || !strings.Contains(saved, "could not find a version") {
		t.Errorf("full raw and setup logs not retained:\n%s", saved)
	}
}

// The error a failed build returns — the run's final ✗ line — names the
// failing step and its cause; the details block above it keeps the rest.
func TestRunBuildWithProgressReturnsTheStepAndCause(t *testing.T) {
	restore := forceBuildProgressInteractive(false)
	defer restore()
	var out strings.Builder
	restoreOut := setBuildProgressOut(&out)
	defer restoreOut()
	originalPersist := persistBuildFailureLog
	defer func() { persistBuildFailureLog = originalPersist }()
	persistBuildFailureLog = func(string, string) (string, error) { return "/tmp/wendy-build-image-test.log", nil }

	buildErr := &imageBuildFailedError{errors.New("docker buildx build (OCI export) failed: exit status 1")}
	err := runBuildWithProgress(context.Background(), "Building image (OCI layout)...", shouldDumpChunkDiffBuildLog(chunkingAuto), func(_ context.Context, stream, _ io.Writer) error {
		io.WriteString(stream, readBuildFailureFixture(t, "go-compile.log"))
		return buildErr
	})
	if got, want := fmt.Sprint(err), "build failed at [build 4/4] RUN go build -o /out/app .: ./main.go:6:14: undefined: foo"; got != want {
		t.Fatalf("err = %q, want %q", got, want)
	}
	if !errors.Is(err, buildErr) || !isImageBuildFailure(err) || ErrorClass(err) != "build_failed" {
		t.Fatalf("err %q lost its build failure (class %q)", err, ErrorClass(err))
	}
	for _, want := range []string{
		"Build failure details",
		"  Step: build 4/4 — RUN go build -o /out/app .",
		// The builder's error stays in the output; CI's OCI-export retry
		// (go/scripts/test-ci.sh) greps for it.
		"  Error: docker buildx build (OCI export) failed: exit status 1",
		"  Build log: /tmp/wendy-build-image-test.log",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("details block missing %q:\n%s", want, out.String())
		}
	}
}

// --chunking=force renders every chunk-diff build failure, builder-setup ones
// included; those keep the builder's message as the run's error.
func TestRunBuildWithProgressKeepsABuilderSetupFailure(t *testing.T) {
	restore := forceBuildProgressInteractive(false)
	defer restore()
	var out strings.Builder
	restoreOut := setBuildProgressOut(&out)
	defer restoreOut()
	originalPersist := persistBuildFailureLog
	defer func() { persistBuildFailureLog = originalPersist }()
	persistBuildFailureLog = func(string, string) (string, error) { return "/tmp/wendy-build-image-test.log", nil }

	setupErr := classifyCommandError(errBuilderUnavailable, errors.New(`bootstrapping buildx builder "wendy-oci": exit status 1`))
	err := runBuildWithProgress(context.Background(), "Building image (OCI layout)...", shouldDumpChunkDiffBuildLog(chunkingForce), func(_ context.Context, stream, logw io.Writer) error {
		io.WriteString(logw, "[buildx] bootstrapping builder \"wendy-oci\"\n")
		io.WriteString(stream, "#1 [internal] booting buildkit\n#1 ERROR: error while pulling image moby/buildkit: toomanyrequests\n")
		return setupErr
	})
	if err != setupErr || ErrorClass(err) != "builder_unavailable" {
		t.Fatalf("err = %q (class %q), want the builder's own error", err, ErrorClass(err))
	}
	if !strings.Contains(out.String(), "Build failure details") || !strings.Contains(out.String(), "toomanyrequests") {
		t.Errorf("details block missing:\n%s", out.String())
	}
}

func TestRunBuildWithProgressSuppressesRawOnFailureWhenDumpDisabled(t *testing.T) {
	restore := forceBuildProgressInteractive(false)
	defer restore()
	var out strings.Builder
	restoreOut := setBuildProgressOut(&out)
	defer restoreOut()

	wantErr := errors.New("oci layout build failed")
	err := runBuildWithProgress(context.Background(), "Building image (OCI layout)...", func(error) bool { return false }, func(_ context.Context, stream, logw io.Writer) error {
		io.WriteString(stream, "#5 [3/5] RUN apt-get install\n")
		io.WriteString(stream, "#5 12.34 ERROR: package not found\n")
		io.WriteString(logw, "[buildx] starting builder instance\n")
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	got := out.String()
	// With dumpRawOnFailure=false, raw build output and setup log must NOT appear.
	if strings.Contains(got, "package not found") {
		t.Errorf("raw build output should be suppressed when dumpRawOnFailure=false, but got:\n%s", got)
	}
	if strings.Contains(got, "starting builder instance") {
		t.Errorf("setup log should be suppressed when dumpRawOnFailure=false, but got:\n%s", got)
	}
}

// Regression test for WDY-1813: an apple-container (or buildx) image-build
// failure on the default chunk-diff path is surfaced directly to the user
// without a registry-push fallback, so the captured build log must be dumped —
// previously it was discarded and the user saw only the ✗ line.
func TestChunkDiffBuildLogDumpedForImageBuildFailureUnderAutoChunking(t *testing.T) {
	restore := forceBuildProgressInteractive(false)
	defer restore()
	var out strings.Builder
	restoreOut := setBuildProgressOut(&out)
	defer restoreOut()
	originalPersist := persistBuildFailureLog
	defer func() { persistBuildFailureLog = originalPersist }()
	var saved string
	persistBuildFailureLog = func(_ string, raw string) (string, error) {
		saved = raw
		return "/tmp/wendy-build-image-test.log", nil
	}

	wantErr := &imageBuildFailedError{errors.New("container build (OCI layout) failed: exit status 1")}
	err := runBuildWithProgress(context.Background(), "Building image (OCI layout)...", shouldDumpChunkDiffBuildLog(chunkingAuto), func(_ context.Context, stream, logw io.Writer) error {
		io.WriteString(stream, "#5 [3/5] COPY Package.swift .\n")
		io.WriteString(stream, "#5 ERROR: failed to compute cache key: \"/Package.swift\": not found\n")
		io.WriteString(logw, "[apple-container] building OCI image: container build --progress plain ...\n")
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	got := out.String()
	if !strings.Contains(got, "failed to compute cache key") {
		t.Errorf("raw build output not surfaced on image-build failure:\n%s", got)
	}
	// The retained log carries the exact builder command line for reproduction.
	if !strings.Contains(saved, "building OCI image: container build") {
		t.Errorf("builder command line not retained in full log:\n%s", saved)
	}
}

func TestAppleContainerContextMonitorDiagnosesEmptyTmpTransfer(t *testing.T) {
	m := &appleContainerBuildContextMonitor{
		contextPath: "/tmp/ctxprobe",
		pathInTmp:   true,
		stats:       appleContainerContextStats{fileCount: 2},
	}
	if _, err := m.Write([]byte("#4 [internal] load build context\n#4 transferring context: 2")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Write([]byte("B\n")); err != nil {
		t.Fatal(err)
	}

	diagnosis := m.diagnosis()
	for _, want := range []string{"transferred an empty build context", "known apple-container issue", "non-/tmp directory"} {
		if !strings.Contains(diagnosis, want) {
			t.Fatalf("diagnosis missing %q:\n%s", want, diagnosis)
		}
	}
}

func TestAppleContainerContextMonitorIgnoresNonTmpOrNonEmptyTransfer(t *testing.T) {
	cases := []struct {
		name string
		m    appleContainerBuildContextMonitor
		line string
	}{
		{
			name: "non tmp path",
			m:    appleContainerBuildContextMonitor{contextPath: "/Users/me/app", pathInTmp: false, stats: appleContainerContextStats{fileCount: 2}},
			line: "#4 transferring context: 2B\n",
		},
		{
			name: "normal context transfer",
			m:    appleContainerBuildContextMonitor{contextPath: "/tmp/app", pathInTmp: true, stats: appleContainerContextStats{fileCount: 2}},
			line: "#4 transferring context: 40B\n",
		},
		{
			name: "empty local project",
			m:    appleContainerBuildContextMonitor{contextPath: "/tmp/app", pathInTmp: true, stats: appleContainerContextStats{}},
			line: "#4 transferring context: 2B\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.m.Write([]byte(tc.line)); err != nil {
				t.Fatal(err)
			}
			if got := tc.m.diagnosis(); got != "" {
				t.Fatalf("diagnosis = %q, want empty", got)
			}
		})
	}
}

func TestShouldDumpChunkDiffBuildLog(t *testing.T) {
	buildErr := &imageBuildFailedError{errors.New("boom")}
	setupErr := errors.New("creating buildx builder: boom")
	cases := []struct {
		chunking string
		err      error
		want     bool
	}{
		{chunkingAuto, buildErr, true},  // surfaced directly (#1166) → dump
		{chunkingAuto, setupErr, false}, // falls back to registry push → quiet
		{chunkingForce, buildErr, true}, // no fallback → dump
		{chunkingForce, setupErr, true}, // no fallback → dump
	}
	for _, c := range cases {
		if got := shouldDumpChunkDiffBuildLog(c.chunking)(c.err); got != c.want {
			t.Errorf("shouldDumpChunkDiffBuildLog(%q)(%v) = %v, want %v", c.chunking, c.err, got, c.want)
		}
	}
}

func TestDumpRawUnlessRegistryUnavailable(t *testing.T) {
	friendly := &registryUnavailableError{host: "mac.local", dialErr: errors.New("connection refused")}
	if dumpRawUnlessRegistryUnavailable(friendly) {
		t.Error("raw dump not suppressed for a bare registryUnavailableError")
	}
	wrapped := fmt.Errorf("building service web: %w", friendly)
	if dumpRawUnlessRegistryUnavailable(wrapped) {
		t.Error("raw dump not suppressed for a wrapped registryUnavailableError")
	}
	if !dumpRawUnlessRegistryUnavailable(errors.New("plain build failure")) {
		t.Error("raw dump suppressed for an unrelated error")
	}
}
