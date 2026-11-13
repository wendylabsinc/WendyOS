package analytics

import (
	"context"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

type attemptKey struct{}
type entrypointKey struct{}

// An attempt is one invocation, or a tour and its child operations. The
// installation's analytics_id remains the only identity across invocations.
var processAttemptID = validInheritedAttemptID()

func validInheritedAttemptID() string {
	if id, err := uuid.Parse(os.Getenv("WENDY_TELEMETRY_ATTEMPT_ID")); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

func validAttemptID(value string) string {
	if id, err := uuid.Parse(value); err == nil {
		return id.String()
	}
	return processAttemptID
}

// WithAttempt preserves the current operation's correlation identifier.
func WithAttempt(ctx context.Context) context.Context {
	if _, ok := ctx.Value(attemptKey{}).(string); ok {
		return ctx
	}
	return context.WithValue(ctx, attemptKey{}, processAttemptID)
}

// NewAttempt gives a long-lived MCP server a separate identifier for each call.
func NewAttempt(ctx context.Context) context.Context {
	return context.WithValue(ctx, attemptKey{}, uuid.NewString())
}

// WithAttemptID resumes an attempt from Wendy's own durable job state or child
// environment. Only UUIDs are accepted; resource names must never be passed.
func WithAttemptID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, attemptKey{}, validAttemptID(id))
}

func AttemptID(ctx context.Context) string {
	if ctx != nil {
		if id, ok := ctx.Value(attemptKey{}).(string); ok {
			return id
		}
	}
	return processAttemptID
}

// WithEntrypoint propagates a bounded caller type through in-process operations.
func WithEntrypoint(ctx context.Context, source string) context.Context {
	if source != "cli" && source != "mcp" && source != "tour" {
		source = "cli"
	}
	return context.WithValue(ctx, entrypointKey{}, source)
}

// TrackStage records a lifecycle fact at the instant it happens. Callers must
// use product-owned event/command names. Unrecognized properties are dropped.
func TrackStage(ctx context.Context, event, command, outcome, errorClass string, extra map[string]string) {
	trackStage(ctx, event, command, outcome, errorClass, extra, nil)
}

func trackStage(ctx context.Context, event, command, outcome, errorClass string, extra map[string]string, onDelivered func()) {
	props := make(map[string]string, len(extra)+7)
	props["entrypoint"] = "cli"
	if source := os.Getenv("WENDY_TELEMETRY_ENTRYPOINT"); source == "mcp" || source == "tour" {
		props["entrypoint"] = source
	}
	if ctx != nil {
		if source, ok := ctx.Value(entrypointKey{}).(string); ok {
			props["entrypoint"] = source
		}
	}
	for k, v := range extra {
		props[k] = v
	}
	props["command_name"], props["attempt_id"] = command, AttemptID(ctx)
	parts := strings.Fields(command)
	if len(parts) > 1 {
		props["command_root"] = parts[1]
	}
	props["outcome"], props["success"] = outcome, strconv.FormatBool(outcome == "completed")
	if errorClass != "" {
		props["error_class"] = errorClass
	}
	if outcome == "failed" || outcome == "cancelled" {
		code, detail := Diagnostic(errorClass)
		if props["error_code"] == "" {
			props["error_code"] = code
		}
		if props["error_detail"] == "" {
			props["error_detail"] = detail
		}
	}
	track(event, props, onDelivered)
	trackActivationFollowups(ctx, event, command, outcome)
}

// allowedProperty is the producer's privacy boundary, mirrored by the receiver.
// Paths, hostnames, device/app names, error text and unknown keys never pass.
func allowedProperty(key, value string) bool {
	if key == "error_detail" {
		return allowedDiagnosticDetail(value)
	}
	if len(value) > maxPropertyLen {
		return false
	}
	var values string
	switch key {
	case "error_code":
		values = "dependency_missing permission_denied config_invalid device_unreachable readiness_timeout auth_required build_failed disk_full cancelled deadline_exceeded step_failed connection_failed update_method_unsupported update_process_failed update_verification_failed update_version_unchanged update_failed"
	case "deploy_build_ms", "deploy_push_ms", "deploy_upload_ms", "deploy_device_prepare_ms", "deploy_start_ms", "deploy_bytes_sent", "deploy_chunks_sent", "deploy_chunks_total", "deploy_layers_total", "deploy_layers_reused", "deploy_build_push_ms", "deploy_chunk_attempt_ms", "deploy_image_bytes", "retry_number":
		n, err := strconv.ParseInt(value, 10, 64)
		return err == nil && n >= 0
	case "underlying_exit_code":
		_, err := strconv.ParseInt(value, 10, 32)
		return err == nil
	case "deploy_started", "is_interactive", "project_recognized", "target_selected", "device_reachable", "auth_valid", "build_prerequisites", "docs_url_opened", "help_opened":
		values = "true false unknown not_required"
	case "deploy_transport":
		values = "fastpath chunk registry buildhost multiservice native xcode swiftpm swift compose"
	case "deploy_fallback":
		values = "unimplemented transport other darwin_agent deploy_only chunking_off not_attempted"
	case "deploy_compression":
		values = "none gzip zstd"
	case "deploy_target_platform":
		values = "linux/arm64 linux/amd64 linux/arm linux/arm/v6 linux/arm/v7 linux/arm/v8 linux/riscv64 darwin/arm64 darwin/amd64 wendy-lite/arm wendy-lite/arm64 wendy-lite/riscv64 other"
	case "deploy_device_type":
		values = "raspberry-pi-4 raspberry-pi-5 jetson-orin-nano jetson-orin-nano-devkit jetson-orin-nx jetson-agx-orin jetson-agx-thor vm-arm64 generic x86_64 aarch64 darwin mac esp32 esp32-s3 esp32-p4 other"
	case "outcome":
		values = "started completed failed cancelled unknown"
	case "entrypoint":
		values = "cli mcp tour"
	case "route":
		values = "physical simulator existing_device"
	case "stage":
		values = "install os_install run tour cli_update"
	case "step":
		values = "prepare download verify_image permission dependency_check auth build connect deploy flash provision first_boot readiness select_target project"
	case "install_method":
		values = "curl brew winget npm package_manager direct unknown"
	default:
		return false
	}
	for _, allowed := range strings.Fields(values) {
		if value == allowed {
			return true
		}
	}
	return false
}

// Diagnostic returns product-owned remediation text, never an error's message.
// A generic class has no inferred root cause; the separate step identifies only
// where the operation stopped.
func Diagnostic(class string) (code, detail string) {
	switch class {
	case "update_method_unsupported", "update_process_failed", "update_verification_failed", "update_version_unchanged", "update_failed":
		return class, "Run wendy update for installation-specific guidance, then restart your Wendy MCP server."
	case "tool_not_found":
		return "dependency_missing", "A required tool is missing. Run wendy doctor --report to check prerequisites."
	case "permission_denied":
		return "permission_denied", "Check access to the selected device or installation destination, then retry."
	case "config_invalid", "cli_usage", "project_target_mismatch":
		return "config_invalid", "Check the project configuration and selected target with wendy doctor --report."
	case "no_device", "device_offline", "device_unreachable":
		return "device_unreachable", "Check that the intended device is powered on and reachable with wendy discover."
	case "grpc_unavailable", "network_refused", "network_dns", "network_unreachable":
		return "connection_failed", "Check connectivity to the service used by this step, then retry."
	case "readiness_timeout":
		return "readiness_timeout", "Inspect the application logs and its configured readiness port, then retry."
	case "auth_required", "device_auth_required", "grpc_unauthenticated":
		return "auth_required", "Run wendy auth login for the intended account, then retry."
	case "build_failed", "builder_unavailable":
		return "build_failed", "Check local build output and run wendy doctor --report to check the builder."
	case "disk_full":
		return "disk_full", "Free space on the affected disk, then retry."
	case "user_cancelled", "context_canceled":
		return "cancelled", "The operation was cancelled before completion."
	case "context_deadline", "grpc_deadline", "network_timeout":
		return "deadline_exceeded", "The operation timed out. Check connectivity and retry."
	default:
		return "step_failed", "Run wendy doctor --report, then retry the failed step."
	}
}

func allowedDiagnosticDetail(value string) bool {
	for _, class := range []string{"tool_not_found", "permission_denied", "config_invalid", "device_unreachable", "readiness_timeout", "auth_required", "build_failed", "disk_full", "user_cancelled", "context_deadline", "grpc_unavailable", "other", "update_failed"} {
		_, detail := Diagnostic(class)
		if detail == value {
			return true
		}
	}
	return false
}
