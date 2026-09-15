package commands

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sirupsen/logrus"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// buildProgressInteractive and buildProgressOut are indirection points so tests
// can force non-interactive rendering and capture output. In production they
// resolve to isInteractiveTerminal() and os.Stdout.
var (
	buildProgressInteractive           = func() bool { return isInteractiveTerminal() }
	buildProgressOut         io.Writer = os.Stdout
	buildProgressProgram               = func(model tea.Model) *tea.Program { return tui.NewProgressProgram(model) }
)

func forceBuildProgressInteractive(v bool) func() {
	prev := buildProgressInteractive
	buildProgressInteractive = func() bool { return v }
	return func() { buildProgressInteractive = prev }
}

func setBuildProgressOut(w io.Writer) func() {
	prev := buildProgressOut
	buildProgressOut = w
	return func() { buildProgressOut = prev }
}

// runBuildProgressProgram keeps in-process BuildKit/containerd diagnostics from
// moving the cursor underneath the live renderer. These libraries also use the
// global logger, so a logger on the build context alone does not catch them.
// The CLI owns one interactive build view, shared by all service workers.
func runBuildProgressProgram(prog *tea.Program) (tea.Model, error) {
	logger := logrus.StandardLogger()
	out := logger.Out
	logs := &boundedBuffer{max: maxRawBuildCapture}
	logger.SetOutput(logs)
	defer func() {
		// SetOutput waits for in-flight log writes before we read the buffer.
		logger.SetOutput(out)
		_, _ = out.Write(logs.Bytes())
	}()
	return prog.Run()
}

// maxRawBuildCapture bounds the raw buildx log retained for failure replay.
const maxRawBuildCapture = 256 << 10

// buildxRawJSONMinor is the buildx minor version that introduced
// --progress=rawjson (v0.13.0).
const buildxRawJSONMinor = 13

var (
	buildxProgressModeOnce sync.Once
	buildxProgressModeVal  string
	buildxVersionRe        = regexp.MustCompile(`\bv(\d+)\.(\d+)\.`)
)

// buildxProgressMode picks the progress format to ask buildx for.
//
// rawjson (buildx >= 0.13) reports exact per-vertex byte counters and identifies
// vertices by digest, which is what makes download rate and percentage honest
// rather than re-parsed from formatted text. plain is the fallback, and is all
// the Apple Container CLI speaks. tui.BuildParser accepts either format and
// detects it per line, so a wrong guess here costs detail, not correctness.
//
// WENDY_BUILD_PROGRESS overrides the probe for debugging.
func buildxProgressMode(ctx context.Context) string {
	buildxProgressModeOnce.Do(func() {
		buildxProgressModeVal = detectBuildxProgressMode(ctx)
	})
	return buildxProgressModeVal
}

func detectBuildxProgressMode(ctx context.Context) string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WENDY_BUILD_PROGRESS"))) {
	case "plain":
		return "plain"
	case "rawjson":
		return "rawjson"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, "docker", "buildx", "version").Output()
	if err != nil {
		return "plain"
	}
	m := buildxVersionRe.FindSubmatch(out)
	if m == nil {
		return "plain"
	}
	major, err1 := strconv.Atoi(string(m[1]))
	minor, err2 := strconv.Atoi(string(m[2]))
	if err1 != nil || err2 != nil {
		return "plain"
	}
	if major > 0 || minor >= buildxRawJSONMinor {
		return "rawjson"
	}
	return "plain"
}

// runAppleContainerBuildWithProgress builds a local image with the Apple
// Container CLI, rendering its --progress=plain output through the shared build
// progress UI (the same renderer used by the buildx/buildctl paths).
func runAppleContainerBuildWithProgress(ctx context.Context, dir, imageName, platform, dockerfile string) error {
	title := fmt.Sprintf("Building Apple Container image %s for %s...", tui.Value(imageName), tui.Value(platform))
	return runBuildWithProgress(ctx, title, dumpRawAlways, func(buildCtx context.Context, stream, logw io.Writer) error {
		return buildImageWithAppleContainer(buildCtx, dir, imageName, platform, dockerfile, nil, stream, logw)
	})
}

// dumpRawAlways is the dumpRawOnFailure predicate for build paths whose
// failures are always surfaced to the user (no fallback rebuild follows).
func dumpRawAlways(error) bool { return true }

// dumpRawUnlessRegistryUnavailable suppresses the raw buildx replay when the
// failure was converted to the friendly "no registry on the Mac agent" error —
// the retried-EOF spam would bury the actionable message.
func dumpRawUnlessRegistryUnavailable(err error) bool { return !isRegistryUnavailable(err) }

// buildSetupStepID is the synthetic vertex ID for the builder-setup step
// emitted by newBuildSetupStepWriter. It cannot collide with IDs the parser
// itself issues: "#N" (plain), a rawjson vertex digest, or
// tui.BuildExportVertexID ("export").
const buildSetupStepID = "wendy:builder-setup"

// buildSetupDetailMaxRunes caps a setup-log line's Detail at the same length
// tui.maxDetailLen uses for step progress lines. That constant is unexported,
// so this mirrors the convention rather than importing it.
const buildSetupDetailMaxRunes = 72

// truncateSetupDetail trims s to at most buildSetupDetailMaxRunes runes,
// trimming on rune boundaries so multi-byte output is never cut mid-character.
func truncateSetupDetail(s string) string {
	if utf8.RuneCountInString(s) <= buildSetupDetailMaxRunes {
		return s
	}
	r := []rune(s)
	return string(r[:buildSetupDetailMaxRunes-1]) + "…"
}

// newBuildSetupStepWriter returns the writer passed to build() as logw, and a
// finish func to call when the setup phase is over. Every byte written is
// mirrored into tee — the setupLog failure-replay buffer — before any line
// parsing happens, so the dump-on-failure contract is preserved exactly
// regardless of how lines get split for display. Each complete line is also
// turned into a synthetic Running BuildStepEvent (ID buildSetupStepID, Kind
// tui.BuildVertexSetup, Display "preparing buildx builder", Detail the
// trimmed line capped at buildSetupDetailMaxRunes runes) so the cold
// "docker buildx create" / "docker buildx inspect --bootstrap" wait is
// visible in both renderers instead of reading as a silent hang: the plain
// renderer's heartbeat ticks for any running step (tui/buildplain.go), and
// the interactive renderer shows a spinner row. Kind BuildVertexSetup keeps
// it out of both renderers' cached/rebuilt tally.
//
// finish first flushes any buffered trailing line that never got a '\n' (a
// message printed just before the process exited) as one last Running event,
// then emits a terminal Done event carrying the elapsed duration. It is
// idempotent, and it is a no-op if no setup-log line was ever seen (e.g. the
// buildx builder was already warm and bootstrapOCIBuilder was never invoked)
// so a synthetic step never appears out of nowhere. The writer has its own
// mutex because cmd.Stdout/Stderr copiers can be separate goroutines.
func newBuildSetupStepWriter(emit func(tui.BuildStepEvent), tee io.Writer) (w io.Writer, finish func()) {
	sw := &buildSetupStepWriter{emit: emit, tee: tee, start: time.Now()}
	return sw, sw.finish
}

type buildSetupStepWriter struct {
	emit  func(tui.BuildStepEvent)
	tee   io.Writer
	start time.Time

	mu       sync.Mutex
	buf      []byte
	started  bool
	finished bool
}

func (s *buildSetupStepWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.tee != nil {
		if _, err := s.tee.Write(p); err != nil {
			return 0, err
		}
	}

	s.buf = append(s.buf, p...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(s.buf[:i]))
		s.buf = s.buf[i+1:]
		if line == "" {
			// Deliberate: a blank line alone must not flip started to true —
			// otherwise a build whose only logw traffic is blank chatter would
			// still get a synthetic Done, defeating the "no setup output => no
			// synthetic step" contract that keeps already-warm builds quiet.
			continue
		}
		s.emitRunning(line)
	}
	return len(p), nil
}

// emitRunning emits a Running event for one detail line and marks the step
// started. Callers must hold s.mu.
func (s *buildSetupStepWriter) emitRunning(line string) {
	s.started = true
	s.emit(tui.BuildStepEvent{
		ID:      buildSetupStepID,
		Kind:    tui.BuildVertexSetup,
		Display: "preparing buildx builder",
		Status:  tui.BuildStepRunning,
		Detail:  truncateSetupDetail(line),
	})
}

func (s *buildSetupStepWriter) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.finished = true

	// Flush a trailing unterminated line (e.g. an error printed without a
	// final newline before the process exits). Write only turns complete
	// '\n'-terminated lines into Running events, so without this the last
	// thing the setup command said would reach tee (the failure-replay
	// buffer) but silently vanish from the live synthetic step.
	if line := strings.TrimSpace(string(s.buf)); line != "" {
		s.buf = nil
		s.emitRunning(line)
	}

	if !s.started {
		return
	}
	s.emit(tui.BuildStepEvent{
		ID:      buildSetupStepID,
		Kind:    tui.BuildVertexSetup,
		Display: "preparing buildx builder",
		Status:  tui.BuildStepDone,
		Dur:     time.Since(s.start),
	})
}

// firstWriteHook wraps w so hook runs exactly once, immediately before the
// first Write is forwarded. runBuildWithProgress uses it to mark builder
// setup finished the instant buildx's own build output starts arriving on
// stream — buildx talking means the setup/bootstrap phase is over.
func firstWriteHook(w io.Writer, hook func()) io.Writer {
	return &firstWriteHookWriter{w: w, hook: hook}
}

type firstWriteHookWriter struct {
	w    io.Writer
	once sync.Once
	hook func()
}

func (f *firstWriteHookWriter) Write(p []byte) (int, error) {
	f.once.Do(f.hook)
	return f.w.Write(p)
}

// runBuildWithProgress runs build, rendering its buildx output as a clean live
// step list (interactive) or concise per-step lines (non-interactive). When a
// build fails and dumpRawOnFailure(err) is true, the useful step/cause/location
// are printed and the detailed raw output is retained in a temporary log file
// (never on cancellation). Setup-log chatter written to logw is retained in
// the same file, AND (WDY-2432) rendered live as a synthetic "preparing
// buildx builder" step via newBuildSetupStepWriter, so a cold builder
// bootstrap is visible rather than a silent gap until either it fails or
// buildx's own output starts (which flips the synthetic step to Done).
// Callers whose failures can trigger a fallback rebuild (which would repeat
// the same diagnostics) use the predicate to stay quiet in those cases.
func runBuildWithProgress(ctx context.Context, title string, dumpRawOnFailure func(error) bool, build func(context.Context, io.Writer, io.Writer) error) error {
	start := time.Now()
	raw := &boundedBuffer{max: maxRawBuildCapture}
	var setupLog bytes.Buffer

	if !buildProgressInteractive() {
		emit, tally, stopHeartbeat := tui.NewBuildPlainRenderer(buildProgressOut)
		parser := tui.NewBuildParser(emit)
		buildStream := io.MultiWriter(parser, raw)
		setupw, finishSetup := newBuildSetupStepWriter(emit, &setupLog)
		stream := firstWriteHook(buildStream, finishSetup)
		fmt.Fprintf(buildProgressOut, "%s\n", title)
		err := build(ctx, stream, setupw)
		finishSetup() // idempotent; catches builds that never write to stream
		stopHeartbeat()
		if err != nil {
			if ctx.Err() == nil && dumpRawOnFailure(err) {
				renderBuildFailure(buildProgressOut, "", string(raw.Bytes())+setupLog.String(), err)
			}
			return err
		}
		printBuildSummary(buildProgressOut, tally(), time.Since(start))
		return nil
	}

	// Interactive: run the steps model while the build streams events to it.
	m := tui.NewBuildStepsModel(title)
	prog := buildProgressProgram(m)
	emit := func(e tui.BuildStepEvent) { prog.Send(tui.BuildStepMsg(e)) }
	parser := tui.NewBuildParser(emit)
	buildStream := io.MultiWriter(parser, raw)
	setupw, finishSetup := newBuildSetupStepWriter(emit, &setupLog)
	stream := firstWriteHook(buildStream, finishSetup)
	buildCtx, cancelBuild := context.WithCancel(ctx)
	defer cancelBuild()

	buildErrC := make(chan error, 1)
	go func() {
		err := build(buildCtx, stream, setupw)
		finishSetup() // idempotent; catches builds that never write to stream
		prog.Send(tui.BuildAllDoneMsg{Err: err})
		buildErrC <- err
	}()

	final, runErr := runBuildProgressProgram(prog)
	if runErr != nil {
		cancelBuild()
		<-buildErrC
		return fmt.Errorf("build progress UI: %w", runErr)
	}
	fm, ok := final.(tui.BuildStepsModel)
	if !ok {
		cancelBuild()
		<-buildErrC
		return fmt.Errorf("build progress UI: unexpected final model %T", final)
	}
	if cancelErr := fm.Err(); cancelErr == tui.ErrCancelled {
		// Bubble Tea consumes SIGINT for its own event loop. Explicitly cancel
		// the solve as well, then wait for the builder goroutine to exit before
		// returning. Without this, the UI disappeared while docker/buildctl/
		// Apple Container kept running and later fallbacks could start another
		// build after the user had already cancelled.
		cancelBuild()
		<-buildErrC
		return ErrUserCancelled
	}
	buildErr := <-buildErrC
	if buildErr != nil {
		if ctx.Err() == nil && dumpRawOnFailure(buildErr) {
			renderBuildFailure(buildProgressOut, "", string(raw.Bytes())+setupLog.String(), buildErr)
		}
		return buildErr
	}
	elapsed := time.Since(start)
	printBuildSummary(buildProgressOut, fm.Tally(), elapsed)
	maybeSuggestOptimizeAfterBuild(fm.Tally(), elapsed)
	return nil
}

func printBuildSummary(w io.Writer, t tui.BuildTally, d time.Duration) {
	fmt.Fprintf(w, "✓ Built & pushed (%d cached, %d rebuilt) in %s\n",
		t.Cached, t.Rebuilt, d.Round(time.Millisecond))
}

// boundedBuffer keeps only the last max bytes written to it.
type boundedBuffer struct {
	max int
	buf []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = append([]byte(nil), b.buf[len(b.buf)-b.max:]...)
	}
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte { return b.buf }

func (b *boundedBuffer) String() string { return string(b.buf) }
