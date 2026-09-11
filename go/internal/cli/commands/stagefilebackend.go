package commands

// Scope note: this file adds the decision of *which* backend a Stagefile
// should compile through. It deliberately stops there — nothing here calls
// stagefile.CompileToLLB, solve.Run, or ensurePlaintextBuilder. Actually
// executing an LLB build still needs: (1) bootstrapping the "wendy" buildx
// builder before calling solve.Address, since solve.Address resolves an
// endpoint but never creates one; and (2) routing a push through the correct
// builder identity — solve.Address always names the plaintext "wendy"
// builder, while docker.go's own push paths use "wendy-oci" for OCI export
// and "wendy-mtls" (which carries the registry client certs) for an mTLS
// registry push. Wiring stagefileBackendLLB's result into the actual
// build/push call sites without getting that identity wrong is left to a
// follow-up change; landing it here was out of this task's scope.

import (
	"fmt"
	"os"
	"strings"
)

// stagefileBackendDockerfileValue and stagefileBackendLLBValue are the two
// values --stagefile-backend and WENDY_STAGEFILE_BACKEND accept.
const (
	stagefileBackendDockerfileValue = "dockerfile"
	stagefileBackendLLBValue        = "llb"
)

// stagefileBackendEnvVar is consulted when --stagefile-backend is not given
// explicitly on the command line.
const stagefileBackendEnvVar = "WENDY_STAGEFILE_BACKEND"

// stagefileBackendLLB decides whether a detected Stagefile should compile
// through the LLB backend (stagefile.CompileToLLB, solved directly against a
// buildkitd) rather than the default Dockerfile backend
// (stagefile.CompileFile, whose output feeds the existing docker/buildkit
// build path unchanged).
//
// Selection order, most explicit signal first:
//
//  1. flagValue — --stagefile-backend as given on the command line. Empty
//     means the flag was not passed.
//  2. WENDY_STAGEFILE_BACKEND.
//  3. Neither set: the Dockerfile backend. The LLB backend is opt-in only —
//     an absent flag and an absent env var must never select it, because
//     every existing build (and every existing test) depends on the
//     Dockerfile backend staying the silent default.
//
// builder is the --builder value the build is targeting, normalized the same
// way normalizeImageBuilder normalizes it (imageBuilderDocker,
// imageBuilderAppleContainer, or imageBuilderBuildkit). Apple Container has no
// BuildKit daemon underneath it, so an explicit request for the LLB backend
// combined with apple-container is a hard error rather than a silent fallback
// to the Dockerfile backend: a fallback here would let the user believe they
// exercised the new backend when the build actually took the old path.
//
// An unrecognised value for either the flag or the environment variable is a
// hard error for the same reason: silently treating a typo as "dockerfile"
// would hide that the requested backend was never used.
func stagefileBackendLLB(flagValue, builder string) (bool, error) {
	source := "--stagefile-backend"
	value := strings.TrimSpace(flagValue)
	if value == "" {
		source = stagefileBackendEnvVar
		value = strings.TrimSpace(os.Getenv(stagefileBackendEnvVar))
	}
	if value == "" {
		return false, nil
	}

	var useLLB bool
	switch strings.ToLower(value) {
	case stagefileBackendDockerfileValue:
		useLLB = false
	case stagefileBackendLLBValue:
		useLLB = true
	default:
		return false, fmt.Errorf(
			"invalid value %q for %s: must be %q or %q",
			value, source, stagefileBackendDockerfileValue, stagefileBackendLLBValue,
		)
	}

	if useLLB && builder == imageBuilderAppleContainer {
		return false, fmt.Errorf(
			"--stagefile-backend=%s is incompatible with --builder=%s: Apple Container has no BuildKit daemon to solve an LLB definition against; pick a different builder or drop --stagefile-backend=%s",
			stagefileBackendLLBValue, imageBuilderAppleContainer, stagefileBackendLLBValue,
		)
	}
	return useLLB, nil
}
