package commands

import (
	"context"
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

type stagefileBackendContextKey struct{}

// withStagefileBackend carries the command-line value through the existing
// build pipeline without adding a backend argument to every deploy helper.
// Environment selection remains lazy so tests can exercise it independently.
func withStagefileBackend(ctx context.Context, value string) context.Context {
	return context.WithValue(ctx, stagefileBackendContextKey{}, value)
}

func stagefileBackendFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(stagefileBackendContextKey{}).(string)
	return value
}

// resolvedStagefileBackend returns the raw selected backend string — the
// command-line value carried in ctx, else WENDY_STAGEFILE_BACKEND, else "" —
// mirroring the flag-then-env order stagefileBackendLLB resolves. Build
// fingerprints hash this so switching backends with no source change still
// forces a rebuild instead of reusing the other backend's layout.
func resolvedStagefileBackend(ctx context.Context) string {
	value := strings.TrimSpace(stagefileBackendFromContext(ctx))
	if value == "" {
		value = strings.TrimSpace(os.Getenv(stagefileBackendEnvVar))
	}
	return value
}

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
