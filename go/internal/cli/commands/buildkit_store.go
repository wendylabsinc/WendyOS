package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

const wendyBuildkitHostEnv = "WENDY_BUILDKIT_HOST"

var (
	wendyRuntimeCacheDir         = config.CacheDir
	managedBuildkitSocketPresent = func(path string) bool {
		info, err := os.Stat(path)
		return err == nil && info.Mode()&os.ModeSocket != 0
	}
)

// localBuildkitCommandContext is a seam for the local image-store build. It is
// deliberately separate from the agent's buildctl seam: this command runs on
// the developer host and will eventually target the Wendy-managed build VM.
var localBuildkitCommandContext = exec.CommandContext

// buildkitImageStoreArgs asks BuildKit's image exporter to commit a verification
// build directly to its worker's private image store and unpack it. Wendy does
// not use this store to run Mac applications; Apple container owns that role.
func buildkitImageStoreArgs(contextDir, dockerfileDir, dockerfileName, platform, imageName string, buildArgs map[string]string) []string {
	args := buildkitFrontendArgs(contextDir, dockerfileDir, dockerfileName, platform, buildArgs)
	return append(args,
		"--output",
		"type=image,name="+imageName+",store=true,unpack=true,oci-mediatypes=true",
	)
}

// managedBuildkitAddress discovers the socket exposed by Wendy's optional local
// build service. The desktop app owns lifecycle; the CLI only consumes its stable
// endpoint after the user starts it.
func managedBuildkitAddress() string {
	cacheDir, err := wendyRuntimeCacheDir()
	if err != nil {
		return ""
	}
	socket := filepath.Join(cacheDir, "runtime", "buildkitd.sock")
	if !managedBuildkitSocketPresent(socket) {
		return ""
	}
	return "unix://" + socket
}

// buildkitCommandArgs prepends an explicit or auto-discovered Wendy endpoint.
// WENDY_BUILDKIT_HOST wins; BUILDKIT_HOST remains supported natively by
// buildctl; otherwise a running Wendy build service is discovered from its
// stable cache socket. With none of those configured, buildctl retains its
// standard local-daemon default.
func buildkitCommandArgs(args []string) ([]string, error) {
	addr := strings.TrimSpace(os.Getenv(wendyBuildkitHostEnv))
	buildkitHost := strings.TrimSpace(os.Getenv("BUILDKIT_HOST"))
	if addr == "" && buildkitHost == "" {
		addr = managedBuildkitAddress()
	}
	if addr == "" {
		// buildctl consumes BUILDKIT_HOST itself and otherwise uses its normal
		// local-daemon endpoint. Wendy discovery must not make either path fail.
		return args, nil
	}
	out := make([]string, 0, len(args)+2)
	out = append(out, "--addr", addr)
	return append(out, args...), nil
}

// buildDockerProjectWithBuildkit runs a verification build in a
// containerd-backed BuildKit worker. VM bootstrap and lifecycle remain separate
// from solve semantics.
func buildDockerProjectWithBuildkit(ctx context.Context, dir, imageName, platform, dockerfile string, buildArgs map[string]string, streamOutput, logOutput io.Writer) error {
	dockerfileDir := dir
	dockerfileName := ""
	if dockerfile != "" {
		resolved, err := confinedDockerfilePath(dir, dockerfile)
		if err != nil {
			return err
		}
		dockerfileDir = filepath.Dir(resolved)
		dockerfileName = filepath.Base(resolved)
	}
	if _, err := sortedValidatedBuildArgKeys(buildArgs); err != nil {
		return err
	}

	args, err := buildkitCommandArgs(buildkitImageStoreArgs(
		dir, dockerfileDir, dockerfileName, platform, imageName, buildArgs,
	))
	if err != nil {
		return err
	}
	fmt.Fprintf(logOutput, "[buildkit] building into the worker image store: buildctl %s\n", strings.Join(redactBuildctlArgsForLog(args), " "))

	cmd := localBuildkitCommandContext(ctx, "buildctl", args...)
	cmd.Dir = dir
	// BuildKit progress is written to stderr. Use the same writer for both
	// streams so the progress parser sees one ordered stream.
	cmd.Stdout = streamOutput
	cmd.Stderr = streamOutput
	if err := cmd.Run(); err != nil {
		return &imageBuildFailedError{fmt.Errorf("buildctl build (containerd image store) failed: %w", err)}
	}
	return nil
}
