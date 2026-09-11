package commands

// Board-agnostic machinery for the USB-recovery install flows: the step
// progress UI, download verification, and waiting for a board to appear.
// Shared by the Jetson (Thor, Orin) and Dragonwing installers.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// Step IDs for the flashing progress list.
const (
	stepDownload = iota
	stepProvision
	stepStage1
	stepStage2
	stepFlashPartitions
)

func verifySHA256(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hashing %s: %w", filepath.Base(path), err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("download checksum mismatch: got %s, manifest says %s", got, want)
	}
	return nil
}

// ---- flashing progress UI (cross-platform) ----

type flashStep struct {
	id    int
	label string
	// abortWarning, when set, arms the steps UI's ctrl+c guard while this step
	// runs: the first ctrl+c shows the warning and only a second press within a
	// few seconds actually cancels. Steps without it keep instant cancel.
	abortWarning string
	run          func(out io.Writer, detail func(string)) (cached bool, err error)
}

func runFlashSteps(title string, steps []flashStep, cancelWork func(), logW io.Writer) (int, error) {
	if !isInteractiveTerminal() {
		return runFlashStepsPlain(title, steps, cancelWork, logW)
	}
	m := tui.NewStepsModel(title)
	prog := tui.NewProgressProgram(m)
	type outcome struct {
		failedID int
		err      error
		verbose  []byte
	}
	// curID tracks the step being run so a ctrl+c cancel can report which step
	// was interrupted (the goroutine keeps running briefly after the UI exits).
	var curID atomic.Int32
	curID.Store(-1)
	resC := make(chan outcome, 1)
	go func() {
		failedID, runErr := -1, error(nil)
		var failVerbose []byte
		for _, s := range steps {
			buf := &boundedBuffer{max: maxRawBuildCapture}
			// The step's raw output goes to the bounded in-memory buffer (dumped to
			// stdout on failure) and, unbounded, to the persistent log file.
			out := io.MultiWriter(buf, logW)
			curID.Store(int32(s.id))
			prog.Send(tui.StepAbortGuardMsg{Warning: s.abortWarning})
			prog.Send(tui.StepStartMsg{ID: s.id, Label: s.label})
			cached, err := s.run(out, func(d string) { prog.Send(tui.StepDetailMsg{ID: s.id, Detail: d}) })
			if err != nil {
				prog.Send(tui.StepFailMsg{ID: s.id})
				failedID, runErr, failVerbose = s.id, err, buf.Bytes()
				break
			}
			prog.Send(tui.StepDoneMsg{ID: s.id, Cached: cached})
		}
		resC <- outcome{failedID, runErr, failVerbose}
		prog.Send(tui.StepsDoneMsg{Err: runErr})
	}()
	final, uiErr := prog.Run()
	if uiErr != nil {
		return -1, fmt.Errorf("flash progress UI: %w", uiErr)
	}
	if cancelErr := final.(tui.StepsModel).Err(); errors.Is(cancelErr, tui.ErrCancelled) {
		// Abort the in-flight step (cancels the stage-2 engine's context) and
		// reap the worker so nothing outlives the CLI. Bounded: a step that
		// ignores cancellation (e.g. stage-1 USB ops) shouldn't wedge the exit.
		cancelWork()
		select {
		case <-resC:
		case <-time.After(10 * time.Second):
			fmt.Fprintln(os.Stderr, "warning: the flash worker didn't stop within 10s; temp files may be left behind")
		}
		return int(curID.Load()), cancelErr
	}
	res := <-resC
	if res.err != nil {
		os.Stdout.Write(res.verbose)
	}
	return res.failedID, res.err
}

func runFlashStepsPlain(title string, steps []flashStep, cancelWork func(), logW io.Writer) (int, error) {
	fmt.Println(title)
	for _, s := range steps {
		fmt.Printf("==> %s\n", s.label)
		out := io.MultiWriter(os.Stdout, logW)
		// A non-interactive flash has no live progress UI, and the multi-minute
		// rootfs write produces no output; a periodic heartbeat keeps CI/SSH
		// idle-output timeouts from killing the process mid-write.
		done := make(chan struct{})
		go stepHeartbeat(out, s.label, done)
		cached, err := runFlashStepPlain(s, out, cancelWork)
		close(done)
		if err != nil {
			return s.id, err
		}
		if cached {
			fmt.Println("    (cached)")
		}
	}
	return -1, nil
}

func runFlashStepPlain(step flashStep, out io.Writer, cancelWork func()) (bool, error) {
	if step.abortWarning == "" {
		return step.run(out, func(string) {})
	}
	type result struct {
		cached bool
		err    error
	}
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt)
	defer signal.Stop(signals)
	resultC := make(chan result, 1)
	go func() {
		cached, err := step.run(out, func(string) {})
		resultC <- result{cached: cached, err: err}
	}()

	var armedAt time.Time
	const abortWindow = 3 * time.Second
	for {
		select {
		case res := <-resultC:
			return res.cached, res.err
		case <-signals:
			if armedAt.IsZero() || time.Since(armedAt) > abortWindow {
				armedAt = time.Now()
				fmt.Fprintln(os.Stderr, "\n"+step.abortWarning)
				continue
			}
			cancelWork()
			select {
			case <-resultC:
			case <-time.After(10 * time.Second):
				fmt.Fprintln(os.Stderr, "warning: the flash worker didn't stop within 10s; temp files may be left behind")
			}
			return false, tui.ErrCancelled
		}
	}
}

// stepHeartbeat prints a periodic "still in progress" line until done is closed.
func stepHeartbeat(w io.Writer, label string, done <-chan struct{}) {
	start := time.Now()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			fmt.Fprintf(w, "    … %s still in progress (%s elapsed)\n", label, time.Since(start).Round(time.Second))
		}
	}
}

func throttledDetail(detail func(string), format func(downloaded, total int64) string) func(downloaded, total int64) {
	var lastNanos atomic.Int64
	const minInterval = int64(66 * time.Millisecond)
	return func(downloaded, total int64) {
		now := time.Now().UnixNano()
		prev := lastNanos.Load()
		if now-prev < minInterval {
			return
		}
		if !lastNanos.CompareAndSwap(prev, now) {
			return
		}
		detail(format(downloaded, total))
	}
}

// byteProgress formats transfer progress like "40% · 1.2/3.0 GiB". The percent
// is capped at 99 — completion is the step's ✓, and the stage-2 byte count can
// slightly overshoot its estimated total (push retries).
func byteProgress(written, total int64) string {
	const gib = 1 << 30
	if total <= 0 {
		return fmt.Sprintf("%.1f GiB", float64(written)/gib)
	}
	pct := min(written*100/total, 99)
	return fmt.Sprintf("%d%% · %.1f/%.1f GiB", pct, float64(written)/gib, float64(total)/gib)
}

// recoveryWaitHints carries the device-specific text the shared wait UI shows,
// so it names the right board, mode and cabling rather than hardcoding one.
type recoveryWaitHints struct {
	label       string // e.g. "Thor" or "Jetson AGX Orin"
	family      string // family named when none was found, e.g. "Jetson"
	mode        string // e.g. "USB recovery mode" or "EDL mode"
	cablingLine string // body of the "USB-C cable is in ..." bullet
	buttonLine  string // body of the "recovery button sequence: ..." bullet
}

// waitFamily and waitMode default to the Jetson wording, so callers that set
// neither keep their existing text.
func (h recoveryWaitHints) waitFamily() string {
	if h.family == "" {
		return "Jetson"
	}
	return h.family
}

func (h recoveryWaitHints) waitMode() string {
	if h.mode == "" {
		return "USB recovery mode"
	}
	return h.mode
}

// waitForRecovery handles an empty scan: the user already said the board is
// ready, so it usually means cabling or the button/switch needs another try.
// Explains what to check once, then rescans every 1.5s under a spinner until a
// board appears or the user quits. Always returns ≥1 device on success.
func waitForRecovery[T any](hints recoveryWaitHints, scan func() ([]T, error)) ([]T, error) {
	if !isInteractiveTerminal() {
		return nil, fmt.Errorf("no %s found in %s", hints.waitFamily(), hints.waitMode())
	}

	fmt.Println()
	fmt.Println(tui.WarningMessage(fmt.Sprintf(
		"No %s in %s yet — it will be picked up automatically once it appears.",
		hints.waitFamily(), hints.waitMode())))
	fmt.Println("  While this keeps scanning, double-check:")
	fmt.Println("   • " + hints.cablingLine)
	fmt.Println("   • " + hints.buttonLine)
	fmt.Println()

	p := tui.NewProgressProgram(tui.NewSpinner("Waiting for the " + hints.label + " to appear... (press q to quit)"))
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(1500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				devs, err := scan()
				if err != nil {
					p.Send(tui.SpinnerDoneMsg{Err: err})
					return
				}
				if len(devs) > 0 {
					p.Send(tui.SpinnerDoneMsg{Result: devs})
					return
				}
			}
		}
	}()

	finalModel, err := p.Run()
	close(stop)
	if err != nil {
		return nil, fmt.Errorf("recovery scan: %w", err)
	}
	model := finalModel.(tui.SpinnerModel)
	if !model.Done() {
		return nil, ErrUserCancelled // q / ctrl+c
	}
	result, serr := model.Result()
	if serr != nil {
		return nil, serr
	}
	return result.([]T), nil
}

// readQuit reads a line and reports whether the user asked to quit (q/quit).
func readQuit() bool {
	s := bufio.NewScanner(os.Stdin)
	if !s.Scan() {
		return true // EOF (e.g. closed stdin) — don't loop forever
	}
	switch strings.ToLower(strings.TrimSpace(s.Text())) {
	case "q", "quit":
		return true
	default:
		return false
	}
}
