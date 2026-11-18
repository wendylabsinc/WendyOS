package containerd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

const (
	inferenceWorkerPrefix = "wendy-inference-"
	// Like the ROS 2 helpers, a model worker carries its own label and no app
	// version label, which keeps it out of app listings, stats, restarts and
	// app-sidecar reconciliation. Its value is the worker's image.
	labelKeyInferenceWorker = "sh.wendy/inference.worker"
	// inferenceImagePullTimeout ends a pull that hangs, so the campaign gets
	// its process worker instead of waiting forever. It is the worker's own
	// ready deadline (inference/runtime.go).
	inferenceImagePullTimeout = 15 * time.Minute
)

func inferenceWorkerLabels(worker inference.WorkerContainer) map[string]string {
	return map[string]string{labelKeyInferenceWorker: worker.Image}
}

// inferenceImageMu makes campaigns that start together share one pull.
// ponytail: one lock for every image; per-image locks once there are several.
var inferenceImageMu sync.Mutex

// RunInferenceWorker implements inference.ContainerRuntime.
func (c *Client) RunInferenceWorker(ctx context.Context, worker inference.WorkerContainer, stdin io.Reader, stdout, stderr io.Writer) (wait func() error, err error) {
	defer func() {
		if err != nil && ctx.Err() == nil {
			// The campaign's status has the reason too; the factory does not
			// try again for some minutes, so this is not logged at every start.
			c.logger.Warn("Model worker container did not start; the worker runs as a process", zap.String("image", worker.Image), zap.Error(err))
		}
	}()
	ctx = c.withNamespace(ctx)
	image, err := c.inferenceImage(ctx, worker)
	if err != nil {
		return nil, err
	}
	hostResolvConf := ""
	if systemdResolvedStubAvailable() {
		// Without it the spec falls back to the host's resolver file.
		if hostResolvConf, err = writeHostResolvConf(); err != nil {
			c.logger.Warn("host DNS: could not prepare the model worker's resolver configuration", zap.Error(err))
		}
	}
	spec, err := inferenceWorkerSpec(worker, hostResolvConf)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%s%d-%d", inferenceWorkerPrefix, time.Now().UnixNano(), execCounter.Add(1))
	// The worker is charged to the app budget, which keeps the host's reserve.
	placeROS2Helper(spec, name, c.ros2HelperUnified())
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	ctr, err := c.client.NewContainer(ctx, name,
		containerd.WithImage(image), containerd.WithNewSnapshot(name, image),
		containerd.WithContainerLabels(inferenceWorkerLabels(worker)),
		containerd.WithNewSpec(oci.WithSpecFromBytes(specJSON)),
	)
	if err != nil {
		return nil, fmt.Errorf("creating model worker container: %w", err)
	}
	// remove ends the worker and removes its container. Deleting the task the
	// agent holds the stdio of also removes that stdio's FIFOs.
	remove := func(task containerd.Task, running bool) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*killWaitTimeout)
		defer cancel()
		var err error
		if running {
			err = c.terminateTask(cleanupCtx, task, name, syscall.SIGKILL, killWaitTimeout, killWaitTimeout)
		} else if task != nil {
			if _, err = task.Delete(cleanupCtx, containerd.WithProcessKill); errdefs.IsNotFound(err) {
				err = nil
			}
		}
		if deleteErr := ctr.Delete(cleanupCtx, containerd.WithSnapshotCleanup); deleteErr != nil && !errdefs.IsNotFound(deleteErr) {
			err = errors.Join(err, deleteErr)
		}
		if err != nil {
			c.logger.Warn("Failed to remove model worker container", zap.String("container", name), zap.Error(err))
		}
	}
	task, err := ctr.NewTask(ctx, cio.NewCreator(cio.WithStreams(stdin, stdout, stderr)))
	if err != nil {
		remove(nil, false)
		return nil, fmt.Errorf("creating model worker task: %w", err)
	}
	// Not ctx: a cancelled wait reports no exit status.
	exited, err := task.Wait(context.WithoutCancel(ctx))
	if err == nil {
		err = task.Start(ctx)
	}
	if err != nil {
		remove(task, false)
		return nil, fmt.Errorf("starting model worker: %w", err)
	}
	c.logger.Info("Model worker runs in a container", zap.String("container", name), zap.String("image", worker.Image))
	return func() error {
		select {
		case status := <-exited:
			if err := status.Error(); err != nil {
				// containerd could not say how the task ended, such as across
				// its own restart, so the task may still run.
				remove(task, true)
				return err
			}
			// The end of its stderr says why it exited; deleting the task
			// first could cut it off.
			if taskIO := task.IO(); taskIO != nil {
				taskIO.Wait()
			}
			remove(task, false)
			if code := status.ExitCode(); code != 0 {
				return fmt.Errorf("exit status %d", code)
			}
			return nil
		case <-ctx.Done():
			remove(task, true)
			return errors.New("signal: killed")
		}
	}, nil
}

// inferenceImage is the worker's image, pulled and unpacked on first use, and
// refused unless it carries the labels the worker needs.
func (c *Client) inferenceImage(ctx context.Context, worker inference.WorkerContainer) (containerd.Image, error) {
	inferenceImageMu.Lock()
	defer inferenceImageMu.Unlock()
	image, err := c.client.GetImage(ctx, worker.Image)
	if err != nil {
		pullCtx, cancel := context.WithTimeout(ctx, inferenceImagePullTimeout)
		defer cancel()
		image, err = c.client.Pull(pullCtx, worker.Image, containerd.WithPullUnpack)
		if err != nil {
			return nil, fmt.Errorf("preparing model worker image %q (first use requires downloading the image): %w", worker.Image, err)
		}
	}
	config, err := image.Spec(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading model worker image %q: %w", worker.Image, err)
	}
	if err := inferenceImageMismatch(worker, config.Config.Labels); err != nil {
		return nil, err
	}
	unpacked, err := c.imageUnpacked(ctx, image)
	if err != nil {
		return nil, fmt.Errorf("checking model worker image: %w", err)
	}
	if !unpacked {
		if err := c.UnpackImage(ctx, image, nil); err != nil {
			return nil, fmt.Errorf("unpacking model worker image: %w", err)
		}
	}
	return image, nil
}

// inferenceImageMismatch says which label the image lacks: an image built for
// other worker dependencies than this agent's never starts.
func inferenceImageMismatch(worker inference.WorkerContainer, labels map[string]string) error {
	for key, want := range worker.ImageLabels {
		if got := labels[key]; got != want {
			return fmt.Errorf("model worker image %q has label %s=%q, and this agent needs %q", worker.Image, key, got, want)
		}
	}
	return nil
}

// inferenceWorkerSpec confines the worker: a read-only root, no capabilities,
// no new privileges, no devices, and of the host's files only its own mounts.
// It stays uid 0 because root owns the model cache that the process worker
// wrote and still shares; without capabilities, uid 0 reaches only files that
// root owns.
func inferenceWorkerSpec(worker inference.WorkerContainer, hostResolvConf string) (*localoci.Spec, error) {
	spec := localoci.DefaultSpec("rootfs", worker.Args)
	spec.Process.Cwd = worker.Dir
	spec.Process.Env = append(spec.Process.Env, worker.Env...)
	spec.Process.Capabilities = &localoci.LinuxCapabilities{}
	spec.Root.Readonly = true
	spec.Mounts = append(spec.Mounts, localoci.Mount{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=1777", "size=64m"}})
	for _, mount := range worker.Mounts {
		access := "rw"
		if mount.ReadOnly {
			access = "ro"
		}
		spec.Mounts = append(spec.Mounts, localoci.Mount{Destination: mount.Path, Type: "bind", Source: mount.Path, Options: []string{"bind", access, "nosuid", "nodev", "noexec"}})
	}
	// Models come from Hugging Face, so the worker has the host's network and
	// resolver, set up the way a host-network app's are.
	network := &appconfig.AppConfig{AppID: "wendy-inference", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "host"}}}
	if err := localoci.ApplyEntitlements(spec, network, localoci.ApplyOptions{HostResolvConfPath: hostResolvConf}); err != nil {
		return nil, err
	}
	return spec, nil
}

// RemoveInferenceWorkers removes every model worker container. The agent calls
// it when it starts: nothing reads the results of an earlier agent's worker.
func (c *Client) RemoveInferenceWorkers(ctx context.Context) error {
	ctx = c.withNamespace(ctx)
	workers, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyInferenceWorker))
	if err != nil {
		return fmt.Errorf("listing model worker containers: %w", err)
	}
	var errs []error
	for _, worker := range workers {
		errs = append(errs, c.deleteROS2Sidecar(ctx, worker))
	}
	return errors.Join(errs...)
}
