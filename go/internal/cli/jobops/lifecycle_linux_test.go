package jobops_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/jobops"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func lifecycleServer(t *testing.T, address string, h *host) (*grpcclient.AgentConnection, string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	agentpb.RegisterWendyShellServiceServer(server, h)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	client, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	connection := grpcclient.NewFromConn(client)
	connection.IsMTLS = true
	return connection, listener.Addr().String(), server.Stop
}

func awaitLifecycle(t *testing.T, duration time.Duration, label string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("deadline waiting for " + label)
}

func lifecycleJSON(path string) map[string]any {
	raw, err := os.ReadFile(path)
	var value map[string]any
	if err == nil {
		_ = json.Unmarshal(raw, &value)
	}
	return value
}

// Keep PID start ticks with every process inspected/signaled by the fixture.
// A recycled PID must never let cleanup kill an unrelated process.
func lifecycleProcess(pid int) (state, start string, group int) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", "", 0
	}
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		return "", "", 0
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) < 20 {
		return "", "", 0
	}
	group, _ = strconv.Atoi(fields[2])
	return fields[0], fields[19], group
}

func ownLifecyclePID(t *testing.T, pid int) func(syscall.Signal) error {
	t.Helper()
	_, start, _ := lifecycleProcess(pid)
	if start == "" {
		t.Fatalf("owned process %d was not observable", pid)
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, confirmed, _ := lifecycleProcess(pid)
	if confirmed != start {
		_ = unix.Close(fd)
		t.Fatal("owned PID changed while opening its process handle")
	}
	t.Cleanup(func() {
		_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
		_ = unix.Close(fd)
	})
	return func(signal syscall.Signal) error { return unix.PidfdSendSignal(fd, signal, nil, 0) }
}

func TestDurableJobSurvivesTransportRestartWithoutRepeatingSteps(t *testing.T) {
	h := &host{}
	connection, address, stop := lifecycleServer(t, "127.0.0.1:0", h)
	working := t.TempDir()
	program := `import json, os, pathlib, time
root=pathlib.Path(os.environ['WENDY_JOB_DIR']); step=pathlib.Path(os.environ['WENDY_STEP_DIR'])
with (root/'executions.txt').open('a') as f: f.write('train\n')
(step/'identity.json').write_text(json.dumps({'pid':os.getpid()}))
(step/'checkpoint.bin').write_bytes(b'verified checkpoint\x00')
i=0
while not (root/'release').exists():
 i+=1
 with (step/'metrics.jsonl').open('a') as f: f.write(json.dumps({'iteration':i})+'\n')
 time.sleep(.03)
`
	finish := `import os,pathlib
p=pathlib.Path(os.environ['WENDY_JOB_DIR'])/'executions.txt'
with p.open('a') as f:f.write('finish\n')`
	spec := jobops.Spec{Name: "transport-lifecycle", WorkingDirectory: working, Steps: []jobops.Step{
		{Name: "train", Argv: []string{"python3", "-c", program}, MetricsFile: "metrics.jsonl", TimeoutSeconds: 20},
		{Name: "finish", Argv: []string{"python3", "-c", finish}, TimeoutSeconds: 2},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	receipt, err := jobops.Submit(ctx, connection, spec)
	if err != nil {
		t.Fatal(err)
	}
	root := receipt["job_path"].(string)
	runner := int(receipt["runner_pid"].(float64))
	ownLifecyclePID(t, runner)
	var initial map[string]any
	awaitLifecycle(t, 4*time.Second, "live job progress", func() bool {
		initial, err = jobops.Inspect(ctx, connection, root, jobops.InspectOptions{})
		if err != nil {
			return false
		}
		progress, ok := initial["progress"].(map[string]any)["train"].(map[string]any)
		return ok && progress["iteration"].(float64) > 0 && initial["child_pid"] != nil
	})
	if initial["reported_state"] != "running" || initial["process"].(map[string]any)["status"] != "live" || initial["health"] != "unknown" {
		t.Fatalf("process liveness was lost or invented application readiness: %+v", initial)
	}
	workload := int(initial["child_pid"].(float64))
	ownLifecyclePID(t, workload)
	if lifecycleJSON(filepath.Join(root, "train/identity.json"))["pid"] != initial["child_pid"] {
		t.Fatal("child_pid no longer identifies the actual argv process")
	}
	originalReceipt, err := os.ReadFile(filepath.Join(root, "receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	originalIteration := initial["progress"].(map[string]any)["train"].(map[string]any)["iteration"].(float64)
	started := h.inspectCalls.Load()
	interrupted := make(chan error, 1)
	go func() {
		_, inspectErr := jobops.Inspect(ctx, connection, root, jobops.InspectOptions{WaitSeconds: 30, UntilTerminal: true})
		interrupted <- inspectErr
	}()
	awaitLifecycle(t, time.Second, "active observation RPC", func() bool { return h.inspectCalls.Load() > started })
	stop()
	select {
	case err := <-interrupted:
		if err == nil {
			t.Fatal("interrupted observation claimed a successful response")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("interrupted observation did not return")
	}
	connection, _, _ = lifecycleServer(t, address, h)
	again, err := jobops.Submit(ctx, connection, spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"job_path", "runner_pid", "spec_sha256", "runner_sha256"} {
		if again[key] != receipt[key] {
			t.Fatalf("reconnect changed %s: %v / %v", key, receipt, again)
		}
	}
	if again["reused"] != true {
		t.Fatal("active job retry was not reused")
	}
	awaitLifecycle(t, 3*time.Second, "same worker advancing after reconnect", func() bool {
		observation, observeErr := jobops.Inspect(ctx, connection, root, jobops.InspectOptions{})
		if observeErr != nil {
			return false
		}
		progress := observation["progress"].(map[string]any)["train"].(map[string]any)
		return observation["process"].(map[string]any)["pid"] == receipt["runner_pid"] &&
			observation["child_pid"] == initial["child_pid"] && progress["iteration"].(float64) > originalIteration
	})

	// Retrieval after reconnection is still confined to the named job, and a
	// bad entry may not publish even an earlier valid file in the same batch.
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside-private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "train/unsafe")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "train/fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{"train/unsafe", "train/fifo", "../escape"} {
		out := t.TempDir()
		if _, err := jobops.Pull(ctx, connection, root, out, []string{"train/checkpoint.bin", unsafe}); err == nil {
			t.Fatal("unsafe artifact accepted after transport recovery:", unsafe)
		}
		if _, err := os.Stat(filepath.Join(out, "train/checkpoint.bin")); !os.IsNotExist(err) {
			t.Fatal("failed artifact batch published a partial checkpoint")
		}
	}
	out := t.TempDir()
	if _, err := jobops.Pull(ctx, connection, root, out, []string{"receipt.json", "train/checkpoint.bin"}); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(out, "train/checkpoint.bin")); err != nil || !bytes.Equal(got, []byte("verified checkpoint\x00")) {
		t.Fatalf("recovered checkpoint changed: %q %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	terminal, err := jobops.WaitTerminal(ctx, connection, root, 5)
	if err != nil || terminal["reported_state"] != "completed" {
		t.Fatalf("job did not complete after reconnect: %v %v", terminal, err)
	}
	if len(terminal["step_outcomes"].([]any)) != 2 {
		t.Fatal("lost step outcomes", terminal)
	}
	after, err := jobops.Submit(ctx, connection, spec)
	if err != nil || after["reused"] != true || after["runner_pid"] != receipt["runner_pid"] {
		t.Fatalf("terminal retry changed job: %v %v", after, err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "executions.txt")); string(data) != "train\nfinish\n" {
		t.Fatalf("steps repeated or disappeared: %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "receipt.json")); !bytes.Equal(data, originalReceipt) {
		t.Fatal("reconnect/retry rewrote the durable receipt")
	}
}

func TestDurableJobSupervisorCrashBoundsOwnedProcessesAndPreservesEvidence(t *testing.T) {
	connection := connection(t, &host{})
	unrelated := exec.Command("python3", "-c", "import time;time.sleep(30)")
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() })

	spec := jobops.Spec{Name: "supervisor-crash", WorkingDirectory: t.TempDir(), Steps: []jobops.Step{
		{Name: "train", Argv: []string{"python3", "-c", lifecycleStubbornWorkload}, TimeoutSeconds: 20, MetricsFile: "metrics.jsonl"},
		{Name: "never", Argv: []string{"python3", "-c", "raise RuntimeError('must not run')"}, TimeoutSeconds: 1},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	receipt, err := jobops.Submit(ctx, connection, spec)
	if err != nil {
		t.Fatal(err)
	}
	root := receipt["job_path"].(string)
	runner := int(receipt["runner_pid"].(float64))
	signalRunner := ownLifecyclePID(t, runner)
	var ids map[string]any
	awaitLifecycle(t, 3*time.Second, "owned workload and descendant", func() bool {
		ids = lifecycleJSON(filepath.Join(root, "train/pids.json"))
		return ids["workload"] != nil && ids["descendant"] != nil
	})
	pids := []int{int(ids["workload"].(float64)), int(ids["descendant"].(float64))}
	starts := make(map[int]string)
	for _, pid := range pids {
		ownLifecyclePID(t, pid)
		_, starts[pid], _ = lifecycleProcess(pid)
	}
	if err := signalRunner(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	awaitLifecycle(t, 7*time.Second, "owned process group cleanup after supervisor SIGKILL", func() bool {
		for _, pid := range pids {
			state, start, _ := lifecycleProcess(pid)
			if start == starts[pid] && state != "" && state != "Z" {
				return false
			}
		}
		return true
	})
	if err := unrelated.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("supervisor cleanup affected an unrelated process", err)
	}
	statusBefore, err := os.ReadFile(filepath.Join(root, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	observed, err := jobops.Inspect(ctx, connection, root, jobops.InspectOptions{})
	if err != nil || observed["reported_state"] != "running" || observed["health"] != "not_running" {
		t.Fatalf("supervisor crash was reported as completion/readiness: %v %v", observed, err)
	}
	again, err := jobops.Submit(ctx, connection, spec)
	if err != nil || again["reused"] != true || again["runner_pid"] != receipt["runner_pid"] {
		t.Fatalf("crashed job was silently restarted: %v %v", again, err)
	}
	if after, _ := os.ReadFile(filepath.Join(root, "status.json")); !bytes.Equal(after, statusBefore) {
		t.Fatal("inspection/retry rewrote the crashed job's recorded state")
	}
	if _, err := os.Stat(filepath.Join(root, "never.log")); !os.IsNotExist(err) {
		t.Fatal("dependent step ran after supervisor crash")
	}
	out := t.TempDir()
	if _, err := jobops.Pull(ctx, connection, root, out, []string{"status.json", "receipt.json", "train/checkpoint.bin"}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(out, "train/checkpoint.bin")); string(data) != "partial but retained" {
		t.Fatalf("crash destroyed previously written evidence: %q", data)
	}
}

const lifecycleStubbornWorkload = `import json,os,pathlib,signal,subprocess,time
step=pathlib.Path(os.environ['WENDY_STEP_DIR'])
signal.signal(signal.SIGTERM,signal.SIG_IGN)
child=subprocess.Popen(['python3','-c','import signal,time;signal.signal(signal.SIGTERM,signal.SIG_IGN);time.sleep(30)'])
(step/'pids.json').write_text(json.dumps({'workload':os.getpid(),'descendant':child.pid}))
(step/'checkpoint.bin').write_bytes(b'partial but retained')
while True:
 with (step/'metrics.jsonl').open('a') as f:f.write(json.dumps({'iteration':time.monotonic()})+'\n')
 time.sleep(.03)
`

func TestDurableJobGuardRetainsStopAndTimeoutSemantics(t *testing.T) {
	for _, stopping := range []bool{true, false} {
		t.Run(fmt.Sprintf("supervisor-TERM-%t", stopping), func(t *testing.T) {
			connection := connection(t, &host{})
			timeout := 1
			if stopping {
				timeout = 20
			}
			spec := jobops.Spec{Name: "bounded-group", WorkingDirectory: t.TempDir(), Steps: []jobops.Step{
				{Name: "train", Argv: []string{"python3", "-c", lifecycleStubbornWorkload}, TimeoutSeconds: timeout},
				{Name: "never", Argv: []string{"python3", "-c", "print('must not run')"}, TimeoutSeconds: 1},
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			receipt, err := jobops.Submit(ctx, connection, spec)
			if err != nil {
				t.Fatal(err)
			}
			root := receipt["job_path"].(string)
			runner := int(receipt["runner_pid"].(float64))
			signalRunner := ownLifecyclePID(t, runner)
			var ids map[string]any
			awaitLifecycle(t, 3*time.Second, "stubborn workload", func() bool {
				ids = lifecycleJSON(filepath.Join(root, "train/pids.json"))
				return ids["workload"] != nil && ids["descendant"] != nil
			})
			pids := []int{int(ids["workload"].(float64)), int(ids["descendant"].(float64))}
			starts := make(map[int]string)
			for _, pid := range pids {
				ownLifecyclePID(t, pid)
				_, starts[pid], _ = lifecycleProcess(pid)
			}
			if stopping {
				if err := signalRunner(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			var state map[string]any
			awaitLifecycle(t, 8*time.Second, "bounded terminal state", func() bool {
				state = lifecycleJSON(filepath.Join(root, "status.json"))
				return state["state"] == "stopped" || state["state"] == "failed"
			})
			want := "failed"
			if stopping {
				want = "stopped"
			}
			if state["state"] != want || state["child_pid"] != nil {
				t.Fatalf("wrong stop/timeout state: %+v", state)
			}
			stages := state["stages"].([]any)
			if len(stages) != 1 || stages[0].(map[string]any)["timed_out"] != !stopping {
				t.Fatalf("stop confused with deadline or dependent step ran: %+v", state)
			}
			for _, pid := range pids {
				status, start, _ := lifecycleProcess(pid)
				if start == starts[pid] && status != "" && status != "Z" {
					t.Fatalf("owned process %d survived %s", pid, want)
				}
			}
		})
	}
}

//go:embed worker.py
var lifecycleWorker string

func TestDurableJobGuardStartupIsBoundedBeforeIdentity(t *testing.T) {
	for _, stopping := range []bool{true, false} {
		t.Run(fmt.Sprintf("supervisor-TERM-%t", stopping), func(t *testing.T) {
			root := t.TempDir()
			timeout := 1
			if stopping {
				timeout = 20
			}
			spec := jobops.Spec{Name: "stalled-guard", WorkingDirectory: root, Steps: []jobops.Step{
				{Name: "never", Argv: []string{"python3", "-c", "raise RuntimeError('must not launch')"}, TimeoutSeconds: timeout},
			}}
			encoded, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "spec.json"), encoded, 0600); err != nil {
				t.Fatal(err)
			}
			// Fault injection stops the real guard before it can publish the argv
			// PID. Production supervision must still enforce timeout and TERM.
			prefix := `import os, pathlib, signal, sys
if '--step-guard' in sys.argv:
 pathlib.Path(__file__).with_name('guard.pid').write_text(str(os.getpid()))
 os.kill(os.getpid(), signal.SIGSTOP)
`
			runnerPath := filepath.Join(root, "runner.py")
			if err := os.WriteFile(runnerPath, []byte(prefix+lifecycleWorker), 0600); err != nil {
				t.Fatal(err)
			}
			runner := exec.Command("python3", runnerPath)
			var output bytes.Buffer
			runner.Stdout, runner.Stderr = &output, &output
			if err := runner.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runner.Process.Kill(); _ = runner.Wait() })
			guard := 0
			awaitLifecycle(t, 3*time.Second, "guard stopped before identity", func() bool {
				data, _ := os.ReadFile(filepath.Join(root, "guard.pid"))
				guard, _ = strconv.Atoi(string(data))
				state, _, _ := lifecycleProcess(guard)
				return guard > 0 && state == "T"
			})
			ownLifecyclePID(t, guard)
			if stopping {
				if err := runner.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			var status map[string]any
			awaitLifecycle(t, 9*time.Second, "terminal state despite missing guard identity", func() bool {
				status = lifecycleJSON(filepath.Join(root, "status.json"))
				return status["state"] == "stopped" || status["state"] == "failed"
			})
			want := "failed"
			if stopping {
				want = "stopped"
			}
			if status["state"] != want || status["child_pid"] != nil {
				t.Fatalf("wrong startup termination: %+v", status)
			}
			stages := status["stages"].([]any)
			if len(stages) != 1 || stages[0].(map[string]any)["timed_out"] != !stopping {
				t.Fatalf("startup deadline/TERM was lost: %+v", status)
			}
			state, _, _ := lifecycleProcess(guard)
			if state != "" && state != "Z" {
				t.Fatal("stopped guard survived its deadline")
			}
		})
	}
}

func TestDurableJobGuardIgnoresInaccessibleUnrelatedProcesses(t *testing.T) {
	// Execute the production membership function against a hidepid-like view.
	// An inaccessible foreign PID must not turn a successful owned step into
	// failure, and a visible member of our own group must still be detected.
	program := `import ast, os, sys
from pathlib import Path
from unittest.mock import Mock, patch
source = sys.stdin.read()
module = ast.parse(source)
function = next(n for n in module.body if isinstance(n, ast.FunctionDef) and n.name == 'group_has_workload')
exec(compile(ast.Module(body=[function], type_ignores=[]), '<worker>', 'exec'))
foreign = Mock(); foreign.name = str(os.getpid()+100000)
foreign.__truediv__ = Mock(return_value=Mock(read_text=Mock(side_effect=PermissionError('hidepid'))))
owned = Mock(); owned.name = str(os.getpid()+100001)
stat = Mock(); stat.read_text.return_value = f'{owned.name} (workload) S 1 {os.getpgrp()} 0'
owned.__truediv__ = Mock(return_value=stat)
proc = Mock(); proc.is_dir.return_value = True
with patch('__main__.Path', return_value=proc):
 proc.iterdir.return_value = [foreign]
 assert group_has_workload() is False
 proc.iterdir.return_value = [foreign, owned]
 assert group_has_workload() is True
`
	command := exec.Command("python3", "-c", program)
	command.Stdin = strings.NewReader(lifecycleWorker)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("owned-group visibility regression: %v\n%s", err, output)
	}
}
