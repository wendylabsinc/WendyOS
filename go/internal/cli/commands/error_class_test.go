package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCommandErrorClass(t *testing.T) {
	privateCause := errors.New("secret.local /private/project TOKEN=secret")
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"unknown", privateCause, ""},
		{"build", classifyCommandError(errBuildFailed, privateCause), "build_failed"},
		{"OCI build", &imageBuildFailedError{privateCause}, "build_failed"},
		{"missing build tool", &imageBuildFailedError{&exec.Error{Name: "secret-tool", Err: exec.ErrNotFound}}, "tool_not_found"},
		{"builder setup", classifyCommandError(errBuilderUnavailable, privateCause), "builder_unavailable"},
		{"config", classifyCommandError(errConfigInvalid, privateCause), "config_invalid"},
		{"project mismatch", classifyCommandError(errProjectTargetMismatch, privateCause), "project_target_mismatch"},
		{"transfer", classifyCommandError(errTransferFailed, privateCause), "transfer_failed"},
		{"start", classifyCommandError(errContainerStartFailed, privateCause), "container_start_failed"},
		{"readiness", classifyCommandError(errReadinessTimeout, privateCause), "readiness_timeout"},
		{"registry auth before setup", classifyCommandError(errBuilderUnavailable, classifyCommandError(errRegistryAuth, privateCause)), "registry_auth"},
		{"registry unavailable", &registryUnavailableError{host: "secret.local", dialErr: privateCause}, "registry_unavailable"},
		{"device identity", refuseIdentity("secret.local changed identity"), "device_identity_mismatch"},
		{"device org", orgMismatchWithCause{mismatch: orgMismatchDeviceError{deviceOrg: 99}, cause: privateCause}, "device_org_mismatch"},
		{"device TLS", newTLSHandshakeRejectedError(status.Error(codes.Unavailable, "secret.local")), "device_tls_rejected"},
		{"device auth", newProvisionedAgentUnauthorizedError(privateCause), "device_auth_required"},
		{"agent down", agentNotListeningError{addr: "secret.local", cause: privateCause}, "device_unreachable"},
		{"simulator", fmt.Errorf("private VM: %w", errSimulatorUnavailable), "simulator_unavailable"},
		{"pinned unreachable", &noAuthenticatedEndpointError{msg: "secret.local"}, "device_unreachable"},
		{"auth required", config.ErrNotLoggedIn, "auth_required"},
		{"auth ambiguous", config.ErrMultipleSessions, "auth_session_ambiguous"},
		{"certificate response", cloudCertError{message: "secret.local"}, "auth_certificate_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, err := range []error{tc.err, fmt.Errorf("outer private context: %w", tc.err)} {
				if got := ErrorClass(err); got != tc.want {
					t.Errorf("ErrorClass = %q, want %q", got, tc.want)
				}
			}
		})
	}
}

func TestClassifiedCommandErrorPreservesCause(t *testing.T) {
	cause := status.Error(codes.PermissionDenied, "private/path")
	err := classifyCommandError(errTransferFailed, cause)
	if err.Error() != cause.Error() || !errors.Is(err, cause) || !errors.Is(err, errTransferFailed) {
		t.Fatalf("classification changed message or lost cause: %v", err)
	}
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("gRPC code = %s", got)
	}
	if classifyCommandError(errBuildFailed, nil) != nil {
		t.Fatal("successful operation became an error")
	}
	joined := joinServiceErrors(map[string]error{
		"api":    classifyCommandError(errBuildFailed, cause),
		"worker": errors.New("private build output"),
	})
	if got := ErrorClass(joined); got != "build_failed" || !errors.Is(joined, cause) {
		t.Fatalf("multi-service failure lost classification or cause: %s, %v", got, joined)
	}
	for _, cause := range []error{ErrUserCancelled, context.Canceled, context.DeadlineExceeded} {
		if !errors.Is(classifyCommandError(errBuildFailed, cause), cause) {
			t.Fatalf("lost cancellation/deadline: %v", cause)
		}
	}
}

func TestRunValidationErrorClasses(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts runOptions
	}{
		{"chunking", runOptions{chunking: "invalid"}},
		{"builder", runOptions{builder: "invalid"}},
		{"concurrency", runOptions{maxConcurrency: -1}},
		{"build type", runOptions{buildType: "invalid"}},
		{"missing Dockerfile", runOptions{dockerfile: "Dockerfile.missing"}},
		{"malformed config", runOptions{dockerfile: "Dockerfile"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, data := range map[string]string{"Dockerfile": "FROM scratch\n", "wendy.json": "{"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			opts := tc.opts
			opts.prefix, opts.yes = dir, true
			err := runCommand(context.Background(), opts)
			if got := ErrorClass(err); got != "config_invalid" {
				t.Fatalf("class = %q, error = %v", got, err)
			}
		})
	}
}

func TestCloudDeviceSelectionErrorClasses(t *testing.T) {
	_, missing := resolveCloudAsset(nil, "secret-device")
	_, empty := resolveCloudAsset(nil, "")
	devices := []*cloudpb.Asset{{Id: 1, Name: "secret-device"}, {Id: 2, Name: "other-device"}}
	_, ambiguous := resolveCloudAsset(devices, "")
	offline := upgradeOfflineResolveErr(missing, "secret-device", func() ([]*cloudpb.Asset, error) { return devices, nil })
	for _, tc := range []struct {
		err  error
		want string
	}{{missing, "no_device"}, {empty, "no_device"}, {ambiguous, "device_ambiguous"}, {offline, "device_offline"}} {
		if got := ErrorClass(tc.err); got != tc.want {
			t.Errorf("class = %q, want %q, error = %v", got, tc.want, tc.err)
		}
	}
}

func TestAppleContainerFailureClasses(t *testing.T) {
	setupAppleContainerEnsureSeams(t)
	t.Setenv("IMAGE_BUILDER_FAIL_STATUS", "1")
	if err := checkAppleContainerBuilder(context.Background()); ErrorClass(err) != "builder_unavailable" {
		t.Fatalf("builder setup classification: %q, %v", ErrorClass(err), err)
	}
	t.Setenv("IMAGE_BUILDER_FAIL_STATUS", "0")
	t.Setenv("IMAGE_BUILDER_FAIL_CONTAINER_BUILD", "1")
	err := buildImageWithAppleContainer(context.Background(), t.TempDir(), "app", "linux/arm64", "", nil, io.Discard, io.Discard)
	if ErrorClass(err) != "build_failed" {
		t.Fatalf("image build classification: %q, %v", ErrorClass(err), err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatal("build error lost subprocess cause")
	}
}
