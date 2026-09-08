package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/wendylabsinc/wendy/go/internal/cli/ble"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	clitimesync "github.com/wendylabsinc/wendy/go/internal/cli/timesync"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"golang.org/x/term"
)

const defaultAgentPort = 50051
const agentMTLSPortOffset = 1

const agentPlaintextProbeTimeout = 3 * time.Second

// mtlsProbeTimeout bounds a single mTLS connect+probe. The dial target is
// already an IP (resolveAddrOnce), so this only needs to cover TCP + TLS
// handshake; keeping it bounded stops an unreachable/plaintext-only mTLS port
// from stalling the connect before the plaintext fallback.
//
// Provisioned devices authenticate with post-quantum ML-DSA certificates,
// whose handshake is noticeably slower on constrained hardware (Jetson,
// Raspberry Pi) than a classical ECDSA/RSA handshake — ~2.2s observed on a
// quiet device, but under load (CPU contention from an app, concurrent
// connects, etc.) it has been observed exceeding a 3s budget entirely. When
// that happens the probe context is cancelled mid-handshake, the direct
// device-connect path (connectAgentAtAddressWithProvisionedHint) reports it
// as a timeout, and connectToAgent surfaces a misleading "Unauthorized" error
// even though the device accepted the client certificate — it just didn't
// finish in time. 7s gives comfortable headroom above the slowest handshakes
// observed on hardware while still failing a truly-unreachable device in a
// bounded time (see retryOnHandshakeTimeout for the belt-and-braces retry on
// top of this budget).
const mtlsProbeTimeout = 7 * time.Second

// lanAddressProbeBudget bounds a single candidate address in the discover/
// picker version probe (resolveLANAgentVersion). It wraps connectWithAutoTLS,
// whose successful path for a provisioned device costs a TCP+TLS handshake plus
// one mtlsProbeTimeout GetAgentVersion probe (~2.2s observed). A budget shorter
// than mtlsProbeTimeout cancelled the mTLS probe before it could answer, leaving
// provisioned rows — especially USB devices, probed via .local first — stuck on
// the failure glyph even though `wendy device info` (which connects with the
// un-capped root context) succeeded (PR #1297).
//
// connectWithAutoTLSDiagnostics also tries every stored org cert in turn, so a
// user logged into N orgs pays up to N sequential mTLS probes before the cert
// matching the device's org connects. When that cert is not the first one tried
// (e.g. the device is in the user's second org), a fixed single-probe budget
// expires before it is reached — so the picker showed a failure glyph for
// devices in a non-first org while `wendy device info` (uncapped) still worked.
// Scale the budget by the cert count so the outer and inner budgets can't
// silently invert regardless of how many orgs the user is logged into.
func lanAddressProbeBudget(numCerts int) time.Duration {
	if numCerts < 1 {
		numCerts = 1
	}
	return time.Duration(numCerts)*mtlsProbeTimeout + 2*time.Second
}

const provisionedAgentMetadataDiscoveryTimeout = 500 * time.Millisecond

const provisionedAgentUnauthorizedMessage = "Unauthorized. Run 'wendy auth login' with an account that can access this provisioned wendy-agent."

var errProvisionedAgentUnauthorized = errors.New(provisionedAgentUnauthorizedMessage)

// errTLSHandshakeRejected is the sentinel for "the device rejected our client
// certificate during the TLS handshake" — the signature of clock skew (the
// device clock lags the cert's validity window) or a genuine cert mismatch.
// connectWithAutoTLSDiagnostics returns a tlsHandshakeRejectedError, so callers
// can detect this case with errors.Is rather than string matching.
var errTLSHandshakeRejected = errors.New("TLS handshake rejected by device")

type tlsHandshakeRejectedError struct {
	cause error
}

func newTLSHandshakeRejectedError(cause error) error {
	return tlsHandshakeRejectedError{cause: cause}
}

func (e tlsHandshakeRejectedError) Is(target error) bool {
	return target == errTLSHandshakeRejected
}

func (e tlsHandshakeRejectedError) Unwrap() error {
	return e.cause
}

func (e tlsHandshakeRejectedError) Error() string {
	return "TLS handshake rejected by device (possible clock skew or cert mismatch).\n  Check the device clock: ssh wendy@<host> 'timedatectl status'\n  For full TLS details rerun with WENDY_TLS_DEBUG=1"
}

// orgMismatchDeviceError reports that the device's server certificate belongs
// to an org the user holds no credentials for — a genuine cross-org mismatch
// distinct from a same-org handshake failure (clock skew / stale cert).
type orgMismatchDeviceError struct {
	deviceOrg int32
	userOrgs  []int32 // distinct orgs the CLI has credentials for
	// userOrgNames is a best-effort org ID -> name map for the user's own orgs,
	// resolved live from the cloud when the error is built. It may be nil or
	// partial (offline, or a name couldn't be fetched); missing entries render as
	// the bare numeric ID. The device's org is deliberately not resolved — the
	// account has no access to it, so its name isn't obtainable here.
	userOrgNames map[int32]string
}

// formatOrg renders an org as "Name (org N)" when a name was resolved, else the
// bare "org N".
func (e orgMismatchDeviceError) formatOrg(id int32) string {
	if name := e.userOrgNames[id]; name != "" {
		return fmt.Sprintf("%s (org %d)", name, id)
	}
	return fmt.Sprintf("org %d", id)
}

func (e orgMismatchDeviceError) Error() string {
	parts := make([]string, len(e.userOrgs))
	for i, o := range e.userOrgs {
		parts[i] = e.formatOrg(o)
	}
	have := "none"
	if len(parts) > 0 {
		have = strings.Join(parts, ", ")
	}
	return fmt.Sprintf(
		"The device's certificate indicates it belongs to org %d; your credentials cover %s.\n"+
			"Your account isn't a member of org %d — run 'wendy cloud login' with an account that can access org %d.",
		e.deviceOrg, have, e.deviceOrg, e.deviceOrg)
}

// orgInCerts reports whether any of the given certs carries the org ID.
func orgInCerts(org int32, certs []config.CertificateInfo) bool {
	for i := range certs {
		if int32(certs[i].OrganizationID) == org {
			return true
		}
	}
	return false
}

// newOrgMismatchDeviceError builds an orgMismatchDeviceError, deduplicating the
// user's org IDs (in first-seen order) for the message. orgNames is a best-effort
// ID -> name map for those orgs (may be nil/partial); it only enriches the
// display and never gates the error.
func newOrgMismatchDeviceError(deviceOrg int32, userCerts []config.CertificateInfo, orgNames map[int32]string) error {
	var userOrgs []int32
	seen := map[int32]bool{}
	for i := range userCerts {
		o := int32(userCerts[i].OrganizationID)
		if o == 0 || seen[o] {
			continue
		}
		seen[o] = true
		userOrgs = append(userOrgs, o)
	}
	return orgMismatchDeviceError{deviceOrg: deviceOrg, userOrgs: userOrgs, userOrgNames: orgNames}
}

// orgNameResolveTimeout bounds the best-effort ListOrganizations lookup that
// turns numeric org IDs into names in the cross-org mismatch message. The lookup
// runs on an already-failed connection path, so keep it tight and non-fatal.
const orgNameResolveTimeout = 2 * time.Second

// resolveUserOrgNamesFn resolves the user's own org IDs to human-readable names.
// Indirected so tests don't hit the network.
var resolveUserOrgNamesFn = resolveUserOrgNames

// resolveUserOrgNames returns a best-effort org ID -> name map for the orgs the
// user holds credentials for. Names come from the cloud (ListOrganizations), so
// this needs connectivity; on any error it returns whatever it resolved (possibly
// nil) and the message falls back to the bare numeric ID. Only the user's own
// orgs are resolved — the device's org is one the account has no access to, so
// its name isn't obtainable here.
func resolveUserOrgNames(ctx context.Context, userCerts []config.CertificateInfo) map[int32]string {
	want := map[int32]bool{}
	for i := range userCerts {
		if o := int32(userCerts[i].OrganizationID); o != 0 {
			want[o] = true
		}
	}
	if len(want) == 0 {
		return nil
	}
	cfg, err := config.Load()
	if err != nil || len(cfg.Auth) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, orgNameResolveTimeout)
	defer cancel()

	names := map[int32]string{}
	for i := range cfg.Auth {
		orgs, err := listOrgsFromCloud(ctx, &cfg.Auth[i])
		if err != nil {
			continue
		}
		for _, org := range orgs {
			if id := org.GetId(); want[id] && org.GetName() != "" {
				names[id] = org.GetName()
			}
		}
		if len(names) == len(want) {
			break // every org resolved; skip the remaining cloud sessions
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

type orgMismatchWithCause struct {
	mismatch error
	cause    error
}

func (e orgMismatchWithCause) Error() string {
	return e.mismatch.Error()
}

func (e orgMismatchWithCause) Unwrap() []error {
	if e.cause == nil {
		return []error{e.mismatch}
	}
	return []error{e.mismatch, e.cause}
}

// chooseRejectionError picks the error for an mTLS cert-rejection outcome. A
// genuine cross-org mismatch (the device's observed org is set and is not one
// the user holds a cert for) gets the actionable orgMismatchDeviceError; every
// other case (same-org failure such as clock skew, or no org observed) falls
// through to errTLSHandshakeRejected, whose downstream handling offers the
// clock-skew and refresh-certs remedies.
func chooseRejectionError(ctx context.Context, observedDeviceOrg int32, allCerts []config.CertificateInfo, cause error) error {
	if observedDeviceOrg != 0 && !orgInCerts(observedDeviceOrg, allCerts) {
		return orgMismatchWithCause{
			mismatch: newOrgMismatchDeviceError(observedDeviceOrg, allCerts, resolveUserOrgNamesFn(ctx, allCerts)),
			cause:    cause,
		}
	}
	return newTLSHandshakeRejectedError(cause)
}

type provisionedAgentUnauthorizedError struct {
	cause error
}

func newProvisionedAgentUnauthorizedError(cause error) error {
	if cause == nil {
		return errProvisionedAgentUnauthorized
	}
	return provisionedAgentUnauthorizedError{cause: cause}
}

func (e provisionedAgentUnauthorizedError) Error() string {
	msg := fmt.Sprintf("%s\nLast mTLS error: %v", provisionedAgentUnauthorizedMessage, e.cause)
	if isCertRefreshableError(e.cause) {
		msg += "\nYour stored certificates may be outdated. Run 'wendy auth refresh-certs' to re-issue them."
	} else if isReachabilityTimeoutError(e.cause) {
		msg += "\nThe device is enrolled and only serves mTLS on the secure port. Your wendy CLI may be too old or its certificates stale — upgrade the CLI and run 'wendy auth refresh-certs'."
	}
	return msg
}

// agentNotListeningError reports that the mTLS port refused the TCP connection:
// the host answered, but no wendy-agent was bound to the port.
//
// This is deliberately NOT a provisionedAgentUnauthorizedError. The two arrive
// at the same place — every mTLS rung failed, the plaintext probe failed, and
// mDNS still advertises an mTLS agent — but a refused connection carries no
// authentication verdict at all: the agent never got far enough to look at our
// certificate. Reporting it as "Unauthorized. Run 'wendy auth login'" sends the
// user to re-authenticate credentials that were never in question, and the most
// common way to hit it is the few-second window after `wendy device update`
// restarts the very agent we are dialling.
type agentNotListeningError struct {
	addr  string // mTLS address that refused, "" when the cause did not name one
	cause error
}

func (e agentNotListeningError) Error() string {
	where := "the device's mTLS port"
	if e.addr != "" {
		where = e.addr
	}
	// Deliberately free of the raw "rpc error: code = ..." text. formatError in
	// cmd/wendy rewrites everything from that marker onwards into "Could not
	// connect to device. Is it powered on and connected to the network?", which
	// contradicts this diagnosis — the device answered, its agent just isn't
	// bound. The cause stays reachable through Unwrap for callers and for
	// WENDY_TLS_DEBUG=1, which already logs every rung verbatim.
	return fmt.Sprintf(
		"No wendy-agent is listening on %s (connection refused).\n"+
			"The device advertises an mTLS agent, so this is not an authentication problem — the agent is most likely restarting, which is expected for a few seconds after 'wendy device update'. Retry shortly.\n"+
			"If it persists, check the agent on the device: ssh wendy@<host> 'systemctl status wendy-agent'\n"+
			"For full TLS details rerun with WENDY_TLS_DEBUG=1",
		where)
}

func (e agentNotListeningError) Unwrap() error { return e.cause }

// isConnectionRefusedError reports whether an mTLS failure is a refused TCP
// connection. Matched on the message rather than errors.Is(syscall.ECONNREFUSED)
// because gRPC flattens the dial error into a status description long before it
// reaches us, which is also why every sibling predicate here
// (isCertRefreshableError, isReachabilityTimeoutError) matches on text.
func isConnectionRefusedError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "connection refused")
}

// provisionedAgentConnectError picks the error for "a provisioned, mTLS-only
// agent would not talk to us": a refused mTLS port means the agent is down, and
// anything else (cert rejection, timeout, no certs at all) means we were not
// authorized. mtlsErr is the dial ladder's last mTLS error and is nil when the
// CLI holds no certificates — the original "you are not logged in" case, which
// correctly falls through to the unauthorized error.
func provisionedAgentConnectError(mtlsErr error) error {
	if isConnectionRefusedError(mtlsErr) {
		var attempt mtlsAttemptError
		addr := ""
		if errors.As(mtlsErr, &attempt) {
			addr = attempt.addr
		}
		return agentNotListeningError{addr: addr, cause: mtlsErr}
	}
	return newProvisionedAgentUnauthorizedError(mtlsErr)
}

// isReachabilityTimeoutError reports whether an error is a connection timeout
// against an mTLS-enrolled device's plaintext port. This indicates the device
// is up and enrolled (only the mTLS port is open), which may mean the CLI is
// too old to speak mTLS or its certificates are stale.
func isReachabilityTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "connection timed out") ||
		strings.Contains(msg, "deadline exceeded")
}

// isCertRefreshableError reports whether an mTLS failure is one that
// re-issuing the client certificate can fix: the agent rejecting a cert
// without the clientAuth EKU, an expired or not-yet-valid cert, or a
// server-sent TLS alert rejecting the presented cert. Reachability problems
// and plaintext ports probed with TLS are excluded — new certs cannot fix
// those.
func isCertRefreshableError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "first record does not look like a TLS handshake") {
		return false
	}
	for _, signal := range []string{
		"certificate is not valid for client authentication",
		"certificate not valid at current time",
		"certificate has expired",
		"expired certificate",
		"remote error: tls: bad certificate",
		"remote error: tls: certificate required",
	} {
		if strings.Contains(msg, signal) {
			return true
		}
	}
	return false
}

// confirmFn asks a yes/no question defaulting to Yes (empty input / Enter
// counts as yes), using the shared styled tui.Confirm prompt. It is a package
// var so tests can stub it. The question must not carry a "[Y/n]" suffix — the
// prompt renders its own hint. A cancelled prompt (Ctrl+C / q) counts as no.
var confirmFn = func(question string) bool {
	ok, err := tui.ConfirmDefaultYes(question, tea.WithOutput(os.Stderr))
	return err == nil && ok
}

// confirmDefaultNoFn asks a yes/no question defaulting to No (empty input /
// Enter counts as no). Used for more speculative or destructive offers (e.g. a
// timeout against an enrolled device, where refreshing certs is a guess rather
// than a clear diagnosis, or applying an OS update). Stubbed in tests.
var confirmDefaultNoFn = func(question string) bool {
	ok, err := tui.Confirm(question, tea.WithOutput(os.Stderr))
	return err == nil && ok
}

var refreshAllCertsFn = refreshAllCerts

// offerCertRefreshAndRetry prompts to re-issue mTLS certificates after a
// provisioned agent rejected the client certificate for a reason that
// re-issuance fixes, then retries the connection once. Returns (conn, true)
// only when the user accepted, the refresh succeeded, and the retry
// connected; in every other case the caller should surface the original
// error (whose message already carries the refresh-certs hint).
func offerCertRefreshAndRetry(ctx context.Context, cause error, retry func() (*grpcclient.AgentConnection, error)) (*grpcclient.AgentConnection, bool) {
	certRejected := isCertRefreshableError(cause)
	enrolledTimeout := isReachabilityTimeoutError(cause)
	if jsonOutput || !isInteractiveTerminal() || !(certRejected || enrolledTimeout) {
		return nil, false
	}
	var accepted bool
	if certRejected {
		// Clear diagnosis: the agent rejected the cert. Default to yes.
		fmt.Fprintln(os.Stderr, "The device rejected your client certificate; it may be outdated.")
		accepted = confirmFn("Refresh certificates and retry?")
	} else {
		// Timeout against an enrolled (mTLS-only) device. Refreshing certs is a
		// reasonable guess (e.g. clock skew stalling the handshake) but less
		// certain, so default to no.
		fmt.Fprintln(os.Stderr, "The device is enrolled and only responds on the secure (mTLS) port. Your certificates may be stale or your CLI too old.")
		accepted = confirmDefaultNoFn("Refresh certificates and retry?")
	}
	if !accepted {
		return nil, false
	}
	if err := refreshAllCertsFn(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Certificate refresh failed: %v\n", err)
		return nil, false
	}
	conn, err := retry()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Still unable to connect after refreshing certificates: %v\n", err)
		return nil, false
	}
	return conn, true
}

// clockSkewSyncTimeout bounds the Roughtime query + multicast.
const clockSkewSyncTimeout = 5 * time.Second

// clockSkewRetryDelay gives the device a moment to receive the multicast time
// proof and advance its clock before we retry the TLS handshake.
const clockSkewRetryDelay = 1500 * time.Millisecond

// broadcastTimeFn fetches a signed time proof and multicasts it to devices on
// the LAN. Indirected for tests.
var broadcastTimeFn = func(ctx context.Context) error {
	_, err := clitimesync.BroadcastTime(ctx)
	return err
}

// clockSkewSyncSleep is the post-broadcast wait. Indirected for tests.
var clockSkewSyncSleep = func(d time.Duration) { time.Sleep(d) }

// clockSkewSyncAttempted guards against syncing more than once per CLI run, so
// repeated connect attempts don't trigger a sync-and-sleep storm.
var clockSkewSyncAttempted bool

// isClockSkewSuspectError reports whether a connection failure looks like the
// device rejected our client cert during the TLS handshake — the signature of
// clock skew (which a time sync can fix). It matches the typed handshake
// rejection as well as cert-refreshable TLS alerts.
func isClockSkewSuspectError(err error) bool {
	return errors.Is(err, errTLSHandshakeRejected) || isCertRefreshableError(err)
}

// autoSyncTimeAndRetry handles a likely clock-skew rejection automatically: it
// broadcasts a signed time proof to the device (the same work as
// `wendy sync-time`), waits briefly for the device to adopt it, then retries
// the connection once. Returns (conn, true) only when the sync ran and the
// retry connected; in every other case the caller falls through to its
// existing error handling (e.g. the interactive cert-refresh offer).
//
// The sync runs at most once per CLI invocation. Unlike offerCertRefreshAndRetry
// it is non-interactive — clock skew has an unambiguous, side-effect-free remedy
// — so it does not gate on an interactive terminal.
func autoSyncTimeAndRetry(ctx context.Context, cause error, retry func() (*grpcclient.AgentConnection, error)) (*grpcclient.AgentConnection, bool) {
	if !isClockSkewSuspectError(cause) || clockSkewSyncAttempted {
		return nil, false
	}
	clockSkewSyncAttempted = true

	if !jsonOutput {
		fmt.Fprintln(os.Stderr, "⏱  Possible clock skew — syncing device time and retrying...")
	}

	syncCtx, cancel := context.WithTimeout(ctx, clockSkewSyncTimeout)
	syncErr := broadcastTimeFn(syncCtx)
	cancel()
	if syncErr != nil {
		// Without a fresh time proof the device clock won't move, so retrying
		// would just fail again. Surface the cause under the TLS debug flag.
		if os.Getenv("WENDY_TLS_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "[tls-debug] time sync failed: %v\n", syncErr)
		}
		return nil, false
	}

	clockSkewSyncSleep(clockSkewRetryDelay)

	conn, err := retry()
	if err != nil {
		return nil, false
	}
	return conn, true
}

// maxHandshakeTimeoutRetries bounds automatic retries of a direct device
// connection that failed with a handshake-timeout-class error (see
// isReachabilityTimeoutError) rather than a genuine rejection. Post-quantum
// ML-DSA mTLS handshakes are slow enough on constrained hardware (Jetson,
// Raspberry Pi) that even the widened mtlsProbeTimeout occasionally isn't
// enough under load; retrying turns that one-in-a-few flake into a reliable
// connect instead of surfacing a misleading "Unauthorized" error for a device
// that is up and holds a perfectly valid certificate.
const maxHandshakeTimeoutRetries = 2

// retryOnHandshakeTimeout automatically retries a direct device connection up
// to maxHandshakeTimeoutRetries times when cause is a transient mTLS
// handshake timeout rather than a genuine certificate rejection. Unlike
// offerCertRefreshAndRetry's "enrolled timeout" branch, this runs
// unconditionally (no interactive prompt, no jsonOutput gate) because
// retrying a bare timeout has no side effects and no plausible downside — it
// only ever repeats the same connect attempt.
//
// Returns (conn, nil, true) once a retry connects. On failure it returns
// (nil, cause, false) where cause is the *freshest* error observed: if a retry
// stops looking like a timeout (e.g. the device now rejects the cert outright),
// that newer, more specific error is surfaced instead of the original timeout,
// so the caller's downstream handling — including the interactive cert-refresh
// offer — diagnoses the real failure rather than the flake. A persistently
// timing-out or genuinely-unreachable device still fails in bounded time.
func retryOnHandshakeTimeout(ctx context.Context, cause error, retry func() (*grpcclient.AgentConnection, error)) (*grpcclient.AgentConnection, error, bool) {
	if !isReachabilityTimeoutError(cause) || isCertRefreshableError(cause) {
		return nil, cause, false
	}
	for attempt := 1; attempt <= maxHandshakeTimeoutRetries; attempt++ {
		if ctx.Err() != nil {
			return nil, cause, false
		}
		if !jsonOutput {
			// The trigger is any timeout-class error (isReachabilityTimeoutError
			// also matches a bare dial timeout to a down device), not strictly a
			// handshake stall, so keep the wording generic.
			fmt.Fprintf(os.Stderr, "connection timed out; retrying (%d/%d)...\n", attempt, maxHandshakeTimeoutRetries)
		}
		conn, err := retry()
		if err == nil {
			return conn, nil, true
		}
		cause = err
		if !isReachabilityTimeoutError(err) || isCertRefreshableError(err) {
			return nil, cause, false
		}
	}
	return nil, cause, false
}

func (e provisionedAgentUnauthorizedError) Is(target error) bool {
	return target == errProvisionedAgentUnauthorized
}

func (e provisionedAgentUnauthorizedError) Unwrap() error {
	return e.cause
}

var getAgentVersionAtAddress = func(ctx context.Context, address string) (bool, *agentpb.GetAgentVersionResponse, error) {
	conn, err := connectWithAutoTLS(ctx, address)
	if err != nil {
		return false, nil, err
	}
	defer conn.Close()

	resp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	return conn.IsMTLS, resp, err
}

var discoverLANDevices = func(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error) {
	return discovery.CollectLAN(ctx, cliLANStreamOptions(ctx), timeout)
}

var isInteractiveTerminalFn = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

var runAgentConnectionSpinner = func(ctx context.Context, label string, fn func(context.Context) (*grpcclient.AgentConnection, error)) (*grpcclient.AgentConnection, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	prog := tui.NewProgressProgram(tui.NewSpinner(label))

	var (
		conn   *grpcclient.AgentConnection
		runErr error
		doneCh = make(chan struct{})
	)
	go func() {
		defer close(doneCh)
		conn, runErr = fn(ctx)
		// Keep spinner teardown quiet; callers handle the returned error.
		prog.Send(tui.SpinnerDoneMsg{})
	}()

	finalModel, err := prog.Run()
	if err != nil {
		cancel()
		<-doneCh
		if conn != nil {
			conn.Close()
		}
		return nil, fmt.Errorf("spinner TUI: %w", err)
	}

	if sm, ok := finalModel.(tui.SpinnerModel); ok && !sm.Done() {
		cancel()
		<-doneCh
		if conn != nil {
			conn.Close()
		}
		return nil, ErrUserCancelled
	}

	<-doneCh
	return conn, runErr
}

// ErrUserCancelled is returned when the user cancels an interactive prompt (e.g. Ctrl+C).
var ErrUserCancelled = errors.New("cancelled")

// ErrDefaultCleared is returned after the user chooses to unset the default
// device from the recovery menu. main.go treats this as a graceful exit (code 0).
var ErrDefaultCleared = errors.New("default device cleared")

// hostPort formats a host and port into an address string,
// wrapping IPv6 addresses in brackets as required by RFC 3986.
// Uses netip.ParseAddr so IPv6 link-local addresses with zone IDs
// (e.g. fe80::1%en0) are correctly detected and bracketed.
func hostPort(host string, port int) string {
	if addr, err := netip.ParseAddr(host); err == nil && addr.Is6() {
		return fmt.Sprintf("[%s]:%d", host, port)
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// lanAgentAddresses returns candidate gRPC addresses for a LAN device.
// Prefer the discovered IP address so commands still work when .local
// hostname resolution is unavailable on the host machine.
//
// For provisioned (mTLS) devices the Avahi advertisement carries the mTLS
// port. connectWithAutoTLS derives the mTLS port as plaintext plus
// agentMTLSPortOffset, so we subtract that offset here to keep that
// convention working correctly.
func lanAgentAddresses(dev models.LANDevice) []string {
	port := dev.Port
	if port == 0 {
		port = defaultAgentPort
	}
	if dev.IsMTLS && dev.Port != 0 && port > agentMTLSPortOffset {
		port -= agentMTLSPortOffset // advertised port is mTLS; connectWithAutoTLS will add the offset back
	}

	ip, hostname := strings.TrimSpace(dev.IPAddress), strings.TrimSpace(dev.Hostname)
	hosts := []string{ip, hostname}
	// A zoned IPv6 literal (fe80::5741:1%enx0) can only come from the USB
	// well-known-address probe, which just proved the agent answers there. Such
	// a device is typically one whose mDNS is broken — that is why the probe
	// found it — so its .local name costs a full resolver timeout plus a dial
	// timeout before the address that works is even tried. It stays first.
	if strings.TrimSpace(dev.USB) != "" && !strings.Contains(ip, "%") {
		// A USB-NCM path exists. The routed Wi-Fi IP (dev.IPAddress) may be
		// black-holed by AP isolation on residential routers, so try the
		// link-local .local hostname (reachable over USB) first.
		hosts = []string{hostname, ip}
	}

	var addresses []string
	seen := make(map[string]bool)
	for _, host := range hosts {
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		addresses = append(addresses, hostPort(host, port))
	}

	return addresses
}

func preferredLANAddress(dev models.LANDevice) string {
	addresses := lanAgentAddresses(dev)
	if len(addresses) == 0 {
		return ""
	}
	return addresses[0]
}

// resolveLANAgentVersion tries the discovered LAN addresses in order and
// returns the first one that answers GetAgentVersion, along with whether that
// connection used mTLS.
func resolveLANAgentVersion(ctx context.Context, dev models.LANDevice) (string, bool, *agentpb.GetAgentVersionResponse, error) {
	// connectWithAutoTLSDiagnostics tries every stored org cert in turn, so the
	// per-address budget must cover all of them — otherwise a device whose org
	// isn't the user's first cert is cancelled before its cert is reached, even
	// though `wendy device info` (uncapped context) connects to it fine.
	budget := lanAddressProbeBudget(len(loadAllCLICerts()))
	var lastErr error
	for _, address := range lanAgentAddresses(dev) {
		attemptCtx, cancel := context.WithTimeout(ctx, budget)
		isMTLS, resp, err := getAgentVersionAtAddress(attemptCtx, address)
		cancel()
		if err == nil {
			return address, isMTLS, resp, nil
		}
		lastErr = err
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no LAN address available for %q", dev.DisplayName)
	}
	return "", false, nil, lastErr
}

// resolveLANVersion queries a single LAN device's gRPC endpoint to populate
// version metadata. It also returns whether that connection used mTLS.
func resolveLANVersion(ctx context.Context, dev models.LANDevice) (models.LANDevice, bool, error) {
	_, isMTLS, resp, err := resolveLANAgentVersion(ctx, dev)
	if err != nil {
		return dev, false, err
	}
	dev.AgentVersion = resp.GetVersion()
	dev.DeviceType = resp.GetDeviceType()
	dev.OS = resp.GetOs()
	dev.OSVersion = resp.GetOsVersion()
	dev.CPUArchitecture = resp.GetCpuArchitecture()
	return dev, isMTLS, nil
}

// lanProber is the CLI's discovery.LANProber: it probes a device's agent for
// version metadata and stamps the connection's authoritative mTLS status onto
// the returned device.
func lanProber(ctx context.Context, dev models.LANDevice) (models.LANDevice, error) {
	resolved, isMTLS, err := resolveLANVersion(ctx, dev)
	if err != nil {
		return dev, err
	}
	resolved.IsMTLS = isMTLS
	return resolved, nil
}

// lanStreamFn is a seam over discovery.StreamLAN so tests can substitute a
// fake event stream; production never reassigns it.
var lanStreamFn = discovery.StreamLAN

// lanRowState maps one streaming LAN discovery event onto the row state a
// surface renders it as, plus whether the row is marked insecure. Shared by
// the device picker and the discover TUI so the two can never drift.
//
//   - a cached row, and a live sighting no probe has answered for yet, are
//     both "verifying" (spinner);
//   - a probe that failed on a device mDNS can see stops the spinner: the row
//     shows the failure glyph and may show the no-access hint;
//   - only a successful probe can speak for the connection's mTLS status,
//     so nothing else ever marks a row insecure;
//   - a cached row nothing confirmed goes offline, and stays listed.
func lanRowState(ev discovery.LANEvent) (probe tui.ProbeState, insecure bool) {
	switch {
	case ev.Kind == discovery.LANOffline:
		return tui.ProbeOffline, false
	case ev.Kind == discovery.LANCached:
		return tui.ProbePending, false
	case ev.ProbeFailed:
		return tui.ProbeFailed, false
	case ev.Probed:
		return tui.ProbeOK, !ev.Device.IsMTLS
	default:
		return tui.ProbePending, false
	}
}

// cliLANStreamOptions is the CLI's single definition of how a LAN scan should
// run: read/write the on-disk cache (so a device seen in a prior run appears
// instantly), confirm every candidate with lanProber (an agent probe), never a
// bare mDNS sighting, and keep this machine's own VMs out of the list (see
// simulatorFilter). Every CLI surface that collects LAN devices — the discover
// TUI, the run picker, one-shot/JSON discover, MCP's device_list, fleet
// commands, and the batch helpers below — shares this so they all get the
// same cache+probe acceleration and the same idea of what a device is. ctx
// bounds the filter's background learning; pass the session's.
func cliLANStreamOptions(ctx context.Context) discovery.StreamOptions {
	return discovery.StreamOptions{UseCache: true, Prober: lanProber, Exclude: newSimulatorFilter(ctx)}
}

// SelectedDevice represents either a gRPC agent, BLE device, or an external provider device.
type SelectedDevice struct {
	// Exactly one of Agent/Bluetooth/External is set.
	Agent     *grpcclient.AgentConnection
	Bluetooth *models.BluetoothDevice
	External  *models.ExternalDevice
	Provider  providers.DeviceProvider
	// PinKey is the name the device pin for this selection is filed under: the
	// hostname behind the picker row the user chose. It is the only part of a
	// picker selection that identifies a device durably — the address came from
	// discovery, and a pin keyed on a DHCP lease would be worthless. Empty for
	// selections with no hostname to key on, which are left unenforced (see
	// enforceSelectedDevicePin).
	PinKey string
}

// Close releases any resources held by this SelectedDevice.
func (s *SelectedDevice) Close() {
	if s.Agent != nil {
		s.Agent.Close()
	}
}

// resolveDeviceAddress turns --device (or the saved default) into the address to
// dial and the key its pin is filed under.
//
// pinKey is derived from addr by pinKeyForAddr — the very function the dial
// ladder uses on the address it is handed — so the key a connection is CHECKED
// against and the key it is DIALLED with are the same function of the same
// input, and cannot drift apart. That matters more than it looks: a host pinned
// under one key and verified under another would leave enforcement switched off
// while every log line and config entry said it was on.
func resolveDeviceAddress() (addr string, pinKey string, isDefault bool, err error) {
	hostname := deviceFlag
	if hostname == "" {
		cfg, loadErr := config.Load()
		if loadErr != nil {
			return "", "", false, fmt.Errorf("loading config: %w", loadErr)
		}
		hostname = cfg.DefaultDevice
		isDefault = hostname != ""
	}
	if hostname == "" {
		return "", "", false, fmt.Errorf("no device specified; use --device flag or set a default with 'wendy device set-default'")
	}
	// If the hostname already contains a port, use it as-is.
	addr = hostname
	if _, _, splitErr := net.SplitHostPort(hostname); splitErr != nil {
		addr = hostPort(hostname, defaultAgentPort)
	}
	return addr, pinKeyForAddr(addr), isDefault, nil
}

// recoveryChoice represents the user's selection in the default-device recovery menu.
type recoveryChoice int

const (
	recoveryDiscover     recoveryChoice = iota // run device discovery picker
	recoveryUnsetDefault                       // clear the default device
	recoveryExit                               // exit with the original error
)

// recoveryModel is a minimal Bubble Tea model for the default-device recovery menu.
type recoveryModel struct {
	choices  []string
	cursor   int
	chosen   int
	hostname string
	quit     bool
}

func (m recoveryModel) Init() tea.Cmd { return nil }

func (m recoveryModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.choices)-1 {
				m.cursor++
			}
		case "enter":
			m.chosen = m.cursor
			return m, tea.Quit
		case "q", "ctrl+c":
			m.chosen = len(m.choices) - 1 // treat as Exit
			m.quit = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m recoveryModel) View() string {
	if m.quit {
		return ""
	}

	warnStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214")) // amber
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	selectStyle := lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary)

	var sb strings.Builder
	sb.WriteString(warnStyle.Render(fmt.Sprintf("Attempting to reach default device %q but it is unavailable.", m.hostname)))
	sb.WriteString("\n\n")
	sb.WriteString(dimStyle.Render("Would you like to:"))
	sb.WriteString("\n")

	for i, choice := range m.choices {
		if i == m.cursor {
			sb.WriteString(selectStyle.Render("  > " + choice))
		} else {
			sb.WriteString(dimStyle.Render("    " + choice))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// promptDefaultDeviceRecovery shows an interactive menu when the saved default
// device is unreachable. It returns the user's chosen recovery action.
func promptDefaultDeviceRecovery(hostname string) recoveryChoice {
	m := recoveryModel{
		hostname: hostname,
		choices: []string{
			"Discover another device",
			"Unset the default device",
			"Exit",
		},
	}
	final, err := tea.NewProgram(m).Run()
	if err != nil {
		return recoveryExit
	}
	fm, ok := final.(recoveryModel)
	if !ok {
		return recoveryExit
	}
	return recoveryChoice(fm.chosen)
}

// isInteractiveTerminal returns true when both stdin and stdout are TTYs,
// meaning it is safe to show interactive Bubble Tea prompts.
func isInteractiveTerminal() bool {
	return isInteractiveTerminalFn()
}

// handleDefaultDeviceRecovery runs the recovery flow after a default device
// connection failure. Shows a warning and immediately opens the device picker
// where the user can select a new device and optionally set/unset default
// via 'd'/'x' shortcuts.
func handleDefaultDeviceRecovery(ctx context.Context, hostname string, elapsed time.Duration, _ error, excludeProviders map[string]bool, includeBluetooth bool, suppressUpdateCheck bool) (*SelectedDevice, error) {
	warnStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	fmt.Println(warnStyle.Render(fmt.Sprintf("⚠ Default device %q is unreachable after %s.", hostname, formatElapsedSeconds(elapsed))))
	fmt.Println()

	return pickDevice(ctx, excludeProviders, includeBluetooth, suppressUpdateCheck)
}

func defaultDeviceSearchLabel(hostname string) string {
	return fmt.Sprintf("Searching for default device %q...", hostname)
}

func formatElapsedSeconds(elapsed time.Duration) string {
	roundedElapsed := elapsed.Round(10 * time.Millisecond)
	seconds := roundedElapsed.Seconds()
	unit := "seconds"
	if roundedElapsed == time.Second {
		unit = "second"
	}
	return fmt.Sprintf("%.2f %s", seconds, unit)
}

// deferProvisionedMTLSCheck starts the "does this address advertise an
// mTLS-only agent?" mDNS browse concurrently with the connection attempt and
// returns a getter for its result. The browse (~0.5s) is only consulted when a
// plaintext probe FAILS — to tell an unprovisioned device apart from a
// provisioned one rejecting plaintext — so on the common success path the
// getter is never called and the browse stays off the critical path. Starting
// it now (rather than after the probe) keeps the observation tied to this
// connection attempt, matching the original eager-snapshot intent.
func deferProvisionedMTLSCheck(ctx context.Context, addr string) func() bool {
	// Snapshot the discovery hook here, on the caller's goroutine. The browse
	// below outlives this function, and tests swap this seam back in t.Cleanup —
	// reading it from inside the goroutine races that restore.
	discover := discoverLANDevices
	ch := make(chan bool, 1)
	go func() { ch <- provisionedAgentAdvertisedMTLSVia(ctx, discover, addr) }()
	var (
		once sync.Once
		res  bool
	)
	return func() bool {
		once.Do(func() { res = <-ch })
		return res
	}
}

func connectAgentAtAddress(ctx context.Context, addr string) (*grpcclient.AgentConnection, error) {
	return connectAgentAtAddressWithProvisionedHint(ctx, addr, func() bool { return false })
}

func connectAgentAtAddressWithProvisionedHint(ctx context.Context, addr string, provisionedMTLS func() bool) (*grpcclient.AgentConnection, error) {
	tm := phaseTimer()
	conn, mtlsErr, err := connectWithAutoTLSDiagnostics(ctx, addr)
	if err != nil {
		return nil, err
	}
	tm("  ↳ mTLS attempts (connectWithAutoTLSDiagnostics)")
	if !conn.IsMTLS {
		// gRPC plaintext connections are lazy. Probe before returning so
		// command UIs don't surface delayed transport errors, and so provisioned
		// agents that only expose the mTLS port can report an auth error.
		probeCtx, cancel := context.WithTimeout(ctx, agentPlaintextProbeTimeout)
		resp, probeErr := conn.AgentService.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
		cancel()
		tm("  ↳ plaintext probe (GetAgentVersion)")
		if probeErr != nil {
			conn.Close()
			// The provisionedMTLS observation was initiated at connection time
			// (concurrently with this attempt); consult it now to tell an
			// unprovisioned device apart from a provisioned one rejecting
			// plaintext, rather than launching a second, later browse.
			if provisionedMTLS() {
				return nil, provisionedAgentConnectError(mtlsErr)
			}
			return nil, probeErr
		}
		conn.CacheAgentVersion(resp)
	}
	// This is the choke point's only real proof-of-life exit: conn.IsMTLS
	// means dialAgentLadder already verified it with a live probe, and the
	// plaintext branch just passed its own probe above. connectWithAutoTLSDiagnostics
	// itself can't make this call — a lazy plaintext grpc.NewClient "succeeds"
	// whether or not anything is listening — so the device-cache warm-up write
	// lives here instead, where a down device (e.g. mid-restart polling loops
	// like waitForAgentRestart, which calls the lower-level connectWithAutoTLS
	// and never reaches this function) can't phantom-refresh its cache entry.
	cacheConnectSuccess(addr, conn)
	return conn, nil
}

func connectResolvedAgent(ctx context.Context, hostname, addr string, isDefault bool) (*grpcclient.AgentConnection, error) {
	return connectResolvedAgentWithProvisionedHint(ctx, hostname, addr, isDefault, func() bool { return false })
}

func connectResolvedAgentWithProvisionedHint(ctx context.Context, hostname, addr string, isDefault bool, provisionedMTLS func() bool) (*grpcclient.AgentConnection, error) {
	if isDefault && !jsonOutput && isInteractiveTerminal() {
		conn, err := runAgentConnectionSpinner(ctx, defaultDeviceSearchLabel(hostname), func(spinCtx context.Context) (*grpcclient.AgentConnection, error) {
			return connectAgentAtAddressWithProvisionedHint(spinCtx, addr, provisionedMTLS)
		})
		if err != nil {
			// The unreachable-default paths report the hostname themselves.
			return nil, err
		}
		// The spinner above clears once it succeeds, so without this the choice
		// of device leaves no trace.
		noteImplicitDevice(hostname, implicitDefaultDevice)
		return conn, nil
	}
	conn, err := connectAgentAtAddressWithProvisionedHint(ctx, addr, provisionedMTLS)
	if err == nil && isDefault {
		noteImplicitDevice(hostname, implicitDefaultDevice)
	}
	return conn, err
}

// connectToAgent establishes a gRPC connection to the target device.
// If the CLI has auth certs, it connects via mTLS on the secure port.
// Otherwise, it falls back to plaintext on the default port.
// If no device is specified via --device or config default, an interactive
// device picker is presented (unless running in --json mode).
func connectToAgent(ctx context.Context, opts ...resolveOption) (*grpcclient.AgentConnection, error) {
	cfg := resolveConfig{excludeProviderKeys: make(map[string]bool)}
	for _, o := range opts {
		o(&cfg)
	}
	// connectToAgent only ever returns a gRPC connection, so a BLE device is
	// never a usable answer here — never scan for or offer one, whatever the
	// caller passed. BLE-capable commands use resolveTarget + IncludeBluetooth.
	cfg.includeBluetooth = false

	// Admin-entitled on-device container: dial the local socket, skip discovery.
	if conn, ok, err := dialAgentSocketIfSet(ctx); ok {
		return conn, err
	}

	if cloudCfg, ok := cloudDeviceConfigFromContext(ctx); ok {
		conn, err := connectToCloudAgent(ctx, cloudCfg.CloudGRPC, cloudCfg.DeviceName, cloudCfg.BrokerURL)
		if err != nil {
			return nil, err
		}
		if !cfg.suppressProvisioningHint {
			suggestProvisioning(conn)
		}
		return conn, nil
	}

	// Keep the alias intact, including when it comes from the saved default.
	device := deviceFlag
	if device == "" {
		loaded, err := config.Load()
		if err != nil {
			return nil, err
		}
		device = loaded.DefaultDevice
	}
	if name, matched, err := simulatorName(device); err != nil {
		return nil, err
	} else if matched {
		picked, err := connectSimulatorChoiceFn(ctx, &simulatorChoice{Name: name}, cfg.suppressUpdateCheck)
		if err != nil {
			return nil, err
		}
		return picked.Agent, nil
	}
	addr, pinKey, isDefault, err := resolveDeviceAddress()
	if err == nil {
		// The name the user asked for, used both to talk about this device and
		// to decide which device is acceptable — see resolveDeviceAddress.
		hostname := pinKey
		// Same broker consult/seed pair as resolveTargetInner's named-device
		// branch: the two ladders are parallel front doors to the same devices,
		// and a broker only one of them can use leaves every command on the
		// other paying the full post-quantum handshake per invocation.
		conn, brokerHit := (*grpcclient.AgentConnection)(nil), false
		if !cfg.disableSessionBroker {
			conn, brokerHit = connectPinnedSession(ctx, addr)
		}
		if brokerHit {
			// A broker hit passed a live health probe — a proof-of-life exit
			// that must refresh the discovery/LKG entry like any other.
			cacheConnectSuccess(addr, conn)
			if isDefault {
				noteImplicitDevice(hostname, implicitDefaultDevice)
			}
		} else {
			var finished bool
			conn, finished, err = connectToAgentDirect(ctx, cfg, hostname, addr, isDefault)
			if err != nil || finished {
				// finished: default-device recovery resolved the target through
				// the picker, whose path enforces its own pin and must not be
				// seeded under the requested endpoint (it may have picked a
				// different device).
				return conn, err
			}
		}
		// WDY-1149: verify the device that answered is still the same device, in
		// the same organisation and cloud, that was pinned under this name (and
		// pin it on first use). Every named target is checked, not just the saved
		// default: --device is exactly as spoofable as a default, and a check that
		// only covers the device you did not name is not a check.
		//
		// This runs BEFORE the update check on purpose: that check can offer to
		// upload an agent binary, which must never happen against a device whose
		// identity we are about to reject.
		if pinErr := enforceDevicePin(pinKey, conn); pinErr != nil {
			conn.Close()
			return nil, pinErr
		}
		if !cfg.suppressProvisioningHint {
			suggestProvisioning(conn)
		}
		if !cfg.suppressUpdateCheck {
			var updateErr error
			conn, updateErr = checkAndOfferUpdateFn(ctx, conn)
			if updateErr != nil {
				return nil, updateErr
			}
		}
		if !brokerHit && !cfg.disableSessionBroker {
			startPinnedSession(addr, conn)
		}
		return conn, nil
	}

	// No device configured — fall back to interactive picker.
	if jsonOutput {
		return nil, fmt.Errorf("no device specified; use --device flag or set a default with 'wendy device set-default'")
	}

	target, pickErr := pickDevice(ctx, cfg.excludeProviderKeys, cfg.includeBluetooth, cfg.suppressUpdateCheck)
	if pickErr != nil {
		return nil, pickErr
	}

	return connectFromSelectedDevice(target, cfg)
}

// connectToAgentDirect is connectToAgent's named-device dial with its recovery
// ladder, split out so the broker consult above stays readable. finished
// reports that the result is final: default-device recovery resolved the
// target through the picker (whose path enforces its own pin), so the caller
// must return it untouched instead of running the named-device pin, update,
// and broker-seed steps.
func connectToAgentDirect(ctx context.Context, cfg resolveConfig, hostname, addr string, isDefault bool) (_ *grpcclient.AgentConnection, finished bool, _ error) {
	startedAt := time.Now()
	provisionedMTLS := deferProvisionedMTLSCheck(ctx, addr)
	conn, connErr := connectResolvedAgentWithProvisionedHint(ctx, hostname, addr, isDefault, provisionedMTLS)
	if connErr != nil {
		if errors.Is(connErr, ErrUserCancelled) {
			return nil, false, connErr
		}
		// A cross-org mismatch is a credentials problem, not a reachability
		// one: surface it directly rather than routing it into clock-skew
		// retry, cert-refresh, or the default-device picker (none of which can
		// resolve "you have no credentials for this device's org").
		var orgMismatch orgMismatchDeviceError
		if errors.As(connErr, &orgMismatch) {
			return nil, false, connErr
		}
		retriedConn, connErr, retried := retryOnHandshakeTimeout(ctx, connErr, func() (*grpcclient.AgentConnection, error) {
			return connectResolvedAgentWithProvisionedHint(ctx, hostname, addr, isDefault, provisionedMTLS)
		})
		// retryOnHandshakeTimeout hands back the freshest error it saw, so a
		// retry that revealed a more specific failure (e.g. a cert rejection)
		// now drives the branches below instead of the original timeout.
		if retried {
			conn = retriedConn
		} else if syncedConn, ok := autoSyncTimeAndRetry(ctx, connErr, func() (*grpcclient.AgentConnection, error) {
			return connectResolvedAgentWithProvisionedHint(ctx, hostname, addr, isDefault, provisionedMTLS)
		}); ok {
			conn = syncedConn
		} else if errors.Is(connErr, errProvisionedAgentUnauthorized) {
			refreshedConn, ok := offerCertRefreshAndRetry(ctx, connErr, func() (*grpcclient.AgentConnection, error) {
				return connectResolvedAgentWithProvisionedHint(ctx, hostname, addr, isDefault, provisionedMTLS)
			})
			if !ok {
				return nil, false, connErr
			}
			conn = refreshedConn
		} else if usbConn, ok := usbDirectFallback(ctx, hostname); ok {
			// The stored address is unreachable but the same device (verified
			// by hostname) is on USB — use it directly.
			conn = usbConn
		} else if isDefault && !jsonOutput && !cfg.nonInteractive && isInteractiveTerminal() {
			// Default device is unreachable — offer interactive recovery.
			hostname, _, _ := net.SplitHostPort(addr)
			target, recErr := handleDefaultDeviceRecovery(ctx, hostname, time.Since(startedAt), connErr, cfg.excludeProviderKeys, cfg.includeBluetooth, cfg.suppressUpdateCheck)
			if recErr != nil {
				return nil, true, recErr
			}
			picked, pickErr := connectFromSelectedDevice(target, cfg)
			return picked, true, pickErr
		} else if isDefault {
			return nil, false, defaultDeviceUnreachableError(hostname, connErr)
		} else {
			return nil, false, connErr
		}
	}
	return conn, false, nil
}

// connectFromSelectedDevice converts a SelectedDevice from the picker into a
// gRPC AgentConnection. Returns an error if the selected device does not
// support gRPC.
func connectFromSelectedDevice(target *SelectedDevice, cfg resolveConfig) (*grpcclient.AgentConnection, error) {
	if target.Agent != nil {
		if pinErr := enforceSelectedDevicePin(target); pinErr != nil {
			return nil, pinErr
		}
		if !cfg.suppressProvisioningHint {
			suggestProvisioning(target.Agent)
		}
		return target.Agent, nil
	}

	// The user picked a Bluetooth device — connectToAgent only supports gRPC.
	// Callers that support BLE should use resolveTarget() instead.
	if target.Bluetooth != nil {
		return nil, fmt.Errorf("selected device (%s) is a Bluetooth device; this command requires a LAN connection. Use 'wendy device wifi connect' which supports BLE", target.Bluetooth.DisplayName)
	}

	// The user picked a non-gRPC device (e.g. external provider) which
	// doesn't support agent commands like wifi/apps/hardware.
	if target.External != nil {
		return nil, fmt.Errorf("selected device (%s) does not support this command; select a WendyOS LAN device instead", target.External.DisplayName)
	}

	return nil, fmt.Errorf("selected device does not support gRPC agent commands")
}

// enforceSelectedDevicePin applies the device pin to a picker selection, and
// closes the connection if it is refused.
//
// The dial ladder could not do this itself: the picker dials an address
// discovery handed it, so by the time a certificate arrives the only record of
// WHICH device the user chose is on the row they selected. A selection with no
// key is left unenforced rather than pinned under a substitute — pinning device
// A's certificate under device B's name is worse than not pinning at all,
// because it looks like enforcement while checking nothing.
func enforceSelectedDevicePin(target *SelectedDevice) error {
	if target == nil || target.Agent == nil || target.PinKey == "" {
		return nil
	}
	if err := enforceDevicePin(target.PinKey, target.Agent); err != nil {
		target.Agent.Close()
		target.Agent = nil
		return err
	}
	return nil
}

// pinKeyForLANDevice is the pin key for a picker row: the device's HOSTNAME.
//
// Not its DisplayName, which is the tempting choice and the wrong one. On
// WendyOS that is a Title-Cased friendly name from the `displayname` TXT record
// ("Agx Orin") built from the device name, while the hostname is "wendyos-" +
// that name ("wendyos-agx-orin.local") — one is not a transform of the other.
// Keying picker pins on the display name would file them where the --device and
// default-device paths (which key on the hostname, via pinKeyForAddr) never
// look: the same device pinned twice under two names, each path blind to what
// the other recorded, with nothing in the config or the output to show for it.
// The hostname is also exactly what a user types for --device, which is what
// makes it the one name every path can agree on.
func pinKeyForLANDevice(d *models.LANDevice) string {
	if d == nil {
		return ""
	}
	return strings.TrimSpace(d.Hostname)
}

// connectPickedLANDevice connects to the LAN half of a picked device at addr
// and returns the selection, with the device pin enforced before anything else
// is done over that connection. d.LAN must be non-nil.
//
// The ordering is the whole point. checkAndOfferUpdate can prompt "Update the
// agent now?" and, on a yes, upload a wendy-agent binary and restart it — so it
// must not run against a device whose identity is about to be rejected. That is
// the invariant connectToAgent states for named targets, and the picker needs
// it just as badly: mDNS is unauthenticated, so the row the user clicked is a
// claim about which device answers there, not proof of it.
//
// A refusal is also not a reason to fall back to Bluetooth. The BLE fallback in
// the connect-error branch is for an UNPINNED device that did not answer at all;
// see blocksUnauthenticatedFallback for the two refusals that must never reach
// it, one of which arrives as a connect ERROR and so has to be recognised before
// the fallback, not after it.
//
// Note this also covers `wendy device set-default` with no argument, which
// picks through here: a device whose identity changed can no longer be re-pinned
// by picking it from the TUI. Naming it (`set-default <host>`, which drops the
// pin first) or `wendy device unpin <host>` is the way back — a re-pin has to be
// an act aimed at a specific device, not a row in a list mDNS filled in.
func connectPickedLANDevice(ctx context.Context, d *models.DiscoveredDevice, addr string, suppressUpdateCheck bool) (*SelectedDevice, error) {
	if name, matched, err := simulatorName(d.LAN.ID); err != nil {
		return nil, err
	} else if matched {
		// Re-resolve the live record; discovery may predate a port change.
		return connectSimulatorChoiceFn(ctx, &simulatorChoice{Name: name}, suppressUpdateCheck)
	}
	mtls := d.LAN.IsMTLS
	conn, err := connectAgentAtAddressWithProvisionedHint(ctx, addr, func() bool { return mtls })
	if err != nil {
		// Neither refusal is "the LAN attempt failed", and the BLE half of this
		// row is named by the same unauthenticated advertisement that named the
		// LAN half. Falling back would reach a peer over a transport where
		// nothing enforces the pin at all (attemptBLEConnect sets no
		// ExpectedIdentity, and enforceSelectedDevicePin is a no-op for a
		// Bluetooth selection) and report success. Typed, not text-matched: the
		// wording of these refusals is not a contract, and a message rewrite
		// must not silently reopen this.
		if blocksUnauthenticatedFallback(err) {
			return nil, err
		}
		// LAN failed — fall back to BLE if available.
		if d.Bluetooth != nil {
			return &SelectedDevice{Bluetooth: d.Bluetooth}, nil
		}
		return nil, err
	}

	picked := &SelectedDevice{Agent: conn, PinKey: pinKeyForLANDevice(d.LAN)}
	if pinErr := enforceSelectedDevicePin(picked); pinErr != nil {
		return nil, pinErr
	}
	if !suppressUpdateCheck {
		updatedConn, updateErr := checkAndOfferUpdateFn(ctx, picked.Agent)
		if updateErr != nil {
			return nil, updateErr
		}
		picked.Agent = updatedConn
	}
	return picked, nil
}

// connectWithAutoTLS tries to connect using mTLS if the CLI has auth certs,
// falling back to plaintext if no certs are available or all mTLS attempts fail.
// It tries each stored certificate in order so that both production and local
// pki-core certs are attempted.
//
// If every mTLS probe fails with a TLS handshake error, the plaintext fallback
// is skipped and the TLS error is returned with a diagnostic hint. This prevents
// the misleading "connection refused" from the plaintext port masking the real
// cause (e.g. clock skew causing "certificate not yet valid").
func connectWithAutoTLS(ctx context.Context, plaintextAddr string) (*grpcclient.AgentConnection, error) {
	conn, _, err := connectWithAutoTLSDiagnostics(ctx, plaintextAddr)
	return conn, err
}

// mdnsBrowseTimeout bounds the mDNS fallback browse so an offline default device
// does not stall a command for the full default discovery window.
const mdnsBrowseTimeout = 4 * time.Second

// mdnsBrowseTimeoutValue returns the mDNS fallback browse timeout, allowing
// WENDY_MDNS_TIMEOUT (a Go duration like "8s") to raise it for slow networks
// where the default window is too short to hear a response. Values outside
// [1s, 30s] are ignored in favour of the default.
func mdnsBrowseTimeoutValue() time.Duration {
	if v := os.Getenv("WENDY_MDNS_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= time.Second && d <= 30*time.Second {
			return d
		}
	}
	return mdnsBrowseTimeout
}

// osLookupHostFn resolves a hostname via the operating system resolver. It is a
// package variable so tests can simulate a resolver that cannot see mDNS names.
var osLookupHostFn = func(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}

// lanBrowseFn browses the LAN for WendyOS devices via mDNS. It is a package
// variable so tests can substitute a fixture instead of a real network browse.
//
// Declared without an inline initializer and assigned in init() below: an
// inline `var lanBrowseFn = func(...) { ... cliLANStreamOptions() ... }`
// creates a compile-time initialization cycle, because cliLANStreamOptions
// pulls in lanProber -> resolveLANAgentVersion -> getAgentVersionAtAddress,
// and getAgentVersionAtAddress's own initializer reaches back into this file's
// connectWithAutoTLS -> resolveMDNSHost, which reads lanBrowseFn. Deferring
// the assignment to init() (which runs after all package vars are
// initialized) breaks that cycle while keeping this a plain overridable var.
var lanBrowseFn func(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error)

func init() {
	lanBrowseFn = func(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error) {
		services, err := discovery.BrowseMDNSServices(ctx, "_wendyos._udp", timeout)
		if err != nil {
			return nil, err
		}
		devices := make([]models.LANDevice, 0, len(services))
		for _, svc := range services {
			devices = append(devices, models.LANDevice{
				Hostname:         svc.Hostname,
				IPAddress:        svc.IPAddress,
				Port:             svc.Port,
				NetworkInterface: svc.InterfaceName,
			})
		}
		return devices, nil
	}
}

// resolveHostMDNSFallback resolves a bare hostname to a single IP, preferring
// IPv4. It tries the OS resolver first, then falls back to an mDNS browse for
// ".local" names. The OS resolver (and thus gRPC's) can't see mDNS ".local"
// names on Windows or on Linux hosts without nss-mdns/avahi — and the shipped
// binaries are built CGO_ENABLED=0, so they use Go's pure resolver which
// ignores nss-mdns entirely. Only macOS resolves ".local" natively. The
// mDNS-browse fallback keeps ".local" names working on those platforms (issue
// #1155). A bare IP literal is returned unchanged; "" is returned when the
// name cannot be resolved and no advertised mDNS device matches.
//
// This is the single-address form, kept for the callers that genuinely want one
// address (localIPForHost, resolveHostPreferRoutable). Anything that DIALS the
// result wants resolveHostAllMDNSFallback: taking the first address and
// discarding the rest is what let one stale record make a reachable device look
// unreachable.
func resolveHostMDNSFallback(ctx context.Context, host string) string {
	if all := resolveHostAllMDNSFallback(ctx, host); len(all) > 0 {
		return all[0]
	}
	return ""
}

// maxDialCandidates bounds how many addresses one connect will walk. A name with
// a dozen stale AAAA records must not turn a failed connect into a dozen
// sequential handshake budgets; three covers the real case this exists for (a
// device holding one IPv4 plus a couple of IPv6 addresses).
//
// It is a real multiplier, not a free one: a fully failing walk now costs up to
// maxDialCandidates × 2 ports × certs × mtlsProbeTimeout, i.e. ~42s with a
// single cert against three black-holed addresses, where one address cost ~14s.
// Every caller on this path wraps the connect in its own deadline (typically
// 3-5s), which is what actually bounds it in practice — but do not raise this
// constant without checking those deadlines, and note the cached path can run
// the walk twice (see connectWithAutoTLSDiagnostics's retry).
const maxDialCandidates = 3

// stripZone removes an IPv6 zone suffix ("fe80::1%en0" → "fe80::1") so
// net.ParseIP, which rejects zones, can be used on an address that may carry
// one. USB link-local addresses reach this code routinely.
func stripZone(host string) string {
	if i := strings.IndexByte(host, '%'); i >= 0 {
		return host[:i]
	}
	return host
}

// orderDialCandidates de-duplicates ips and orders them by how likely a dial to
// each is to reach a device on an ordinary LAN: IPv4 first, then routable IPv6,
// then link-local and ULA IPv6 last. Connect paths add host-interface preference
// with orderRoutedDialCandidates below.
//
// The ordering is a preference *within an exhaustive walk*, not a filter. The
// single-address resolvers this replaced also preferred IPv4, but they THREW
// THE REST AWAY, which is what let one stale record make a reachable device
// look unreachable. Anything unparseable is dropped rather than passed through:
// a non-address in this list can only produce a dial that cannot succeed.
func orderDialCandidates(ips []string) []string {
	ordered := orderDialCandidatesUnbounded(ips)
	if len(ordered) > maxDialCandidates {
		ordered = ordered[:maxDialCandidates]
	}
	return ordered
}

func orderDialCandidatesUnbounded(ips []string) []string {
	var v4, v6Global, v6Local []string
	seen := map[string]bool{}
	for _, raw := range ips {
		ip := strings.TrimSpace(raw)
		if ip == "" || seen[ip] {
			continue
		}
		parsed := net.ParseIP(stripZone(ip))
		if parsed == nil {
			continue
		}
		seen[ip] = true
		switch {
		case parsed.To4() != nil:
			v4 = append(v4, ip)
		case parsed.IsLinkLocalUnicast() || parsed.IsPrivate():
			v6Local = append(v6Local, ip)
		default:
			v6Global = append(v6Global, ip)
		}
	}
	ordered := make([]string, 0, len(v4)+len(v6Global)+len(v6Local))
	ordered = append(ordered, v4...)
	ordered = append(ordered, v6Global...)
	ordered = append(ordered, v6Local...)
	return ordered
}

// orderRoutedDialCandidates adds host-interface preference to the ordinary
// address-family ordering. It runs before the candidate cap so a wired answer
// cannot be dropped merely because several Wi-Fi/IPv6 records arrived first.
func orderRoutedDialCandidates(ips []string) []string {
	ordered := preferWiredDialCandidates(orderDialCandidatesUnbounded(ips))
	if len(ordered) > maxDialCandidates {
		ordered = ordered[:maxDialCandidates]
	}
	return ordered
}

// resolveHostAllMDNSFallback resolves a bare hostname to EVERY address it
// answers to, ordered by orderDialCandidates. It is the list-returning form of
// resolveHostMDNSFallback and the same fallback chain: OS resolver first, then
// an mDNS browse for ".local" names (see resolveHostMDNSFallback's doc for why
// the browse is needed at all). A bare IP literal comes back as itself; an
// empty slice means the name could not be resolved.
func resolveHostAllMDNSFallback(ctx context.Context, host string) []string {
	if net.ParseIP(stripZone(host)) != nil {
		return []string{host}
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	ips, err := osLookupHostFn(rctx, host)
	cancel()
	if err != nil {
		ips = nil
	}

	ordered := orderRoutedDialCandidates(ips)
	// A .local hostname can legitimately answer once per interface. When the
	// discovery cache tells us its saved path is Wi-Fi or internally inconsistent
	// (for example en7 metadata with an en0-routed IP), merge interface-scoped
	// DNS-SD sightings even though the system resolver returned an answer. Once a
	// correct wired address is cached, the last-known-good path avoids this browse.
	normalized := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	needsInterfaceBrowse := len(ordered) == 0
	if !needsInterfaceBrowse && strings.HasSuffix(normalized, ".local") {
		if cached, ok := cachedDeviceHostEntry(host); ok {
			needsInterfaceBrowse = !shouldUseCachedDeviceAddress(cached.InterfaceName, cached.IP)
		}
	}
	if needsInterfaceBrowse && strings.HasSuffix(normalized, ".local") {
		ips = append(ips, resolveMDNSHostAll(ctx, host)...)
	}
	return orderRoutedDialCandidates(ips)
}

// resolveAddrOnce resolves a host:port whose host is a DNS/mDNS name to an
// preferred IP:port, so the dials below target a literal IP. A wired host route
// outranks Wi-Fi; within equal/unknown transports IPv4 keeps its usual lead.
// gRPC
// otherwise resolves the name separately for every ClientConn we open (mTLS
// port, mTLS port+1, plaintext), and an mDNS ".local" name that resolves to
// both IPv6 and IPv4 can cost a multi-second IPv6 connect timeout per dial on
// networks without IPv6 routing. Preferring IPv4 and resolving once removes
// both costs. On any resolution failure it returns addr unchanged so gRPC's
// own resolver remains the fallback.
func resolveAddrOnce(ctx context.Context, addr string) string {
	return resolveAddrCandidates(ctx, addr)[0]
}

// resolveAddrCandidates is resolveAddrOnce's list-returning form: every address
// the host answers to, each joined back to port, in routed-candidate order.
// It never returns an empty slice — on any resolution failure the sole element
// is addr unchanged, so gRPC's own resolver remains the last fallback exactly as
// before.
func resolveAddrCandidates(ctx context.Context, addr string) []string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return []string{addr} // not host:port
	}
	if net.ParseIP(stripZone(host)) != nil {
		return []string{addr} // already a literal IP (possibly zoned, e.g. USB link-local)
	}
	ips := resolveHostAllMDNSFallback(ctx, host)
	if len(ips) == 0 {
		return []string{addr}
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, net.JoinHostPort(ip, port))
	}
	return out
}

// resolveMDNSHost browses the LAN via mDNS and returns the IP address advertised
// by a device whose hostname matches host. It is the fallback used when the OS
// resolver cannot resolve an mDNS ".local" name — mirroring the discover/picker
// path, which already prefers discovered IPs for the same reason. Returns "" for
// non-".local" hosts or when no advertised device matches.
func resolveMDNSHost(ctx context.Context, host string) string {
	if all := resolveMDNSHostAll(ctx, host); len(all) > 0 {
		return all[0]
	}
	return ""
}

// resolveMDNSHostAll is resolveMDNSHost's list-returning form: the IPs of every
// advertised device whose hostname matches host, in browse order.
//
// The browse is the ONLY way the shipped CLI resolves a ".local" name (built
// CGO_ENABLED=0, so the pure Go resolver never consults mDNS), which made this
// the path that mattered most and the one with no address preference at all: it
// returned whichever matching record the browse happened to surface first.
// Returning the full list lets the caller order and walk them.
func resolveMDNSHostAll(ctx context.Context, host string) []string {
	// A dial made *by* a discovery probe must never browse: the browse starts
	// another discovery session, whose probes dial, which browse again. Each
	// level re-reads and re-parses the CLI config, certs and pin store, so the
	// tree cost 934 MB of live heap (83% of the process) in one long-running
	// `wendy device logs`. The probe already has the addresses this browse would
	// look up — discovery handed them to it. See discovery.WithinProbe.
	if discovery.IsWithinProbe(ctx) {
		return nil
	}
	normalized := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if !strings.HasSuffix(normalized, ".local") {
		return nil
	}
	timeout := mdnsBrowseTimeoutValue()
	bctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	devices, err := lanBrowseFn(bctx, timeout)
	if err != nil {
		return nil
	}
	want := normalizeMDNSHost(host)
	var ips []string
	for _, dev := range devices {
		if dev.IPAddress == "" {
			continue
		}
		if normalizeMDNSHost(dev.Hostname) == want {
			ips = append(ips, dev.IPAddress)
		}
	}
	return ips
}

// normalizeMDNSHost lowercases a hostname and strips a trailing dot and ".local"
// suffix so "Wendy-Thor.local." and "wendy-thor" compare equal.
func normalizeMDNSHost(host string) string {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	return strings.TrimSuffix(h, ".local")
}

// deviceCacheLoadFn is a seam over discoverycache.Load for tests.
var deviceCacheLoadFn = discoverycache.Load

// cachedDeviceEntry returns the device-cache entry, if any, whose Hostname
// matches host (normalizeMDNSHost equality), regardless of the entry's age
// — the connect fast path deliberately uses stale entries too (a stale IP
// costs one bounded dial attempt; the stale-cache retry re-resolves). When
// several entries' hostnames normalize equal (e.g. a device re-provisioned
// under a new identity), the most recent LastSeen wins. Shared by
// cachedDeviceHostEntry's lookup and cacheConnectSuccess's write path so a
// connect-success write always lands under a real device's existing
// identity.
func cachedDeviceEntry(cache *discoverycache.Cache, host string) (discoverycache.Entry, bool) {
	want := normalizeMDNSHost(host)
	var best discoverycache.Entry
	var found bool
	for _, e := range cache.Entries() {
		if normalizeMDNSHost(e.Hostname) != want {
			continue
		}
		if !found || e.LastSeen.After(best.LastSeen) {
			best, found = e, true
		}
	}
	return best, found
}

// cachedDeviceHostEntry loads the device cache and returns the entry whose
// hostname matches host (any age, most recent wins). This is the device-cache
// fast path's lookup: a hit here lets connectWithAutoTLSDiagnostics skip
// resolveAddrOnce (and the OS-resolver/mDNS-browse work it can do) entirely.
// Returns the empty entry and false when the cache is unavailable or nothing
// matches.
func cachedDeviceHostEntry(host string) (discoverycache.Entry, bool) {
	cache, err := deviceCacheLoadFn()
	if err != nil || cache == nil {
		return discoverycache.Entry{}, false
	}
	return cachedDeviceEntry(cache, host)
}

// lkgTCPConnectTimeout bounds the last-known-good fast path's TCP
// pre-check. A dead or reassigned cached IP must cost at most this before
// the connect falls through to fresh resolution — without the bound, the
// full ladder would burn its mtlsProbeTimeout budgets against a black hole.
const lkgTCPConnectTimeout = 1 * time.Second

// tcpDialTimeoutFn is a seam over net.DialTimeout for LKG fast-path tests.
var tcpDialTimeoutFn = net.DialTimeout

// lkgTCPAlive reports whether addr answers a bounded TCP connect within
// lkgTCPConnectTimeout. It's the shared dead-IP bound: dialAgentLKG uses it
// against the cached mTLS endpoint, and connectWithAutoTLSDiagnostics uses it
// directly against the cached plaintext endpoint for LKG-ineligible entries
// (MTLS=false or Port==0), which never call dialAgentLKG at all.
func lkgTCPAlive(addr string) bool {
	raw, err := tcpDialTimeoutFn("tcp", addr, lkgTCPConnectTimeout)
	if err != nil {
		return false
	}
	raw.Close()
	return true
}

// loadAllCLICertsFn is a seam over loadAllCLICerts for LKG fast-path tests.
var loadAllCLICertsFn = loadAllCLICerts

// lkgOutcome distinguishes dialAgentLKG's three possible results so its
// caller can tell a dead cached IP (never worth retrying against — fresh
// resolution is strictly better) apart from a live host that simply failed
// its handshake (worth the existing cached-IP ladder + stale-retry
// diagnostics, because the host is proven reachable).
type lkgOutcome int

const (
	// lkgConnected: the direct dial succeeded; conn is ready to use.
	lkgConnected lkgOutcome = iota
	// lkgDeadTCP: the TCP pre-check itself failed — the cached IP is dead
	// or unreachable. The caller must not run the cached-IP ladder against
	// it; fresh resolution is the only useful next step.
	lkgDeadTCP
	// lkgHandshakeFailed: TCP answered but the mTLS ladder didn't produce a
	// usable mTLS connection (ladder error, nil conn, or a plaintext
	// downgrade). The host is alive, so the ordinary cached-IP ladder and
	// its diagnostics still have value.
	lkgHandshakeFailed
)

// dialAgentLKG is the last-known-good direct dial: one bounded attempt at a
// cached device's advertised mTLS endpoint with the entry's org's cert
// first. The returned lkgOutcome tells the caller how to fall through —
// dialAgentLKG never surfaces its own failures as the connect's outcome.
// Trust is unchanged: the same certs, verifiers, and pins run here as on
// the ordinary path; the cache contributes routing only — pinKey is the name
// the caller was asked for, NOT the entry's IP or display name, so a poisoned
// cache row cannot pick which identity this dial is allowed to accept.
func dialAgentLKG(ctx context.Context, e discoverycache.Entry, pinKey string) (*grpcclient.AgentConnection, error, lkgOutcome) {
	addr := hostPort(e.IP, e.Port)
	tlsDebug := os.Getenv("WENDY_TLS_DEBUG") != ""
	if !lkgTCPAlive(addr) {
		if tlsDebug {
			fmt.Fprintf(os.Stderr, "[tls-debug] lkg %s: tcp pre-check failed\n", addr)
		}
		return nil, nil, lkgDeadTCP
	}
	certs := rotateCertsForOrg(loadAllCLICertsFn(), e.OrgID)
	if len(certs) == 0 {
		// TCP answered, so the host is alive — this just means there's
		// nothing to dial with. Route through the ordinary path rather than
		// treating it like a dead IP.
		return nil, nil, lkgHandshakeFailed
	}
	conn, mtlsErr, err := dialAgentLadderWithCertsFn(ctx, newDialTarget(pinKey, addr), certs)
	if err != nil || conn == nil || !conn.IsMTLS {
		// The entry advertised mTLS; a plaintext downgrade here would be
		// surprising, so route it through the ordinary path instead.
		// Describe the reason before closing conn — two of the three cases
		// carry no err at all, and reading conn.IsMTLS after Close would be
		// reading a torn-down connection.
		reason := lkgDialFailureReason(conn, mtlsErr, err)
		if conn != nil {
			conn.Close()
		}
		if tlsDebug {
			fmt.Fprintf(os.Stderr, "[tls-debug] lkg %s: direct dial failed: %s\n", addr, reason)
		}
		return nil, mtlsErr, lkgHandshakeFailed
	}
	if tlsDebug {
		fmt.Fprintf(os.Stderr, "[tls-debug] lkg %s: connected\n", addr)
	}
	return conn, mtlsErr, lkgConnected
}

// lkgDialFailureReason describes, for WENDY_TLS_DEBUG output, why the LKG
// ladder attempt didn't yield a usable mTLS connection. Only one of the three
// failure modes actually carries a ladder error: a nil conn and a plaintext
// downgrade both come back with err == nil, so formatting err alone printed a
// bare "<nil>" for exactly the two cases whose cause is least obvious. The
// downgrade case reports mtlsErr — the mTLS-probe diagnostic explaining why
// the ladder fell back to plaintext — since that, not the (absent) ladder
// error, is the reason the entry's advertised mTLS endpoint wasn't usable.
func lkgDialFailureReason(conn *grpcclient.AgentConnection, mtlsErr, err error) string {
	switch {
	case err != nil:
		return err.Error()
	case conn == nil:
		return "ladder returned no connection"
	case mtlsErr != nil:
		return fmt.Sprintf("ladder downgraded to plaintext though the cache entry advertised mTLS: %v", mtlsErr)
	default:
		return "ladder downgraded to plaintext though the cache entry advertised mTLS"
	}
}

// dialAgentLKGFn is a seam over dialAgentLKG for connect-flow tests.
var dialAgentLKGFn = dialAgentLKG

// isMDNSShapedHost reports whether host is a plausible mDNS device name: a
// bare hostname (no dot) other than the reserved loopback name "localhost",
// or one already carrying the ".local" suffix. Real FQDNs and tunnel/relay
// hostnames (dotted, non-".local") are excluded — those are never advertised
// over mDNS, so minting a fabricated device-cache identity for them would be
// actively wrong rather than merely useless. "localhost" is excluded
// separately since it's never a real device; without this a dev pointed at a
// local agent by that name would get a nonsense "localhost.local" entry.
func isMDNSShapedHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "localhost" {
		return false
	}
	if strings.HasSuffix(h, ".local") {
		return true
	}
	return !strings.Contains(h, ".")
}

// mdnsLocalHint returns guidance for ".local" mDNS resolution failures. The
// shipped CLI is built CGO_ENABLED=0, so it can't see ".local" names via the OS
// resolver (nss-mdns) and relies on an mDNS browse (avahi/raw multicast)
// instead; that browse needs multicast on the path. Returns "" for
// non-".local" hosts.
func mdnsLocalHint(host string) string {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if !strings.HasSuffix(h, ".local") {
		return ""
	}
	return "\n  Resolving a .local name needs mDNS: ensure avahi-daemon is running and" +
		" UDP 5353 isn't firewalled (e.g. 'sudo ufw allow 5353/udp'), or connect by IP."
}

// defaultDeviceUnreachableError wraps a connection failure for a saved default
// device so the message makes clear the default IS persisted but could not be
// reached — rather than letting the failure read as if set-default never took
// effect (issue #1155).
func defaultDeviceUnreachableError(hostname string, err error) error {
	return fmt.Errorf("default device %q is set but could not be reached: %w\n"+
		"  Confirm it with 'wendy device get-default'; change it with 'wendy device set-default' or clear it with 'wendy device unset-default'.%s",
		hostname, err, mdnsLocalHint(hostname))
}

// connectWithAutoTLSDiagnostics resolves plaintextAddr and runs the mTLS/
// plaintext dial ladder (see dialAgentLadder) against it. A DNS/mDNS-name host
// with a device-cache entry (any age; see cachedDeviceEntry) normally skips
// resolution and dials the cached IP directly — the "instant connect" fast
// path. A row positively observed on Wi-Fi is re-resolved so a simultaneous
// Ethernet answer can win; legacy/unknown and wired rows stay instant. The
// cached IP can be stale (the device moved, rebooted onto a new
// DHCP lease, etc.), so this distinguishes a dead cached IP from a live one
// that simply failed its handshake:
//
//   - A dead cached IP (dialAgentLKG's TCP pre-check fails, or — for entries
//     ineligible for the LKG direct dial — lkgTCPAlive's own pre-check
//     fails) never enters the cached-IP ladder at all: fromCache stays
//     false and the flow below falls straight through to fresh resolution,
//     bounding the dead-IP worst case at ~lkgTCPConnectTimeout instead of a
//     full ladder against a black hole.
//   - A live cached IP that fails its handshake (lkgHandshakeFailed, or an
//     LKG-ineligible entry that passed its TCP pre-check) is never treated
//     as "device unreachable" (spec §4) on that basis alone: it runs the
//     cached-IP ladder, and if that doesn't answer, re-resolves exactly as
//     the cache-miss path would and retries the whole ladder once — for ANY
//     failure, including a completed-but-rejected handshake. A cached address
//     is a hint, never a fact: on a network that rotates DHCP leases it can be
//     held by a different device that answers and legitimately fails the
//     check, so "something answered here" is not evidence that the right
//     something is still here. The retry is skipped only when re-resolution
//     yields the very addresses just tried (no new information, and the case
//     that preserves a proven diagnostic) or when ctx has no budget left.
//
// The device-cache write for a confirmed-live connection does NOT happen
// here — a lazy plaintext "success" from this function proves nothing (see
// cacheFastPathReachable's doc); it happens at
// connectAgentAtAddressWithProvisionedHint's real post-connect proof of life.
func connectWithAutoTLSDiagnostics(ctx context.Context, plaintextAddr string) (*grpcclient.AgentConnection, error, error) {
	// An admin-entitled on-device container reaches the agent over its local
	// unix socket (bind-mounted by the `admin` entitlement) with no mTLS. When
	// WENDY_AGENT_SOCKET is set, route every command through it and skip all
	// discovery/mTLS logic. Empty/unset => unchanged off-device behavior.
	if sock := os.Getenv("WENDY_AGENT_SOCKET"); sock != "" {
		conn, err := grpcclient.ConnectUnix(ctx, sock)
		return conn, nil, err
	}

	tlsDebug := os.Getenv("WENDY_TLS_DEBUG") != ""
	originalAddr := plaintextAddr
	// The pin key is the host the caller was ASKED to reach, captured before
	// any resolution, cache lookup, or retry can substitute an address for it.
	// Every rung below dials with this same key, so which device is acceptable
	// never depends on what discovery answered.
	pinKey := pinKeyForAddr(originalAddr)
	fromCache := false
	if plainHost, plainPort, splitErr := net.SplitHostPort(plaintextAddr); splitErr == nil && net.ParseIP(plainHost) == nil {
		if e, ok := cachedDeviceHostEntry(plainHost); ok && shouldUseCachedDeviceAddress(e.InterfaceName, e.IP) {
			cachedAddr := net.JoinHostPort(e.IP, plainPort)
			switch {
			case e.MTLS && e.Port > 0:
				// Last-known-good direct dial: the advertised mTLS endpoint
				// with the entry's org's cert first, TCP-bounded so a dead
				// IP costs at most lkgTCPConnectTimeout.
				switch conn, mtlsErr, outcome := dialAgentLKGFn(ctx, e, pinKey); outcome {
				case lkgConnected:
					return conn, mtlsErr, nil
				case lkgHandshakeFailed:
					// The host answered TCP — it's alive, just didn't
					// hand back a usable mTLS connection. The ordinary
					// cached-IP ladder (and its diagnostics) still have
					// value, so fall through to it verbatim.
					plaintextAddr, fromCache = cachedAddr, true
				case lkgDeadTCP:
					// The cached IP didn't even answer TCP. Running the
					// ladder against it would just burn its budget on a
					// black hole, so skip the fromCache path entirely —
					// fromCache stays false, and the flow below
					// re-resolves originalAddr fresh, exactly like the
					// stale-cache retry would have done, minus the
					// wasted ladder attempt.
				}
			case lkgTCPAlive(cachedAddr):
				// LKG-ineligible entry (no advertised mTLS endpoint to
				// direct-dial), but the cached-IP fromCache ladder below
				// still needs the same dead-IP bound the LKG path gets,
				// otherwise a stale entry here is unbounded.
				plaintextAddr, fromCache = cachedAddr, true
			default:
				if tlsDebug {
					fmt.Fprintf(os.Stderr, "[tls-debug] lkg-ineligible %s: tcp pre-check failed, skipping cached-IP ladder\n", cachedAddr)
				}
			}
		}
	}
	candidates := []string{plaintextAddr}
	if !fromCache {
		candidates = resolveAddrCandidates(ctx, plaintextAddr)
	}

	conn, mtlsErr, err := dialAgentLadderFn(ctx, newDialTargetCandidates(pinKey, candidates))
	if fromCache && !cacheFastPathReachableFn(ctx, conn, err) {
		// A cached address is a HINT, never a fact. Whatever went wrong against
		// it — nothing listening, or a real handshake that a real device
		// rejected — the next thing to establish is whether that address is
		// still where this device lives, so re-resolve and retry before drawing
		// any conclusion from the failure.
		//
		// This used to skip the retry for handshake-rejection-class errors, on
		// the reasoning that a completed handshake proves the address wasn't
		// stale. That reasoning does not survive a network that rotates DHCP
		// leases: the cached address gets handed to a DIFFERENT device, that
		// device answers and legitimately fails the check, and the guard then
		// declined to look anywhere else — wedging the entry and reporting a
		// correct pin as an identity problem. A handshake proves something
		// answered, not that the right something answered at the right place.
		switch {
		case ctx.Err() != nil:
			// No budget left for a retry that can only fail the same way —
			// leave the caller's own context-deadline handling to report it.
		default:
			retryAddrs := resolveAddrCandidates(ctx, originalAddr)
			if hasUntriedAddr(retryAddrs, candidates) {
				// Re-resolve exactly as the cache-miss path would (OS resolver,
				// then mDNS browse) and retry the identical ladder once, now
				// across every address the name answers to. A stale cache entry
				// must never make a reachable device look unreachable.
				//
				// The retry runs only when resolution turned up an address we
				// have NOT already dialled — otherwise it is a second identical
				// ladder for no new information. Note the retry passes the FULL
				// fresh list, not just the untried part, so its primary stays
				// the best-ordered address: the primary is what decides the
				// plaintext-suppression verdict (see mtlsWalk), and reordering
				// it here would let a retry reach a different conclusion about
				// the same device.
				if conn != nil {
					conn.Close()
				}
				conn, mtlsErr, err = dialAgentLadderFn(ctx, newDialTargetCandidates(pinKey, retryAddrs))
			}
		}
	}
	return conn, mtlsErr, err
}

// hasUntriedAddr reports whether fresh contains an address that is not in
// tried — the "re-resolution turned up somewhere new to look" test.
//
// Membership, not list equality: the cached path has dialled exactly ONE address,
// so comparing the two lists wholesale said "different" for every name that
// resolves to more than one address, making the retry unconditional and
// re-dialling the cached address for nothing.
func hasUntriedAddr(fresh, tried []string) bool {
	seen := make(map[string]bool, len(tried))
	for _, addr := range tried {
		seen[addr] = true
	}
	for _, addr := range fresh {
		if !seen[addr] {
			return true
		}
	}
	return false
}

// cacheFastPathReachable reports whether conn — the device-cache fast path's
// dial result — actually answers, bounding its own probe by whatever is left
// of ctx's deadline so it can never eat time a subsequent stale-cache retry
// needs (callers typically wrap the whole connect in a 3-5s context). A
// successful mTLS connection was already verified by a real network probe
// inside dialAgentLadder (GetAgentVersion against mtlsProbeTimeout); a
// plaintext gRPC connection is lazy (grpc.NewClient never dials until the
// first RPC), so without an explicit probe here a stale cached IP would sail
// through as a false "success" and only fail later, deep inside a command,
// instead of triggering the stale-cache retry above.
func cacheFastPathReachable(ctx context.Context, conn *grpcclient.AgentConnection, err error) bool {
	if err != nil || conn == nil {
		return false
	}
	if conn.IsMTLS {
		return true
	}
	probeTimeout := agentPlaintextProbeTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < probeTimeout {
			probeTimeout = remaining
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	resp, probeErr := conn.AgentService.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
	if probeErr == nil {
		conn.CacheAgentVersion(resp)
	}
	return probeErr == nil
}

// cacheFastPathReachableFn is a seam over cacheFastPathReachable for tests
// that need to drive connectWithAutoTLSDiagnostics's retry decision directly
// (e.g. proving the retry is skipped for a given error class) without a real
// network probe.
var cacheFastPathReachableFn = cacheFastPathReachable

// cacheConnectSuccess best-effort upserts the device cache with the IP that
// conn actually dialed for originalAddr's host, so subsequent connects to the
// same DNS/mDNS name hit cachedDeviceHostEntry's fast path. Guards, in order:
//   - only DNS/mDNS-name hosts — a literal-IP host has nothing to learn;
//   - only hosts shaped like a real mDNS device name (isMDNSShapedHost) —
//     FQDNs/tunnel relays and "localhost" are never advertised over mDNS, so
//     minting a fabricated identity for them would be actively wrong;
//   - only when conn actually dialed a literal IP — when resolution failed
//     entirely, the raw hostname can fall all the way through to
//     grpcclient.Connect unchanged, and storing a NAME in the IP field would
//     poison the next cachedDeviceHostEntry lookup with exactly the ".local"
//     resolution gap issue #1155 exists to work around.
//
// Callers must only invoke this once liveness is actually confirmed (a real
// probe, not a lazy plaintext "connect"): connectAgentAtAddressWithProvisionedHint
// after its dial-ladder or plaintext probe, and resolveTargetInner's broker-hit
// branch after the broker's health probe.
//
// When an existing entry (any age) already matches this hostname (by
// normalizeMDNSHost equality — e.g. a discovery scan's TXT-id-keyed row),
// this refreshes only that entry's IP/Port/LastSeen under its existing ID/
// DisplayName, via Upsert's non-zero-wins merge — never minting a second row
// under a different key for the same physical device, and never wiping the
// existing entry's probed AgentVersion/OS fields (this connect-only sighting
// carries none). Cache-write errors are ignored: this must never fail an
// otherwise-successful connect.
func cacheConnectSuccess(originalAddr string, conn *grpcclient.AgentConnection) {
	if conn == nil || conn.Host == "" || net.ParseIP(conn.Host) == nil {
		return
	}
	host, portStr, err := net.SplitHostPort(originalAddr)
	if err != nil || net.ParseIP(host) != nil || !isMDNSShapedHost(host) {
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return
	}
	// Prefer the endpoint the connection actually dialed (mTLS port when the
	// ladder stepped to port+1) over originalAddr's port — otherwise a
	// default-device connect (plaintext :50051 in originalAddr) would clobber
	// discovery's advertised mTLS port in the cache on every command.
	if _, connPortStr, splitErr := net.SplitHostPort(conn.Addr); splitErr == nil {
		if connPort, convErr := strconv.Atoi(connPortStr); convErr == nil {
			port = connPort
		}
	}
	cache, err := deviceCacheLoadFn()
	if err != nil || cache == nil {
		return
	}
	entry := discoverycache.Entry{
		Hostname:      cacheHostnameForStorage(host),
		IP:            conn.Host,
		Port:          port,
		MTLS:          conn.IsMTLS,
		InterfaceName: routeInterfaceForIP(conn.Host),
	}
	if org, ok := conn.ObservedServerOrg(); ok {
		entry.OrgID = org
	}
	if existing, ok := cachedDeviceEntry(cache, host); ok {
		entry.ID, entry.DisplayName = existing.ID, existing.DisplayName
	} else {
		norm := normalizeMDNSHost(host)
		entry.ID, entry.DisplayName = norm, norm
	}
	now := time.Now()
	cache.Upsert(entry, now)
	_ = cache.Flush(now)
}

// cacheHostnameForStorage normalizes a bare mDNS-style hostname (no dot) to
// its ".local" form before it's cached, matching the shape a live mDNS
// sighting stores (see discoverycache.EntryFromDevice). normalizeMDNSHost
// equality means cachedDeviceHostEntry's match doesn't actually depend on this, but
// keeping the stored form consistent avoids a confusing on-disk mix of "orin"
// and "orin.local" for the same device. Only called after isMDNSShapedHost
// has already ruled out "localhost" and non-".local" dotted hosts.
func cacheHostnameForStorage(host string) string {
	if !strings.Contains(host, ".") {
		return host + ".local"
	}
	return host
}

// mtlsAttemptError tags a failed mTLS rung with the address it was dialled at.
// It renders exactly as the "<addr>: <err>" wrapping it replaces, so the
// diagnostics that quote the last mTLS error are unchanged; the point is that
// the address survives as data, letting provisionedAgentConnectError name the
// port that refused instead of re-parsing it out of the message.
type mtlsAttemptError struct {
	addr string
	err  error
}

func (e mtlsAttemptError) Error() string { return e.addr + ": " + e.err.Error() }

func (e mtlsAttemptError) Unwrap() error { return e.err }

// mtlsWalk carries the state one ladder walk accumulates ACROSS every candidate
// address: the cert probe order (corrected at most once, and shared so a later
// address inherits an earlier one's correction), the two failure buckets that
// decide whether the plaintext rung is suppressed, and the attempt log the
// unreachable-device message is built from.
//
// It exists because the ladder grew a third nested dimension. Ports × certs were
// two local loops with local counters; addresses × ports × certs needs the
// counters to outlive one address, and "which cert order have we learned" to be
// shared rather than relearned per address.
type mtlsWalk struct {
	target     dialTarget
	allCerts   []config.CertificateInfo
	pins       certs.PinChecker
	probeOrder []int
	jumped     bool
	tlsDebug   bool

	// The suppression buckets below describe the PRIMARY candidate (target.Addr)
	// and nothing else. That scoping is load-bearing in both directions.
	//
	// target.Addr is the only address the plaintext rung ever dials, so it is the
	// only address whose TLS posture may decide whether that rung is offered. Let
	// the buckets accumulate across the whole walk instead and two things break:
	// a single unreachable extra candidate contributes a non-cert failure, which
	// un-suppresses the rung and hands out an unauthenticated connection where
	// one address alone would have refused; and a single black-holed candidate
	// (whose TLS handshake times out, which isCertRejectionError counts as a
	// rejection) suppresses the rung for a primary that legitimately earned it.
	// Neither has anything to do with whether dialing target.Addr in the clear is
	// safe.
	//
	// Non-primary candidates are therefore pure routing attempts: they can only
	// ever find a working device, never change the verdict about the primary.
	//
	// primaryOwnPortCertReject — the primary's OWN port was a TLS endpoint that
	// rejected our cert (the tunnel/mTLS-only-discovery case where that port IS
	// already the mTLS port). isCertRejectionError only fires on server-sent TLS
	// alerts, not on "server sent non-TLS preface" errors from plaintext ports.
	primaryOwnPortCertReject bool
	// primaryMTLSPortCertFails / primaryMTLSPortNonCertFails — cert-rejection vs.
	// other failures at the primary's port+1 (the dedicated mTLS port in the
	// normal case).
	primaryMTLSPortCertFails    int
	primaryMTLSPortNonCertFails int
	// primaryObservedOrg / primaryLastErr are the org read off the primary's
	// server cert and its last failure — the two inputs chooseRejectionError
	// needs. Captured per-candidate rather than read from the walk-wide fields,
	// which a later candidate would have overwritten.
	primaryObservedOrg int32
	primaryLastErr     error
	// anyCertRejection records whether ANY candidate rejected our certificate. It
	// decides nothing; it only keeps the unreachable message from claiming that
	// no certificate was ever compared when one was.
	anyCertRejection bool
	// observedDeviceOrg is the org read from the device's server cert on a failed
	// mTLS probe (0 = none), walk-wide because promoteOrgNext consumes it.
	observedDeviceOrg int32
	lastMTLSErr       error
	// attempts logs every failed rung in order, so a refusal can name the
	// addresses actually dialled instead of asserting that "no endpoint
	// answered" and leaving the user to guess where we looked.
	attempts []mtlsAttemptError
}

// primaryRejectedOurCert reports whether the PRIMARY candidate proved itself a
// TLS endpoint that refused our certificate: either its own port did, or every
// failure at its port+1 was a cert rejection rather than plain unreachability.
//
// It decides both whether the plaintext rung is suppressed and whether the
// caller gets chooseRejectionError's actionable diagnosis, and it reads ONLY the
// primary's buckets — the primary being the only address the plaintext rung
// dials. See the buckets' doc on mtlsWalk for what breaks in each direction when
// non-primary candidates are allowed a vote.
func (w *mtlsWalk) primaryRejectedOurCert() bool {
	return w.primaryOwnPortCertReject ||
		(w.primaryMTLSPortCertFails > 0 && w.primaryMTLSPortNonCertFails == 0)
}

func (w *mtlsWalk) recordMTLSErr(addr string, err error, isPrimary bool) {
	if err == nil {
		return
	}
	attempt := mtlsAttemptError{addr: addr, err: err}
	w.lastMTLSErr = attempt
	if isPrimary {
		w.primaryLastErr = attempt
	}
	w.attempts = append(w.attempts, attempt)
}

// dialAddr runs the two-port × every-cert rungs at ONE candidate address.
//
// The three return shapes are the walk's control flow: a live connection (stop,
// success), a refusal (stop, and the whole walk must end — see the identity
// aborts inline), or (nil, nil) meaning "nothing answered here, try the next
// address".
func (w *mtlsWalk) dialAddr(ctx context.Context, cand string, isPrimary bool) (*grpcclient.AgentConnection, error) {
	host, portStr, err := net.SplitHostPort(cand)
	if err != nil {
		return nil, nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, nil
	}
	// Try the given port first (covers explicit tunnel ports that already point
	// at the mTLS port), then fall back to port+1 (the normal case where
	// discovery returns the plaintext port and mTLS is port+1).
	mtlsAddrs := []string{cand, hostPort(host, port+1)}
	for addrIdx, mtlsAddr := range mtlsAddrs {
		for pos := range w.probeOrder {
			i := w.probeOrder[pos]
			conn, tlsErr := grpcclient.ConnectWithTLSExpecting(ctx, mtlsAddr, &w.allCerts[i], w.pins, w.target.Expected)
			if tlsErr != nil {
				w.recordMTLSErr(mtlsAddr, tlsErr, isPrimary)
				if w.tlsDebug {
					fmt.Fprintf(os.Stderr, "[tls-debug] ConnectWithTLS(%s) error: %v\n", mtlsAddr, tlsErr)
				}
				continue
			}
			// grpc.NewClient is lazy — verify the connection actually works with
			// a fast probe before committing to mTLS. Every candidate is already
			// a literal IP, so this only needs to cover TCP + the TLS handshake;
			// the old 8s budget (which also covered .local mDNS resolution) made
			// an unreachable mTLS port cost 8s before the plaintext fallback.
			probeCtx, cancel := context.WithTimeout(ctx, mtlsProbeTimeout)
			resp, probeErr := conn.AgentService.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
			cancel()
			if probeErr == nil {
				conn.CacheAgentVersion(resp)
				rememberCertOrg(host, w.allCerts[i].OrganizationID)
				return conn, nil
			}
			w.recordMTLSErr(mtlsAddr, probeErr, isPrimary)
			if im, mismatched := identityMismatchFn(conn); mismatched {
				conn.Close()
				// The device is wrong, not our certificate — every remaining
				// cert, port AND ADDRESS would be judged against the same pin,
				// and the plaintext rung must not be reached at all. Trying the
				// next address here would be the bug: "try somewhere else" is
				// the right answer to silence, never to a device that answered
				// and identified itself as someone else.
				//
				// refusalKey, not PinKey: the pin that produced this constraint
				// may be filed under one of the device's other names, and that
				// is the key `wendy device unpin` has to be handed to clear it.
				return nil, identityRefusal(w.target.refusalKey(), im)
			}
			if pm, mismatched := pinMismatchFn(conn); mismatched {
				conn.Close()
				// The SPKI store rejected the peer's public key. Same reasoning
				// as above — the key belongs to the device, not to our
				// certificate — but this refusal also has to exist because
				// gRPC's handshake wrapper is otherwise the only thing the user
				// sees, and it names no way out.
				return nil, spkiRefusal(w.target.refusalKey(), pm)
			}
			if w.tlsDebug {
				fmt.Fprintf(os.Stderr, "[tls-debug] GetAgentVersion(%s) error: %v\n", mtlsAddr, probeErr)
			}
			if org, ok := conn.ObservedServerOrg(); ok {
				w.observedDeviceOrg = org
				if isPrimary {
					w.primaryObservedOrg = org
				}
				// The device just named its own org. Try that org's cert next
				// rather than grinding through the remaining ones in their
				// original order. Purely a reordering of certs we already hold —
				// the trust decision stays with BuildServerVerifyConnection
				// (expected-org, ML-DSA chain, SPKI pin), so an org claimed by a
				// hostile server only ever gets shown a cert this loop would
				// have shown it anyway. Honoured once per WALK, not per address:
				// a device that keeps naming orgs must not be able to reshuffle
				// the ladder indefinitely, and spreading its attempts across
				// several addresses must not buy it more reshuffles.
				if !w.jumped && promoteOrgNext(w.probeOrder, pos, w.allCerts, org) {
					w.jumped = true
				}
			}
			conn.Close()
			certRejected := isCertRejectionError(cand, probeErr)
			if certRejected {
				w.anyCertRejection = true
			}
			if !isPrimary {
				continue
			}
			if addrIdx == 0 {
				if certRejected {
					w.primaryOwnPortCertReject = true
				}
			} else {
				if certRejected {
					w.primaryMTLSPortCertFails++
				} else {
					w.primaryMTLSPortNonCertFails++
				}
			}
		}
	}
	return nil, nil
}

// dialAgentLadderWithCerts walks every candidate address (see
// dialTarget.dialCandidates) and, at each, every stored org cert against that
// address's own port and port+1 — falling back to a plaintext connection only
// when no candidate produced an authenticated one.
//
// Candidates arrive already resolved; this function does no name resolution of
// its own. They are normally literal IP:port, but resolveAddrCandidates returns
// the original host:port unchanged when resolution fails, so a candidate may
// still be a name — deliberately, to leave gRPC's own resolver as the last
// fallback. connectWithAutoTLSDiagnostics calls this for every connect —
// resolved-address and device-cache fast path alike, including the fast path's
// re-resolve retry — so all of them share this exact ladder.
//
// Walking the candidates is what makes a name with several addresses usable. The
// ladder used to be handed one pre-resolved address, so a single unreachable one
// — a stale AAAA record, a DHCP lease the device no longer holds — ended the
// whole connect; and when the device was pinned, it ended it with a message
// about identity for what was really a routing failure.
//
// target.PinKey and target.Expected are what the user asked for, and they make
// this the single point where the two identity rules hold: a peer that proves it
// is the wrong device aborts the ENTIRE walk (no further cert, no further port,
// no further address), and a host with any pin is never offered the plaintext
// rung.
func dialAgentLadderWithCerts(ctx context.Context, target dialTarget, allCerts []config.CertificateInfo) (*grpcclient.AgentConnection, error, error) {
	candidates := target.dialCandidates()
	walk := &mtlsWalk{target: target, tlsDebug: os.Getenv("WENDY_TLS_DEBUG") != ""}
	if len(allCerts) > 0 && len(candidates) > 0 {
		walk.pins = openPinStore()
		// Probe the organisation that last authenticated against this host
		// first. With certs for several orgs loaded, the default order makes
		// every command pay a doomed handshake per non-matching org (see
		// certorder.go). Purely a reordering — the remaining certs still follow
		// in their original order, so a stale hint costs nothing extra.
		primaryHost, _, _ := net.SplitHostPort(target.Addr)
		preferredOrg, havePreferredOrg := preferredCertOrgForHost(primaryHost)
		walk.allCerts = orderCertsByOrg(allCerts, preferredOrg, havePreferredOrg)
		// probeOrder indexes walk.allCerts. It starts as the caller's order and
		// is corrected at most once, in place, the first time the device's own
		// server certificate names an org we hold an untried cert for (see
		// promoteOrgNext). That correction is what saves an agent too old to
		// advertise an mDNS `orgid` TXT record from a full linear scan on a cold
		// certorder memo: BuildServerVerifyConnection fires OnServerIdentity
		// before the expected-org and chain checks, so even a rejected probe
		// tells us which org the device actually belongs to. Shared across every
		// rung of every address, so a later address inherits what an earlier one
		// learned. In practice the correction lands on whichever rung is
		// actually an mTLS endpoint: probing a plaintext port with TLS fails
		// before any server certificate arrives, so it observes no org.
		walk.probeOrder = make([]int, len(walk.allCerts))
		for i := range walk.probeOrder {
			walk.probeOrder[i] = i
		}
		for i, cand := range candidates {
			conn, refusal := walk.dialAddr(ctx, cand, i == 0)
			if refusal != nil {
				return nil, walk.lastMTLSErr, refusal
			}
			if conn != nil {
				return conn, nil, nil
			}
		}
		if walk.primaryRejectedOurCert() {
			// A genuine cross-org mismatch (device's org is one we hold no cert
			// for) gets a clear, actionable message. A same-org failure (observed
			// org is one we have, e.g. clock skew / stale cert) or no observed org
			// falls through to the generic handshake-rejected error, which
			// connectToAgent already post-processes with clock-skew and
			// refresh-certs remedies.
			return nil, walk.primaryLastErr, chooseRejectionError(ctx, walk.primaryObservedOrg, walk.allCerts, walk.primaryLastErr)
		}
	}
	if target.pinned() {
		// A host we have already reached over mTLS must never be reached
		// unauthenticated. Unlike provisionedAgentAdvertisedMTLS this rests on
		// local state, not a TXT record the attacker also controls — and on the
		// SAME resolution of that state that produced target.Expected and
		// target.refusalKey, so the three can never disagree about which pin
		// they are talking about (see dialTarget.pinned).
		//
		// Nothing answered, so no certificate ever arrived and no identity was
		// ever compared: this refusal is about reachability and says so, and it
		// is deliberately NOT an errDeviceIdentityRefused (see
		// errNoAuthenticatedEndpoint).
		return nil, walk.lastMTLSErr, pinnedHostNoAuthenticatedEndpointError(
			target.refusalKey(), candidates, walk.attempts, walk.anyCertRejection)
	}
	// The plaintext rung stays on the primary address rather than walking the
	// candidates. grpc.NewClient is lazy, so a plaintext "success" against a dead
	// address is indistinguishable from one against a live address until the
	// first RPC — walking here would spend dials that cannot report which of them
	// actually worked. The authenticated walk above is where reachability gets
	// established; cacheFastPathReachable is what proves a plaintext one.
	conn, err := plaintextConnectFn(ctx, target.Addr)
	return conn, walk.lastMTLSErr, err
}

// dialAgentLadder is dialAgentLadderWithCerts with the CLI's stored certs
// in config order — the shape every non-fast-path caller wants.
func dialAgentLadder(ctx context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
	return dialAgentLadderWithCerts(ctx, target, loadAllCLICerts())
}

// dialAgentLadderWithCertsFn is a seam over dialAgentLadderWithCerts for
// tests that need to observe the cert order the LKG fast path passes.
var dialAgentLadderWithCertsFn = dialAgentLadderWithCerts

// dialAgentLadderFn is a seam over dialAgentLadder for tests that need to
// count or fake ladder invocations directly (e.g. proving a stale-cache
// retry did or didn't redial) without standing up a real mTLS cert chain.
var dialAgentLadderFn = dialAgentLadder

// rotateCertsForOrg returns certs reordered so entries whose OrganizationID
// matches orgID come first, preserving relative order within both groups
// (a stable partition). orgID 0 (unknown) or no match returns certs
// unchanged. Never mutates the input.
func rotateCertsForOrg(certs []config.CertificateInfo, orgID int32) []config.CertificateInfo {
	if orgID == 0 {
		return certs
	}
	matched := make([]config.CertificateInfo, 0, len(certs))
	rest := make([]config.CertificateInfo, 0, len(certs))
	for _, c := range certs {
		if int32(c.OrganizationID) == orgID {
			matched = append(matched, c)
		} else {
			rest = append(rest, c)
		}
	}
	if len(matched) == 0 {
		return certs
	}
	return append(matched, rest...)
}

// isCertRejectionError reports whether a gRPC probe error is a server-sent TLS
// alert rejecting the client certificate, as distinct from the client failing to
// complete the handshake because the server isn't a TLS endpoint at all.
// Matches "remote error: tls:" (server sent an alert) and other cert-specific
// signals; deliberately excludes "tls: first record does not look like a TLS
// handshake" (plaintext server probed with TLS) and plain transport errors.
// addr is the endpoint the probe was aimed at: over loopback the verdict has
// one extra exclusion, described below.
func isCertRejectionError(addr string, err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// A handshake ending in EOF got no TLS alert back, so nothing rejected
	// anything: something accepted the connection and closed it. A port forward
	// does exactly that when the far side is not listening -- QEMU's user-mode
	// networking accepts on the host and only then finds the guest port closed.
	// Only over loopback: elsewhere an EOF may be an on-path reset, and reading
	// that as "not a TLS endpoint" would re-offer the plaintext rung.
	if isLoopbackHost(addr) && strings.Contains(msg, "handshake failed: EOF") {
		return false
	}
	// A plaintext (unprovisioned) agent probed with TLS reports "first record
	// does not look like a TLS handshake", which gRPC wraps inside its
	// "authentication handshake failed" envelope. That is NOT a cert rejection —
	// the server simply isn't a TLS endpoint — so it must not suppress the
	// plaintext fallback. Exclude it explicitly before the broad substring match
	// below would otherwise catch the "authentication handshake failed" wrapper.
	if strings.Contains(msg, "first record does not look like a TLS handshake") {
		return false
	}
	return strings.Contains(msg, "remote error: tls:") ||
		strings.Contains(msg, "authentication handshake failed") ||
		strings.Contains(msg, "certificate required")
}

// provisionedAgentAdvertisedMTLS takes a short pre-connection LAN discovery
// snapshot and checks whether the target address was already advertised as an
// mTLS-only agent.
//
// This is a PHRASING HINT FOR ERROR MESSAGES ONLY and must never be used as a
// guard. What it reports comes from the device's own mDNS TXT records, which
// whoever answered the address controls — so "it didn't advertise mTLS" is not
// evidence that plaintext is safe. The rule that actually withholds the
// plaintext rung is dialTarget.pinned, which rests on the pin state resolved
// for this dial (see dialAgentLadderWithCerts). This snapshot is also not
// refreshed after a failed connection attempt.
func provisionedAgentAdvertisedMTLS(ctx context.Context, plaintextAddr string) bool {
	return provisionedAgentAdvertisedMTLSVia(ctx, discoverLANDevices, plaintextAddr)
}

// provisionedAgentAdvertisedMTLSVia is provisionedAgentAdvertisedMTLS with the
// discovery hook passed in rather than read from the package variable.
//
// deferProvisionedMTLSCheck runs this browse on a goroutine that outlives the
// call that started it, so reading the seam in here would read it at an
// unpredictable time — which races any test that restores the seam in
// t.Cleanup while the browse is still in flight. Callers snapshot the hook on
// their own goroutine and hand it over instead.
func provisionedAgentAdvertisedMTLSVia(ctx context.Context, discover func(context.Context, time.Duration) ([]models.LANDevice, error), plaintextAddr string) bool {
	devices, err := discover(ctx, provisionedAgentMetadataDiscoveryTimeout)
	if err != nil {
		return false
	}
	return provisionedAgentAdvertisedMTLSInSnapshot(plaintextAddr, devices)
}

func provisionedAgentAdvertisedMTLSInSnapshot(plaintextAddr string, devices []models.LANDevice) bool {
	for _, dev := range devices {
		if !dev.IsMTLS {
			continue
		}
		for _, candidate := range lanAgentAddresses(dev) {
			if sameAgentAddress(plaintextAddr, candidate) {
				return true
			}
		}
	}
	return false
}

func sameAgentAddress(a, b string) bool {
	aHost, aPort, aErr := net.SplitHostPort(a)
	bHost, bPort, bErr := net.SplitHostPort(b)
	if aErr != nil || bErr != nil {
		return a == b
	}
	return aPort == bPort && normalizeAgentHost(aHost) == normalizeAgentHost(bHost)
}

func normalizeAgentHost(host string) string {
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Unmap().String()
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// suggestProvisioning prints a hint when the connection is not using mTLS,
// nudging the user to provision the device.
func suggestProvisioning(conn *grpcclient.AgentConnection) {
	if conn.IsMTLS || jsonOutput {
		return
	}
	fmt.Fprintf(os.Stderr, "Hint: connected without mTLS. Run 'wendy device setup' to provision this device.\n")
}

// updateCheckTTL bounds how often checkAndOfferUpdate probes the agent. Within
// this window of a prior "agent is current" result, the probe (a gRPC
// round-trip that otherwise sits on the deploy hot path) is skipped entirely.
const updateCheckTTL = 6 * time.Hour

// updateCheckMarkerPath returns the per-host marker file recording the last time
// the agent was confirmed current. The CLI version is part of the key so that
// upgrading the CLI forces a fresh check immediately.
func updateCheckMarkerPath(host string) string {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	key := sha256.Sum256([]byte(host + "\x00" + version.Version))
	return filepath.Join(cacheDir, "wendy", "update-check", hex.EncodeToString(key[:])+".json")
}

// updateCheckRecentlyPassed reports whether the agent at host was confirmed
// current within updateCheckTTL, in which case the version probe can be skipped.
func updateCheckRecentlyPassed(host string) bool {
	path := updateCheckMarkerPath(host)
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) < updateCheckTTL
}

// markUpdateCheckPassed records that the agent at host is current as of now.
func markUpdateCheckPassed(host string) {
	path := updateCheckMarkerPath(host)
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte("{}"), 0o644)
}

// checkAndOfferUpdate probes the agent version and, when the agent is behind
// the CLI, either warns (non-interactive) or prompts [Y/n] (interactive). If
// the user accepts, it downloads the latest release, uploads it, and waits for
// the agent to restart, returning a fresh connection. On decline, or if the
// upload fails, the original conn is returned unchanged. If the upload succeeds
// but the agent does not come back, conn is closed and an error is returned.
func checkAndOfferUpdate(ctx context.Context, conn *grpcclient.AgentConnection) (*grpcclient.AgentConnection, error) {
	if jsonOutput {
		return conn, nil
	}
	// Skip the probe when this agent was confirmed current within updateCheckTTL.
	// This keeps the gRPC round-trip off the deploy hot path on repeat runs.
	if updateCheckRecentlyPassed(conn.Host) {
		return conn, nil
	}
	resp, ok := conn.CachedAgentVersion()
	if !ok {
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		var err error
		resp, err = conn.AgentService.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
		cancel()
		if err != nil {
			return conn, nil
		}
		conn.CacheAgentVersion(resp)
	}

	agentVer := resp.GetVersion()
	// Dev CLI builds skip the update check entirely.
	if version.IsDev(version.Version) {
		markUpdateCheckPassed(conn.Host)
		return conn, nil
	}
	// A dev agent build is running intentionally — never offer to replace it
	// with a stable release (dev is treated as the latest version).
	if version.IsDev(agentVer) {
		markUpdateCheckPassed(conn.Host)
		return conn, nil
	}
	// Unknown agent version — skip to avoid spurious update prompts.
	if agentVer == "" {
		markUpdateCheckPassed(conn.Host)
		return conn, nil
	}
	if version.CompareVersions(version.Version, agentVer) <= 0 {
		markUpdateCheckPassed(conn.Host)
		return conn, nil
	}

	if !isInteractiveTerminal() {
		fmt.Fprintf(os.Stderr, "Warning: agent is behind the CLI (agent: %s, CLI: %s). Run 'wendy device update' to update.\n", agentVer, version.Version)
		return conn, nil
	}
	warn := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))

	fmt.Fprintf(os.Stderr, warn.Render("Agent is behind the CLI (agent: %s, CLI: %s).")+"\n", agentVer, version.Version)
	if !confirmFn("Update the agent now?") {
		return conn, nil
	}

	arch := resp.GetCpuArchitecture()
	osName := resp.GetOs()

	if err := performAgentUpdate(ctx, conn, osName, arch, false); err != nil {
		fmt.Fprintf(os.Stderr, "Update failed: %v\nContinuing with existing connection.\n", err)
		return conn, nil
	}

	conn.Close()

	fmt.Fprintf(os.Stderr, "Waiting for agent to restart...")
	newConn, err := reconnectAgentAfterRestart(ctx, conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, " failed.\n")
		return nil, fmt.Errorf("agent did not come back after update: %w", err)
	}
	fmt.Fprintf(os.Stderr, " ready.\n")
	return newConn, nil
}

// checkAndOfferUpdateFn is a seam over checkAndOfferUpdate. It exists for tests
// that must prove the update check was NOT reached: it can upload and restart a
// wendy-agent binary, so "did this run?" is a security assertion about the
// identity gates that precede it, not an implementation detail.
var checkAndOfferUpdateFn = checkAndOfferUpdate

// performAgentUpdate downloads the latest release for the given osName/arch and
// uploads it to conn. Pass nightly=true to fetch the latest prerelease instead
// of stable. The agent will restart after this returns successfully.
func performAgentUpdate(ctx context.Context, conn *grpcclient.AgentConnection, osName, arch string, nightly bool) error {
	if arch == "" {
		return fmt.Errorf("device did not report CPU architecture")
	}
	fmt.Fprintf(os.Stderr, "Fetching agent for %s...\n", agentPlatformLabel(osName, arch))
	binaryData, resolvedVer, source, err := resolveAgentArtifact(osName, arch, nightly)
	if err != nil {
		return fmt.Errorf("resolving agent binary: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Downloaded agent %s (from %s)\n", resolvedVer, source)

	h := sha256.Sum256(binaryData)
	sha256Hash := hex.EncodeToString(h[:])

	fmt.Fprintf(os.Stderr, "Uploading to device...\n")
	return deviceUpdateUpload(ctx, conn.AgentService, binaryData, sha256Hash)
}

// waitForAgentRestart polls addr with connectWithAutoTLS until the agent answers
// GetAgentVersion or 60 s elapse. Returns a fresh connection on success. This
// flat 60 s already covers the Mac agent's slower unzip/codesign-verify/relaunch
// restart (see agentRestartTimeoutFor in device.go for the equivalent OS-aware
// timeout used by `device update`'s own restart wait), so no OS-specific
// branching is needed here.
func waitForAgentRestart(ctx context.Context, addr string) (*grpcclient.AgentConnection, error) {
	deadline := time.Now().Add(60 * time.Second)
	time.Sleep(time.Second) // give the agent a moment to begin shutdown
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		conn, err := connectWithAutoTLS(ctx, addr)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		resp, probeErr := conn.AgentService.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
		cancel()
		if probeErr == nil {
			conn.CacheAgentVersion(resp)
			return conn, nil
		}
		conn.Close()
		time.Sleep(time.Second)
	}
	return nil, fmt.Errorf("timed out waiting for agent to restart")
}

func loadCLICert() *config.CertificateInfo {
	auth := loadCLIAuth()
	if auth == nil {
		return nil
	}
	cert := auth.Certificates[0]
	return &cert
}

func loadAllCLICerts() []config.CertificateInfo {
	cfg, err := config.Load()
	if err != nil || len(cfg.Auth) == 0 {
		return nil
	}
	var out []config.CertificateInfo
	for _, auth := range cfg.Auth {
		if len(auth.Certificates) > 0 {
			out = append(out, auth.Certificates[0])
		}
	}
	return out
}

func loadCLIAuth() *config.AuthConfig {
	cfg, err := config.Load()
	if err != nil || len(cfg.Auth) == 0 {
		return nil
	}
	for _, auth := range cfg.Auth {
		if len(auth.Certificates) > 0 {
			return &auth
		}
	}
	return nil
}

// openPinStore loads the device pin store from the wendy config directory.
// Returns nil (without error) if the store cannot be opened, so callers can
// treat nil PinChecker as "pinning disabled" without failing the connection.
func openPinStore() certs.PinChecker {
	dir, err := config.ConfigDir()
	if err != nil {
		return nil
	}
	store, err := devicepin.Open(dir)
	if err != nil {
		return nil
	}
	return store
}

// findCertByOrgID returns the first CertificateInfo across all auth entries
// whose OrganizationID matches orgID, or nil if none is found.
func findCertByOrgID(authEntries []config.AuthConfig, orgID int) *config.CertificateInfo {
	for i := range authEntries {
		for j := range authEntries[i].Certificates {
			if authEntries[i].Certificates[j].OrganizationID == orgID {
				return &authEntries[i].Certificates[j]
			}
		}
	}
	return nil
}

// attemptBLEConnect builds a TLS config and connects to device using the
// given certificate info and pin store.
func attemptBLEConnect(device *models.BluetoothDevice, cert config.CertificateInfo, pins certs.PinChecker) (*ble.AgentClient, error) {
	keyPEM, err := cert.PrivateKeyPEM()
	if err != nil {
		return nil, fmt.Errorf("loading client key: %w", err)
	}
	tlsCfg, err := ble.NewClientTLSConfig(cert.PemCertificate, keyPEM, certs.ServerVerifyOpts{
		ChainPEM:      cert.PemCertificateChain,
		ExpectedOrgID: int32(cert.OrganizationID),
		PinStore:      pins,
	})
	if err != nil {
		return nil, fmt.Errorf("building BLE TLS config: %w", err)
	}
	return ble.ConnectAgent(device, tlsCfg)
}

// connectBLEAgent connects to device via BLE mTLS, automatically retrying
// with the matching cert if the device belongs to a different org than the
// default auth session.
func connectBLEAgent(device *models.BluetoothDevice) (*ble.AgentClient, error) {
	auth := loadCLIAuth()
	if auth == nil || len(auth.Certificates) == 0 {
		return nil, fmt.Errorf("not logged in; run 'wendy auth login' to authenticate")
	}
	pins := openPinStore()
	cert := auth.Certificates[0]

	// Best-effort time sync before mTLS handshake — gives the device a chance
	// to advance its clock before we attempt the TLS handshake.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	clitimesync.BroadcastTime(ctx) //nolint:errcheck
	cancel()

	client, err := attemptBLEConnect(device, cert, pins)
	if err == nil {
		return client, nil
	}

	var mismatch *certs.OrgMismatchError
	if !errors.As(err, &mismatch) {
		return nil, err
	}

	// The device belongs to a different org. Search all auth entries.
	cfg, cfgErr := config.Load()
	if cfgErr != nil {
		return nil, fmt.Errorf("device belongs to org %d but could not load config to find matching certificate: %w", mismatch.Got, cfgErr)
	}
	alt := findCertByOrgID(cfg.Auth, int(mismatch.Got))
	if alt == nil {
		return nil, fmt.Errorf("device belongs to org %d; authenticate for that org with 'wendy auth login'", mismatch.Got)
	}
	return attemptBLEConnect(device, *alt, pins)
}

// resolveOption configures resolveTarget behaviour.
type resolveOption func(*resolveConfig)

type resolveConfig struct {
	excludeProviderKeys      map[string]bool
	includeBluetooth         bool
	suppressProvisioningHint bool
	suppressUpdateCheck      bool
	nonInteractive           bool
	device                   string
	disableSessionBroker     bool
}

var (
	connectSessionBrokerFn = sessionbroker.Connect
	startSessionBrokerFn   = sessionbroker.Start
)

// connectPinnedSession consults the session broker for addr — the normalized
// host:port the caller is about to dial. The broker key is that full endpoint,
// never just the host: two agents can share a hostname on different ports (a
// dev agent pushed beside the production one), present the SAME asset
// certificate, and still be different endpoints — so the identity check alone
// cannot tell them apart, and a host-keyed broker would route an explicit
// :51000 request to whichever agent a previous default-port command brokered.
// The pin lookup, by contrast, is host-keyed on purpose: identity is a
// property of the device, not of the port it answers on.
func connectPinnedSession(ctx context.Context, addr string) (*grpcclient.AgentConnection, bool) {
	// The GOOS check lives here as well as in sessionbroker.Connect: bailing
	// only inside Connect would still charge Windows the expectedIdentityFor
	// config read on every invocation, for a feature it never uses.
	if runtime.GOOS == "windows" {
		return nil, false
	}
	expected := expectedIdentityFor(pinKeyForAddr(addr))
	if expected == nil {
		return nil, false
	}
	conn, err := connectSessionBrokerFn(ctx, addr, *expected)
	return conn, err == nil && conn != nil
}

func startPinnedSession(addr string, conn *grpcclient.AgentConnection) {
	// The current invocation already has a verified connection. Broker startup
	// is an optimization for the next invocation and must never affect this one.
	// Same endpoint key as connectPinnedSession, or the next consult misses.
	_ = startSessionBrokerFn(addr, conn)
}

// DisableSessionBroker forces a fresh device transport. Watch already retains
// one connection for its whole lifetime, so routing it through the short-lived
// cross-process broker adds a hop without avoiding any setup.
func DisableSessionBroker() resolveOption {
	return func(c *resolveConfig) { c.disableSessionBroker = true }
}

// SelectDevice makes device selection an explicit property of this resolve
// call. It is primarily useful for nested connections (for example, resolving
// a build host while a run command remains connected to its target) where
// temporarily mutating the package-global --device value would race with other
// in-flight commands.
func SelectDevice(device string) resolveOption {
	return func(c *resolveConfig) {
		c.device = strings.TrimSpace(device)
	}
}

// SuppressUpdateCheck prevents connectToAgent from running the automatic
// agent-version check. Use this for commands that manage updates explicitly
// (e.g. "wendy device update") to avoid a double-prompt.
func SuppressUpdateCheck() resolveOption {
	return func(c *resolveConfig) {
		c.suppressUpdateCheck = true
	}
}

// SuppressProvisioningHint prevents connectToAgent from printing the
// "run 'wendy device setup'" hint when connected without mTLS.
func SuppressProvisioningHint() resolveOption {
	return func(c *resolveConfig) {
		c.suppressProvisioningHint = true
	}
}

// NonInteractive prevents resolveTarget from opening an interactive device
// picker. When no device is specified in non-interactive mode, a clear error
// is returned instead of attempting to open a TTY.
func NonInteractive() resolveOption {
	return func(c *resolveConfig) {
		c.nonInteractive = true
	}
}

// IncludeBluetooth opts a command into BLE discovery: the picker runs a BLE
// scan, offers BLE-only devices, and resolveTarget may hand back a
// SelectedDevice with only Bluetooth set.
//
// BLE is opt-in because reaching a device over BLE needs explicit handling in
// the caller (see connectBLEAgent and the ble.AgentClient command set, which
// covers only wifi/apps/hardware/version). A command that has no BLE code path
// must not offer BLE devices in its picker — the user would pick one only to be
// told the command can't use it. Pass this only from commands that branch on
// target.Bluetooth.
func IncludeBluetooth() resolveOption {
	return func(c *resolveConfig) {
		c.includeBluetooth = true
	}
}

// ExcludeProviders prevents the named provider keys from appearing in the
// interactive device picker.
func ExcludeProviders(keys ...string) resolveOption {
	return func(c *resolveConfig) {
		for _, k := range keys {
			c.excludeProviderKeys[k] = true
		}
	}
}

// resolveTarget resolves the target device and, for agent connections,
// best-effort corrects a lagging device clock before the caller operates on it
// (issue #1171). This is the single funnel every command uses to obtain a
// device, so the clock fix applies to info, run, deploy, ros2 bag, etc.
func resolveTarget(ctx context.Context, opts ...resolveOption) (*SelectedDevice, error) {
	sel, err := resolveTargetInner(ctx, opts...)
	if err != nil {
		return nil, err
	}
	if sel != nil && sel.Agent != nil {
		maybeFixClock(ctx, sel.Agent)
	}
	return sel, nil
}

// dialAgentSocketIfSet returns a connection to the local agent unix socket when
// WENDY_AGENT_SOCKET is set — the admin-entitled on-device container case, where
// the socket is bind-mounted in and there is no device PKI / discovery to do. It
// reports ok=true whenever the env var is set (even if the dial errors), so
// callers short-circuit ALL discovery/mTLS logic rather than falling through to
// mDNS. The socket is the entire trust boundary (see the admin entitlement).
func dialAgentSocketIfSet(ctx context.Context) (conn *grpcclient.AgentConnection, ok bool, err error) {
	sock := os.Getenv("WENDY_AGENT_SOCKET")
	if sock == "" {
		return nil, false, nil
	}
	conn, err = grpcclient.ConnectUnix(ctx, sock)
	return conn, true, err
}

// resolveTargetInner inspects the --device flag and returns either an external
// provider device or falls back to the gRPC agent connection. If no device
// is specified and no default is configured, an interactive device picker
// is presented.
func resolveTargetInner(ctx context.Context, opts ...resolveOption) (*SelectedDevice, error) {
	cfg := resolveConfig{excludeProviderKeys: make(map[string]bool)}
	for _, o := range opts {
		o(&cfg)
	}

	// An admin-entitled on-device container reaches the agent over its local
	// unix socket; skip all discovery/selection when WENDY_AGENT_SOCKET is set.
	if conn, ok, err := dialAgentSocketIfSet(ctx); ok {
		if err != nil {
			return nil, err
		}
		return &SelectedDevice{Agent: conn}, nil
	}

	if cloudCfg, ok := cloudDeviceConfigFromContext(ctx); ok {
		deviceName := cloudCfg.DeviceName
		if cfg.device != "" {
			deviceName = cfg.device
		}
		conn, err := connectToCloudAgent(ctx, cloudCfg.CloudGRPC, deviceName, cloudCfg.BrokerURL)
		if err != nil {
			return nil, err
		}
		return &SelectedDevice{Agent: conn}, nil
	}

	device := cfg.device
	if device == "" {
		device = deviceFlag
	}
	isDefault := false
	if device == "" {
		loadedCfg, err := config.Load()
		if err != nil {
			return nil, fmt.Errorf("loading config: %w", err)
		}
		device = loadedCfg.DefaultDevice
		isDefault = device != ""
	}

	rt := phaseTimer()

	if name, matched, err := simulatorName(device); err != nil {
		return nil, err
	} else if matched {
		return connectSimulatorChoiceFn(ctx, &simulatorChoice{Name: name}, cfg.suppressUpdateCheck)
	}

	// Check if the device flag matches a known provider key.
	if device != "" {
		if p := providers.ProviderForKey(device); p != nil {
			devices, err := p.DiscoverDevices(ctx)
			if err != nil {
				return nil, fmt.Errorf("discovering %s devices: %w", p.DisplayName(), err)
			}
			if len(devices) == 0 {
				return nil, fmt.Errorf("no %s devices found", p.DisplayName())
			}
			return &SelectedDevice{
				External: &devices[0],
				Provider: p,
			}, nil
		}
	}

	// Check if the device flag matches a discovered device ID (e.g.
	// "adb:emulator-5554"). Skip this for anything that looks like a network
	// address — a ".local" mDNS name, hostname, or IP all contain a "." (or
	// "[" for IPv6) — because provider IDs are short dotless tokens and the
	// discovery loop here spins up every provider (e.g. the adb server), costing
	// seconds. A WendyOS agent address falls through to the gRPC connect below.
	if device != "" && !strings.Contains(device, ".") && !strings.HasPrefix(device, "[") {
		if sel := findDeviceByID(ctx, device); sel != nil {
			return sel, nil
		}
	}
	rt("  ↳ findDeviceByID (provider discovery)")

	// If a device hostname was given, connect via gRPC (with mTLS if authenticated).
	if device != "" {
		addr := device
		if _, _, splitErr := net.SplitHostPort(device); splitErr != nil {
			addr = hostPort(device, defaultAgentPort)
		}
		conn, brokerHit := (*grpcclient.AgentConnection)(nil), false
		if !cfg.disableSessionBroker {
			conn, brokerHit = connectPinnedSession(ctx, addr)
		}
		if brokerHit {
			rt("  ↳ reusable session connection")
			// A broker hit passed a live health probe: it is a proof-of-life
			// exit like connectAgentAtAddressWithProvisionedHint's, and must
			// refresh the discovery/LKG entry the same way — otherwise a
			// device reached exclusively through its broker ages out of the
			// cache while being connected to continuously.
			cacheConnectSuccess(addr, conn)
			if isDefault {
				noteImplicitDevice(device, implicitDefaultDevice)
			}
		} else {
			startedAt := time.Now()
			provisionedMTLS := deferProvisionedMTLSCheck(ctx, addr)
			var err error
			conn, err = connectResolvedAgentWithProvisionedHint(ctx, device, addr, isDefault, provisionedMTLS)
			rt("  ↳ connectResolvedAgent (dial+probe)")
			if err != nil {
				if errors.Is(err, ErrUserCancelled) {
					return nil, err
				}
				if syncedConn, ok := autoSyncTimeAndRetry(ctx, err, func() (*grpcclient.AgentConnection, error) {
					return connectResolvedAgentWithProvisionedHint(ctx, device, addr, isDefault, provisionedMTLS)
				}); ok {
					conn = syncedConn
				} else if errors.Is(err, errProvisionedAgentUnauthorized) {
					refreshedConn, ok := offerCertRefreshAndRetry(ctx, err, func() (*grpcclient.AgentConnection, error) {
						return connectResolvedAgentWithProvisionedHint(ctx, device, addr, isDefault, provisionedMTLS)
					})
					if !ok {
						return nil, err
					}
					conn = refreshedConn
				} else if isDefault && !jsonOutput && !cfg.nonInteractive && isInteractiveTerminal() {
					// Default device is unreachable — offer interactive recovery.
					recovered, recErr := handleDefaultDeviceRecovery(ctx, device, time.Since(startedAt), err, cfg.excludeProviderKeys, cfg.includeBluetooth, cfg.suppressUpdateCheck)
					if recErr != nil {
						return nil, recErr
					}
					if pinErr := enforceSelectedDevicePin(recovered); pinErr != nil {
						return nil, pinErr
					}
					return recovered, nil
				} else if isDefault {
					return nil, defaultDeviceUnreachableError(device, err)
				} else {
					return nil, err
				}
			}
		}
		// Same pin key as connectToAgent's: the host of the address dialled, via
		// the same pinKeyForAddr the ladder uses. resolveTarget reaches devices
		// connectToAgent never sees, and an unchecked path is the whole attack.
		if pinErr := enforceDevicePin(pinKeyForAddr(addr), conn); pinErr != nil {
			conn.Close()
			return nil, pinErr
		}
		if !cfg.suppressUpdateCheck {
			var updateErr error
			conn, updateErr = checkAndOfferUpdateFn(ctx, conn)
			if updateErr != nil {
				return nil, updateErr
			}
		}
		rt("  ↳ checkAndOfferUpdate")
		if !brokerHit && !cfg.disableSessionBroker {
			startPinnedSession(addr, conn)
		}
		return &SelectedDevice{Agent: conn}, nil
	}

	// No device specified — run interactive picker if we have a TTY.
	if jsonOutput || cfg.nonInteractive {
		return nil, fmt.Errorf("no device specified; use --device flag or set a default with 'wendy device set-default'")
	}

	picked, pickErr := pickDevice(ctx, cfg.excludeProviderKeys, cfg.includeBluetooth, cfg.suppressUpdateCheck)
	if pickErr != nil {
		return nil, pickErr
	}
	if pinErr := enforceSelectedDevicePin(picked); pinErr != nil {
		return nil, pinErr
	}
	return picked, nil
}

// findDeviceByID searches all available providers for a device whose ID
// matches the given string (e.g. "adb:emulator-5554").
func findDeviceByID(ctx context.Context, id string) *SelectedDevice {
	for _, p := range providers.AvailableProviders() {
		devices, err := p.DiscoverDevices(ctx)
		if err != nil {
			continue
		}
		for _, d := range devices {
			if d.ID == id {
				d := d // copy for stable pointer
				return &SelectedDevice{
					External: &d,
					Provider: p,
				}
			}
		}
	}
	return nil
}

// ensureAppConfig loads wendy.json from cfgPath. If the file does not exist
// and stdin is a TTY (or autoAccept is true), a default config is created automatically.
func ensureAppConfig(cfgPath string, autoAccept bool) (*appconfig.AppConfig, error) {
	cfg, err := appconfig.LoadFromFile(cfgPath)
	if err == nil {
		return cfg, nil
	}

	// If the error is anything other than "file not found", return it as-is
	// (e.g. a JSON parse error).
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	dir := filepath.Dir(cfgPath)
	dirName := filepath.Base(dir)

	if !autoAccept {
		// File doesn't exist. If we're not in an interactive terminal, give a
		// helpful error message.
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return nil, fmt.Errorf("wendy.json not found; run 'wendy init <app-id>' to create one")
		}

		fmt.Println("No wendy.json found in current directory.")
		if !confirmFn(fmt.Sprintf("Create one with app ID %q?", dirName)) {
			return nil, fmt.Errorf("wendy.json is required; run 'wendy init <app-id>' to create one")
		}
	}

	// Detect language from the project files on disk.
	language := ""
	projectType, _ := detectProjectType(dir) // ignore multiple-xcodeproj error for config init
	switch projectType {
	case "python":
		language = "python"
	case "swift":
		language = "swift"
	case "xcode":
		language = "swift"
	}

	entitlements := defaultEntitlements(language, "")

	newCfg := &appconfig.AppConfig{
		AppID:        dirName,
		Version:      "0.1.0",
		Language:     language,
		Entitlements: entitlements,
	}

	data, marshalErr := json.MarshalIndent(newCfg, "", "  ")
	if marshalErr != nil {
		return nil, fmt.Errorf("marshaling config: %w", marshalErr)
	}

	if writeErr := os.WriteFile(cfgPath, data, 0o644); writeErr != nil {
		return nil, fmt.Errorf("writing wendy.json: %w", writeErr)
	}

	fmt.Printf("Created wendy.json for %s\n", dirName)
	return newCfg, nil
}

// pickerItemDeviceID extracts a hostname or provider key from a picker item,
// suitable for storing as the default device (must be resolvable by resolveDeviceAddress).
func pickerItemDeviceID(item tui.PickerItem) string {
	entry, ok := item.Value.(*pickerEntry)
	if !ok {
		return ""
	}
	// For LAN devices, use the mDNS hostname (matches what pickDeviceForDefault returns).
	if entry.mergedDevice != nil && entry.mergedDevice.LAN != nil {
		addr := entry.mergedDevice.LAN.Hostname
		if addr == "" {
			addr = entry.mergedDevice.LAN.IPAddress
		}
		return addr
	}
	if entry.externalDevice != nil {
		return entry.externalDevice.ProviderKey
	}
	if entry.mergedDevice != nil && len(entry.mergedDevice.Externals) > 0 {
		return entry.mergedDevice.Externals[0].ProviderKey
	}
	return ""
}

// pickerEntry is the value stored in each PickerItem.
type pickerEntry struct {
	mergedDevice   *models.DiscoveredDevice
	externalDevice *models.ExternalDevice
	provider       providers.DeviceProvider
}

// mergePickerItem merges a newly discovered transport into an existing picker
// item for the same physical device. It combines connection types, prefers
// LAN addresses, and merges the underlying DiscoveredDevice fields.
// nextProbeState resolves the probe state for a merged picker row. A succeeded
// probe (ProbeOK) is sticky: it survives a later transient failure and is never
// reset to the spinner when the device is rediscovered. A failed probe stays
// failed until a retry succeeds, and is not flipped back to the spinner on
// rediscovery. ProbeNone (non-LAN transports) never overrides a real state.
func nextProbeState(existing, incoming tui.ProbeState) tui.ProbeState {
	switch incoming {
	case tui.ProbeOK:
		return tui.ProbeOK
	case tui.ProbeFailed:
		if existing == tui.ProbeOK {
			return tui.ProbeOK
		}
		return tui.ProbeFailed
	case tui.ProbePending:
		if existing == tui.ProbeNone {
			return tui.ProbePending
		}
		return existing
	default:
		return existing
	}
}

func mergePickerItem(existing *tui.PickerItem, incoming tui.PickerItem) {
	e, eOK := existing.Value.(*pickerEntry)
	n, nOK := incoming.Value.(*pickerEntry)
	if !eOK || !nOK || e.mergedDevice == nil || n.mergedDevice == nil {
		return
	}

	md := e.mergedDevice
	nd := n.mergedDevice

	if nd.LAN != nil && md.LAN == nil {
		md.LAN = nd.LAN
		existing.Address = incoming.Address
		// A LAN entry carries the friendly displayname TXT record; prefer it over
		// a BLE-advertised hostname for both the visible name and the sort order.
		if incoming.Name != "" {
			existing.Name = incoming.Name
			md.DisplayName = nd.DisplayName
			existing.SortKey = deviceSortKey(existing.Name, existing.USB)
		}
	}
	if nd.LAN != nil && md.LAN != nil && nd.LAN.USB != "" && md.LAN.USB == "" {
		md.LAN.USB = nd.LAN.USB
		md.LAN.NetworkInterface = nd.LAN.NetworkInterface
	}
	if nd.LAN != nil && nd.LAN.USB != "" && existing.USB == "" {
		existing.USB = nd.LAN.USB
		existing.SortKey = deviceSortKey(existing.Name, nd.LAN.USB)
	}
	if nd.Bluetooth != nil && md.Bluetooth == nil {
		md.Bluetooth = nd.Bluetooth
	}
	for _, ext := range nd.Externals {
		found := false
		for _, e := range md.Externals {
			if e.ID == ext.ID {
				found = true
				break
			}
		}
		if !found {
			md.Externals = append(md.Externals, ext)
			sort.Slice(md.Externals, func(i, j int) bool {
				return md.Externals[i].Rank() > md.Externals[j].Rank()
			})
		}
	}
	if md.LAN == nil && len(nd.Externals) > 0 && existing.Address == "" {
		existing.Address = incoming.Address
	}

	if md.AgentVersion == "" {
		md.AgentVersion = nd.AgentVersion
		existing.AgentVersion = incoming.AgentVersion
	}
	if md.OS == "" {
		md.OS = nd.OS
	}
	if md.OSVersion == "" {
		md.OSVersion = nd.OSVersion
		existing.OSVersion = incoming.OSVersion
	}
	if md.CPUArchitecture == "" {
		md.CPUArchitecture = nd.CPUArchitecture
	}

	if existing.AgentVersion == "" {
		existing.AgentVersion = incoming.AgentVersion
	}
	if existing.OSVersion == "" {
		existing.OSVersion = incoming.OSVersion
	}

	if existing.Name == "" {
		existing.Name = incoming.Name
	}

	// Rebuild the type string from the merged transports.
	existing.Type = md.ConnectionTypes()

	// Propagate security status: LAN probes determine mTLS, BLE doesn't. Once
	// we know a device is insecure (or secure), update the existing item.
	// The same goes for the provisioned state and the no-access hint, which
	// clears once a probe succeeds.
	if nd.LAN != nil {
		existing.Insecure = incoming.Insecure
		existing.Provisioned = incoming.Provisioned
		existing.Hint = incoming.Hint
	}
	// The no-access hint must stay consistent with the version cell no matter
	// which transport supplied the version: AgentVersion is carried over from
	// earlier LAN probes or backfilled from BLE above, and a hint claiming
	// agent details are unreadable must not accompany a displayed version.
	if existing.AgentVersion != "" && existing.Hint == discoverNoAccessHint {
		existing.Hint = ""
	}

	existing.Probe = nextProbeState(existing.Probe, incoming.Probe)
}

// deviceSortKey orders device rows by display name, floating USB-tethered
// devices ("0_") above everything else ("1_"). It sorts on the human-facing
// name rather than the dedup key so rows stay ordered by name even though
// deduplication now keys on the hostname (see deviceDedupKey).
func deviceSortKey(name, usb string) string {
	prefix := "1_"
	if usb != "" {
		prefix = "0_"
	}
	return prefix + strings.ToLower(name)
}

// deviceDedupKey returns the cross-transport dedup key for a picker row: the
// device's normalized hostname when known, so an mDNS entry (friendly
// displayname) and a BLE entry (raw hostname) for the same physical device
// collapse into a single row. Falls back to the display name when no hostname
// is available.
func deviceDedupKey(hostKey, displayName string) string {
	if hostKey != "" {
		return hostKey
	}
	return strings.ToLower(displayName)
}

// hideLocalProviders returns a copy of excludes that additionally hides the
// local run targets (this machine, Docker/OrbStack, Apple Container) unless the
// user opts them back in via providers.ShowLocalDevices. The input map is left
// untouched (and may be nil); the picker lists separate WendyOS devices by
// default so local runtimes don't crowd out real hardware.
func hideLocalProviders(excludes map[string]bool) map[string]bool {
	merged := make(map[string]bool, len(excludes)+len(providers.LocalProviderKeys()))
	for k, v := range excludes {
		merged[k] = v
	}
	if !providers.ShowLocalDevices() {
		for _, k := range providers.LocalProviderKeys() {
			merged[k] = true
		}
	}
	return merged
}

// unflashedLiteDedupKey keys a board with no Wendy Lite firmware by its port
// rather than its synthetic display name, so the row it gets once it identifies
// itself can supersede it.
func unflashedLiteDedupKey(serialPort string) string {
	return "wendy-lite-unflashed:" + serialPort
}

// externalProviderPickerItem builds the picker row for a device discovered
// through an external provider. wendy-lite devices are presented as merged
// devices (like LAN discoveries) so they share the LAN row layout.
func externalProviderPickerItem(prov providers.DeviceProvider, dev *models.ExternalDevice) tui.PickerItem {
	if prov.Key() == "wendy-lite" {
		item := tui.PickerItem{
			Name:         dev.DisplayName,
			DedupKey:     dev.DisplayName,
			Type:         dev.ConnectionType() + " (Lite)",
			Address:      dev.ConnectionInfo["ip"],
			AgentVersion: dev.AgentVersion,
			OS:           dev.OS,
			OSVersion:    dev.OSVersion,
			Value: &pickerEntry{mergedDevice: &models.DiscoveredDevice{
				DisplayName:     dev.DisplayName,
				AgentVersion:    dev.AgentVersion,
				OSVersion:       dev.OSVersion,
				CPUArchitecture: dev.CPUArchitecture,
				Externals:       []*models.ExternalDevice{dev},
			}},
		}
		if port := dev.ConnectionInfo["serialPort"]; port != "" {
			if dev.ConnectionInfo["needsInstall"] == "true" {
				item.DedupKey = unflashedLiteDedupKey(port)
				// The branch sets no SortKey, so ordering falls back to the
				// dedup key; pin it to the name to keep the row's position.
				item.SortKey = strings.ToLower(dev.DisplayName)
			} else {
				item.Supersedes = unflashedLiteDedupKey(port)
			}
		}
		return item
	}
	return tui.PickerItem{
		Name:         dev.DisplayName,
		Type:         prov.DisplayName(),
		Address:      externalProviderAddress(dev.ProviderKey, dev.ID),
		AgentVersion: dev.AgentVersion,
		OS:           dev.OS,
		OSVersion:    dev.OSVersion,
		DedupKey:     dev.DisplayName,
		SortKey:      externalProviderSortKey(prov.Key(), dev.DisplayName),
		Hint:         externalProviderPickerHint(prov.Key()),
		Value:        &pickerEntry{externalDevice: dev, provider: prov},
	}
}

// providerPollDelay returns how long to wait before the next DiscoverDevices
// poll given how long the previous scan took: the remainder of the 3s cycle,
// but never less than 500ms between scans.
func providerPollDelay(elapsed time.Duration) time.Duration {
	delay := 3*time.Second - elapsed
	if delay < 500*time.Millisecond {
		delay = 500 * time.Millisecond
	}
	return delay
}

// discoverProviderForPicker feeds picker items for prov until ctx is done.
// Providers implementing providers.ContinuousDiscoverer are consumed as a
// stream; otherwise DiscoverDevices is polled on a 3-second cadence measured
// from the start of each scan (with a 500ms minimum gap, so slow scans don't
// stretch the period). If the stream fails to start or closes while the
// picker is still open, discovery falls back to polling.
func discoverProviderForPicker(ctx context.Context, prov providers.DeviceProvider, send func([]tui.PickerItem)) {
	if cd, ok := prov.(providers.ContinuousDiscoverer); ok {
		if ch, err := cd.DiscoverDevicesContinuous(ctx); err == nil {
			for dev := range ch {
				send([]tui.PickerItem{externalProviderPickerItem(prov, &dev)})
			}
			if ctx.Err() != nil {
				return
			}
		}
	}

	for {
		start := time.Now()
		devices, err := prov.DiscoverDevices(ctx)
		if err == nil {
			var items []tui.PickerItem
			for i := range devices {
				items = append(items, externalProviderPickerItem(prov, &devices[i]))
			}
			if len(items) > 0 {
				send(items)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(providerPollDelay(time.Since(start))):
		}
	}
}

// pickDevice runs the interactive Local | Cloud TUI. The local page discovers
// devices across all transports and providers; the cloud page lists the
// selected organization's online Wendy Cloud devices.
// LAN discovery runs continuously so devices that come online after the
// initial scan still appear in the picker.
// excludeProviders hides the named provider keys from the picker. Local run
// targets are hidden on top of these unless providers.ShowLocalDevices is set.
// includeBluetooth enables the BLE scan; it is off by default so commands that
// cannot talk over BLE never show a device they can't use (see
// IncludeBluetooth).
func pickDevice(ctx context.Context, excludeProviders map[string]bool, includeBluetooth bool, suppressUpdateCheck bool) (*SelectedDevice, error) {
	cfg, err := config.Load()
	if err != nil {
		cfg = nil
	}
	cloudAuth := devicePickerInitialAuth(cfg)

	for {
		selected, err := pickDeviceWithCloudAuth(ctx, excludeProviders, includeBluetooth, suppressUpdateCheck, cloudAuth)
		switch {
		case errors.Is(err, errDevicePickerLogin):
			if err := performLogin(ctx, defaultCloudDashboard, defaultCloudGRPC); err != nil {
				return nil, err
			}
			cfg, err = config.Load()
			if err != nil {
				return nil, fmt.Errorf("loading config after login: %w", err)
			}
			cloudAuth = devicePickerInitialAuth(cfg)
		case errors.Is(err, errDevicePickerSwitchOrg):
			cfg, err = config.Load()
			if err != nil {
				return nil, fmt.Errorf("loading config: %w", err)
			}
			picked, _, pickErr := switchCloudOrganization(ctx, cfg)
			if errors.Is(pickErr, ErrUserCancelled) {
				continue
			}
			if pickErr != nil {
				return nil, pickErr
			}
			cloudAuth = picked
		default:
			return selected, err
		}
	}
}

var (
	errDevicePickerLogin     = errors.New("device picker requested cloud login")
	errDevicePickerSwitchOrg = errors.New("device picker requested organization switch")
)

func pickDeviceWithCloudAuth(ctx context.Context, excludeProviders map[string]bool, includeBluetooth bool, suppressUpdateCheck bool, cloudAuth *config.AuthConfig) (*SelectedDevice, error) {
	excludeProviders = hideLocalProviders(excludeProviders)

	picker := tui.NewPicker()
	picker.MergeItem = mergePickerItem

	// Load current default device to show ✦ indicator.
	defaultOrgID := int32(0)
	if loadedCfg, err := config.Load(); err == nil {
		defaultOrgID = defaultOrgForCloudAuth(loadedCfg, cloudAuth)
		if loadedCfg.DefaultDevice != "" {
			picker.DefaultKey = strings.ToLower(loadedCfg.DefaultDevice)
		}
	}

	// Allow 'd' to set default and 'x' to unset default from the picker.
	picker.OnSetDefault = func(item tui.PickerItem) string {
		deviceID := pickerItemDeviceID(item)
		if deviceID == "" {
			return ""
		}
		if cfg, err := config.Load(); err == nil {
			cfg.DefaultDevice = deviceID
			_ = config.Save(cfg)
		}
		return fmt.Sprintf("Default device set to %s.", item.Name)
	}
	picker.OnUnsetDefault = func() string {
		if cfg, err := config.Load(); err == nil {
			cfg.DefaultDevice = ""
			_ = config.Save(cfg)
		}
		return "Default device cleared."
	}

	// Cancel continuous discovery when the picker exits.
	discoverCtx, discoverCancel := context.WithCancel(ctx)
	p := tea.NewProgram(newDevicePickerModel(discoverCtx, picker, cloudAuth, defaultOrgID))

	sendLANItem := func(dev models.LANDevice, insecure bool, probe tui.ProbeState) {
		p.Send(devicePickerLocalMsg{msg: tui.PickerAddMsg{Items: []tui.PickerItem{lanPickerItem(dev, insecure, probe)}}})
	}
	// Streaming LAN discovery — cached rows appear instantly, live sightings
	// and probe outcomes follow, and the engine itself handles offline
	// detection and retry (see discovery.StreamLAN). Prober must be set: with
	// a nil Prober a cached row can never be confirmed offline.
	events := lanStreamFn(discoverCtx, cliLANStreamOptions(discoverCtx))
	go func() {
		// ev.Supersedes needs no handling here: picker rows dedup by hostname
		// (deviceDedupKey/HostKey), so a superseded connect-minted row and the
		// TXT-id row that replaces it are already the same row.
		for ev := range events {
			if ev.Kind == discovery.LANRetracted {
				// Listed, then found to be one of this machine's VMs: it
				// belongs on the Simulator tab, not here.
				p.Send(devicePickerLocalMsg{msg: lanPickerRemoveMsg(ev.Device)})
				continue
			}
			probe, insecure := lanRowState(ev)
			sendLANItem(ev.Device, insecure, probe)
		}
	}()

	// USB well-known-address probe: a USB-attached device appears in the picker
	// even when mDNS is broken on this host — precisely the case the stream
	// above cannot cover, since it only ever learns of devices that announce
	// themselves. The picker's MergeItem dedupes the row against the mDNS entry
	// for the same device.
	go func() {
		// probeUSBDirectDevices returns only candidates whose agent already
		// answered GetAgentVersion, so these rows arrive fully resolved: no
		// pending spinner, no re-probe, and IsMTLS is the connection it made.
		for _, dev := range probeUSBDirectDevices(discoverCtx) {
			if discoverCtx.Err() != nil {
				return
			}
			sendLANItem(dev, !dev.IsMTLS, tui.ProbeOK)
		}
	}()

	// Continuous provider discovery — streamed when the provider supports it,
	// otherwise re-scanned on a 3-second cadence.
	for _, prov := range providers.AvailableProviders() {
		if excludeProviders[prov.Key()] {
			continue
		}
		go discoverProviderForPicker(discoverCtx, prov, func(items []tui.PickerItem) {
			p.Send(devicePickerLocalMsg{msg: tui.PickerAddMsg{Items: items}})
		})
	}

	// Continuous Bluetooth discovery — re-scan every 5 seconds.
	if includeBluetooth {
		go func() {
			for {
				bleDevices, err := discovery.DiscoverBluetooth(discoverCtx, true)
				if err == nil && len(bleDevices) > 0 {
					var items []tui.PickerItem
					for i := range bleDevices {
						connType := "BLE"
						if !bleDevices[i].IsWendyAgent() {
							connType = "BLE (Lite)"
						}
						items = append(items, tui.PickerItem{
							Name:     bleDevices[i].DisplayName,
							DedupKey: deviceDedupKey(bleDevices[i].HostKey(), bleDevices[i].DisplayName),
							SortKey:  deviceSortKey(bleDevices[i].DisplayName, ""),
							Type:     connType,
							Address:  bleDevices[i].Address,
							// A Lite device reports firmware here, not an agent version.
							AgentVersion:  bleDevices[i].AgentVersion,
							AgentOutdated: bleDevices[i].IsWendyAgent() && agentBehindCLI(version.Version, bleDevices[i].AgentVersion),
							OS:            bleDevices[i].OS,
							OSVersion:     bleDevices[i].OSVersion,
							Value: &pickerEntry{mergedDevice: &models.DiscoveredDevice{
								DisplayName:     bleDevices[i].DisplayName,
								AgentVersion:    bleDevices[i].AgentVersion,
								OS:              bleDevices[i].OS,
								OSVersion:       bleDevices[i].OSVersion,
								CPUArchitecture: bleDevices[i].CPUArchitecture,
								Bluetooth:       &bleDevices[i],
							}},
						})
					}
					p.Send(devicePickerLocalMsg{msg: tui.PickerAddMsg{Items: items}})
				}

				select {
				case <-discoverCtx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
		}()
	}

	finalModel, err := p.Run()
	discoverCancel() // stop all background discovery
	if err != nil {
		return nil, fmt.Errorf("device picker: %w", err)
	}

	dm, ok := finalModel.(devicePickerModel)
	if !ok {
		return nil, fmt.Errorf("device picker returned unexpected model %T", finalModel)
	}
	switch dm.action {
	case devicePickerLogin:
		return nil, errDevicePickerLogin
	case devicePickerSwitchOrg:
		return nil, errDevicePickerSwitchOrg
	}
	if dm.cancelled {
		return nil, ErrUserCancelled
	}
	choice, ok := dm.choice()
	if !ok {
		return nil, fmt.Errorf("no device selected")
	}
	switch choice.Tab {
	case devicePickerCloudTab:
		cliLogln("Connecting to %s via cloud tunnel...", choice.Cloud.GetName())
		conn, err := connectCloudAsset(ctx, cloudAuth, choice.Cloud, dm.cloud.brokerURL)
		if err != nil {
			return nil, err
		}
		return &SelectedDevice{Agent: conn}, nil
	case devicePickerSimulatorTab:
		return connectSimulatorChoiceFn(ctx, choice.Simulator, suppressUpdateCheck)
	default:
		return connectLocalPickerChoice(ctx, choice.Local, suppressUpdateCheck)
	}
}

// lanPickerItem is the run picker's row for a LAN device.
func lanPickerItem(dev models.LANDevice, insecure bool, probe tui.ProbeState) tui.PickerItem {
	devCopy := dev
	// While the probe is still in flight the Agent/OS columns show a
	// spinner, so suppress the no-access hint until we actually know the
	// probe failed.
	hint := ""
	if probe != tui.ProbePending {
		hint = lanNoAccessHint(&devCopy, dev.AgentVersion)
	}
	return tui.PickerItem{
		Name:          dev.DisplayName,
		Type:          "LAN",
		USB:           dev.USB,
		Address:       preferredLANAddress(dev),
		AgentVersion:  dev.AgentVersion,
		AgentOutdated: agentBehindCLI(version.Version, dev.AgentVersion),
		OS:            dev.OS,
		OSVersion:     dev.OSVersion,
		Provisioned:   lanProvisionedDisplay(&devCopy),
		Hint:          hint,
		Probe:         probe,
		DedupKey:      deviceDedupKey(dev.HostKey(), dev.DisplayName),
		SortKey:       deviceSortKey(dev.DisplayName, dev.USB),
		Insecure:      insecure,
		Value: &pickerEntry{mergedDevice: &models.DiscoveredDevice{
			DisplayName:     dev.DisplayName,
			AgentVersion:    dev.AgentVersion,
			OS:              dev.OS,
			OSVersion:       dev.OSVersion,
			CPUArchitecture: dev.CPUArchitecture,
			LAN:             &devCopy,
		}},
	}
}

// lanPickerRemoveMsg takes back the row lanPickerItem built for dev, under the
// same key it was added with.
func lanPickerRemoveMsg(dev models.LANDevice) tui.PickerRemoveMsg {
	return tui.PickerRemoveMsg{Key: deviceDedupKey(dev.HostKey(), dev.DisplayName)}
}

// connectLocalPickerChoice turns a Local-tab selection into a connection. Lifted
// verbatim out of pickDeviceWithCloudAuth so the three-way dispatch above stays
// readable on one screen.
func connectLocalPickerChoice(ctx context.Context, sel *tui.PickerItem, suppressUpdateCheck bool) (*SelectedDevice, error) {
	if sel == nil {
		return nil, fmt.Errorf("no device selected")
	}

	entry, ok := sel.Value.(*pickerEntry)
	if !ok {
		return nil, fmt.Errorf("invalid picker selection")
	}

	// Merged LAN/Bluetooth/External device — prefer LAN (gRPC), fall back to BLE/External.
	if entry.mergedDevice != nil {
		d := entry.mergedDevice
		if d.LAN != nil {
			addr, _, _, err := resolveLANAgentVersion(ctx, *d.LAN)
			if err != nil {
				// LAN metadata lookups can fail on provisioned devices without CLI certs.
				// In that case, still try the preferred address once before falling back.
				addr = preferredLANAddress(*d.LAN)
			}
			if addr == "" {
				if d.Bluetooth != nil {
					return &SelectedDevice{Bluetooth: d.Bluetooth}, nil
				}
				if err != nil {
					return nil, err
				}
				return nil, fmt.Errorf("selected LAN device has no usable address")
			}
			return connectPickedLANDevice(ctx, d, addr, suppressUpdateCheck)
		}

		// Wendy Lite device — set both BLE and External+Provider when
		// available so callers can pick the right transport.
		sel := &SelectedDevice{}
		if d.Bluetooth != nil {
			sel.Bluetooth = d.Bluetooth
		}
		if len(d.Externals) > 0 {
			sel.External = d.Externals[0]
			sel.Provider = providers.ProviderForKey(d.Externals[0].ProviderKey)
		}
		if sel.Bluetooth != nil || sel.External != nil {
			return sel, nil
		}
	}

	// External provider device.
	if entry.externalDevice != nil && entry.provider != nil {
		return &SelectedDevice{
			External: entry.externalDevice,
			Provider: entry.provider,
		}, nil
	}

	return nil, fmt.Errorf("selected device type is not yet supported")
}

// resolveAgentPlatform determines the target platform string from the user's
// wendy.json platform field, the agent's OS, and the agent's CPU architecture.
//
// Rules:
//   - If cfgPlatform is a full "os/arch" string, use it as-is.
//   - If cfgPlatform is OS-only (e.g., "linux" or "darwin"), append the agent arch.
//   - If cfgPlatform is empty, default to Linux with the agent architecture.
//   - "wendyos" is a compatibility alias for "linux" and is normalized before
//     passing the platform to container builders.
func resolveAgentPlatform(cfgPlatform, agentOS, agentArch string) string {
	if cfgPlatform == "" {
		return appconfig.PlatformLinux + "/" + agentArch
	}
	if i := strings.IndexByte(cfgPlatform, '/'); i >= 0 {
		return normalizePlatformOS(cfgPlatform[:i]) + cfgPlatform[i:]
	}
	// OS-only: append agent architecture.
	return normalizePlatformOS(cfgPlatform) + "/" + agentArch
}

func normalizePlatformOS(os string) string {
	if strings.EqualFold(os, appconfig.PlatformWendyOS) {
		return appconfig.PlatformLinux
	}
	return os
}

func registryPort(agentOS string) int {
	if agentOS == "darwin" {
		return 5555
	}
	return 5000
}

// platformOS extracts the OS component from a platform string like "linux/arm64".
func platformOS(platform string) string {
	if i := strings.IndexByte(platform, '/'); i >= 0 {
		return platform[:i]
	}
	return platform
}
