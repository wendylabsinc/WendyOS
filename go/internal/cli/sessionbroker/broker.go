// Package sessionbroker keeps an authenticated agent gRPC transport alive for
// a short period between CLI invocations and exposes it through a user-owned
// Unix socket. It is deliberately limited to pinned, direct-LAN mTLS devices:
// plaintext targets have no identity to bind a broker to, while cloud sessions
// also need a persistent registry/tunnel dialer and remain outside this bounded
// first implementation.
package sessionbroker

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"

	// The proxy decompresses inbound messages and re-compresses them on the
	// upstream call (see rpcCompression); both directions need the gzip codec
	// registered in this process.
	"google.golang.org/grpc/codes"
	_ "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

const (
	DefaultIdleTTL = 2 * time.Minute
	healthTimeout  = 750 * time.Millisecond
	// The helper's first probe includes the device's post-quantum mTLS
	// handshake, which can take a few seconds on constrained boards. Broker-hit
	// health probes are local and use the much smaller bound above.
	upstreamProbeTimeout = 7 * time.Second
	// Parent liveness keeps a newly prepared broker available while the
	// preparing invocation is still doing local build work. The primary signal
	// is an inherited pipe whose EOF marks the parent's exit exactly; this cap
	// applies only to the PID fallback (helpers started without a pipe), where
	// PID reuse could otherwise keep an idle helper around indefinitely.
	maxParentLease = 30 * time.Minute
)

var ErrUnavailable = errors.New("session broker unavailable")

type Spec struct {
	Key             string              `json:"key"`
	Host            string              `json:"host"`
	Addr            string              `json:"addr"`
	CertFingerprint string              `json:"certFingerprint"`
	Expected        certs.WendyIdentity `json:"expected"`
	ParentPID       int                 `json:"parentPid,omitempty"`
}

type stateFile struct {
	Spec Spec `json:"spec"`
}

var (
	configDir  = config.ConfigDir
	loadConfig = config.Load
)

// parentLeases retains the write ends of every helper's lease pipe for the
// life of this process. The open descriptor IS the lease — its EOF tells the
// helper the parent exited — and letting the *os.File become garbage would
// have the runtime's finalizer close it, ending the lease while the parent
// still runs.
var (
	parentLeasesMu sync.Mutex
	parentLeases   []*os.File
)

// Start launches a detached helper for a verified direct mTLS connection. It
// is best-effort by contract: the caller already has a working direct
// connection, and session reuse must never make that invocation fail.
func Start(key string, conn *grpcclient.AgentConnection) error {
	if runtime.GOOS == "windows" || !configDirOwnershipSafe() {
		return nil
	}
	spec, ok := specForConnection(key, conn)
	if !ok {
		return nil
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// The helper inherits the pipe's read end as fd 3 and treats its EOF as
	// "the parent invocation exited" — exact where PID polling is a heuristic
	// (PID reuse fakes liveness; a hard cap breaks builds longer than it).
	leaseR, leaseW, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "__session-broker", "--spec", base64.RawURLEncoding.EncodeToString(b), "--parent-lease")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.ExtraFiles = []*os.File{leaseR}
	cmd.SysProcAttr = detachedProcessAttributes()
	if err := cmd.Start(); err != nil {
		_ = leaseR.Close()
		_ = leaseW.Close()
		return err
	}
	_ = leaseR.Close()
	parentLeasesMu.Lock()
	parentLeases = append(parentLeases, leaseW)
	parentLeasesMu.Unlock()
	// Reap instead of Release: a helper that exits while this process still
	// runs (it lost the flock seconds from now, or expired first) would
	// otherwise sit as a zombie in the process table until this process exits.
	go func() { _ = cmd.Wait() }()
	return nil
}

func specForConnection(key string, conn *grpcclient.AgentConnection) (Spec, bool) {
	if conn == nil || !conn.IsMTLS || conn.IsSessionProxy || conn.CertInfo == nil || conn.Addr == "" || conn.Reconnect != nil || conn.RegistryDialer != nil {
		return Spec{}, false
	}
	id, ok := conn.ObservedServerIdentity()
	if !ok || id.OrgID != int32(conn.CertInfo.OrganizationID) || id.EntityType != "asset" || id.EntityID == "" {
		return Spec{}, false
	}
	spec := Spec{
		Key:             strings.TrimSpace(key),
		Host:            conn.Host,
		Addr:            conn.Addr,
		CertFingerprint: certificateFingerprint(*conn.CertInfo),
		Expected:        id,
		ParentPID:       os.Getpid(),
	}
	if err := spec.validate(); err != nil {
		return Spec{}, false
	}
	return spec, true
}

// InvalidateAll unpublishes every prepared session broker by removing their
// sockets and state files. Auth logout and device unpin call it: both are the
// user severing trust, and a broker retaining an authenticated device session
// for up to its lease+TTL after "trust removed" would quietly outlive that
// decision. A running broker notices its missing state file on its next tick
// and exits promptly (see serve), releasing the retained mTLS session. Lock
// files are deliberately left in place — see acquireIdentityLock.
func InvalidateAll() error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := brokerDir()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".sock") || strings.HasSuffix(name, ".json") {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Connect returns a healthy local proxy connection only when the broker is
// bound to the same pinned identity the caller currently expects. Every error
// is intended to fall through to the ordinary direct dial ladder.
func Connect(ctx context.Context, key string, expected certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
	if runtime.GOOS == "windows" || key == "" || expected.EntityType != "asset" || expected.EntityID == "" {
		return nil, ErrUnavailable
	}
	dir, err := brokerDir()
	if err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	if err := validatePrivateDir(dir); err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	socketPath, statePath := paths(dir, key, expected)
	if err := validatePrivateFile(socketPath, true); err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	if err := validatePrivateFile(statePath, false); err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	b, err := os.ReadFile(statePath)
	if err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	var state stateFile
	if err := json.Unmarshal(b, &state); err != nil || state.Spec.Key != key || state.Spec.Expected != expected {
		return nil, ErrUnavailable
	}
	// A dead socket is the common miss shape (a SIGKILLed helper leaves its
	// inode and state behind): a raw dial discovers it in microseconds, so it
	// comes before findCertificate's config read and per-certificate PEM
	// hashing. On refusal the miss is also entitled to sweep the leftovers —
	// under the identity lock, so a starting or serving broker's freshly bound
	// socket is never the thing swept — sparing every later invocation this
	// walk until a new broker publishes.
	probe, dialErr := net.DialTimeout("unix", socketPath, healthTimeout)
	if dialErr != nil {
		sweepStaleSession(socketPath, statePath)
		return nil, errors.Join(ErrUnavailable, dialErr)
	}
	_ = probe.Close()
	certInfo, err := findCertificate(state.Spec.CertFingerprint, state.Spec.Expected.OrgID)
	if err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	conn, err := grpcclient.ConnectSessionProxy(ctx, socketPath, state.Spec.Host, state.Spec.Addr, certInfo, state.Spec.Expected)
	if err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, healthTimeout)
	resp, err := conn.AgentService.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
	cancel()
	if err != nil {
		_ = conn.Close()
		return nil, errors.Join(ErrUnavailable, err)
	}
	conn.CacheAgentVersion(resp)
	return conn, nil
}

// Run decodes the hidden helper's non-secret connection recipe and serves it
// until it has had no active RPCs for idleTTL. parentLease is the inherited
// read end of the launching invocation's lease pipe (nil when launched
// without one); its EOF is the exact "parent exited" signal.
func Run(ctx context.Context, encoded string, idleTTL time.Duration, parentLease *os.File) error {
	b, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("decoding session broker spec: %w", err)
	}
	var spec Spec
	if err := json.Unmarshal(b, &spec); err != nil {
		return fmt.Errorf("decoding session broker spec: %w", err)
	}
	if err := spec.validate(); err != nil {
		return err
	}
	if !configDirOwnershipSafe() {
		return fmt.Errorf("session broker: config directory is not owned by the current user")
	}
	dir, err := brokerDir()
	if err != nil {
		return err
	}
	if err := ensurePrivateDir(dir); err != nil {
		return err
	}
	// The identity lock comes before ANY device or config work. Every losing
	// helper used to complete the full post-quantum handshake — multi-second
	// work on constrained boards, imposed at exactly the moment the device is
	// already busy — only to exit at the flock. The lock is local and
	// microsecond-cheap; take it first so a doomed helper costs nothing.
	socketPath, _ := paths(dir, spec.Key, spec.Expected)
	lock, locked, err := acquireIdentityLock(socketPath)
	if err != nil {
		return err
	}
	if !locked {
		return nil // another healthy or starting broker owns this identity
	}
	defer func() {
		releaseLock(lock)
		_ = lock.Close()
	}()
	certInfo, err := findCertificate(spec.CertFingerprint, spec.Expected.OrgID)
	if err != nil {
		return err
	}
	pins, err := devicepin.Open(filepath.Dir(dir))
	if err != nil {
		return err
	}
	// WithIdleTimeout(0) pins the retained transport open: gRPC's default
	// 30-minute client idle timeout would otherwise tear it down during a
	// long RPC-quiet stretch — exactly the 30+ minute local build the parent
	// lease and the proxy channel's own idle exemption keep this broker alive
	// for — and watchUpstreamState treats that departure from Ready as fatal
	// (correctly: a redial would re-verify against this process's startup pin
	// snapshot), stopping the broker under a command that already hit it.
	// The one verified transport must therefore simply never idle away;
	// genuine transport loss still surfaces as TransientFailure/Shutdown and
	// still evicts.
	upstream, err := grpcclient.ConnectWithTLSExpecting(ctx, spec.Addr, certInfo, pins, &spec.Expected, grpc.WithIdleTimeout(0))
	if err != nil {
		return err
	}
	defer upstream.Close()
	probeCtx, cancel := context.WithTimeout(ctx, upstreamProbeTimeout)
	_, err = upstream.AgentService.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
	cancel()
	if err != nil {
		return err
	}
	return serve(ctx, dir, spec, upstream.Conn, idleTTL, parentLeaseSignal(parentLease, spec.ParentPID))
}

// parentLeaseSignal reports whether the invocation that prepared this broker
// is still running. With a lease pipe the answer is exact and uncapped: the
// read end EOFs when the parent exits, no sooner (PID reuse cannot fake it)
// and no later (a 35-minute first build keeps its lease). Without one it
// falls back to PID polling under maxParentLease, since a recycled PID could
// otherwise renew an abandoned broker forever.
func parentLeaseSignal(lease *os.File, pid int) func() bool {
	if lease == nil {
		deadline := time.Now().Add(maxParentLease)
		return func() bool { return time.Now().Before(deadline) && processAlive(pid) }
	}
	var gone atomic.Bool
	go func() {
		buf := make([]byte, 1)
		for {
			// The parent never writes; reads only ever return EOF (parent
			// exited) or an error on a broken descriptor — either way the
			// lease is over.
			if _, err := lease.Read(buf); err != nil {
				gone.Store(true)
				return
			}
		}
	}()
	return func() bool { return !gone.Load() }
}

// sweepStaleSession removes a dead broker's leftover socket and state, but
// only while holding the identity flock: winning it proves no broker owns this
// identity, so whatever files exist are a crashed helper's remains. Losing it
// means a broker is starting or serving — its files are live, leave them.
func sweepStaleSession(socketPath, statePath string) {
	lock, locked, err := acquireIdentityLock(socketPath)
	if err != nil || !locked {
		return
	}
	defer func() {
		releaseLock(lock)
		_ = lock.Close()
	}()
	_ = os.Remove(socketPath)
	_ = os.Remove(statePath)
}

// acquireIdentityLock takes the non-blocking flock that makes one broker per
// device identity. It returns (nil, false, nil) when another process holds it.
// The lock file itself is deliberately never removed: unlinking a file whose
// inode another process may have just opened and flocked lets two brokers each
// hold "the" lock (one on the orphaned inode, one on its replacement), which
// is the split-brain the lock exists to prevent. An empty .lock per identity
// is the cheap price of that guarantee.
func acquireIdentityLock(socketPath string) (*os.File, bool, error) {
	lock, err := os.OpenFile(socketPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	locked, err := acquireLock(lock)
	if err != nil || !locked {
		_ = lock.Close()
		return nil, false, err
	}
	return lock, true, nil
}

func (s Spec) validate() error {
	if strings.TrimSpace(s.Key) == "" || s.Host == "" || s.Addr == "" || s.CertFingerprint == "" {
		return fmt.Errorf("invalid session broker spec")
	}
	if s.Expected.OrgID <= 0 || s.Expected.EntityType != "asset" || s.Expected.EntityID == "" {
		return fmt.Errorf("session broker requires an exact asset identity")
	}
	return nil
}

func brokerDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sessions"), nil
}

// configDirOwnershipSafe reports whether the config directory (and thus the
// session dir under it) belongs to this process's user. A root re-exec over a
// preserved user HOME — sudo invocations like the Thor flash elevation — must
// not plant root-owned files there: one root-run helper used to create a
// root-owned sessions dir that silently disabled the feature for the user
// forever after (every later helper died at ensurePrivateDir's Chmod with
// stderr wired to /dev/null, and the dir could not even be deleted without
// sudo). A missing directory is safe: whoever creates it owns it.
//
// The unsafe branch is deliberately untested: exercising it needs a file
// owned by another uid, which an unprivileged test cannot create.
func configDirOwnershipSafe() bool {
	dir, err := configDir()
	if err != nil {
		return false
	}
	return pathOwnedByCurrentUser(dir)
}

func paths(dir, key string, expected certs.WendyIdentity) (string, string) {
	sum := sha256.Sum256([]byte(key + "\x00" + expected.IdentityKey()))
	base := hex.EncodeToString(sum[:10])
	return filepath.Join(dir, base+".sock"), filepath.Join(dir, base+".json")
}

func certificateFingerprint(cert config.CertificateInfo) string {
	sum := sha256.Sum256([]byte(cert.PemCertificate))
	return hex.EncodeToString(sum[:])
}

func findCertificate(fingerprint string, expectedOrg int32) (*config.CertificateInfo, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	for i := range cfg.Auth {
		for j := range cfg.Auth[i].Certificates {
			cert := &cfg.Auth[i].Certificates[j]
			if int32(cert.OrganizationID) == expectedOrg && certificateFingerprint(*cert) == fingerprint {
				return cert, nil
			}
		}
	}
	return nil, fmt.Errorf("certificate used by session broker is no longer configured")
}

type rawMessage []byte
type rawCodec struct{}

func (rawCodec) Name() string { return "proto" }
func (rawCodec) Marshal(v any) ([]byte, error) {
	m, ok := v.(*rawMessage)
	if !ok {
		return nil, fmt.Errorf("session broker codec: want *rawMessage, got %T", v)
	}
	return *m, nil
}
func (rawCodec) Unmarshal(data []byte, v any) error {
	m, ok := v.(*rawMessage)
	if !ok {
		return fmt.Errorf("session broker codec: want *rawMessage, got %T", v)
	}
	*m = append((*m)[:0], data...)
	return nil
}

// maxUnansweredRPCs is how many consecutive proxied RPCs may end without the
// upstream ever answering before the broker concludes it is useless and exits.
// A broker that cannot answer within the budgets its clients use — a
// black-holed transport that still reports Ready, or a device answering
// slower than Connect's healthTimeout — makes every invocation pay a probe
// timeout on top of the direct dial it falls back to; exiting instead lets
// that direct dial prepare a healthy replacement. Three strikes rather than
// one so a single deadline expiry cannot evict a working broker.
const maxUnansweredRPCs = 3

type activity struct {
	activeRPCs  atomic.Int64
	connections atomic.Int64
	last        atomic.Int64
	failStreak  atomic.Int64
	upstreamBad chan struct{}
	badOnce     sync.Once
}

func newActivity() *activity {
	a := &activity{upstreamBad: make(chan struct{})}
	a.touch()
	return a
}
func (a *activity) touch() { a.last.Store(time.Now().UnixNano()) }

func (a *activity) markBad() { a.badOnce.Do(func() { close(a.upstreamBad) }) }

// rpcAnswered / rpcUnanswered feed the eviction streak: an RPC counts as
// answered when anything at all came back from the upstream — a message, a
// clean end-of-stream, or an application-level error status — because any of
// those proves the retained transport still reaches the device. Deadline
// expiries prove nothing (they are generated client-side) and a run of them
// is exactly how a black-holed transport looks. Downstream cancellations
// without a deadline do not feed the streak: a caller may intentionally
// cancel many parallel uploads when it falls back to another deployment path.
func (a *activity) rpcAnswered() { a.failStreak.Store(0) }

func (a *activity) rpcUnanswered() {
	if a.failStreak.Add(1) >= maxUnansweredRPCs {
		a.markBad()
	}
}

func (a *activity) noteUpstreamError(upstream *grpc.ClientConn, err error) {
	if err == nil || upstream.GetState() == connectivity.Ready {
		return
	}
	a.markBad()
}

// rpcCompression carries the grpc-encoding an inbound RPC arrived with from
// the stats layer — the only place grpc-go exposes it — to the proxy handler,
// which mirrors it onto the upstream call. Without that mirror the proxy
// silently decompresses: chunk pushes open WriteChunks with gzip specifically
// because raw chunk payloads have stalled USB-NCM links (see chunkpush.go),
// and a broker in the middle must keep that property on the real device link.
//
// TagRPC attaches the holder and HandleRPC's InHeader fills it on the same
// goroutine that then invokes the handler (grpc-go's server does both before
// dispatch), so the plain field needs no synchronization.
type rpcCompression struct{ name string }

type rpcCompressionKey struct{}

func requestCompression(ctx context.Context) string {
	if rc, ok := ctx.Value(rpcCompressionKey{}).(*rpcCompression); ok {
		return rc.name
	}
	return ""
}

func proxyHandler(upstream *grpc.ClientConn, activity *activity) grpc.StreamHandler {
	return func(_ any, downstream grpc.ServerStream) error {
		method, ok := grpc.MethodFromServerStream(downstream)
		if !ok {
			return fmt.Errorf("session broker: missing gRPC method")
		}
		activity.activeRPCs.Add(1)
		activity.touch()
		answered := false
		defer func() {
			// A client deadline may reach us as RST_CANCEL before our own
			// deadline timer fires, so preserve failures for deadline-bearing
			// requests. Only cancellation without a deadline proves the caller
			// abandoned the operation independently of an unanswered timeout.
			_, hasDeadline := downstream.Context().Deadline()
			if answered {
				activity.rpcAnswered()
			} else if hasDeadline || !errors.Is(downstream.Context().Err(), context.Canceled) {
				activity.rpcUnanswered()
			}
			activity.activeRPCs.Add(-1)
			activity.touch()
		}()

		ctx := downstream.Context()
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			ctx = metadata.NewOutgoingContext(ctx, md.Copy())
		}
		callOpts := []grpc.CallOption{grpc.ForceCodec(rawCodec{})}
		if comp := requestCompression(ctx); comp != "" && comp != "identity" {
			callOpts = append(callOpts, grpc.UseCompressor(comp))
		}
		upstreamStream, err := upstream.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, method, callOpts...)
		if err != nil {
			activity.noteUpstreamError(upstream, err)
			return err
		}

		requestDone := make(chan error, 1)
		go func() {
			for {
				var msg rawMessage
				err := downstream.RecvMsg(&msg)
				if errors.Is(err, io.EOF) {
					closeErr := upstreamStream.CloseSend()
					activity.noteUpstreamError(upstream, closeErr)
					requestDone <- closeErr
					return
				}
				if err != nil {
					requestDone <- err
					return
				}
				if err := upstreamStream.SendMsg(&msg); err != nil {
					// io.EOF here is gRPC's "the stream already completed"
					// signal: the real terminal status — possibly a success
					// response — is only available from the receive side.
					// Returning it as the RPC's error would clobber that
					// status with codes.Unknown "EOF", so stop sending and
					// let the response loop deliver the upstream's verdict.
					if errors.Is(err, io.EOF) {
						requestDone <- nil
						return
					}
					activity.noteUpstreamError(upstream, err)
					requestDone <- err
					return
				}
				activity.touch()
			}
		}()

		headersSent := false
		for {
			var msg rawMessage
			err := upstreamStream.RecvMsg(&msg)
			if !headersSent {
				if header, headerErr := upstreamStream.Header(); headerErr == nil {
					_ = downstream.SetHeader(header)
				}
				headersSent = true
			}
			if errors.Is(err, io.EOF) {
				answered = true
				downstream.SetTrailer(upstreamStream.Trailer())
				return nil
			}
			if err != nil {
				// An application-level status is still an answer — it proves
				// the retained transport reaches the device. Only the errors a
				// dead link produces (client-side cancellation or deadline,
				// transport unavailability) leave the RPC unanswered.
				switch status.Code(err) {
				case codes.Canceled, codes.DeadlineExceeded, codes.Unavailable:
				default:
					answered = true
				}
				downstream.SetTrailer(upstreamStream.Trailer())
				activity.noteUpstreamError(upstream, err)
				return err
			}
			answered = true
			if err := downstream.SendMsg(&msg); err != nil {
				return err
			}
			activity.touch()
			select {
			case requestErr := <-requestDone:
				if requestErr != nil {
					return requestErr
				}
			default:
			}
		}
	}
}

// watchUpstreamState ends the broker the moment the retained transport leaves
// Ready. Any departure is treated as fatal, which is why Run dials the
// upstream with WithIdleTimeout(0): without that exemption, gRPC's 30-minute
// client idle teardown during an RPC-quiet stretch (a long local build) would
// trip this watcher and stop the broker under a command that already hit it.
// The broker's whole value is the ALREADY-VERIFIED transport it
// retains; anything that replaces it would be re-dialed and re-verified
// against the pin snapshot this process loaded at startup, and
// devicepin.Store is not multi-process safe: another invocation may have
// re-pinned the device since (a legitimate re-enrollment), and a redial
// completed here would both trust the stale key and clobber the newer pin
// file from the stale in-memory map on its next flush. Exiting instead means
// a re-dial simply never happens under this broker's identity lock: the next
// invocation direct-dials with fresh pin state and prepares a fresh broker.
//
// The transport may legitimately still be establishing when serve() starts
// (tests hand serve() an un-dialed conn; Run() hands it a probed, Ready one),
// so departure only counts after Ready has been observed once.
func watchUpstreamState(ctx context.Context, upstream *grpc.ClientConn, activity *activity) {
	state := upstream.GetState()
	for state != connectivity.Ready {
		if state == connectivity.Shutdown {
			activity.markBad()
			return
		}
		if !upstream.WaitForStateChange(ctx, state) {
			return
		}
		state = upstream.GetState()
	}
	for {
		if !upstream.WaitForStateChange(ctx, connectivity.Ready) {
			return
		}
		if upstream.GetState() != connectivity.Ready {
			activity.markBad()
			return
		}
	}
}

// connectionActivity keeps a broker alive while a CLI invocation still owns
// its local gRPC channel, even if that invocation spends several minutes in a
// local image build between RPCs. RPC activity alone cannot distinguish that
// useful quiet period from an abandoned broker.
type connectionActivity struct{ activity *activity }

func (h connectionActivity) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return context.WithValue(ctx, rpcCompressionKey{}, &rpcCompression{})
}
func (h connectionActivity) HandleRPC(ctx context.Context, s stats.RPCStats) {
	h.activity.touch()
	if ih, ok := s.(*stats.InHeader); ok {
		if rc, ok := ctx.Value(rpcCompressionKey{}).(*rpcCompression); ok {
			rc.name = ih.Compression
		}
	}
}
func (h connectionActivity) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}
func (h connectionActivity) HandleConn(_ context.Context, event stats.ConnStats) {
	switch event.(type) {
	case *stats.ConnBegin:
		h.activity.connections.Add(1)
		h.activity.touch()
	case *stats.ConnEnd:
		h.activity.connections.Add(-1)
		h.activity.touch()
	}
}

// serve publishes the retained transport on the identity's Unix socket. The
// caller must hold the identity flock (see acquireIdentityLock in Run) for the
// whole call: holding it is what entitles serve to clobber and later remove
// the socket and state files.
func serve(ctx context.Context, dir string, spec Spec, upstream *grpc.ClientConn, idleTTL time.Duration, parentAlive func() bool) (err error) {
	if idleTTL <= 0 {
		idleTTL = DefaultIdleTTL
	}
	if parentAlive == nil {
		parentAlive = parentLeaseSignal(nil, spec.ParentPID)
	}
	socketPath, statePath := paths(dir, spec.Key, spec.Expected)

	_ = os.Remove(socketPath)
	_ = os.Remove(statePath)
	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer lis.Close()
	defer os.Remove(socketPath)
	defer os.Remove(statePath)
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return err
	}
	state, err := json.Marshal(stateFile{Spec: spec})
	if err != nil {
		return err
	}
	if err := writePrivateFile(statePath, state); err != nil {
		return err
	}

	activity := newActivity()
	server := grpc.NewServer(
		grpc.ForceServerCodec(rawCodec{}),
		grpc.UnknownServiceHandler(proxyHandler(upstream, activity)),
		grpc.StatsHandler(connectionActivity{activity: activity}),
	)
	serveCtx, cancelServe := context.WithCancel(ctx)
	defer cancelServe()
	go watchUpstreamState(serveCtx, upstream, activity)
	done := make(chan struct{})
	go func() {
		defer close(done)
		interval := min(idleTTL/4, 5*time.Second)
		if interval < time.Millisecond {
			interval = time.Millisecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-serveCtx.Done():
				server.Stop()
				return
			case <-activity.upstreamBad:
				// A broker whose retained transport has failed — an RPC-observed
				// drop, a departure from Ready seen by watchUpstreamState, or a
				// streak of unanswered RPCs over a transport that still claims
				// Ready — must release its identity lock promptly so the
				// invocation's ordinary direct-dial fallback can prepare a healthy
				// replacement. Stop rather than waiting gracefully: a broken
				// bidirectional stream can otherwise keep its handler blocked
				// while GracefulStop waits for that same handler, stranding the
				// lock we are trying to release.
				server.Stop()
				return
			case <-ticker.C:
				// A missing state file means this broker was unpublished from
				// outside — auth logout or device unpin severing trust — and the
				// retained authenticated session must end NOW, before any lease
				// or idle grace. Stop, not GracefulStop: an open channel would
				// otherwise keep the session alive past the user's decision.
				if _, statErr := os.Lstat(statePath); statErr != nil {
					server.Stop()
					return
				}
				// The invocation that prepared this broker does not route its
				// already-open direct connection back through us. Treat that
				// process as a lease so a long first build cannot consume the
				// entire idle TTL before there is a second invocation to reuse it.
				if parentAlive() {
					activity.touch()
					continue
				}
				last := time.Unix(0, activity.last.Load())
				if activity.connections.Load() == 0 && activity.activeRPCs.Load() == 0 && time.Since(last) >= idleTTL {
					drainThenStop(serveCtx, server, activity, socketPath, statePath)
					return
				}
			}
		}
	}()
	err = server.Serve(lis)
	cancelServe()
	<-done
	if errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}

// drainThenStop is the broker's idle exit. The idle check and GracefulStop
// used to race: a client could dial the socket between them, probe
// successfully, and then lose the broker under its still-running command —
// and with fallback existing only at probe time, that failed the invocation
// outright. Unpublishing the socket first guarantees no further client can
// arrive; any straggler that made it in is then served until it releases its
// channel (which, with the proxy channel pinned non-idle client-side, is when
// its invocation finishes).
func drainThenStop(ctx context.Context, server *grpc.Server, activity *activity, socketPath, statePath string) {
	_ = os.Remove(socketPath)
	_ = os.Remove(statePath)
	for activity.connections.Load() > 0 || activity.activeRPCs.Load() > 0 {
		select {
		case <-ctx.Done():
			server.Stop()
			return
		case <-activity.upstreamBad:
			server.Stop()
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
	server.GracefulStop()
}

func writePrivateFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".session-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	return validatePrivateDir(dir)
}

func validatePrivateDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("session directory is not private")
	}
	return validateOwner(info)
}

func validatePrivateFile(path string, socket bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	isExpectedType := info.Mode().IsRegular()
	if socket {
		isExpectedType = info.Mode()&os.ModeSocket != 0
	}
	if !isExpectedType || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("unsafe session broker file %s", path)
	}
	return validateOwner(info)
}
