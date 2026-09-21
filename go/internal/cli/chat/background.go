package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	maxBackgroundRunning  = 4
	maxBackgroundHistory  = 16
	backgroundLogBytes    = 4096
	backgroundStartupWait = 300 * time.Millisecond
)

type backgroundJobInfo struct {
	JobID      string `json:"job_id"`
	Kind       string `json:"kind"`
	State      string `json:"state"`
	PID        int    `json:"pid"`
	Device     string `json:"device"`
	Transport  string `json:"transport"`
	StartedAt  string `json:"started_at"`
	EndedAt    string `json:"ended_at,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	OutputTail string `json:"output_tail,omitempty"`
	Error      string `json:"error,omitempty"`
}

type backgroundJob struct {
	info     backgroundJobInfo // guarded by backgroundProcesses.mu
	args     []string
	cmd      *exec.Cmd
	log      *backgroundLog
	done     chan struct{}
	stopping bool
}

// Background processes belong to the chat session, not an individual turn.
// Only the bundled CLI's two media commands can reach start.
type backgroundProcesses struct {
	mu                    sync.Mutex
	executable, workspace string
	jobs                  []*backgroundJob
	nextID                uint64
	closed                bool
	closeOnce             sync.Once
	closeErr              error
}

func (p *backgroundProcesses) start(ctx context.Context, kind string, target backgroundTarget, args []string) (backgroundJobInfo, error) {
	p.mu.Lock()
	if err := ctx.Err(); err != nil {
		p.mu.Unlock()
		return backgroundJobInfo{}, err
	}
	if p.closed {
		p.mu.Unlock()
		return backgroundJobInfo{}, errors.New("chat background processes are closed")
	}
	running := 0
	for _, job := range p.jobs {
		if job.info.State == "running" {
			running++
			if !job.stopping && job.info.Kind == kind && slices.Equal(job.args, args) {
				info := p.snapshotLocked(job, true)
				p.mu.Unlock()
				return info, nil
			}
		}
	}
	if running >= maxBackgroundRunning {
		p.mu.Unlock()
		return backgroundJobInfo{}, fmt.Errorf("already running %d background viewers/listeners; stop one first", running)
	}
	if len(p.jobs) >= maxBackgroundHistory {
		for i, job := range p.jobs {
			if job.info.State != "running" {
				p.jobs = slices.Delete(p.jobs, i, i+1)
				break
			}
		}
	}
	cmd := backgroundCommand(p.executable, args...)
	cmd.Dir = p.workspace
	// Never inherit Chat's terminal. Media playback uses its own window or
	// native audio device; bounded logs are available through the status tool.
	cmd.Stdin = nil
	// A socket override takes precedence over --device and cloud flags in the
	// CLI. Replay the current MCP target even if Chat inherited a local socket.
	cmd.Env = append(os.Environ(), "NO_COLOR=1", "TERM=dumb", "WENDY_AGENT_SOCKET=")
	log := &backgroundLog{}
	cmd.Stdout, cmd.Stderr = log, log
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		p.mu.Unlock()
		return backgroundJobInfo{}, fmt.Errorf("starting %s: %w", kind, err)
	}
	p.nextID++
	job := &backgroundJob{args: slices.Clone(args), cmd: cmd, log: log, done: make(chan struct{}), info: backgroundJobInfo{
		JobID: fmt.Sprintf("media-%d", p.nextID), Kind: kind, State: "running", PID: cmd.Process.Pid,
		Device: target.Device, Transport: target.Transport, StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}}
	p.jobs = append(p.jobs, job)
	p.mu.Unlock()
	go func() {
		err := cmd.Wait()
		// A player can outlive its CLI parent, including after an early exit.
		// Reap the complete process group before marking the job finished.
		_ = killBackground(cmd)
		p.mu.Lock()
		job.info.EndedAt = time.Now().UTC().Format(time.RFC3339Nano)
		code := cmd.ProcessState.ExitCode()
		job.info.ExitCode = &code
		switch {
		case job.stopping:
			job.info.State = "stopped"
		case err != nil:
			job.info.State, job.info.Error = "failed", err.Error()
		default:
			job.info.State = "exited"
		}
		close(job.done)
		p.mu.Unlock()
	}()
	// Catch immediate failures without waiting for the lifetime of the stream.
	timer := time.NewTimer(backgroundStartupWait)
	defer timer.Stop()
	select {
	case <-job.done:
	case <-timer.C:
	case <-ctx.Done():
		_, _ = p.stop(job.info.JobID)
		return p.snapshot(job, true), ctx.Err()
	}
	info := p.snapshot(job, true)
	if info.State == "failed" {
		return info, fmt.Errorf("%s exited during startup: %s", kind, info.Error)
	}
	return info, nil
}

func (p *backgroundProcesses) snapshotLocked(job *backgroundJob, logs bool) backgroundJobInfo {
	info := job.info
	if logs {
		info.OutputTail = job.log.String()
	}
	return info
}

func (p *backgroundProcesses) snapshot(job *backgroundJob, logs bool) backgroundJobInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshotLocked(job, logs)
}

func (p *backgroundProcesses) list(id string) ([]backgroundJobInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	jobs := make([]backgroundJobInfo, 0, len(p.jobs))
	for _, job := range p.jobs {
		if id == "" || job.info.JobID == id {
			jobs = append(jobs, p.snapshotLocked(job, id != ""))
		}
	}
	if id != "" && len(jobs) == 0 {
		return nil, fmt.Errorf("unknown background job %q; use background_process_list", id)
	}
	return jobs, nil
}

func (p *backgroundProcesses) stop(id string) (backgroundJobInfo, error) {
	p.mu.Lock()
	var found *backgroundJob
	for _, job := range p.jobs {
		if job.info.JobID == id {
			found = job
			break
		}
	}
	if found == nil {
		p.mu.Unlock()
		return backgroundJobInfo{}, fmt.Errorf("unknown background job %q; only this chat's jobs can be stopped", id)
	}
	if found.info.State != "running" {
		info := p.snapshotLocked(found, true)
		p.mu.Unlock()
		return info, nil
	}
	first := !found.stopping
	found.stopping = true
	p.mu.Unlock()
	if first {
		_ = interruptBackground(found.cmd)
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-found.done:
		return p.snapshot(found, true), nil
	case <-timer.C:
		_ = killBackground(found.cmd)
	}
	timer.Reset(2 * time.Second)
	select {
	case <-found.done:
		return p.snapshot(found, true), nil
	case <-timer.C:
		return p.snapshot(found, true), errors.New("background process has not exited after termination; check its status")
	}
}

func (p *backgroundProcesses) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		ids := make([]string, 0, len(p.jobs))
		for _, job := range p.jobs {
			if job.info.State == "running" {
				ids = append(ids, job.info.JobID)
			}
		}
		p.mu.Unlock()
		var wg sync.WaitGroup
		var errsMu sync.Mutex
		for _, id := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := p.stop(id)
				if err != nil {
					errsMu.Lock()
					p.closeErr = errors.Join(p.closeErr, err)
					errsMu.Unlock()
				}
			}()
		}
		wg.Wait()
	})
	return p.closeErr
}

// Retain recent diagnostics, including failures after hours of playback.
type backgroundLog struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

func (b *backgroundLog) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(data)
	if len(b.data)+n > backgroundLogBytes {
		b.truncated = true
	}
	if n >= backgroundLogBytes {
		b.data = append(b.data[:0], data[n-backgroundLogBytes:]...)
	} else {
		if excess := len(b.data) + n - backgroundLogBytes; excess > 0 {
			b.data = b.data[excess:]
		}
		b.data = append(b.data, data...)
	}
	return n, nil
}

func (b *backgroundLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	text := strings.ToValidUTF8(string(b.data), "?")
	if b.truncated {
		text = "[Earlier output omitted.]\n" + text
	}
	return text
}
