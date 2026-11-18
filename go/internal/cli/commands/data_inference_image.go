package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// inferenceWorkerImageTimeout bounds the build and push, so that a build or a
// link that hangs cannot keep a campaign from being deployed.
const inferenceWorkerImageTimeout = 30 * time.Minute

// What the device says about its worker image names a registry repository, a
// build argument and files to write, so each is held to a plain name.
var (
	inferenceRepositoryRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	inferenceDepsHashRE    = regexp.MustCompile(`^[0-9a-f]{8,64}$`)
	inferenceContextFileRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

var errInvalidInferenceImage = errors.New("the device named an invalid image")

const (
	inferenceContextMaxFiles = 8
	inferenceContextMaxBytes = 8 << 20
)

// buildInferenceWorkerImageFn builds the worker's image and pushes it to the
// device. Tests replace it.
var buildInferenceWorkerImageFn = buildInferenceWorkerImage

// ensureInferenceWorkerImage puts the image a campaign's model worker runs in
// on the device, when the device can use one and does not have it: built here
// from the files the device's agent serves, and pushed to the device registry
// as `wendy run` pushes an app's image. Nothing but the operator cancelling
// is an error: without the image the worker runs as a process of the agent,
// as it always did, so the campaign is deployed either way and notices says
// once why. An agent that predates the image answers Unimplemented and gets
// exactly the deploy it got before, with nothing printed.
func ensureInferenceWorkerImage(ctx context.Context, conn *grpcclient.AgentConnection, notices io.Writer) error {
	info, err := conn.DataService.InferenceWorkerImage(ctx, &agentpbv2.DataInferenceWorkerImageRequest{})
	if status.Code(err) == codes.Unimplemented {
		return nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		noInferenceWorkerImage(notices, fmt.Sprintf("the device did not say which image it needs (%v)", err), "with this device reachable")
		return nil
	}
	if info.GetRepository() == "" || info.GetPresent() {
		return nil
	}
	if isCloudConnection(conn) {
		noInferenceWorkerImage(notices, "this connection goes through Wendy Cloud, and the image is about 1.2 GB", "over a direct connection to the device")
		return nil
	}
	if ready, _ := dockerDaemonReady(ctx); !ready {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		noInferenceWorkerImage(notices, "Docker is not running on this machine", "on a machine with Docker running")
		return nil
	}
	buildCtx, cancel := context.WithTimeout(ctx, inferenceWorkerImageTimeout)
	defer cancel()
	err = buildInferenceWorkerImageFn(buildCtx, conn, info)
	switch {
	case err == nil:
		fmt.Fprintln(notices, "The model worker image is on the device. Model workers started from now on run in a container.")
	case errors.Is(err, ErrUserCancelled) || ctx.Err() != nil:
		return err
	case buildCtx.Err() != nil:
		noInferenceWorkerImage(notices, fmt.Sprintf("building and pushing the image took longer than %s", inferenceWorkerImageTimeout), "on a faster connection to the device")
	default:
		noInferenceWorkerImage(notices, fmt.Sprintf("building or pushing the image failed (%v)", err), "once that is fixed")
	}
	return nil
}

func noInferenceWorkerImage(notices io.Writer, reason, fix string) {
	fmt.Fprintf(notices, "The model worker will run as a process of the agent, not in a container: %s. The campaign is deployed either way. To build the worker's container image, deploy the campaign again %s.\n", reason, fix)
}

// buildInferenceWorkerImage writes the device's build context to a temporary
// directory and runs it through the `wendy run` build and push.
func buildInferenceWorkerImage(ctx context.Context, conn *grpcclient.AgentConnection, info *agentpbv2.DataInferenceWorkerImageResponse) error {
	if !inferenceRepositoryRE.MatchString(info.GetRepository()) || !inferenceDepsHashRE.MatchString(info.GetDepsHash()) {
		return fmt.Errorf("%w (repository %q, dependencies %q)", errInvalidInferenceImage, info.GetRepository(), info.GetDepsHash())
	}
	dir, err := os.MkdirTemp("", "wendy-inference-worker-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := writeInferenceBuildContext(dir, info.GetBuildContext()); err != nil {
		return err
	}
	version, err := agentVersionForRun(ctx, conn)
	if err != nil {
		return fmt.Errorf("querying device version: %w", err)
	}
	architecture := version.GetCpuArchitecture()
	if architecture == "" {
		architecture = "arm64"
	}
	platform := resolveAgentPlatform("", version.GetOs(), architecture)
	if err := requireRegistryAuth(ctx, conn); err != nil {
		return err
	}
	title := fmt.Sprintf("Building the model worker image for %s and pushing it to the device...", platform)
	return runBuildWithProgress(ctx, title, dumpRawUnlessRegistryUnavailable, func(buildCtx context.Context, stream, logw io.Writer) error {
		return buildAndPushImageForAgent(buildCtx, conn, registryPort(version.GetOs()), version.GetOs(), "", dir, info.GetRepository(), platform, "",
			map[string]string{"DEPS_HASH": info.GetDepsHash()}, "", stream, logw)
	})
}

// writeInferenceBuildContext writes the files the device sent, and nothing
// outside dir: a name is a plain file name or the context is refused.
func writeInferenceBuildContext(dir string, files map[string][]byte) error {
	if _, ok := files["Dockerfile"]; !ok || len(files) > inferenceContextMaxFiles {
		return fmt.Errorf("the device sent a build context of %d files without a Dockerfile, or with too many", len(files))
	}
	total := 0
	for name, contents := range files {
		if total += len(contents); !inferenceContextFileRE.MatchString(name) || total > inferenceContextMaxBytes {
			return fmt.Errorf("the device sent a build context with an invalid file name %q, or one over %d MiB", name, inferenceContextMaxBytes>>20)
		}
		if err := os.WriteFile(filepath.Join(dir, name), contents, 0o600); err != nil {
			return err
		}
	}
	return nil
}
