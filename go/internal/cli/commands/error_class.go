package commands

import (
	"errors"
	"fmt"
	"os/exec"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

var (
	errBuildFailed           = errors.New("build failed")
	errBuilderUnavailable    = errors.New("builder unavailable")
	errNoDevice              = errors.New("no device")
	errDeviceOffline         = errors.New("device offline")
	errDeviceUnreachable     = errors.New("device unreachable")
	errDeviceAmbiguous       = errors.New("device selection ambiguous")
	errProjectTargetMismatch = errors.New("project/target mismatch")
	errConfigInvalid         = errors.New("invalid configuration")
	errRegistryAuth          = errors.New("registry authentication required")
	errTransferFailed        = errors.New("transfer failed")
	errContainerStartFailed  = errors.New("container start failed")
	errReadinessTimeout      = errors.New("readiness timed out")
	errAppCrashed            = errors.New("app crashed")
	errTerminated            = errors.New("run terminated")
)

// classifiedCommandError adds a category without changing the message or
// hiding the cause from errors.Is, errors.As, and gRPC status extraction.
type classifiedCommandError struct {
	category error
	cause    error
}

func (e *classifiedCommandError) Error() string        { return e.cause.Error() }
func (e *classifiedCommandError) Unwrap() error        { return e.cause }
func (e *classifiedCommandError) Is(target error) bool { return target == e.category }

func classifyCommandError(category, cause error) error {
	if cause == nil {
		return nil
	}
	return &classifiedCommandError{category: category, cause: cause}
}

func commandErrorf(category error, format string, args ...any) error {
	return classifyCommandError(category, fmt.Errorf(format, args...))
}

// ErrorClass returns a fixed telemetry category for a known command failure,
// or "" when the caller should classify its underlying transport/OS error.
// Never return error messages, command output, paths, or other user input.
// Categories apply to all commands that share these operations, including
// run, watch, build, and device commands.
func ErrorClass(err error) string {
	if err == nil {
		return ""
	}
	var missing *errCloudDeviceNotFound
	var orgMismatch orgMismatchDeviceError
	var agentDown agentNotListeningError
	var certErr cloudCertError
	switch {
	// A SIGTERM outranks whatever the interrupted operation reported: the run
	// ended because a supervisor stopped it, not because that operation failed.
	case errors.Is(err, errTerminated):
		return "terminated"
	case errors.Is(err, errDeviceIdentityRefused):
		return "device_identity_mismatch"
	case errors.As(err, &orgMismatch):
		return "device_org_mismatch"
	case errors.Is(err, errTLSHandshakeRejected):
		return "device_tls_rejected"
	case errors.Is(err, errProvisionedAgentUnauthorized):
		return "device_auth_required"
	case errors.Is(err, errNoAuthenticatedEndpoint), errors.Is(err, errDeviceUnreachable), errors.As(err, &agentDown):
		return "device_unreachable"
	case errors.Is(err, config.ErrNotLoggedIn):
		return "auth_required"
	case errors.Is(err, config.ErrMultipleSessions):
		return "auth_session_ambiguous"
	case errors.As(err, &certErr):
		return "auth_certificate_failed"
	case errors.Is(err, errSimulatorUnavailable):
		return "simulator_unavailable"
	case errors.Is(err, errDeviceOffline):
		return "device_offline"
	case errors.Is(err, errDeviceAmbiguous):
		return "device_ambiguous"
	case errors.Is(err, errNoDevice), errors.Is(err, errNoCloudDevicesEnrolled), errors.As(err, &missing):
		return "no_device"
	case errors.Is(err, errProjectTargetMismatch):
		return "project_target_mismatch"
	case errors.Is(err, errRegistryAuth):
		return "registry_auth"
	case isRegistryUnavailable(err):
		return "registry_unavailable"
	case errors.Is(err, errBuilderUnavailable):
		return "builder_unavailable"
	// A missing executable during a build is a missing tool, not a failed
	// compilation. exec.ErrNotFound survives the build failure wrapper.
	case errors.Is(err, exec.ErrNotFound):
		return "tool_not_found"
	case errors.Is(err, errConfigInvalid):
		return "config_invalid"
	case errors.Is(err, errBuildFailed), isImageBuildFailure(err):
		return "build_failed"
	case errors.Is(err, errTransferFailed):
		return "transfer_failed"
	case errors.Is(err, errContainerStartFailed):
		return "container_start_failed"
	case errors.Is(err, errReadinessTimeout):
		return "readiness_timeout"
	case errors.Is(err, errAppCrashed):
		return "app_crashed"
	default:
		return ""
	}
}
