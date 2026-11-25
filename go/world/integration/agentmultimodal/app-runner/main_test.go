package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestChildHelper(t *testing.T) {
	if os.Getenv("WORLD_APP_RUNNER_CHILD") != "1" {
		return
	}
	switch os.Getenv("WORLD_APP_RUNNER_MODE") {
	case "ok":
		_, _ = os.Stdout.WriteString("{\"proof\":\"exact bytes\"}\n")
		os.Exit(0)
	case "fail":
		_, _ = os.Stdout.WriteString("{\"previous_receipt\":true}\n")
		_, _ = os.Stderr.WriteString("bounded diagnostic\n")
		os.Exit(7)
	case "stdout":
		_, _ = os.Stdout.WriteString(strings.Repeat("x", 65536))
		time.Sleep(10 * time.Second)
	case "stderr":
		_, _ = os.Stderr.WriteString(strings.Repeat("e", 65536))
		time.Sleep(10 * time.Second)
	case "sleep":
		_, _ = os.Stdout.WriteString(strconv.Itoa(os.Getpid()))
		time.Sleep(10 * time.Second)
	case "spawn":
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		child := exec.Command(os.Args[0], "-test.run=^TestChildHelper$")
		child.Env = append(os.Environ(), "WORLD_APP_RUNNER_MODE=sleep")
		child.Stdout = os.Stdout
		if child.Start() != nil {
			os.Exit(12)
		}
		<-signals
		_ = child.Wait()
		os.Exit(0)
	}
	os.Exit(9)
}

func TestTimeoutCleansDescendantProcessGroup(t *testing.T) {
	c := config(t, "spawn")
	c.duration = 200 * time.Millisecond
	r, err := run(t.Context(), c)
	if err == nil || r.Succeeded || r.Termination != "timeout" {
		t.Fatalf("descendant timeout: %+v %v", r, err)
	}
	b, _ := os.ReadFile(filepath.Join(c.directory, "stdout.json"))
	pid, e := strconv.Atoi(string(b))
	if e != nil {
		t.Fatal("descendant did not start")
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatal("descendant survived process-group cleanup")
	}
}

func config(t *testing.T, mode string) settings {
	t.Helper()
	t.Setenv("WORLD_APP_RUNNER_CHILD", "1")
	t.Setenv("WORLD_APP_RUNNER_MODE", mode)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return settings{directory: t.TempDir(), duration: 2 * time.Second, stdoutLimit: 4096, stderrLimit: 4096, command: []string{exe, "-test.run=^TestChildHelper$"}}
}
func persisted(t *testing.T, c settings) receipt {
	t.Helper()
	var r receipt
	b, err := os.ReadFile(filepath.Join(c.directory, "exit.json"))
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(b, &r) != nil {
		t.Fatal("invalid receipt")
	}
	return r
}

func TestExactOutputAndNonzeroRetainReceipt(t *testing.T) {
	for _, mode := range []string{"ok", "fail"} {
		t.Run(mode, func(t *testing.T) {
			c := config(t, mode)
			r, err := run(t.Context(), c)
			if r != persisted(t, c) {
				t.Fatal("receipt changed on disk")
			}
			b, _ := os.ReadFile(filepath.Join(c.directory, "stdout.json"))
			if mode == "ok" {
				if err != nil || !r.Succeeded || r.ExitCode != 0 || string(b) != "{\"proof\":\"exact bytes\"}\n" {
					t.Fatalf("exact output: %+v %v", r, err)
				}
			} else {
				if err == nil || r.Succeeded || r.ExitCode != 7 || string(b) != "{\"previous_receipt\":true}\n" {
					t.Fatalf("nonzero output: %+v %v", r, err)
				}
			}
		})
	}
}

func TestOutputQuotaIsFailureWithRetainedPrefix(t *testing.T) {
	for _, mode := range []string{"stdout", "stderr"} {
		t.Run(mode, func(t *testing.T) {
			c := config(t, mode)
			r, err := run(t.Context(), c)
			if err == nil || r.Succeeded || !r.OutputTruncated || r.Termination != "output_limit" || r != persisted(t, c) {
				t.Fatalf("quota result: %+v %v", r, err)
			}
			name := "stdout.json"
			if mode == "stderr" {
				name = "stderr.log"
			}
			b, _ := os.ReadFile(filepath.Join(c.directory, name))
			if len(b) != 4096 {
				t.Fatal("quota did not retain exact bounded prefix")
			}
		})
	}
}

func TestTimeoutAndSignalCleanChild(t *testing.T) {
	for _, mode := range []string{"timeout", "signal"} {
		t.Run(mode, func(t *testing.T) {
			c := config(t, "sleep")
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(context.Canceled)
			if mode == "timeout" {
				c.duration = 100 * time.Millisecond
			} else {
				go func() { time.Sleep(100 * time.Millisecond); cancel(&requestedSignal{value: syscall.SIGINT}) }()
			}
			r, err := run(ctx, c)
			if err == nil || r.Succeeded || r.Termination != mode || r.Signal == 0 || r != persisted(t, c) {
				t.Fatalf("termination: %+v %v", r, err)
			}
			if mode == "signal" && (r.ForwardedSignal != int(syscall.SIGINT) || r.Signal != int(syscall.SIGINT)) {
				t.Fatal("original signal was not forwarded")
			}
			b, _ := os.ReadFile(filepath.Join(c.directory, "stdout.json"))
			pid, e := strconv.Atoi(string(b))
			if e != nil {
				t.Fatal("child did not start")
			}
			if syscall.Kill(pid, 0) == nil {
				t.Fatal("child survived bounded runner")
			}
		})
	}
}

func TestExistingOutputAndSymlinkAreRefused(t *testing.T) {
	c := config(t, "ok")
	if _, err := run(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(c.directory, "exit.json"))
	if _, err := run(t.Context(), c); err == nil {
		t.Fatal("old receipts overwritten")
	}
	after, _ := os.ReadFile(filepath.Join(c.directory, "exit.json"))
	if string(before) != string(after) {
		t.Fatal("receipt changed")
	}
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	c.directory = link
	if _, err := run(t.Context(), c); err == nil {
		t.Fatal("symlink output accepted")
	}
}

func TestGateWaitIsBoundedAndDoesNotStartChildEarly(t *testing.T) {
	t.Run("release", func(t *testing.T) {
		c := config(t, "ok")
		c.startFile = filepath.Join(t.TempDir(), "start")
		go func() { time.Sleep(100 * time.Millisecond); _ = os.WriteFile(c.startFile, nil, 0600) }()
		r, err := run(t.Context(), c)
		if err != nil || !r.ChildStarted || !r.Succeeded {
			t.Fatalf("released gate: %+v %v", r, err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		c := config(t, "ok")
		c.startFile = filepath.Join(t.TempDir(), "start")
		c.duration = 60 * time.Millisecond
		r, err := run(t.Context(), c)
		if err == nil || r.ChildStarted || r.Succeeded || r.Termination != "gate_timeout" || r != persisted(t, c) {
			t.Fatalf("gate deadline: %+v %v", r, err)
		}
		b, _ := os.ReadFile(filepath.Join(c.directory, "stdout.json"))
		if len(b) != 0 {
			t.Fatal("child ran before gate release")
		}
	})
	t.Run("total-budget", func(t *testing.T) {
		c := config(t, "sleep")
		c.startFile = filepath.Join(t.TempDir(), "start")
		c.duration = 200 * time.Millisecond
		go func() { time.Sleep(130 * time.Millisecond); _ = os.WriteFile(c.startFile, nil, 0600) }()
		start := time.Now()
		r, err := run(t.Context(), c)
		if err == nil || !r.ChildStarted || r.Termination != "timeout" || time.Since(start) > 400*time.Millisecond {
			t.Fatalf("gate reset total budget: %+v %v", r, err)
		}
	})
	t.Run("symlink", func(t *testing.T) {
		c := config(t, "ok")
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		_ = os.WriteFile(target, nil, 0600)
		c.startFile = filepath.Join(dir, "start")
		_ = os.Symlink(target, c.startFile)
		r, err := run(t.Context(), c)
		if err == nil || r.ChildStarted || r.Termination != "gate_refused" {
			t.Fatalf("symlink gate: %+v %v", r, err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		c := config(t, "ok")
		c.startFile = filepath.Join(t.TempDir(), "start")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		r, err := run(ctx, c)
		if err == nil || r.ChildStarted || r.Termination != "signal" {
			t.Fatalf("canceled gate: %+v %v", r, err)
		}
	})
}
