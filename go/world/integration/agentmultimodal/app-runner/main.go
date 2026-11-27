// app-runner bounds an ordinary app process and retains its exact output in
// its own persistence volume. It grants no credentials or World authority.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type settings struct {
	directory                string
	startFile                string
	duration                 time.Duration
	stdoutLimit, stderrLimit int64
	command                  []string
}
type receipt struct {
	Version         int    `json:"version"`
	Termination     string `json:"termination"`
	ExitCode        int    `json:"exit_code"`
	Signal          int    `json:"signal,omitempty"`
	ForwardedSignal int    `json:"forwarded_signal,omitempty"`
	StdoutBytes     int64  `json:"stdout_bytes"`
	StderrBytes     int64  `json:"stderr_bytes"`
	OutputTruncated bool   `json:"output_truncated"`
	Succeeded       bool   `json:"succeeded"`
	ChildStarted    bool   `json:"child_started"`
}
type boundedFile struct {
	mu             sync.Mutex
	file           *os.File
	limit, written int64
	truncated      bool
	failure        chan<- string
}

func (w *boundedFile) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	allowed := int64(len(p))
	if allowed > w.limit-w.written {
		allowed = w.limit - w.written
		w.truncated = true
	}
	n, err := w.file.Write(p[:int(allowed)])
	w.written += int64(n)
	if err != nil || w.truncated {
		reason := "output_limit"
		if err != nil {
			reason = "io_error"
		}
		select {
		case w.failure <- reason:
		default:
		}
		if err == nil {
			err = io.ErrShortWrite
		}
	}
	return n, err
}
func (w *boundedFile) result() (int64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written, w.truncated
}

func valid(c settings) bool {
	return filepath.IsAbs(c.directory) && filepath.Clean(c.directory) == c.directory && c.directory != "/" && (c.startFile == "" || filepath.IsAbs(c.startFile) && filepath.Clean(c.startFile) == c.startFile && c.startFile != "/") && c.duration >= time.Millisecond && c.duration <= 10*time.Minute && c.stdoutLimit >= 1 && c.stdoutLimit <= 32<<20 && c.stderrLimit >= 1 && c.stderrLimit <= 1<<20 && len(c.command) > 0 && len(c.command) <= 64 && filepath.IsAbs(c.command[0])
}

func awaitGate(ctx context.Context, path string) error {
	if path == "" {
		return ctx.Err()
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0022 != 0 {
		return errors.New("protected start-file directory required")
	}
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		st, err := os.Lstat(path)
		if err == nil {
			owner, ok := st.Sys().(*syscall.Stat_t)
			if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() != 0 || !ok || owner.Uid != 0 && owner.Uid != uint32(os.Geteuid()) {
				return errors.New("owned empty start-file required")
			}
			return ctx.Err()
		}
		if !errors.Is(err, os.ErrNotExist) {
			return errors.New("start-file unavailable")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
func newFile(dir, name string) (*os.File, error) {
	return os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
}
func saveReceipt(dir string, r receipt) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := newFile(dir, "exit.json.pending")
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	// A hard link publishes a complete receipt without replacing prior output.
	if err = os.Link(filepath.Join(dir, "exit.json.pending"), filepath.Join(dir, "exit.json")); err != nil {
		return err
	}
	if err = os.Remove(filepath.Join(dir, "exit.json.pending")); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func run(ctx context.Context, c settings) (receipt, error) {
	r := receipt{Version: 1, Termination: "start_failed", ExitCode: -1}
	if !valid(c) {
		return r, errors.New("invalid bounded app runner settings")
	}
	budget, cancel := context.WithTimeout(ctx, c.duration)
	defer cancel()
	st, err := os.Lstat(c.directory)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0022 != 0 {
		return r, errors.New("owned output directory required")
	}
	entries, err := os.ReadDir(c.directory)
	if err != nil || len(entries) != 0 {
		return r, errors.New("fresh output directory required")
	}
	out, err := newFile(c.directory, "stdout.json")
	if err != nil {
		return r, err
	}
	defer out.Close()
	errFile, err := newFile(c.directory, "stderr.log")
	if err != nil {
		return r, err
	}
	defer errFile.Close()
	failure := make(chan string, 1)
	stdout := &boundedFile{file: out, limit: c.stdoutLimit, failure: failure}
	stderr := &boundedFile{file: errFile, limit: c.stderrLimit, failure: failure}
	if err = awaitGate(budget, c.startFile); err != nil {
		r.Termination = "gate_refused"
		if errors.Is(err, context.DeadlineExceeded) {
			r.Termination = "gate_timeout"
		} else if errors.Is(err, context.Canceled) {
			r.Termination = "signal"
		}
		_ = out.Sync()
		_ = errFile.Sync()
		if saveErr := saveReceipt(c.directory, r); saveErr != nil {
			return r, saveErr
		}
		return r, err
	}
	cmd := exec.Command(c.command[0], c.command[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second
	if err = cmd.Start(); err != nil {
		_ = saveReceipt(c.directory, r)
		return r, err
	}
	r.ChildStarted = true
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
		r.Termination = "completed"
	case <-budget.Done():
		r.Termination = "timeout"
		if ctx.Err() != nil {
			r.Termination = "signal"
		}
	case r.Termination = <-failure:
	}
	if r.Termination != "completed" {
		forward := syscall.SIGTERM
		var requested *requestedSignal
		if r.Termination == "signal" && errors.As(context.Cause(ctx), &requested) {
			forward = requested.value
		}
		r.ForwardedSignal = int(forward)
		_ = syscall.Kill(-cmd.Process.Pid, forward)
		grace := time.NewTimer(250 * time.Millisecond)
		select {
		case waitErr = <-done:
			if !grace.Stop() {
				<-grace.C
			}
		case <-grace.C:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			select {
			case waitErr = <-done:
			case <-time.After(2 * time.Second):
				return r, errors.New("app process cleanup did not finish")
			}
		}
	}
	// Also remove descendants left behind by a child that exited independently.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if cmd.ProcessState != nil {
		r.ExitCode = cmd.ProcessState.ExitCode()
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			r.Signal = int(ws.Signal())
		}
	}
	r.StdoutBytes, r.OutputTruncated = stdout.result()
	var truncated bool
	r.StderrBytes, truncated = stderr.result()
	r.OutputTruncated = r.OutputTruncated || truncated
	if r.OutputTruncated {
		r.Termination = "output_limit"
	}
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		r.Termination = "output_cleanup_timeout"
	}
	if e := out.Sync(); e != nil {
		r.Termination = "io_error"
	}
	if e := errFile.Sync(); e != nil {
		r.Termination = "io_error"
	}
	r.Succeeded = r.Termination == "completed" && waitErr == nil && r.ExitCode == 0
	if err = saveReceipt(c.directory, r); err != nil {
		return r, err
	}
	if !r.Succeeded {
		return r, errors.New("app did not complete successfully")
	}
	return r, nil
}

type requestedSignal struct{ value syscall.Signal }

func (*requestedSignal) Error() string { return "runner received termination signal" }

func main() {
	f := flag.NewFlagSet("app-runner", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var c settings
	f.StringVar(&c.directory, "directory", "", "fresh service output directory")
	f.StringVar(&c.startFile, "start-file", "", "optional owned empty start gate")
	f.DurationVar(&c.duration, "duration", 0, "maximum process lifetime")
	f.Int64Var(&c.stdoutLimit, "stdout-limit", 0, "maximum exact stdout bytes")
	f.Int64Var(&c.stderrLimit, "stderr-limit", 0, "maximum stderr bytes")
	if f.Parse(os.Args[1:]) != nil {
		os.Exit(2)
	}
	c.command = f.Args()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		select {
		case s := <-signals:
			cancel(&requestedSignal{value: s.(syscall.Signal)})
		case <-ctx.Done():
		}
	}()
	if _, err := run(ctx, c); err != nil {
		os.Exit(1)
	}
}
