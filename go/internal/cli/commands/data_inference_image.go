package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// inferenceWorkerImageTimeout bounds the build and push, so that a build or a
// link that hangs cannot keep a campaign from being deployed.
const inferenceWorkerImageTimeout = 30 * time.Minute

// inferenceWorkerPlatforms are the platforms the worker image exists for,
// by the architecture a device reports.
var inferenceWorkerPlatforms = map[string]string{"arm64": "linux/arm64", "amd64": "linux/amd64"}

// buildInferenceWorkerImageFn builds the worker's image for a platform and
// pushes it to the device. Tests replace it.
var buildInferenceWorkerImageFn = buildInferenceWorkerImage

// ensureInferenceWorkerImage puts the image a campaign's model worker runs in
// on the device, when the device can use one and does not have it: built here
// and pushed to the device registry as `wendy run` pushes an app's image.
//
// A device is less trusted than this machine, so nothing it says is an input
// to the build. The build context is the one compiled into this CLI, the
// build argument is this CLI's own hash of it, and the push target is this
// CLI's own constant. The device's answer is only compared with those: when
// it differs, nothing is built.
//
// Nothing but the operator cancelling is an error: without the image the
// worker runs as a process of the agent, as it always did, so the campaign is
// deployed either way and notices says once why. An agent that predates the
// image answers Unimplemented and gets exactly the deploy it got before, with
// nothing printed.
func ensureInferenceWorkerImage(ctx context.Context, conn *grpcclient.AgentConnection, notices io.Writer) error {
	info, err := conn.DataService.InferenceWorkerImage(ctx, &agentpbv2.DataInferenceWorkerImageRequest{})
	if status.Code(err) == codes.Unimplemented {
		return nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		noInferenceWorkerImage(notices, fmt.Sprintf("the device did not say which image it needs (%v)", err), "deploy the campaign again with this device reachable")
		return nil
	}
	if info.GetRepository() == "" || info.GetPresent() {
		return nil
	}
	if info.GetRepository() != inference.WorkerRepository {
		noInferenceWorkerImage(notices, "the device asks for an image this CLI does not build", "bring the CLI and the device's agent to matching versions, then deploy the campaign again")
		return nil
	}
	if info.GetDepsHash() != inference.DepsHash() {
		noInferenceWorkerImage(notices, "this CLI and the device's agent ship different worker dependencies, so this CLI cannot build the image that agent runs",
			"bring the CLI and the device's agent to matching versions, then deploy the campaign again")
		return nil
	}
	version, err := agentVersionForRun(ctx, conn)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		noInferenceWorkerImage(notices, fmt.Sprintf("the device did not report its platform (%v)", err), "deploy the campaign again with this device reachable")
		return nil
	}
	platform, ok := inferenceWorkerPlatforms[version.GetCpuArchitecture()]
	if !ok || version.GetOs() != "linux" {
		noInferenceWorkerImage(notices, "there is no worker image for the platform the device reports", "deploy to a Linux arm64 or amd64 device")
		return nil
	}
	if isCloudConnection(conn) {
		noInferenceWorkerImage(notices, "this connection goes through Wendy Cloud, and the image is over 1 GB", "deploy the campaign again over a direct connection to the device")
		return nil
	}
	if ready, _ := dockerDaemonReady(ctx); !ready {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		noInferenceWorkerImage(notices, "Docker is not running on this machine", "deploy the campaign again on a machine with Docker running")
		return nil
	}
	buildCtx, cancel := context.WithTimeout(ctx, inferenceWorkerImageTimeout)
	defer cancel()
	err = buildInferenceWorkerImageFn(buildCtx, conn, platform)
	switch {
	case err == nil:
		fmt.Fprintln(notices, "The model worker image is on the device. Model workers started from now on run in a container.")
	case errors.Is(err, ErrUserCancelled) || ctx.Err() != nil:
		return err
	case buildCtx.Err() != nil:
		noInferenceWorkerImage(notices, fmt.Sprintf("building and pushing the image took longer than %s", inferenceWorkerImageTimeout), "deploy the campaign again on a faster connection to the device")
	default:
		noInferenceWorkerImage(notices, fmt.Sprintf("building or pushing the image failed (%v)", err), "deploy the campaign again once that is fixed")
	}
	return nil
}

func noInferenceWorkerImage(notices io.Writer, reason, fix string) {
	fmt.Fprintf(notices, "The model worker will run as a process of the agent, not in a container: %s. The campaign is deployed either way. To run it in a container, %s.\n", reason, fix)
}

// buildInferenceWorkerImage builds the worker image from the files compiled
// into this CLI, for one of inferenceWorkerPlatforms, and pushes it through
// the `wendy run` build and push to the repository the agent of the same
// version looks in.
func buildInferenceWorkerImage(ctx context.Context, conn *grpcclient.AgentConnection, platform string) error {
	dir, err := os.MkdirTemp("", "wendy-inference-worker-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	for name, contents := range inference.WorkerBuildContext() {
		if err := os.WriteFile(filepath.Join(dir, name), contents, 0o600); err != nil {
			return err
		}
	}
	if err := requireRegistryAuth(ctx, conn); err != nil {
		return err
	}
	const agentOS = "linux"
	title := fmt.Sprintf("Building the model worker image for %s and pushing it to the device...", platform)
	return runBuildWithProgress(ctx, title, dumpRawUnlessRegistryUnavailable, func(buildCtx context.Context, stream, logw io.Writer) error {
		return buildAndPushImageForAgent(buildCtx, conn, registryPort(agentOS), agentOS, "", dir, inference.WorkerRepository, platform, "",
			map[string]string{"DEPS_HASH": inference.DepsHash()}, "", stream, logw)
	})
}
