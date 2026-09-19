//go:build !windows

package chat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var backgroundTestTarget = backgroundTarget{Device: "test:50051", Transport: "direct"}

func backgroundTestProcesses(t *testing.T, body string) *backgroundProcesses {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "media-fixture")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &backgroundProcesses{executable: script, workspace: dir}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("closing background fixture: %v", err)
		}
		// A cleanup regression must not leave the test's descendants running.
		p.mu.Lock()
		jobs := append([]*backgroundJob(nil), p.jobs...)
		p.mu.Unlock()
		for _, job := range jobs {
			_ = killBackground(job.cmd)
		}
	})
	return p
}

func backgroundTestJob(t *testing.T, p *backgroundProcesses, id string) backgroundJobInfo {
	t.Helper()
	jobs, err := p.list(id)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("listing %q: jobs=%+v err=%v", id, jobs, err)
	}
	return jobs[0]
}

func backgroundTestWait(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

// A killed child can briefly remain a zombie until its new parent reaps it.
// Such a process cannot continue playback or retain open descriptors.
func backgroundTestProcessAlive(pid int) bool {
	output, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.TrimSpace(string(output)) != "" && !strings.HasPrefix(strings.TrimSpace(string(output)), "Z")
}

func TestBackgroundProcessOutlivesLaunchingTurnAndReusesJob(t *testing.T) {
	p := backgroundTestProcesses(t, "exec sleep 30")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	args := []string{"camera", "one"}
	started := time.Now()
	job, err := p.start(ctx, "camera_view", backgroundTestTarget, args)
	if err != nil || job.State != "running" || job.PID <= 0 || time.Since(started) > 2*time.Second {
		t.Fatalf("launch did not return a running job promptly: %+v, %v", job, err)
	}
	if job.Device != backgroundTestTarget.Device || job.Transport != "direct" || job.StartedAt == "" {
		t.Fatalf("missing launch metadata: %+v", job)
	}
	cancel()
	time.Sleep(50 * time.Millisecond)
	if got := backgroundTestJob(t, p, job.JobID); got.State != "running" || !backgroundTestProcessAlive(got.PID) {
		t.Fatalf("turn cancellation terminated successful background launch: %+v", got)
	}
	// Caller-owned argument slices cannot silently change duplicate detection.
	args[1] = "changed"
	again, err := p.start(context.Background(), "camera_view", backgroundTestTarget, []string{"camera", "one"})
	if err != nil || again.JobID != job.JobID || again.PID != job.PID {
		t.Fatalf("identical live command was duplicated: first=%+v again=%+v err=%v", job, again, err)
	}
	stopped, err := p.stop(job.JobID)
	if err != nil || stopped.State != "stopped" || stopped.EndedAt == "" {
		t.Fatalf("stop failed: %+v, %v", stopped, err)
	}
	if again, err = p.stop(job.JobID); err != nil || again.State != "stopped" {
		t.Fatalf("repeated stop failed: %+v, %v", again, err)
	}
	if _, err := p.stop("unowned-job"); err == nil {
		t.Fatal("accepted stop for an unknown job")
	}
}

func TestBackgroundProcessStartupCancellationStopsProcess(t *testing.T) {
	p := backgroundTestProcesses(t, "exec sleep 30")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	defer timer.Stop()
	job, err := p.start(ctx, "camera_view", backgroundTestTarget, nil)
	if !errors.Is(err, context.Canceled) || job.State != "stopped" || backgroundTestProcessAlive(job.PID) {
		t.Fatalf("cancelled startup retained a process: %+v err=%v", job, err)
	}
}

func TestBackgroundProcessEarlyFailureRetainsDiagnostics(t *testing.T) {
	p := backgroundTestProcesses(t, "printf 'stdout diagnostic\\n'\nprintf 'decoder unavailable\\n' >&2\nexit 7")
	job, err := p.start(context.Background(), "camera_view", backgroundTestTarget, nil)
	if err == nil || job.State != "failed" || job.ExitCode == nil || *job.ExitCode != 7 || job.Error == "" {
		t.Fatalf("early failure was not reported: %+v err=%v", job, err)
	}
	for _, want := range []string{"stdout diagnostic", "decoder unavailable"} {
		if !strings.Contains(job.OutputTail, want) {
			t.Fatalf("lost %q in failure output: %+v", want, job)
		}
	}
	if got := backgroundTestJob(t, p, job.JobID); got.OutputTail != job.OutputTail || got.EndedAt == "" {
		t.Fatalf("failed job history lost diagnostics: %+v", got)
	}
	all, err := p.list("")
	if err != nil || len(all) != 1 || all[0].OutputTail != "" {
		t.Fatalf("listing all jobs unexpectedly included output logs: %+v err=%v", all, err)
	}
}

func TestBackgroundProcessDoesNotInheritTerminal(t *testing.T) {
	p := backgroundTestProcesses(t, "if read line; then printf 'INHERITED_STDIN\\n'; exit 8; fi\nprintf 'captured stdout\\n'\nprintf 'captured stderr\\n' >&2")
	input, inputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := io.WriteString(inputWriter, "terminal input\n"); err != nil {
		t.Fatal(err)
	}
	_ = inputWriter.Close()
	output, outputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	defer outputWriter.Close()
	originalIn, originalOut, originalErr := os.Stdin, os.Stdout, os.Stderr
	defer func() { os.Stdin, os.Stdout, os.Stderr = originalIn, originalOut, originalErr }()
	os.Stdin, os.Stdout, os.Stderr = input, outputWriter, outputWriter
	job, launchErr := p.start(context.Background(), "audio_listen", backgroundTestTarget, nil)
	os.Stdin, os.Stdout, os.Stderr = originalIn, originalOut, originalErr
	_ = outputWriter.Close()
	leaked, readErr := io.ReadAll(output)
	if launchErr != nil || job.State != "exited" || readErr != nil || len(leaked) != 0 {
		t.Fatalf("terminal inherited: job=%+v launch=%v leaked=%q read=%v", job, launchErr, leaked, readErr)
	}
	if !strings.Contains(job.OutputTail, "captured stdout") || !strings.Contains(job.OutputTail, "captured stderr") || strings.Contains(job.OutputTail, "INHERITED_STDIN") {
		t.Fatalf("media output was not privately captured: %+v", job)
	}
}

func TestBackgroundProcessClearsInheritedDeviceOverride(t *testing.T) {
	t.Setenv("WENDY_AGENT_SOCKET", "/tmp/different-device.sock")
	p := backgroundTestProcesses(t, `if [ -n "$WENDY_AGENT_SOCKET" ]; then printf 'inherited socket override'; exit 8; fi
printf 'using explicit device target'`)
	job, err := p.start(context.Background(), "camera_view", backgroundTestTarget, nil)
	if err != nil || job.State != "exited" || !strings.Contains(job.OutputTail, "using explicit device target") {
		t.Fatalf("inherited socket could override the selected device: %+v err=%v", job, err)
	}
}

func TestBackgroundProcessCleansUpChildrenOnStopAndParentExit(t *testing.T) {
	for _, stopParent := range []bool{true, false} {
		t.Run(fmt.Sprintf("stop=%v", stopParent), func(t *testing.T) {
			body := "trap 'exit 0' TERM INT\n( trap '' TERM INT; exec sleep 30 ) &\nprintf '%s\\n' \"$!\" > \"$1\"\n"
			if stopParent {
				body += "wait"
			} else {
				body += "sleep 0.5\nexit 0"
			}
			p := backgroundTestProcesses(t, body)
			pidFile := filepath.Join(p.workspace, "child.pid")
			job, err := p.start(context.Background(), "camera_view", backgroundTestTarget, []string{pidFile})
			if err != nil || job.State != "running" {
				t.Fatalf("fixture startup: %+v %v", job, err)
			}
			childBytes, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			childPID, err := strconv.Atoi(strings.TrimSpace(string(childBytes)))
			if err != nil || !backgroundTestProcessAlive(childPID) {
				t.Fatalf("fixture child not alive: pid=%d err=%v", childPID, err)
			}
			if stopParent {
				if stopped, err := p.stop(job.JobID); err != nil || stopped.State != "stopped" {
					t.Fatalf("stopping parent: %+v %v", stopped, err)
				}
			} else {
				backgroundTestWait(t, "parent exit", func() bool { return backgroundTestJob(t, p, job.JobID).State != "running" })
			}
			backgroundTestWait(t, "descendant cleanup", func() bool { return !backgroundTestProcessAlive(childPID) })
		})
	}
}

func TestBackgroundProcessLimitAndCloseAllJobs(t *testing.T) {
	p := backgroundTestProcesses(t, "exec sleep 30")
	var jobs []backgroundJobInfo
	for i := 0; i < maxBackgroundRunning; i++ {
		job, err := p.start(context.Background(), "camera_view", backgroundTestTarget, []string{strconv.Itoa(i)})
		if err != nil || job.State != "running" {
			t.Fatalf("launch %d: %+v %v", i, job, err)
		}
		jobs = append(jobs, job)
	}
	if _, err := p.start(context.Background(), "audio_listen", backgroundTestTarget, []string{"extra"}); err == nil {
		t.Fatal("running job limit was not enforced")
	}
	if same, err := p.start(context.Background(), "camera_view", backgroundTestTarget, []string{"0"}); err != nil || same.JobID != jobs[0].JobID {
		t.Fatalf("duplicate at process limit should reuse the existing job: %+v %v", same, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("repeated session close: %v", err)
	}
	for _, job := range jobs {
		if got := backgroundTestJob(t, p, job.JobID); got.State != "stopped" || backgroundTestProcessAlive(got.PID) {
			t.Fatalf("session close retained a running process: %+v", got)
		}
	}
	if _, err := p.start(context.Background(), "camera_view", backgroundTestTarget, nil); err == nil {
		t.Fatal("closed session accepted a launch")
	}
}

func TestBackgroundProcessHistoryIsBounded(t *testing.T) {
	p := backgroundTestProcesses(t, "exit 0")
	var last backgroundJobInfo
	for i := 0; i < maxBackgroundHistory+3; i++ {
		job, err := p.start(context.Background(), "camera_view", backgroundTestTarget, []string{strconv.Itoa(i)})
		if err != nil || job.State != "exited" {
			t.Fatalf("short-lived launch: %+v %v", job, err)
		}
		last = job
	}
	jobs, err := p.list("")
	if err != nil || len(jobs) != maxBackgroundHistory || jobs[0].JobID != "media-4" || jobs[len(jobs)-1].JobID != last.JobID {
		t.Fatalf("unexpected retained history: %+v %v", jobs, err)
	}
	if _, err := p.list("media-1"); err == nil {
		t.Fatal("evicted history was still addressable")
	}
}

func TestBackgroundLogRetainsBoundedRecentOutput(t *testing.T) {
	log := &backgroundLog{}
	for _, chunk := range []string{"old output", strings.Repeat("x", backgroundLogBytes+500), "\nfinal decoder error\n"} {
		if n, err := log.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("log write: %d %v", n, err)
		}
	}
	output := log.String()
	if !strings.HasPrefix(output, "[Earlier output omitted.]\n") || !strings.HasSuffix(output, "\nfinal decoder error\n") || strings.Contains(output, "old output") || len(log.data) != backgroundLogBytes || len(output) > backgroundLogBytes+40 {
		t.Fatalf("incorrect log tail: len=%d bytes=%d", len(output), len(log.data))
	}
	if _, err := log.Write([]byte(strings.Repeat("🌋", backgroundLogBytes) + "x")); err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(log.String()) || !strings.HasSuffix(log.String(), "x") || len(log.data) > backgroundLogBytes {
		t.Fatal("UTF-8 split at tail boundary was not repaired")
	}
	if _, err := log.Write([]byte(strings.Repeat("\xffx", backgroundLogBytes))); err != nil {
		t.Fatal(err)
	}
	if output := log.String(); !utf8.ValidString(output) || len(output) > backgroundLogBytes+40 {
		t.Fatalf("repairing invalid UTF-8 expanded the log beyond its byte budget: %d", len(output))
	}
}
