package inference

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

// commandWorker runs command as the worker's container the way containerd
// delivers a task's stdio: stdin is copied in without being waited for, and
// wait returns only once stdout and stderr have been written out in full.
func commandWorker(command *exec.Cmd, stdin io.Reader, stdout, stderr io.Writer) (func() error, error) {
	configureProcess(command) // ctx kills its process group
	command.WaitDelay = 0
	command.Stdout, command.Stderr = stdout, stderr
	in, err := command.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	go func() { _, _ = io.Copy(in, stdin); in.Close() }()
	return command.Wait, nil
}

// shRuntime is a containerd whose containers are /bin/sh scripts. Scripts
// read the config line first, as shWorker's do.
type shRuntime struct {
	script string
	err    error

	mu      sync.Mutex
	started []WorkerContainer
	removed int
}

func (r *shRuntime) RunInferenceWorker(ctx context.Context, worker WorkerContainer, stdin io.Reader, stdout, stderr io.Writer) (func() error, error) {
	r.mu.Lock()
	r.started = append(r.started, worker)
	r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	wait, err := commandWorker(exec.CommandContext(ctx, "/bin/sh", "-c", "read config; "+r.script), stdin, stdout, stderr)
	if err != nil {
		return nil, err
	}
	return func() error {
		err := wait()
		r.mu.Lock()
		r.removed++
		r.mu.Unlock()
		return err
	}, nil
}

func (r *shRuntime) counts() (started, removed int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.started), r.removed
}

// processCalls records what the factory hands to its process fallback.
type processCalls struct {
	err     error
	reasons []string
}

func (p *processCalls) start(ctx context.Context, _ data.CampaignInference, reason string) (Session, error) {
	p.reasons = append(p.reasons, reason)
	if p.err != nil {
		return nil, p.err
	}
	return startWorker(ctx, processLauncher([]string{"/bin/sh", "-c", `read config; echo '{"type":"ready","accelerator":{"active":"cpu"}}'; sleep 30`}, os.TempDir(), nil),
		workerConfig{}, nil, isolation{data.IsolationProcess, reason})
}

const testWorkerImage = "ghcr.io/wendylabsinc/wendy-inference-worker@sha256:0000000000000000000000000000000000000000000000000000000000000000"

func containerFactory(t *testing.T, runtime ContainerRuntime, process *processCalls) *ContainerFactory {
	t.Helper()
	if goosWindows() {
		t.Skip("needs /bin/sh")
	}
	return &ContainerFactory{Image: testWorkerImage, Runtime: runtime, Process: &ManagedFactory{Root: t.TempDir()}, process: process.start}
}

func goosWindows() bool { return runtime.GOOS == "windows" }

// isolationOf is the isolation the session's first report carries.
func isolationOf(t *testing.T, session Session) (string, string) {
	t.Helper()
	select {
	case result := <-session.Results():
		if result.Type != "accelerator" || result.Accelerator == nil {
			t.Fatalf("first result %+v", result)
		}
		return result.Accelerator.Isolation, result.Accelerator.IsolationReason
	case <-time.After(20 * time.Second):
		t.Fatal("the worker reported nothing")
		return "", ""
	}
}

func TestContainerRunsTheAgentsOwnWorkerOnTheSharedModelCache(t *testing.T) {
	t.Setenv("WENDY_TOKEN", "agent-secret")
	runtime := &shRuntime{script: `echo '{"type":"ready","accelerator":{"active":"cpu"}}'
read frame; echo "$frame" >&2; echo '{"type":"error","error":"done"}'`}
	process := &processCalls{}
	factory := containerFactory(t, runtime, process)
	session, err := factory.Start(context.Background(), data.CampaignInference{Model: "owner/model"})
	if err != nil {
		t.Fatal(err)
	}
	if mode, reason := isolationOf(t, session); mode != "container" || reason != "" {
		t.Fatalf("isolation %q, reason %q", mode, reason)
	}
	// A frame reaches the worker over the container's stdin: a line of
	// base64 well over a pipe's buffer.
	if err := session.Send(Input{SourceID: "v4l2:/dev/video0", Payload: bytes.Repeat([]byte("frame"), 1<<20)}); err != nil {
		t.Fatal(err)
	}
	if results := drain(t, session); len(results) != 1 || results[0].Type != "error" {
		t.Fatalf("results %+v", results)
	}
	_ = session.Close()
	if started, removed := runtime.counts(); started != 1 || removed != 1 || len(process.reasons) != 0 {
		t.Fatalf("containers started %d, removed %d; process starts %q", started, removed, process.reasons)
	}

	root, _ := filepath.Abs(factory.Process.Root)
	worker := runtime.started[0]
	dir := worker.Dir
	if worker.Image != testWorkerImage || worker.ImageLabels[WorkerImageDepsLabel] != DepsHash() {
		t.Fatalf("image %q, labels %v", worker.Image, worker.ImageLabels)
	}
	if want := []string{workerImagePython, "-u", filepath.Join(dir, "worker.py")}; strings.Join(worker.Args, " ") != strings.Join(want, " ") {
		t.Fatalf("args %q", worker.Args)
	}
	// The worker in the container is this agent's, byte for byte.
	embedded, _ := assets.ReadFile("worker.py")
	if written, err := os.ReadFile(filepath.Join(dir, "worker.py")); err != nil || !bytes.Equal(written, embedded) {
		t.Fatalf("mounted worker differs from the embedded one: %v", err)
	}
	// Only the worker, read-only, and the directories the process uses too.
	want := []WorkerMount{{dir, true}, {filepath.Join(root, "home"), false}, {filepath.Join(root, "models"), false}}
	if fmt.Sprint(worker.Mounts) != fmt.Sprint(want) || filepath.Dir(dir) != root {
		t.Fatalf("mounts %+v, want %+v", worker.Mounts, want)
	}
	// The agent's secrets and the host's PATH stay out of the container.
	env := "\n" + strings.Join(worker.Env, "\n")
	if !strings.Contains(env, "\nHF_HOME="+filepath.Join(root, "models")) || strings.Contains(env, "agent-secret") || strings.Contains(env, "\nPATH=") {
		t.Fatalf("environment %q", worker.Env)
	}
}

func TestContainerFallsBackToTheProcessAndSaysWhy(t *testing.T) {
	withTensorRTHost(t)
	cpu := data.CampaignInference{Model: "owner/model"}
	ready := `echo '{"type":"ready","accelerator":{"active":"cpu"}}'; sleep 30`
	for _, test := range []struct {
		name      string
		configure func(*ContainerFactory)
		runtime   *shRuntime
		config    data.CampaignInference
		reason    string
		attempts  int
	}{
		{name: "no containerd", configure: func(f *ContainerFactory) { f.Runtime = nil }, runtime: &shRuntime{script: ready}, config: cpu,
			reason: "this agent has no containerd"},
		{name: "no image published", configure: func(f *ContainerFactory) { f.Image = "" }, runtime: &shRuntime{script: ready}, config: cpu,
			reason: "this agent pins no model worker image yet"},
		{name: "the campaign can use TensorRT", configure: func(f *ContainerFactory) { f.Process.Hardware = func() Hardware { return orin } },
			runtime: &shRuntime{script: ready}, config: yoloInference(""),
			reason: "the worker image has no TensorRT, which this campaign asks for or can use on this device"},
		{name: "the campaign asks for TensorRT", runtime: &shRuntime{script: ready}, config: yoloInference("tensorrt"),
			reason: "the worker image has no TensorRT, which this campaign asks for or can use on this device"},
		{name: "the image cannot be pulled", runtime: &shRuntime{err: errors.New("preparing model worker image: not found")}, config: cpu,
			reason: "the worker did not start in its container: preparing model worker image: not found", attempts: 1},
		{name: "the worker dies in its container", runtime: &shRuntime{script: `echo 'ImportError: libfoo.so' >&2; exit 1`}, config: cpu,
			reason: "the worker did not start in its container: loading campaign model: exit status 1: ImportError: libfoo.so\n", attempts: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			process := &processCalls{}
			factory := containerFactory(t, test.runtime, process)
			if test.configure != nil {
				test.configure(factory)
			}
			session, err := factory.Start(context.Background(), test.config)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if mode, reason := isolationOf(t, session); mode != "process" || reason != test.reason {
				t.Fatalf("isolation %q, reason %q", mode, reason)
			}
			if started, removed := test.runtime.counts(); started != test.attempts || removed != started-btoi(test.runtime.err != nil) {
				t.Fatalf("containers started %d, removed %d", started, removed)
			}
		})
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// With neither worker running there is no status to say why the container
// was not used, so the error says both.
func TestContainerAndProcessBothFailingSaysBoth(t *testing.T) {
	process := &processCalls{err: errors.New("downloading campaign runtime: no route to host")}
	factory := containerFactory(t, &shRuntime{err: errors.New("pull failed")}, process)
	_, err := factory.Start(context.Background(), data.CampaignInference{Model: "owner/model"})
	// The message is what the campaign's status shows.
	want := "downloading campaign runtime: no route to host; before that, the worker did not start in its container: pull failed"
	if message := fmt.Sprint(err); !errors.Is(err, process.err) || message != want {
		t.Fatalf("start error %q, want %q", message, want)
	}
	// Without a container attempt, the process's error is unchanged.
	factory.Image = ""
	_, err = factory.Start(context.Background(), data.CampaignInference{Model: "owner/model"})
	if message := fmt.Sprint(err); !errors.Is(err, process.err) || message != fmt.Sprint(process.err) {
		t.Fatalf("start error %q", message)
	}
}

// An image that cannot be pulled is not pulled again at every worker start.
func TestContainerThatCannotBeCreatedIsNotTriedAgainForAWhile(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	previous := timeNow
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = previous })
	runtime := &shRuntime{script: `echo '{"type":"ready","accelerator":{"active":"cpu"}}'; sleep 30`, err: errors.New("pull failed")}
	factory := containerFactory(t, runtime, &processCalls{})
	start := func() (string, string) {
		t.Helper()
		session, err := factory.Start(context.Background(), data.CampaignInference{Model: "owner/model"})
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		return isolationOf(t, session)
	}
	for range 3 {
		if mode, reason := start(); mode != "process" || reason != "the worker did not start in its container: pull failed" {
			t.Fatalf("isolation %q, reason %q", mode, reason)
		}
		now = now.Add(time.Minute)
	}
	if started, _ := runtime.counts(); started != 1 {
		t.Fatalf("the container was tried %d times within the interval", started)
	}
	// The image is published; the next attempt, after the interval, finds it.
	runtime.err = nil
	now = now.Add(tensorRTBindingsRetry)
	if mode, _ := start(); mode != "container" {
		t.Fatalf("after the interval the worker runs as a %s", mode)
	}
}

func TestContainerWorkerExitCarriesItsStderrAndIsRemoved(t *testing.T) {
	// The line that is not protocol JSON makes the agent kill the worker
	// while it still writes to stdout, which nothing reads any more.
	runtime := &shRuntime{script: `echo '{"type":"ready"}'
echo 'RuntimeError: out of memory' >&2
echo 'native noise'
while :; do echo 'more noise'; done`}
	factory := containerFactory(t, runtime, &processCalls{})
	session, err := factory.Start(context.Background(), data.CampaignInference{Model: "owner/model"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	drain(t, session)
	message := fmt.Sprint(session.Err())
	for _, want := range []string{`model process exited: it wrote a line to stdout that is not protocol JSON: "native noise"`, "; stderr: RuntimeError: out of memory"} {
		if !strings.Contains(message, want) {
			t.Fatalf("exit error %q lacks %q", message, want)
		}
	}
	if started, removed := runtime.counts(); started != 1 || removed != 1 {
		t.Fatalf("containers started %d, removed %d", started, removed)
	}
	if err := session.Send(Input{SourceID: "camera"}); err == nil {
		t.Fatal("a frame was sent to a worker that is gone")
	}
}

func TestContainerThatKeepsExitingYieldsToTheProcess(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	previous := timeNow
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = previous })
	runtime := &shRuntime{script: `echo '{"type":"ready","accelerator":{"active":"cpu"}}'; echo 'Bus error' >&2; exit 135`}
	process := &processCalls{}
	factory := containerFactory(t, runtime, process)
	config := data.CampaignInference{Model: "owner/model"}
	for range tensorRTCrashLimit {
		session, err := factory.Start(context.Background(), config)
		if err != nil {
			t.Fatal(err)
		}
		if mode, _ := isolationOf(t, session); mode != "container" {
			t.Fatalf("isolation %q before the limit", mode)
		}
		drain(t, session)
		session.Close()
		now = now.Add(time.Minute)
	}
	session, err := factory.Start(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	want := "the worker's container exited 3 times within 10 minutes, so this model runs as a process until 2026-11-18T13:02:00Z; last exit: exit status 135; stderr: Bus error"
	if mode, reason := isolationOf(t, session); mode != "process" || reason != want {
		t.Fatalf("isolation %q, reason %q", mode, reason)
	}
	// Another model keeps its container, and after the hold so does this one.
	other, err := factory.Start(context.Background(), data.CampaignInference{Model: "owner/other"})
	if err != nil {
		t.Fatal(err)
	}
	if mode, _ := isolationOf(t, other); mode != "container" {
		t.Fatalf("another model runs as a %s", mode)
	}
	other.Close()
	now = now.Add(tensorRTCrashHold)
	again, err := factory.Start(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if mode, _ := isolationOf(t, again); mode != "container" {
		t.Fatalf("after the hold the model runs as a %s", mode)
	}
	again.Close()
}

// The image is labelled by its Dockerfile and checked by the agent, so both
// must name the lock the same way.
func TestWorkerImageIsBuiltForTheHashTheAgentChecks(t *testing.T) {
	hash := sha256.New()
	for _, name := range []string{"pyproject.toml", "uv.lock"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		hash.Write(b)
	}
	if want := hex.EncodeToString(hash.Sum(nil))[:16]; DepsHash() != want {
		t.Fatalf("DepsHash %s, want %s", DepsHash(), want)
	}
	dockerfile, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`RUN test "$(cat pyproject.toml uv.lock | sha256sum | cut -c1-16)" = "$DEPS_HASH"`,
		WorkerImageDepsLabel + `="$DEPS_HASH"`,
		"UV_PROJECT_ENVIRONMENT=" + filepath.ToSlash(filepath.Dir(filepath.Dir(workerImagePython))),
		"uv:" + uvVersion + "@sha256:",
	} {
		if !strings.Contains(string(dockerfile), want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
}

// An agent must never ship pointing at a tag, which can move or not exist
// yet: no image at all, or one pinned by digest.
func TestWorkerImageIsUnsetOrPinnedByDigest(t *testing.T) {
	if WorkerImage != "" && !regexp.MustCompile(`^ghcr\.io/wendylabsinc/wendy-inference-worker@sha256:[0-9a-f]{64}$`).MatchString(WorkerImage) {
		t.Fatalf("WorkerImage %q is not pinned by digest", WorkerImage)
	}
}

// dockerRuntime confines the worker with docker as the agent's containerd
// spec does: read-only root, no capabilities, no new privileges, the host's
// network, and only the worker's own mounts.
type dockerRuntime struct{ name string }

func (d dockerRuntime) RunInferenceWorker(ctx context.Context, worker WorkerContainer, stdin io.Reader, stdout, stderr io.Writer) (func() error, error) {
	for key, want := range worker.ImageLabels {
		got, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", `{{index .Config.Labels "`+key+`"}}`, worker.Image).Output()
		if err != nil || strings.TrimSpace(string(got)) != want {
			return nil, fmt.Errorf("image %s has label %s=%q, and this agent needs %q: %v", worker.Image, key, strings.TrimSpace(string(got)), want, err)
		}
	}
	args := []string{"run", "--rm", "--interactive", "--name", d.name, "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--network=host", "--tmpfs=/tmp:size=64m,mode=1777", "--workdir", worker.Dir}
	if uid := os.Getuid(); uid >= 0 {
		// Without capabilities the worker reaches only its own user's files.
		// On a device that is root, the agent's user; here it is the test's.
		args = append(args, "--user", fmt.Sprintf("%d:%d", uid, os.Getgid()))
	}
	for _, mount := range worker.Mounts {
		access := "rw"
		if mount.ReadOnly {
			access = "ro"
		}
		args = append(args, "--volume", mount.Path+":"+mount.Path+":"+access)
	}
	for _, env := range worker.Env {
		args = append(args, "--env", env)
	}
	wait, err := commandWorker(exec.CommandContext(ctx, "docker", append(append(args, worker.Image), worker.Args...)...), stdin, stdout, stderr)
	if err != nil {
		return nil, err
	}
	return func() error {
		err := wait()
		// Killing the docker client does not stop its container.
		_ = exec.Command("docker", "rm", "--force", d.name).Run()
		return err
	}, nil
}

// Opt-in integration: the real worker.py on a real worker image, through the
// factory. The image workflow runs it before publishing; nothing here needs
// an agent, a camera or containerd.
func TestContainerWorkerImageSmoke(t *testing.T) {
	image := os.Getenv("WENDY_INFERENCE_SMOKE_IMAGE")
	if image == "" {
		t.Skip("set WENDY_INFERENCE_SMOKE_IMAGE to a locally built worker image; needs docker and downloads a model")
	}
	root := os.Getenv("WENDY_INFERENCE_SMOKE_CACHE")
	if root == "" {
		root = t.TempDir()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	factory := &ContainerFactory{Image: image, Runtime: dockerRuntime{name: fmt.Sprintf("wendy-inference-smoke-%d", os.Getpid())}, Process: &ManagedFactory{Root: root},
		process: func(_ context.Context, _ data.CampaignInference, reason string) (Session, error) {
			return nil, errors.New("no process fallback in this test")
		}}
	session, err := factory.Start(ctx, data.CampaignInference{Backend: "yolo_onnx", Model: "webnn/yolo11n", ModelFile: "onnx/yolo11n.onnx",
		Revision: "9c5acfdd74aaff2d0f47c51b878506361039a51f", Labels: []string{"person"}, Threshold: .5, Rate: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result := <-session.Results()
	if result.Accelerator == nil || result.Accelerator.Isolation != "container" || result.Accelerator.Active != "cpu" {
		t.Fatalf("first result %+v", result)
	}
	t.Logf("worker ready in %s on %s", image, result.Accelerator.Runtime)
	// The model is on the host, where a process worker would look for it.
	model := filepath.Join(root, "models", "hub", "models--webnn--yolo11n", "snapshots", "9c5acfdd74aaff2d0f47c51b878506361039a51f", "onnx", "yolo11n.onnx")
	if _, err := os.Stat(model); err != nil {
		t.Fatalf("the model is not in the shared cache: %v", err)
	}
	// A frame of several megabytes goes in over the container's stdin, and
	// the worker's answer to it comes back over its stdout.
	if err := session.Send(Input{SourceID: "camera", Generation: 7, Encoding: "mjpeg", Payload: bytes.Repeat([]byte("frame"), 1<<20)}); err != nil {
		t.Fatal(err)
	}
	for result = range session.Results() {
		if result.Type == "source_error" {
			break
		}
	}
	if answer := fmt.Sprintf("%s/%d: %s", result.SourceID, result.Generation, result.Error); answer != "camera/7: unsupported camera encoding" {
		t.Fatalf("the worker answered the frame with %q, exit %v", answer, session.Err())
	}
	_ = session.Close()
	if session.Err() != nil {
		t.Fatalf("a stopped worker has exit error %v", session.Err())
	}
}
