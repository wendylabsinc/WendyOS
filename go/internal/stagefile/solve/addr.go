// Package solve runs a compiled LLB definition against a BuildKit daemon.
//
// llbgen.Emit is pure — it opens no sockets and resolves nothing. This package
// is the other half: it finds a daemon, hands it the definition, stamps the
// final image config onto the result, and renders progress through the same
// renderer the buildx and buildctl paths use. Resolving base-image digests and
// configs stays in package lock, beside the lockfile that pins them.
package solve

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// lookupEnv and lookPath are the two seams this package reads the world
// through, in the style docker.go's imageBuilderLookPath already establishes.
// Tests replace them; nothing here calls os.Setenv, which is process-global and
// would leak between parallel tests.
var (
	lookupEnv = os.LookupEnv
	lookPath  = exec.LookPath
)

// DeviceAddress is where buildkitd listens on a WendyOS device: its own default
// socket path, which is also what the on-device image sets BUILDKIT_HOST to for
// the buildctl path.
const DeviceAddress = "unix:///run/buildkit/buildkitd.sock"

// defaultBuildxBuilder is the buildx builder instance the CLI creates when
// WENDY_BUILDX_BUILDER is unset — the same default docker.go's builder
// bootstrap uses. Both must name the same instance: this package connects to
// the daemon that bootstrap started.
const defaultBuildxBuilder = "wendy"

// Address returns the buildkitd endpoint to solve against, most explicit
// signal first:
//
//  1. BUILDKIT_HOST, the standard BuildKit variable. Anyone who sets it means
//     it, including on a device.
//  2. The on-device buildkitd socket, under exactly the condition
//     shouldUseBuildkitOnDevice already uses: running inside the device
//     (WENDY_AGENT_SOCKET set) and no docker to defer to. Docker being present
//     on-device still means docker, so the two stay in step.
//  3. buildx's own daemon. On macOS — and under Docker Desktop generally —
//     buildkitd runs inside a container rather than on the host, so there is no
//     socket to dial; the docker-container connection helper reaches it by
//     running `buildctl dial-stdio` through `docker exec`.
//
// The context is accepted for symmetry with the rest of the package's API and
// because a future probe of the builder container would need it; resolution
// itself consults nothing that can block.
func Address(_ context.Context) (string, error) {
	if v, ok := lookupEnv("BUILDKIT_HOST"); ok && strings.TrimSpace(v) != "" {
		return v, nil
	}

	// One lookup, two decisions: whether to fall back to the device socket, and
	// whether the docker-container helper can work at all.
	_, dockerErr := lookPath("docker")

	if v, _ := lookupEnv("WENDY_AGENT_SOCKET"); v != "" && dockerErr != nil {
		return DeviceAddress, nil
	}

	if dockerErr != nil {
		// Returning the address anyway would defer this to a dial failure whose
		// message names a container the user has never heard of.
		return "", fmt.Errorf("no BuildKit daemon: buildx's daemon runs inside a container reached with docker, and docker is not on PATH; set BUILDKIT_HOST to point at a daemon directly")
	}

	builder := defaultBuildxBuilder
	if v, _ := lookupEnv("WENDY_BUILDX_BUILDER"); v != "" {
		builder = v
	}
	// The docker-container driver names its container buildx_buildkit_<name>0.
	return "docker-container://buildx_buildkit_" + builder + "0", nil
}
