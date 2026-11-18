package inference

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	// labels are those of the worker image on the device. nil is no image.
	labels  map[string]string
	deleted []string

	mu      sync.Mutex
	started []WorkerContainer
	removed int
}

func (r *shRuntime) InferenceImageLabels(context.Context, string) (map[string]string, bool, error) {
	return r.labels, r.labels != nil, nil
}

func (r *shRuntime) RemoveInferenceImage(_ context.Context, image string) error {
	r.deleted, r.labels = append(r.deleted, image), nil
	return nil
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

const testWorkerImage = "localhost:5000/wendy-inference-worker:latest"

// containerFactory is a factory on a device that has this agent's worker
// image, as after `wendy data campaign deploy` pushed it.
func containerFactory(t *testing.T, runtime *shRuntime, process *processCalls) *ContainerFactory {
	t.Helper()
	if goosWindows() {
		t.Skip("needs /bin/sh")
	}
	runtime.labels = map[string]string{WorkerImageDepsLabel: DepsHash()}
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
	// The worker answers a frame with the length of the line it read.
	runtime := &shRuntime{script: `echo '{"type":"ready","accelerator":{"active":"cpu"}}'
read frame; echo "{\"type\":\"error\",\"error\":\"${#frame}\"}"`}
	process := &processCalls{}
	factory := containerFactory(t, runtime, process)
	session, err := factory.Start(context.Background(), data.CampaignInference{Model: "owner/model"})
	if err != nil {
		t.Fatal(err)
	}
	if mode, reason := isolationOf(t, session); mode != "container" || reason != "" {
		t.Fatalf("isolation %q, reason %q", mode, reason)
	}
	// A frame reaches the worker whole over the container's stdin: a line
	// many times the size of a pipe's buffer.
	frame := Input{SourceID: "v4l2:/dev/video0", Payload: bytes.Repeat([]byte("frame"), 200_000)}
	line, _ := json.Marshal(frame)
	if err := session.Send(frame); err != nil {
		t.Fatal(err)
	}
	answers := ""
	for _, result := range drain(t, session) {
		answers += result.Type + ":" + result.Error + " "
	}
	if want := fmt.Sprintf("error:%d ", len(line)); answers != want {
		t.Fatalf("the worker answered %q, want %q", answers, want)
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
	// Only the worker, read-only, and the model cache the process uses too.
	want := []WorkerMount{{dir, true}, {filepath.Join(root, "models"), false}}
	if fmt.Sprint(worker.Mounts) != fmt.Sprint(want) || filepath.Dir(dir) != root {
		t.Fatalf("mounts %+v, want %+v", worker.Mounts, want)
	}
	// The agent's secrets and the host's PATH stay out of the container.
	env := "\n" + strings.Join(worker.Env, "\n")
	if !strings.Contains(env, "\nHF_HOME="+filepath.Join(root, "models")) || strings.Contains(env, "agent-secret") || strings.Contains(env, "\nPATH=") {
		t.Fatalf("environment %q", worker.Env)
	}
	// The process worker is root and reads its HOME, so the container gets
	// none of it: one HOME, its own scratch, and no home on the host.
	if strings.Count(env, "\nHOME=") != 1 || !strings.Contains(env, "\nHOME=/tmp\n") {
		t.Fatalf("the container's HOME: %q", worker.Env)
	}
	if _, err := os.Stat(filepath.Join(root, "home")); !os.IsNotExist(err) {
		t.Fatalf("the container path touched the process worker's home: %v", err)
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
		{name: "no device registry", configure: func(f *ContainerFactory) { f.Image = "" }, runtime: &shRuntime{script: ready}, config: cpu,
			reason: "this agent's device registry, which holds the model worker image, is unavailable"},
		// What an older command line, or one without Docker, leaves behind.
		{name: "no image on the device", configure: func(f *ContainerFactory) { f.Runtime.(*shRuntime).labels = nil }, runtime: &shRuntime{script: ready}, config: cpu,
			reason: "this device has no model worker image; deploy the campaign with a current wendy CLI on a machine with Docker, which builds the image and pushes it to this device"},
		{name: "an image for another agent", runtime: &shRuntime{script: ready}, config: cpu,
			configure: func(f *ContainerFactory) {
				f.Runtime.(*shRuntime).labels = map[string]string{WorkerImageDepsLabel: "0000000000000000"}
			},
			reason: `the model worker image on this device was built for another agent version (its sh.wendy.inference.deps label is "0000000000000000", this agent needs "` + DepsHash() +
				`"); deploy the campaign with a current wendy CLI on a machine with Docker, which builds the image and pushes it to this device`},
		{name: "the campaign can use TensorRT", configure: func(f *ContainerFactory) { f.Process.Hardware = func() Hardware { return orin } },
			runtime: &shRuntime{script: ready}, config: yoloInference(""),
			reason: "the worker image has no TensorRT, which this campaign asks for or can use on this device"},
		{name: "the campaign asks for TensorRT", runtime: &shRuntime{script: ready}, config: yoloInference("tensorrt"),
			reason: "the worker image has no TensorRT, which this campaign asks for or can use on this device"},
		{name: "the container cannot be created", runtime: &shRuntime{err: errors.New("creating model worker container: no space left on device")}, config: cpu,
			reason: "the worker did not start in its container: creating model worker container: no space left on device", attempts: 1},
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
	factory := containerFactory(t, &shRuntime{err: errors.New("unpack failed")}, process)
	_, err := factory.Start(context.Background(), data.CampaignInference{Model: "owner/model"})
	// The message is what the campaign's status shows.
	want := "downloading campaign runtime: no route to host; before that, the worker did not start in its container: unpack failed"
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

// The agent serves the build context, the Dockerfile labels the image with
// its hash, and the agent runs only an image with that label, so all three
// must name the same files the same way.
func TestWorkerImageIsBuiltForTheHashTheAgentChecks(t *testing.T) {
	hash := sha256.New()
	served := WorkerBuildContext()
	for _, name := range []string{"Dockerfile", "pyproject.toml", "uv.lock"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(served[name], b) {
			t.Fatalf("the build context's %s is not the file in this directory", name)
		}
		hash.Write(b)
	}
	if want := hex.EncodeToString(hash.Sum(nil))[:16]; DepsHash() != want || len(served) != 3 {
		t.Fatalf("DepsHash %s, want %s, over %d files", DepsHash(), want, len(served))
	}
	for _, want := range []string{
		"COPY Dockerfile pyproject.toml uv.lock ./",
		`RUN test "$(cat Dockerfile pyproject.toml uv.lock | sha256sum | cut -c1-16)" = "$DEPS_HASH"`,
		WorkerImageDepsLabel + `="$DEPS_HASH"`,
		"UV_PROJECT_ENVIRONMENT=" + filepath.ToSlash(filepath.Dir(filepath.Dir(workerImagePython))),
		"uv:" + uvVersion + "@sha256:",
	} {
		if !strings.Contains(string(served["Dockerfile"]), want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
}

// What the command line is told before it deploys a campaign.
func TestWorkerImageInfoTellsTheCommandLineWhatToBuild(t *testing.T) {
	runtime := &shRuntime{}
	factory := containerFactory(t, runtime, &processCalls{})
	present := factory.WorkerImage(context.Background())
	if present.Repository != "wendy-inference-worker" || present.DepsHash != DepsHash() || !present.Present || present.BuildContext != nil {
		t.Fatalf("with the image on the device: %+v", present)
	}
	if testWorkerImage != WorkerImageReference("localhost:5000/") {
		t.Fatalf("the image is looked up as %q", WorkerImageReference("localhost:5000/"))
	}
	for name, labels := range map[string]map[string]string{"no image": nil, "another agent's image": {WorkerImageDepsLabel: "0000000000000000"}} {
		runtime.labels = labels
		absent := factory.WorkerImage(context.Background())
		if absent.Repository != "wendy-inference-worker" || absent.DepsHash != DepsHash() || absent.Present || len(absent.BuildContext["Dockerfile"]) == 0 || len(absent.BuildContext) != 3 {
			t.Fatalf("%s: repository %q, hash %q, present %v, %d context files", name, absent.Repository, absent.DepsHash, absent.Present, len(absent.BuildContext))
		}
	}
	// An agent that can run no container asks for nothing to be built.
	factory.Runtime = nil
	if info := factory.WorkerImage(context.Background()); info.Repository != "" || info.BuildContext != nil {
		t.Fatalf("without containerd: %+v", info)
	}
}

// An agent update that changes the worker's dependencies leaves an image
// nothing will run again.
func TestWorkerImageOfAnotherAgentVersionIsRemoved(t *testing.T) {
	for _, test := range []struct {
		name    string
		labels  map[string]string
		removed bool
	}{
		{"this agent's image", map[string]string{WorkerImageDepsLabel: DepsHash()}, false},
		{"another agent's image", map[string]string{WorkerImageDepsLabel: "0000000000000000"}, true},
		// The name is reserved by convention only: an app could be pushed there.
		{"not a worker image", map[string]string{"other": "label"}, false},
		{"no image", nil, false},
	} {
		runtime := &shRuntime{}
		factory := containerFactory(t, runtime, &processCalls{})
		runtime.labels = test.labels
		if err := factory.RemoveStaleImage(context.Background()); err != nil {
			t.Fatal(err)
		}
		if removed := len(runtime.deleted) == 1 && runtime.deleted[0] == testWorkerImage; removed != test.removed || len(runtime.deleted) > 1 {
			t.Errorf("%s: removed %q", test.name, runtime.deleted)
		}
	}
}

// dockerRuntime confines the worker with docker as the agent's containerd
// spec does: read-only root, no capabilities, no new privileges, the host's
// network, and only the worker's own mounts.
type dockerRuntime struct{ name string }

func (dockerRuntime) InferenceImageLabels(ctx context.Context, image string) (map[string]string, bool, error) {
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{json .Config.Labels}}", image).Output()
	if err != nil {
		return nil, false, nil
	}
	labels := map[string]string{}
	return labels, true, json.Unmarshal(out, &labels)
}

func (dockerRuntime) RemoveInferenceImage(context.Context, string) error { return nil }

func (d dockerRuntime) RunInferenceWorker(ctx context.Context, worker WorkerContainer, stdin io.Reader, stdout, stderr io.Writer) (func() error, error) {
	args := []string{"run", "--rm", "--interactive", "--name", d.name, "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--network=host", "--tmpfs=" + WorkerScratch + ":size=64m,mode=1777,noexec,nosuid,nodev", "--workdir", worker.Dir}
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

// Opt-in integration: the image built from exactly the build context the
// agent serves to the command line, and the real worker.py in it, through the
// factory. Nothing here needs an agent, a camera or containerd.
func TestContainerWorkerImageSmoke(t *testing.T) {
	if os.Getenv("WENDY_INFERENCE_SMOKE") == "" {
		t.Skip("set WENDY_INFERENCE_SMOKE=1; needs docker, builds the worker image and downloads a model")
	}
	root := os.Getenv("WENDY_INFERENCE_SMOKE_CACHE")
	if root == "" {
		root = t.TempDir()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	buildDir := t.TempDir()
	for name, contents := range WorkerBuildContext() {
		if err := os.WriteFile(filepath.Join(buildDir, name), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	image := "wendy-inference-worker-smoke:" + DepsHash()
	if out, err := exec.CommandContext(ctx, "docker", "build", "--build-arg", "DEPS_HASH="+DepsHash(), "--tag", image, buildDir).CombinedOutput(); err != nil {
		t.Fatalf("building the worker image: %v\n%s", err, out)
	}
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
	// Nothing else of the worker's reaches the host: no home for a process
	// worker to read later.
	if entries, _ := os.ReadDir(root); len(entries) != 2 {
		t.Fatalf("the inference directory holds more than the worker and the models: %v", entries)
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
