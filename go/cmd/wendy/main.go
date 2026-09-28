package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/cli/memguard"
	"github.com/wendylabsinc/wendy/go/internal/cli/swifttoolchain"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/env"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func main() {
	start := time.Now()
	// Before anything else, so a leak during command construction or prerun is
	// caught too. A trip kills the process outright, which means no analytics
	// event and no error message from below — the heap profile it leaves behind
	// is the report.
	memguard.Start()
	cmd := commands.NewRootCmd()

	// A Bubble Tea program (device picker, spinners, progress bars) calls
	// signal.Notify then signal.Stop for SIGINT/SIGTERM on every run
	// (bubbletea's handleSignals). Stop only detaches its own channel — Go's
	// os/signal never restores the pre-Notify default (process-terminating)
	// disposition once Notify has been called, so after the first TUI exits,
	// a bare SIGINT is silently swallowed for the rest of the process and any
	// unbounded call made afterward (e.g. an RPC to a selected device) can
	// never be interrupted. Keeping our own listener registered for the whole
	// process lifetime, and threading its cancellation through every command's
	// context, keeps Ctrl+C effective no matter how many TUIs have already run.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Reject an unknown subcommand before cobra can quietly answer it with the
	// parent group's help page and a zero exit code. See UnknownSubcommandError.
	// A panic becomes an internal_error (exit 70) with an envelope in JSON
	// mode, rather than Go's crash output and exit 2, the usage-error status.
	executed, err := executeRecovering(func() (*cobra.Command, error) {
		if err := commands.UnknownSubcommandError(os.Args[1:]); err != nil {
			return nil, err
		}
		return cmd.ExecuteContextC(ctx)
	})
	trackCommand(executed, err, time.Since(start))
	analytics.Close()

	exitCode := reportFailure(os.Stderr, err, executed, os.Args[1:])
	// Windows: when this process owns its console window (UAC-relaunched or
	// double-clicked), exiting would close the window and destroy the output
	// above — hold it open until the user has read it.
	commands.PauseBeforeExitIfSoleConsole()
	os.Exit(exitCode)
}

// trackCommand emits a single analytics event describing the invocation.
// Properties:
//
//   - command_name: canonical cobra path (e.g. "wendy device wifi connect"),
//     never flag values or positional args.
//   - command_root: top-level token (e.g. "device") for low-cardinality
//     breakdowns that survive PostHog's 25-row table cap.
//   - duration_ms: wall-clock time from process start.
//   - success: bool serialized as "true"/"false".
//   - is_dev_build: true for development builds (version.IsDev) — the local
//     "dev" default or a CI branch build with a "-dev" suffix.
//   - error_class (only when err != nil): bounded enum derived from err —
//     never the error message text, which can leak hostnames or paths.
func trackCommand(executed *cobra.Command, err error, dur time.Duration) {
	if executed == nil {
		return
	}
	path := executed.CommandPath()
	if path == "wendy __session-broker" {
		return
	}
	// Homebrew exports HOMEBREW_PREFIX/HOMEBREW_CELLAR/HOMEBREW_REPOSITORY into
	// every interactive shell once `eval "$(brew shellenv)"` is set up (the
	// standard ~/.zprofile line), so env presence alone cannot distinguish the
	// post-install hook from a user manually running `wendy completion
	// install` in a normal Homebrew terminal. The post-install hook runs with
	// a non-interactive stdin (no TTY), so require both.
	homebrewPostInstall := env.IsHomebrewInstall() && !stdinIsInteractive()
	event := eventNameFor(path, homebrewPostInstall)
	props := map[string]string{
		"command_name": path,
		"command_root": commandRoot(executed),
		"duration_ms":  strconv.FormatInt(dur.Milliseconds(), 10),
		"success":      strconv.FormatBool(err == nil),
		"is_dev_build": strconv.FormatBool(version.IsDev(version.Version)),
	}
	if err != nil {
		props["error_class"] = errorClass(err)
	}
	analytics.Track(event, props)

	// Activation milestones (one-time per install, best-effort). first_run and
	// first_real_command are only counted for genuine invocations, not the
	// Homebrew install artifact.
	if event == "command_executed" {
		analytics.TrackMilestoneOnce("first_run")
		if !isSetupCommand(path) {
			analytics.TrackMilestoneOnce("first_real_command")
		}
	}
	if m := milestoneFor(path, err == nil); m != "" {
		analytics.TrackMilestoneOnce(m)
	}
}

func commandRoot(c *cobra.Command) string {
	if c == nil {
		return ""
	}
	if !c.HasParent() {
		return c.Name()
	}
	for c.Parent() != nil && c.Parent().HasParent() {
		c = c.Parent()
	}
	return c.Name()
}

// eventNameFor returns the analytics event name for a command invocation.
// homebrewPostInstall must mean the Homebrew post-install context
// specifically (Homebrew env present AND stdin non-interactive), not merely
// that Homebrew env vars are set — see trackCommand. A Homebrew post-install
// `wendy completion install` is reported as install_completed so it is not
// counted as deliberate CLI usage.
func eventNameFor(commandPath string, homebrewPostInstall bool) string {
	if homebrewPostInstall && commandPath == "wendy completion install" {
		return "install_completed"
	}
	return "command_executed"
}

// stdinIsInteractive reports whether stdin is attached to a terminal. The
// Homebrew post-install hook runs wendy with a non-interactive stdin.
func stdinIsInteractive() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// isSetupCommand reports whether a command path is a meta/setup command that
// does not represent deliberate product use. The first "real" command is the
// first invocation whose path is not one of these.
func isSetupCommand(path string) bool {
	switch path {
	case "wendy completion install",
		"wendy completion bash", "wendy completion zsh",
		"wendy completion fish", "wendy completion powershell",
		"wendy __complete", "wendy __completeNoDesc",
		"wendy __ble-check", "wendy help":
		return true
	}
	return strings.HasPrefix(path, "wendy analytics") ||
		strings.HasPrefix(path, "wendy cache")
}

// milestoneFor maps a successful command invocation to a one-time activation
// milestone event name, or "" if the command is not a milestone.
func milestoneFor(commandPath string, success bool) string {
	if !success {
		return ""
	}
	switch commandPath {
	case "wendy discover":
		return "discover_success"
	case "wendy init":
		return "init_success"
	case "wendy run":
		return "first_deploy_success"
	case "wendy cloud login", "wendy auth login":
		return "auth_success"
	}
	return ""
}

// errorClass maps an execution error to a bounded enum suitable for analytics.
// It must never embed the error message, which can contain hostnames, paths,
// or other user input.
//
// Cancellation and deadlines take precedence over operation categories.
// Command categories identify known failures; the remaining errors use typed
// gRPC, network, filesystem, or subprocess causes. No message matching is used.
func errorClass(err error) string {
	if err == nil {
		return ""
	}
	var internal *internalError
	if errors.As(err, &internal) {
		return "internal_error"
	}
	if errors.Is(err, commands.ErrUserCancelled) || errors.Is(err, commands.ErrDefaultCleared) ||
		errors.Is(err, swifttoolchain.ErrUserCancelled) || errors.Is(err, tui.ErrCancelled) {
		return "user_cancelled"
	}
	if errors.Is(err, context.Canceled) {
		return "context_canceled"
	}
	// A usage error outranks any category it also carries (an invalid flag
	// value in wendy run is also config_invalid): the command line is wrong.
	if commands.IsUsageError(err) {
		return "cli_usage"
	}
	// A failed device dial is classified by what it says about the device
	// before the generic deadline classes: a dial that timed out means the
	// device did not answer (device_unreachable), not a slow operation.
	if class := commands.DeviceDialErrorClass(err); class != "" {
		return class
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context_deadline"
	}
	st, isGRPC := status.FromError(err)
	if isGRPC {
		switch st.Code() {
		case codes.Canceled:
			return "context_canceled"
		case codes.DeadlineExceeded:
			return "grpc_deadline"
		}
	}
	if class := commands.ErrorClass(err); class != "" {
		return class
	}
	// status.FromError returns ok=true only for real gRPC errors (those
	// produced by the grpc package or implementing GRPCStatus()). For
	// non-gRPC errors it returns ok=false with a synthesized Unknown code,
	// which we don't want to claim as a gRPC failure. An explicit
	// Unknown code from a real gRPC error, however, should still bucket
	// under grpc_unknown.
	if isGRPC && st.Code() != codes.OK {
		switch st.Code() {
		case codes.Unavailable:
			return "grpc_unavailable"
		case codes.Unimplemented:
			return "grpc_unimplemented"
		case codes.InvalidArgument:
			return "grpc_invalid_argument"
		case codes.NotFound:
			return "grpc_not_found"
		case codes.AlreadyExists:
			return "grpc_already_exists"
		case codes.PermissionDenied:
			return "grpc_permission_denied"
		case codes.ResourceExhausted:
			return "grpc_resource_exhausted"
		case codes.FailedPrecondition:
			return "grpc_failed_precondition"
		case codes.Aborted:
			return "grpc_aborted"
		case codes.OutOfRange:
			return "grpc_out_of_range"
		case codes.Internal:
			return "grpc_internal"
		case codes.DataLoss:
			return "grpc_data_loss"
		case codes.Unauthenticated:
			return "grpc_unauthenticated"
		case codes.Unknown:
			return "grpc_unknown"
		default:
			return "grpc_other"
		}
	}
	var dnsErr *net.DNSError
	var netErr net.Error
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &dnsErr):
		return "network_dns"
	case errors.As(err, &netErr) && netErr.Timeout():
		return "network_timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "network_refused"
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return "network_unreachable"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "connection_closed"
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	case errors.Is(err, os.ErrNotExist):
		return "file_not_found"
	case errors.Is(err, syscall.ENOSPC):
		return "disk_full"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof"
	case errors.As(err, &exitErr):
		return "process_failed"
	}
	return "other"
}

// renderError lets actionable errors style their heading separately from the
// recovery steps. Use errors.As because commands may add context with %w.
func renderError(err error) string {
	var diagnostic interface {
		error
		CLIMessage() string
	}
	if errors.As(err, &diagnostic) && strings.Contains(err.Error(), diagnostic.Error()) {
		// Preserve surrounding action context and any joined sibling errors.
		return strings.Replace(err.Error(), diagnostic.Error(), diagnostic.CLIMessage(), 1)
	}
	return tui.ErrorMessage(formatError(err).Error())
}

// formattedError is formatError's result: the text shown to a person, and
// the recovery steps formatError itself put at the end of that text, so JSON
// mode can report them as next_steps instead of burying them in the message.
type formattedError struct {
	text  string
	steps []string
}

func formatError(err error) error {
	f := formatErrorParts(err)
	if f.text == err.Error() {
		return err
	}
	return errors.New(f.text)
}

func formatErrorParts(err error) formattedError {
	msg := err.Error()
	if !strings.Contains(msg, "rpc error: code = ") {
		return formattedError{text: msg}
	}

	// prefix is the context callers wrapped around the gRPC error (e.g.
	// "starting agent update: "), grpcText the status error's own text, and
	// suffix whatever a caller appended after it, typically a recovery hint
	// added with fmt.Errorf("...: %w\n  hint"). Only grpcText is rewritten
	// below, so the hint survives.
	prefix, grpcText, suffix := splitGRPCMessage(err, msg)
	rewrite := func(headline string, steps ...string) formattedError {
		text := prefix + headline
		for _, step := range steps {
			text += "\n  " + step
		}
		return formattedError{text: text + suffix, steps: steps}
	}

	// A cloud tunnel the broker closed carries the broker's verdict inside the
	// handshake failure (clouddefaults.BrokerTunnelConn). That verdict is the
	// actionable part: it is neither a cert problem nor a dead device, so show
	// it before the handshake heuristics below can misread it as either.
	if verdict, ok := clouddefaults.ExplainTunnelClose(msg); ok {
		return rewrite("Wendy Cloud closed the tunnel to the device: "+verdict,
			"For full tunnel details rerun with WENDY_TLS_DEBUG=1")
	}

	isPKICoreCall := strings.Contains(prefix, "pki-core")
	isCloudCall := strings.Contains(prefix, "issuing certificate") ||
		strings.Contains(prefix, "refreshing certificate") ||
		strings.Contains(prefix, "creating enrollment token") ||
		strings.Contains(prefix, "connecting to cloud")

	// A genuine mTLS rejection carries a TLS-alert / cert-verification marker: the
	// device rejected our client cert ("remote error: tls: bad certificate"),
	// required one ("certificate required"), or our client rejected the device's
	// server cert ("tls: failed to verify certificate: x509: ..."). Only these
	// indicate a real cert/clock-skew problem worth an ssh timedatectl check.
	isCertRejection := strings.Contains(msg, "tls:") ||
		strings.Contains(msg, "bad certificate") ||
		strings.Contains(msg, "certificate required")

	// A bare "authentication handshake failed" with a transport-death cause (EOF,
	// a closed pipe, a reset) is NOT a cert rejection — the far side vanished
	// mid-handshake before any TLS alert. This is the signature of an unreachable
	// device or, over a cloud tunnel, an offline broker / a device not connected
	// to it: the tunnel byte pipe is dead, so the first RPC's handshake reads a
	// closed pipe. Blaming the device clock here sends the user to ssh a box they
	// can't reach.
	isTransportHandshakeDrop := !isCertRejection &&
		strings.Contains(msg, "authentication handshake failed") &&
		(strings.Contains(msg, "EOF") ||
			strings.Contains(msg, "read/write on closed pipe") ||
			strings.Contains(msg, "connection reset") ||
			strings.Contains(msg, "broken pipe"))

	// Name the device when the error records which address was dialled.
	device := "device"
	addr := deviceAddress(err)
	if addr != "" {
		device = "device at " + addr
	}

	switch {
	case addr != "" && strings.Contains(msg, "code = Unavailable") && isResolverMissText(grpcText):
		// The resolver's own text ("produced zero addresses") never names
		// the host it could not find.
		host := addr
		if h, _, splitErr := net.SplitHostPort(addr); splitErr == nil {
			host = h
		}
		// A device name resolved over mDNS (.local or bare) is classified as
		// unreachable: it only resolves while the device is on the network.
		if commands.DeviceDialErrorClass(err) == "device_unreachable" {
			return rewrite("Could not resolve device host "+host+".",
				"The device may be offline, or mDNS may be blocked on this network; connect by IP address to rule that out.")
		}
		return rewrite("Could not resolve device host "+host+".", "Check the name, or connect by IP address.")
	case strings.Contains(msg, "code = Unavailable") && isCertRejection && !isPKICoreCall && !isCloudCall:
		return rewrite("TLS handshake rejected by device (possible clock skew or cert mismatch).",
			"Check the device clock: ssh wendy@<host> 'timedatectl status'",
			"For full TLS details rerun with WENDY_TLS_DEBUG=1")
	case strings.Contains(msg, "code = Unavailable") && isTransportHandshakeDrop && !isPKICoreCall && !isCloudCall:
		return rewrite("Secure connection dropped during the TLS handshake.\n  The device may be offline or unreachable. If you are connecting through Wendy Cloud, the tunnel broker or the device's link to it may be down.",
			"For full TLS details rerun with WENDY_TLS_DEBUG=1")
	case strings.Contains(msg, "code = Unavailable") && strings.Contains(msg, "connection refused"):
		if isPKICoreCall {
			return rewrite("Could not connect to local pki-core. Check that the gRPC endpoint is reachable from this machine.")
		}
		if isCloudCall {
			return rewrite("Could not connect to Wendy Cloud. Please try again later.")
		}
		return rewrite("Could not connect to " + device + ". Is it powered on and connected to the network?")
	case strings.Contains(msg, "code = Unavailable"):
		// Cloud and PKI services can return actionable dependency failures.
		// Keep that explanation instead of replacing it with a generic outage.
		if isPKICoreCall || isCloudCall {
			if desc, ok := grpcDesc(grpcText); ok {
				return rewrite(desc)
			}
		}
		if isPKICoreCall {
			return rewrite("Local pki-core is unavailable.")
		}
		if isCloudCall {
			return rewrite("Wendy Cloud is unavailable. Please try again later.")
		}
		// Preserve the server's description when it provides actionable
		// detail (e.g. "WiFi management is not available (nmcli not found)").
		// Only fall back to the generic message for transport-level errors
		// that lack a useful desc.
		if idx := strings.Index(grpcText, "desc = "); idx >= 0 && strings.TrimSpace(grpcText[idx+len("desc = "):]) != "" {
			return rewrite(grpcText[idx+len("desc = "):])
		}
		return rewrite(capitalize(device) + " is unavailable.")
	case strings.Contains(msg, "code = DeadlineExceeded"):
		if device != "device" {
			return rewrite("Connection to " + device + " timed out.")
		}
		return rewrite("Connection timed out.")
	case strings.Contains(msg, "code = Unimplemented"):
		// Preserve intentional, contextual Unimplemented descriptions from the
		// agent (for example Wendy Agent for Mac feature gaps). Keep the legacy
		// update hint only for generic protocol-mismatch responses where gRPC or
		// an old agent did not recognize the service/method.
		if desc, ok := grpcDesc(grpcText); ok && !isGenericUnimplementedDesc(desc) {
			return rewrite(desc)
		}
		return rewrite("Not supported by this agent version. Try updating the agent.")
	default:
		// Strip transport noise, keep the desc message.
		if desc, ok := grpcDesc(grpcText); ok {
			return rewrite(desc)
		}
		return formattedError{text: msg}
	}
}

// splitGRPCMessage cuts msg around the text of the gRPC status error inside
// err. When the chain holds no real status error (the caller flattened it
// with %v), the gRPC text runs to the end of msg and nothing after it can be
// told apart from it, which is how formatError always treated messages.
func splitGRPCMessage(err error, msg string) (prefix, grpcText, suffix string) {
	var statusErr interface {
		error
		GRPCStatus() *status.Status
	}
	if errors.As(err, &statusErr) {
		if own := statusErr.Error(); own != "" {
			if i := strings.Index(msg, own); i >= 0 {
				return msg[:i], own, msg[i+len(own):]
			}
		}
	}
	i := strings.Index(msg, "rpc error: code = ")
	return msg[:i], msg[i:], ""
}

// isResolverMissText reports whether a gRPC error's text says a host name
// resolved to no address.
func isResolverMissText(grpcText string) bool {
	return strings.Contains(grpcText, "produced zero addresses") || strings.Contains(grpcText, "no such host")
}

// deviceAddress returns the address a failed device connection was aimed at
// when the error records one (commands wraps device dials so it does).
func deviceAddress(err error) string {
	var dial interface{ DeviceAddress() string }
	if errors.As(err, &dial) {
		return dial.DeviceAddress()
	}
	return ""
}

// capitalize upper-cases the first byte of an ASCII phrase.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func grpcDesc(msg string) (string, bool) {
	idx := strings.Index(msg, "desc = ")
	if idx < 0 {
		return "", false
	}
	desc := strings.TrimSpace(msg[idx+len("desc = "):])
	return desc, desc != ""
}

func isGenericUnimplementedDesc(desc string) bool {
	lower := strings.ToLower(strings.TrimSpace(desc))
	if lower == "" {
		return true
	}
	return strings.HasPrefix(lower, "unknown service ") ||
		strings.HasPrefix(lower, "unknown method ") ||
		(strings.HasPrefix(lower, "method ") && strings.HasSuffix(lower, " not implemented"))
}
