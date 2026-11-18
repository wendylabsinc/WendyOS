package inference

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

const (
	// WorkerRepository is the repository in the device registry that holds
	// the model worker's image, at tag "latest". The agent never pulls it:
	// `wendy data campaign deploy` builds it and pushes it there, the way
	// `wendy run` delivers an app's image. The name is reserved by nothing but
	// this constant, so anything that can push to the registry can replace
	// the image, as it can replace an app's.
	WorkerRepository = "wendy-inference-worker"
	// WorkerImageDepsLabel is the image label holding the DepsHash the image
	// was built for.
	WorkerImageDepsLabel = "sh.wendy.inference.deps"
	// The image's Python environment (Dockerfile in this directory).
	workerImagePython = "/opt/wendy-inference/venv/bin/python"
	// WorkerScratch is the one writable place a ContainerRuntime gives the
	// worker besides its mounts: private to the container, held in memory,
	// and gone with it. It is the worker's HOME and its temporary directory.
	WorkerScratch = "/tmp"
)

// WorkerImageReference is the name containerd knows the worker's image by,
// given the prefix the device registry registers images under, such as
// "localhost:5000/".
func WorkerImageReference(registryPrefix string) string {
	return registryPrefix + WorkerRepository + ":latest"
}

// workerImageFiles are the image's whole build context, in the order DepsHash
// and the Dockerfile hash them.
var workerImageFiles = []string{"Dockerfile", "pyproject.toml", "uv.lock"}

// WorkerBuildContext is what the worker's image is built from. The agent
// serves it to the command line, so that an image is always built from the
// files of the agent that will run it, whatever the command line's version.
func WorkerBuildContext() map[string][]byte {
	files := map[string][]byte{}
	for _, name := range workerImageFiles {
		files[name], _ = assets.ReadFile(name)
	}
	return files
}

// DepsHash names the image the embedded worker needs: the first 16 hex digits
// of the SHA-256 of its build context. The Dockerfile computes the same and
// refuses to build with another, and the agent runs no image labelled with
// another.
func DepsHash() string {
	hash := sha256.New()
	for _, name := range workerImageFiles {
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

// ContainerRuntime is the containerd surface the worker needs. None of it
// reaches a network: an image is on the device or it is not.
type ContainerRuntime interface {
	// InferenceImageLabels are the labels of an image on the device, and
	// whether it is there at all.
	InferenceImageLabels(ctx context.Context, image string) (labels map[string]string, found bool, err error)
	// RemoveInferenceImage removes an image from the device.
	RemoveInferenceImage(ctx context.Context, image string) error
	// RunInferenceWorker starts the worker. Cancelling ctx kills it. wait
	// must be called: it returns how the worker ended, like exec.Cmd.Wait,
	// once it has ended, stdout and stderr hold everything it wrote, and its
	// container is removed.
	RunInferenceWorker(ctx context.Context, worker WorkerContainer, stdin io.Reader, stdout, stderr io.Writer) (wait func() error, err error)
}

// ContainerFactory runs the model worker in a container: the agent's own
// worker.py, mounted read-only, on the image's Python environment. The model
// cache is the same host directory the process uses, so each reuses what the
// other downloaded. A worker that cannot run in a container runs through
// Process, exactly as before, and its status says why.
type ContainerFactory struct {
	// Image is the worker image's name in containerd (WorkerImageReference).
	// Empty means the device registry, and so the image, is unavailable.
	Image string
	// Runtime is nil on an agent without containerd.
	Runtime ContainerRuntime
	// Process is the fallback, and the owner of the inference directory.
	Process *ManagedFactory

	// process starts the fallback. Tests replace it.
	process func(ctx context.Context, config data.CampaignInference, notContained string) (Session, error)
}

const (
	containerNotStarted = "the worker did not start in its container: "
	// What an operator does about a missing or outdated image.
	deployForImage = "deploy the campaign with a current wendy CLI on a machine with Docker, which builds the image and pushes it to this device"
)

func (f *ContainerFactory) Start(ctx context.Context, config data.CampaignInference) (Session, error) {
	plan, reason := f.containerPlan(ctx, config)
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
func (f *ContainerFactory) containerPlan(ctx context.Context, config data.CampaignInference) (AcceleratorPlan, string) {
	if reason := f.unavailable(); reason != "" {
		return AcceleratorPlan{}, reason
	}
	hardware, library := f.Process.probe(config)
	plan, binding := planAccelerator(config, hardware, library)
	if binding != nil || plan.Requested == data.AcceleratorTensorRT {
		// TensorRT needs the host's libnvinfer and CUDA libraries, its GPU
		// devices, and limits that were measured on a host process. None of
		// that has run in a container, so these campaigns keep the process.
		return plan, "the worker image has no TensorRT, which this campaign asks for or can use on this device"
	}
	if reason := f.imageUnusable(ctx); reason != "" {
		return plan, reason
	}
	return plan, f.Process.heldOff(containerCrashKey(config), "the worker's container exited %d times within %d minutes, so this model runs as a process until %s; last exit: %s")
}

// unavailable says why this agent can run no worker in a container at all.
func (f *ContainerFactory) unavailable() string {
	switch {
	case f.Runtime == nil:
		return "this agent has no containerd"
	case f.Image == "":
		return "this agent's device registry, which holds the model worker image, is unavailable"
	}
	return ""
}

// imageUnusable says why the image on the device cannot run this agent's
// worker, and what to do about it, or "" when it can. It only looks: the
// agent never fetches the image.
func (f *ContainerFactory) imageUnusable(ctx context.Context) string {
	labels, found, err := f.Runtime.InferenceImageLabels(ctx, f.Image)
	switch {
	case err != nil:
		return "the model worker image on this device could not be read: " + err.Error()
	case !found:
		return "this device has no model worker image; " + deployForImage
	case labels[WorkerImageDepsLabel] != DepsHash():
		return fmt.Sprintf("the model worker image on this device was built for another agent version (its %s label is %q, this agent needs %q); %s",
			WorkerImageDepsLabel, labels[WorkerImageDepsLabel], DepsHash(), deployForImage)
	}
	return ""
}

// WorkerImageInfo is what the command line needs to put the worker's image
// on the device.
type WorkerImageInfo struct {
	// Repository is where to push it in the device registry. Empty when this
	// agent can run no worker in a container, and there is nothing to build.
	Repository string
	DepsHash   string
	// Present is whether an image for DepsHash is already on the device.
	Present bool
	// BuildContext is what to build it from, when it is not.
	BuildContext map[string][]byte
}

func (f *ContainerFactory) WorkerImage(ctx context.Context) WorkerImageInfo {
	if f.unavailable() != "" {
		return WorkerImageInfo{}
	}
	info := WorkerImageInfo{Repository: WorkerRepository, DepsHash: DepsHash(), Present: f.imageUnusable(ctx) == ""}
	if !info.Present {
		info.BuildContext = WorkerBuildContext()
	}
	return info
}

// RemoveStaleImage removes a worker image built for another agent version,
// which nothing will run again: after an agent update that changes the
// worker's dependencies it would otherwise stay on the device. An image at
// the reserved name without the label is not a worker image, and is left.
func (f *ContainerFactory) RemoveStaleImage(ctx context.Context) error {
	if f.unavailable() != "" {
		return nil
	}
	labels, found, err := f.Runtime.InferenceImageLabels(ctx, f.Image)
	if err != nil || !found {
		return err
	}
	if built, ok := labels[WorkerImageDepsLabel]; !ok || built == DepsHash() {
		return nil
	}
	return f.Runtime.RemoveInferenceImage(ctx, f.Image)
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
	// from exactly this agent's build context.
	dir, err := writeWorkerFiles(root)
	if err != nil {
		return nil, err
	}
	models := filepath.Join(root, "models")
	if err := os.MkdirAll(models, 0700); err != nil {
		return nil, err
	}
	// The root filesystem and the worker's directory are read-only.
	env := []string{"PYTHONDONTWRITEBYTECODE=1"}
	for _, entry := range workerEnvironment(root) {
		// The process worker runs as root with the inference directory's home
		// as its HOME, and reads what is there. A contained worker must not be
		// able to leave it anything, so its HOME is the container's own
		// scratch, which is gone with the container.
		if strings.HasPrefix(entry, "HOME=") {
			entry = "HOME=" + WorkerScratch
		}
		env = append(env, entry)
	}
	worker := WorkerContainer{
		Image:       f.Image,
		ImageLabels: map[string]string{WorkerImageDepsLabel: DepsHash()},
		Args:        []string{workerImagePython, "-u", filepath.Join(dir, "worker.py")},
		Env:         env,
		Dir:         dir,
		// The model cache is the one thing both workers write. The home, the
		// engine cache and the CUDA cache stay with the process.
		Mounts: []WorkerMount{{dir, true}, {models, false}},
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
