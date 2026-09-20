package framesource

// A Source backed by a supervised helper process.
//
// See the note at the top of wire.go for why capture lives in a separate
// executable rather than in the agent. This file is the agent's side of it:
// finding the helper, starting it, reading its records, and — the part that
// matters most — failing LOUDLY and specifically when it is not installed.
// A device with a RealSense attached and no helper must say exactly that, not
// report that it has no depth camera.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"go.uber.org/zap"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// HelperRun is one running capture helper.
type HelperRun struct {
	// Records carries the helper's length-prefixed record stream (its stdout).
	Records io.ReadCloser
	// Wait blocks until the helper exits and returns its error with the tail of
	// its stderr folded in: a helper that died because librealsense could not
	// claim the USB device has already said so on stderr, and an exit status
	// alone would throw that away.
	Wait func() error
	// Stop terminates the helper and releases the camera.
	Stop func()
}

// Launcher starts a capture helper process. Behind an interface so the wire,
// the supervision and every refusal can be tested without a camera — or a
// helper binary.
type Launcher interface {
	Start(ctx context.Context, args []string) (*HelperRun, error)
	// Binary names the executable that would be started, for error messages.
	Binary() string
}

// HelperName is the executable the RealSense source runs.
const HelperName = "wendy-realsense-source"

// HelperEnvOverride points the agent at a helper somewhere else — a developer
// build, or a test's own stand-in.
const HelperEnvOverride = "WENDY_REALSENSE_HELPER"

// helperSearchDirs is where an installed helper is looked for, in order, and
// BEFORE the daemon's PATH. libexec first: this is a program the agent runs,
// not one a person does, and a stale copy somewhere on PATH must not shadow
// the one the image installed.
var helperSearchDirs = []string{
	"/usr/libexec/wendy",
	"/usr/local/libexec/wendy",
	"/opt/wendy/bin",
	"/usr/local/bin",
	"/usr/bin",
}

// ErrHelperNotInstalled is returned when no capture helper can be found. It is
// deliberately distinguishable: a missing helper is a source that is present
// but unavailable, which is a different answer to an operator than "no such
// camera".
var ErrHelperNotInstalled = errors.New("capture helper is not installed")

// FindHelper locates the capture helper. The error names every place that was
// looked, because the fix is to put the binary in one of them.
func FindHelper(name string) (string, error) {
	if override := os.Getenv(HelperEnvOverride); override != "" {
		if err := executable(override); err != nil {
			return "", fmt.Errorf("%s=%s: %w", HelperEnvOverride, override, err)
		}
		return override, nil
	}
	for _, dir := range helperSearchDirs {
		candidate := filepath.Join(dir, name)
		if err := executable(candidate); err == nil {
			return candidate, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		if err := executable(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: no %s in %v or on PATH (set %s to point at one)",
		ErrHelperNotInstalled, name, helperSearchDirs, HelperEnvOverride)
}

// executable accepts a regular file the agent may run and nobody else may
// rewrite. The agent runs as root: a helper anyone can write to is a way to
// run anything as root with the camera in hand, so it is refused wherever it
// was found -- the override included.
func executable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("not an executable file")
	}
	if info.Mode().Perm()&0o002 != 0 {
		return fmt.Errorf("world-writable; refusing to run it")
	}
	return nil
}

// ExecLauncher runs a real helper binary.
type ExecLauncher struct {
	Path   string
	Logger *zap.Logger
}

func (l ExecLauncher) Binary() string { return l.Path }

// maxHelperStderr bounds what is kept from a helper's stderr for the exit
// error. Enough for a librealsense backend complaint and the lines around it,
// small enough to sit inside a gRPC status, and not a memory story if the
// helper spins.
const maxHelperStderr = 4 << 10

func (l ExecLauncher) Start(ctx context.Context, args []string) (*HelperRun, error) {
	cmd := exec.CommandContext(ctx, l.Path, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("piping %s stdout: %w", filepath.Base(l.Path), err)
	}
	// exec.Cmd owns the stderr drain: it copies into tail on a goroutine of
	// its own, and Wait returns only once that copy has finished, so the last
	// line a dying helper wrote -- librealsense's reason for failing to claim
	// the device -- is in the buffer by the time the exit error is built.
	// Reading a StderrPipe ourselves is the pattern the os/exec docs warn
	// about: Wait closes the pipe, and the tail is the part that gets lost.
	tail := &tailBuffer{limit: maxHelperStderr, logger: l.Logger}
	cmd.Stderr = tail
	// The helper exits when its stdin closes, which is how it notices an agent
	// that died without reaping it. A pipe we never write to gives it that
	// signal for free.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("piping %s stdin: %w", filepath.Base(l.Path), err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", l.Path, err)
	}

	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = stdin.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
	}
	return &HelperRun{
		Records: stdout,
		Wait: func() error {
			err := cmd.Wait()
			if err == nil {
				return nil
			}
			if msg := tail.String(); msg != "" {
				return fmt.Errorf("%s: %w: %s", filepath.Base(l.Path), err, msg)
			}
			return fmt.Errorf("%s: %w", filepath.Base(l.Path), err)
		},
		Stop: stop,
	}, nil
}

// tailBuffer keeps the last lines of a helper's stderr within a byte budget.
// It is the io.Writer exec.Cmd drains stderr into, so it splits lines itself.
type tailBuffer struct {
	mu      sync.Mutex
	limit   int
	lines   []string
	size    int
	partial []byte // the unterminated last line, so far
	logger  *zap.Logger
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			t.partial = append(t.partial, p...)
			// A helper that writes forever without a newline is still bounded:
			// it is a tail, so the end of the line is the part that is kept.
			if len(t.partial) > t.limit {
				t.partial = append(t.partial[:0], t.partial[len(t.partial)-t.limit:]...)
			}
			break
		}
		t.partial = append(t.partial, p[:i]...)
		t.flushPartialLocked()
		p = p[i+1:]
	}
	return n, nil
}

func (t *tailBuffer) flushPartialLocked() {
	if len(t.partial) > t.limit {
		t.partial = t.partial[len(t.partial)-t.limit:]
	}
	line := string(t.partial)
	t.partial = t.partial[:0]
	if t.logger != nil {
		t.logger.Debug("realsense helper", zap.String("line", line))
	}
	t.lines = append(t.lines, line)
	t.size += len(line) + 1
	for t.size > t.limit && len(t.lines) > 1 {
		t.size -= len(t.lines[0]) + 1
		t.lines = t.lines[1:]
	}
}

// String is the retained tail, oldest line first. All of it, not the last
// line alone: the line that names the cause -- "failed to claim USB
// interface" -- is rarely the last thing a dying helper prints.
func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.partial) > 0 {
		t.flushPartialLocked()
	}
	return strings.Join(t.lines, "; ")
}

// --- the Source ---

// HelperSource is one camera reachable through a capture helper.
type HelperSource struct {
	// desc is what the helper reported for this camera when it was enumerated.
	// Open re-reads a descriptor from the capture itself, so this one is only
	// ever used to answer a listing.
	desc     *agentpbv2.CalibratedSource
	launcher Launcher
	logger   *zap.Logger
}

// NewHelperSource wraps an enumerated camera.
func NewHelperSource(desc *agentpbv2.CalibratedSource, launcher Launcher, logger *zap.Logger) *HelperSource {
	return &HelperSource{desc: desc, launcher: launcher, logger: logger}
}

func (s *HelperSource) Describe(context.Context) (*agentpbv2.CalibratedSource, error) {
	return s.desc, nil
}

func (s *HelperSource) Open(ctx context.Context, opts Options) (Stream, error) {
	args := []string{"stream", "--source", s.desc.GetSource()}
	if opts.Width > 0 {
		args = append(args, "--width", strconv.FormatUint(uint64(opts.Width), 10))
	}
	if opts.Height > 0 {
		args = append(args, "--height", strconv.FormatUint(uint64(opts.Height), 10))
	}
	if opts.Framerate > 0 {
		args = append(args, "--fps", strconv.FormatUint(uint64(opts.Framerate), 10))
	}
	run, err := s.launcher.Start(ctx, args)
	if err != nil {
		return nil, err
	}
	// The descriptor the CAPTURE reports, not the listing's. A helper that had
	// to fall back to a mode without alignment says so here, and the
	// requirement check above this catches it before the first frame reaches a
	// consumer.
	records := &recordReader{r: run.Records}
	negotiated, err := readSource(records)
	if err != nil {
		run.Stop()
		waitErr := run.Wait()
		if waitErr != nil {
			return nil, fmt.Errorf("capture helper for %s failed before it described its stream: %w",
				s.desc.GetSource(), waitErr)
		}
		return nil, fmt.Errorf("capture helper for %s ended before it described its stream: %w",
			s.desc.GetSource(), err)
	}
	return &helperStream{run: run, records: records, negotiated: negotiated, source: s.desc.GetSource()}, nil
}

type helperStream struct {
	run *HelperRun
	// records is the one reader on this helper's stdout; Next is the only
	// caller, so its buffer is reused from frame to frame.
	records    *recordReader
	negotiated *agentpbv2.CalibratedSource
	source     string
	closeOnce  sync.Once
	closeErr   error
}

// Negotiated is what the helper actually opened, as opposed to what the listing
// promised. The service checks requirements against this.
func (h *helperStream) Negotiated() *agentpbv2.CalibratedSource { return h.negotiated }

func (h *helperStream) Next(ctx context.Context) (*agentpbv2.CalibratedFrame, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := readFrame(h.records)
	if err != nil {
		if errors.Is(err, io.EOF) {
			// A clean end still means the camera stopped. Fold in whatever the
			// helper said on its way out.
			if waitErr := h.run.Wait(); waitErr != nil {
				return nil, waitErr
			}
			return nil, io.EOF
		}
		return nil, err
	}
	if f.GetSource() == "" {
		f.Source = h.source
	}
	return f, nil
}

func (h *helperStream) Close() error {
	h.closeOnce.Do(func() {
		h.run.Stop()
		_ = h.run.Records.Close()
		// The helper was killed, so a non-nil Wait error here is expected and
		// says nothing an operator needs.
		_ = h.run.Wait()
	})
	return h.closeErr
}

// --- a source that is present but cannot be opened ---

// UnavailableSource is a camera this device can SEE but cannot capture from —
// in practice, a RealSense on a machine with no capture helper installed.
//
// It is listed rather than hidden on purpose. Omitting it would tell an
// operator their camera does not exist, and the failure this whole service
// exists to prevent is precisely a missing capability that produces no error.
type UnavailableSource struct{ desc *agentpbv2.CalibratedSource }

// NewUnavailableSource describes a camera that is present but not capturable.
// reason is shown to the operator and should name the fix.
func NewUnavailableSource(name, kind, description, reason string) *UnavailableSource {
	return &UnavailableSource{desc: &agentpbv2.CalibratedSource{
		Source:            name,
		Kind:              kind,
		Description:       description,
		Available:         false,
		UnavailableReason: reason,
	}}
}

func (s *UnavailableSource) Describe(context.Context) (*agentpbv2.CalibratedSource, error) {
	return s.desc, nil
}

func (s *UnavailableSource) Open(context.Context, Options) (Stream, error) {
	return nil, errors.New(s.desc.GetUnavailableReason())
}
