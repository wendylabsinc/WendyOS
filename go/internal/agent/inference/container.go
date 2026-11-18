package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

// WorkerImage is the image the model worker's container runs, pinned by
// digest. While it is empty, every worker runs as a process. To set it: run
// .github/workflows/inference-worker.yml with publish, make the package
// public, check that a device can pull it, and only then write
// "ghcr.io/wendylabsinc/wendy-inference-worker@sha256:<manifest digest>" here.
// An image whose dependencies are not this agent's (DepsHash) is refused at
// start, so changing pyproject.toml or uv.lock needs a new image and pin.
const WorkerImage = ""

const (
	// WorkerImageDepsLabel is the image label holding the DepsHash the image
	// was built for.
	WorkerImageDepsLabel = "sh.wendy.inference.deps"
	// The image's Python environment (Dockerfile in this directory).
	workerImagePython = "/opt/wendy-inference/venv/bin/python"
)

// DepsHash names the Python environment the embedded worker needs: the first
// 16 hex digits of the SHA-256 of pyproject.toml followed by uv.lock. The
// image's Dockerfile computes the same and refuses to build with another.
func DepsHash() string {
	hash := sha256.New()
	for _, name := range []string{"pyproject.toml", "uv.lock"} {
		b, _ := assets.ReadFile(name)
		hash.Write(b)
	}
	return hex.EncodeToString(hash.Sum(nil))[:16]
}

// WorkerContainer is the container a model worker runs in.
type WorkerContainer struct {
	Image string
	// ImageLabels are labels the image must carry, or the worker is not started.
	ImageLabels map[string]string
	Args, Env   []string
	Dir         string
	// Mounts are host directories, each at the same path in the container.
	Mounts []WorkerMount
}

type WorkerMount struct {
	Path     string
	ReadOnly bool
}

// ContainerRuntime is the containerd surface the worker needs.
type ContainerRuntime interface {
	// RunInferenceWorker starts the worker, fetching its image on first use.
	// Cancelling ctx kills it. wait must be called: it returns how the worker
	// ended, like exec.Cmd.Wait, once it has ended, stdout and stderr hold
	// everything it wrote, and its container is removed.
	RunInferenceWorker(ctx context.Context, worker WorkerContainer, stdin io.Reader, stdout, stderr io.Writer) (wait func() error, err error)
}

// ContainerFactory runs the model worker in a container: the agent's own
// worker.py, mounted read-only, on the image's Python environment. The model
// cache is the same host directory the process uses, so each reuses what the
// other downloaded. A worker that cannot run in a container runs through
// Process, exactly as before, and its status says why.
type ContainerFactory struct {
	// Image is the worker image; empty means none is published.
	Image string
	// Runtime is nil on an agent without containerd.
	Runtime ContainerRuntime
	// Process is the fallback, and the owner of the inference directory.
	Process *ManagedFactory

	// process starts the fallback. Tests replace it.
	process func(ctx context.Context, config data.CampaignInference, notContained string) (Session, error)

	// launchFailure is why the last container could not be created at all,
	// such as an image that cannot be pulled. No container is tried again
	// for tensorRTBindingsRetry, so an offline device or a missing image
	// costs one attempt per interval rather than one per worker start.
	launchMu       sync.Mutex
	launchFailure  string
	launchFailedAt time.Time
}

const containerNotStarted = "the worker did not start in its container: "

func (f *ContainerFactory) Start(ctx context.Context, config data.CampaignInference) (Session, error) {
	plan, reason := f.containerPlan(config)
	attempted := reason == ""
	if attempted {
		session, err := f.startContainer(ctx, config, plan)
		if err == nil || ctx.Err() != nil {
			return session, err
		}
		// Whatever went wrong before the worker was ready, the campaign still
		// gets the worker it had before there was a container.
		reason = containerNotStarted + err.Error()
	}
	start := f.process
	if start == nil {
		start = f.Process.start
	}
	session, err := start(ctx, config, reason)
	if err != nil && attempted {
		// Neither started, so there is no status to carry the first reason.
		return nil, fmt.Errorf("%w; before that, %s", err, reason)
	}
	return session, err
}

// containerPlan is the worker's accelerator plan when it can run in a
// container, and otherwise why it cannot.
func (f *ContainerFactory) containerPlan(config data.CampaignInference) (AcceleratorPlan, string) {
	switch {
	case f.Runtime == nil:
		return AcceleratorPlan{}, "this agent has no containerd"
	case f.Image == "":
		return AcceleratorPlan{}, "this agent pins no model worker image yet"
	}
	hardware, library := f.Process.probe(config)
	plan, binding := planAccelerator(config, hardware, library)
	if binding != nil || plan.Requested == data.AcceleratorTensorRT {
		// TensorRT needs the host's libnvinfer and CUDA libraries, its GPU
		// devices, and limits that were measured on a host process. None of
		// that has run in a container, so these campaigns keep the process.
		return plan, "the worker image has no TensorRT, which this campaign asks for or can use on this device"
	}
	if held := f.Process.heldOff(containerCrashKey(config), "the worker's container exited %d times within %d minutes, so this model runs as a process until %s; last exit: %s"); held != "" {
		return plan, held
	}
	f.launchMu.Lock()
	defer f.launchMu.Unlock()
	if f.launchFailure != "" && timeNow().Sub(f.launchFailedAt) < tensorRTBindingsRetry {
		return plan, containerNotStarted + f.launchFailure
	}
	return plan, ""
}

// containerCrashKey holds a model's container crashes apart from its TensorRT
// crashes. Both use the same limit, window and hold (accelerator.go).
func containerCrashKey(config data.CampaignInference) string {
	return "container\x00" + tensorRTCrashKey(config)
}

func (f *ContainerFactory) startContainer(ctx context.Context, config data.CampaignInference, plan AcceleratorPlan) (Session, error) {
	root, err := filepath.Abs(f.Process.Root)
	if err != nil {
		return nil, err
	}
	// The worker is the agent's own, so it cannot differ from what this agent
	// expects. Only its dependencies come from the image, which must be built
	// for exactly this lock.
	dir, err := writeWorkerFiles(root)
	if err != nil {
		return nil, err
	}
	home, models := filepath.Join(root, "home"), filepath.Join(root, "models")
	for _, path := range []string{home, models} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return nil, err
		}
	}
	worker := WorkerContainer{
		Image:       f.Image,
		ImageLabels: map[string]string{WorkerImageDepsLabel: DepsHash()},
		Args:        []string{workerImagePython, "-u", filepath.Join(dir, "worker.py")},
		// The root filesystem and the worker's directory are read-only.
		Env: append(workerEnvironment(root), "PYTHONDONTWRITEBYTECODE=1"),
		Dir: dir,
		// The engine and CUDA caches stay with the process: a worker without
		// TensorRT has no use for them.
		Mounts: []WorkerMount{{dir, true}, {home, false}, {models, false}},
	}
	key := containerCrashKey(config)
	return startWorker(ctx, f.launcher(worker), workerConfig{config, plan},
		func(reason string) { f.Process.workerCrashed(key, reason) }, isolation{data.IsolationContainer, ""})
}

// launcher runs the worker with its stdin and stdout on in-memory pipes, which
// containerd copies to and from the task's FIFOs. A pipe has no buffer, so a
// worker that is not reading holds Send back, as a process's pipe does.
func (f *ContainerFactory) launcher(worker WorkerContainer) launcher {
	return func(ctx context.Context, stderr io.Writer) (io.WriteCloser, io.Reader, func() error, error) {
		stdinReader, stdin := io.Pipe()
		stdout, stdoutWriter := io.Pipe()
		wait, err := f.Runtime.RunInferenceWorker(ctx, worker, stdinReader, stdoutWriter, stderr)
		f.launchMu.Lock()
		f.launchFailure, f.launchFailedAt = "", timeNow()
		if err != nil && ctx.Err() == nil {
			f.launchFailure = err.Error()
		}
		f.launchMu.Unlock()
		if err != nil {
			return nil, nil, nil, err
		}
		ended := make(chan error, 1)
		go func() {
			err := wait()
			// The reader sees the end of stdout, and a Send fails instead of
			// waiting for a worker that is gone.
			stdoutWriter.Close()
			stdinReader.Close()
			ended <- err
		}()
		return stdin, stdout, func() error {
			// A worker still writing to stdout, which nothing reads any more,
			// would hold back containerd's copy of it, and so its own removal.
			stdout.Close()
			return <-ended
		}, nil
	}
}
