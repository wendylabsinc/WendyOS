package services

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/localsocket"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	sharedenv "github.com/wendylabsinc/wendy/go/internal/shared/env"
	"go.uber.org/zap"
)

const (
	DataSocketFilename    = "data.sock"
	dataProtocolMaxRecord = 64 << 10
	dataSocketGroupGID    = 2000
	// dataRatePerSecond bounds records accepted per app across all of the
	// app's connections combined (see appDataSocket.limiter).
	dataRatePerSecond = 200
	// dataMaxLiveConnectionsPerApp bounds how many connections one app may hold
	// open on its data socket at once.
	//
	// The record rate limiter above bounds records, not connections, so it does
	// nothing about a reconnect loop: an app that dials, fails to write, and
	// dials again costs one goroutine and one file descriptor per attempt, none
	// of which the rate limiter sees. Sixteen is well above what a legitimate
	// app needs (one connection per service of the app, and every service of an
	// app shares this one socket), and low enough that a loop is stopped long
	// before the agent's descriptor budget is.
	//
	// Refusing is safe for a correct app: the refusal arrives as a normal
	// rejected ack, so the client learns why instead of seeing a bare close.
	dataMaxLiveConnectionsPerApp = 16
	// dataRefusalLogInterval throttles the connection-cap warning. A reconnect
	// loop is exactly the situation where one line per refusal would flood the
	// journal with the same fact.
	dataRefusalLogInterval = time.Minute
	// dataRefusalWriteTimeout bounds how long the accept loop will spend
	// telling one peer it was refused.
	dataRefusalWriteTimeout = 2 * time.Second
)

// peerCredentials identifies the process on the far end of a data-socket
// connection, read from SO_PEERCRED at accept time.
type peerCredentials struct {
	UID uint32
	PID int32
}

// errPeerCredUnavailable marks the cases where the connection structurally
// carries no peer identity to read: it is not a unix socket, or the platform
// has no SO_PEERCRED. Those, and only those, are the fail-open cases in
// verifyPeer. A lookup that fails for any other reason on a real unix socket
// (SyscallConn or getsockopt returning an error) is an unexplained failure of
// the identity check, not an absence of one, and must fail closed like every
// other unattributable peer.
var errPeerCredUnavailable = errors.New("peer credentials are unavailable on this connection")

var AppDataSocketRootPath = "/var/lib/wendy/app-data"

type appDataSocket struct {
	appID    string
	listener net.Listener
	owners   map[string]struct{}
	// limiter is shared by every connection the app opens, so the rate limit
	// is enforced per app rather than per connection.
	limiter *notificationRateLimiter
	// live counts the connections currently being served for this app and
	// lastRefusalLog is when the connection cap last logged. Both are guarded
	// by AppDataSocketManager.mu.
	live           int
	lastRefusalLog time.Time
}
type AppDataSocketManager struct {
	ctx     context.Context
	logger  *zap.Logger
	capture *data.Manager
	mu      sync.Mutex
	sockets map[string]*appDataSocket
	// newLimiter builds the per-app record rate limiter. It is a field so
	// tests can install a deterministic bucket; production uses a 200 rec/s
	// token bucket.
	newLimiter func() *notificationRateLimiter
	// peerCred and cgroupOfPID are seams over the SO_PEERCRED lookup and the
	// /proc cgroup read so peer-credential verification is testable without
	// real unix-socket credentials or a live cgroup hierarchy.
	peerCred    func(net.Conn) (peerCredentials, error)
	cgroupOfPID func(pid int32) (string, error)
}

func NewAppDataSocketManager(ctx context.Context, logger *zap.Logger, capture *data.Manager) *AppDataSocketManager {
	m := &AppDataSocketManager{
		ctx:     ctx,
		logger:  logger,
		capture: capture,
		sockets: map[string]*appDataSocket{},
		newLimiter: func() *notificationRateLimiter {
			l := newNotificationRateLimiter(dataRatePerSecond, time.Second/dataRatePerSecond)
			return &l
		},
		peerCred:    readPeerCredentials,
		cgroupOfPID: readProcCgroup,
	}
	go func() { <-ctx.Done(); m.stopAll() }()
	return m
}

func (m *AppDataSocketManager) Ensure(appID, service string) (string, error) {
	if err := appconfig.ValidateAppID(appID); err != nil {
		return "", err
	}
	if service != "" {
		if err := appconfig.ValidateServiceName(service); err != nil {
			return "", err
		}
	}
	key := appDataKey(appID)
	dir := filepath.Join(AppDataSocketRootPath, key)
	owner := systemAPIOwner(service)
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sockets[key]; s != nil {
		if s.appID != appID {
			return "", errors.New("app data identity collision")
		}
		s.owners[owner] = struct{}{}
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	// MkdirAll applies the process umask to the requested mode, so the
	// directory that guards an app's private socket would otherwise be as tight
	// as the agent's umask happened to be. State the mode instead of inheriting
	// it, exactly as the sibling System API socket manager does.
	if err := os.Chmod(dir, 0o750); err != nil {
		return "", err
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(dir, 0, dataSocketGroupGID); err != nil {
			return "", err
		}
	}
	id, _ := json.Marshal(map[string]string{"app_id": appID})
	if err := syncWriteFile(filepath.Join(dir, "identity.json"), id, 0o600); err != nil {
		return "", err
	}
	l, err := localsocket.Listen(filepath.Join(dir, DataSocketFilename))
	if err != nil {
		return "", err
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(filepath.Join(dir, DataSocketFilename), 0, dataSocketGroupGID); err != nil {
			l.Close()
			return "", err
		}
	}
	s := &appDataSocket{appID: appID, listener: l, owners: map[string]struct{}{owner: {}}, limiter: m.newLimiter()}
	m.sockets[key] = s
	go m.serve(s)
	return dir, nil
}

func (m *AppDataSocketManager) Release(appID, service string) {
	key := appDataKey(appID)
	m.mu.Lock()
	s := m.sockets[key]
	if s == nil || s.appID != appID {
		m.mu.Unlock()
		return
	}
	delete(s.owners, systemAPIOwner(service))
	if len(s.owners) > 0 {
		m.mu.Unlock()
		return
	}
	delete(m.sockets, key)
	m.mu.Unlock()
	s.listener.Close()
	m.removeRootUnlessRecreated(key)
}
func (m *AppDataSocketManager) ReleaseApp(appID string) {
	key := appDataKey(appID)
	m.mu.Lock()
	s := m.sockets[key]
	delete(m.sockets, key)
	m.mu.Unlock()
	if s != nil {
		s.listener.Close()
		m.removeRootUnlessRecreated(key)
	}
}

// removeRootUnlessRecreated deletes an app's socket directory, unless a
// concurrent redeploy has already recreated the entry for the same app between
// the listener being closed and this call. Removing it then would delete a
// live socket out from under the new listener, which is why the check is taken
// under the lock and the removal happens with the lock still held. The sibling
// System API socket manager guards its own removal the same way.
func (m *AppDataSocketManager) removeRootUnlessRecreated(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sockets[key] != nil {
		return
	}
	if err := os.RemoveAll(filepath.Join(AppDataSocketRootPath, key)); err != nil && m.logger != nil {
		m.logger.Warn("cannot remove unused app data socket directory", zap.Error(err))
	}
}

// SweepOrphanedRoots removes socket directories under AppDataSocketRootPath
// that belong to none of the given app identities.
//
// It exists because a directory can outlive every owner that could release it:
// deleting the only service of an app by its service name removes the
// container, and with it any later chance to learn the app identity whose hash
// named the directory. The sweep runs at restore, where the set of apps that
// still have containers is known exactly, and a directory whose name matches
// no live app's hash therefore belongs to nothing.
//
// Directories currently served are never swept even if the caller omits their
// app, because an in-memory socket entry is proof of a live owner.
func (m *AppDataSocketManager) SweepOrphanedRoots(activeAppIDs []string) {
	live := make(map[string]struct{}, len(activeAppIDs))
	for _, appID := range activeAppIDs {
		live[appDataKey(appID)] = struct{}{}
	}
	entries, err := os.ReadDir(AppDataSocketRootPath)
	if err != nil {
		if !os.IsNotExist(err) && m.logger != nil {
			m.logger.Warn("cannot sweep app data socket directories", zap.Error(err))
		}
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		key := entry.Name()
		if _, ok := live[key]; ok {
			continue
		}
		if m.sockets[key] != nil {
			continue
		}
		if err := os.RemoveAll(filepath.Join(AppDataSocketRootPath, key)); err != nil {
			if m.logger != nil {
				m.logger.Warn("cannot remove orphaned app data socket directory", zap.String("directory", key), zap.Error(err))
			}
			continue
		}
		if m.logger != nil {
			m.logger.Info("removed orphaned app data socket directory", zap.String("directory", key))
		}
	}
}
func (m *AppDataSocketManager) stopAll() {
	m.mu.Lock()
	var all []*appDataSocket
	for k, s := range m.sockets {
		all = append(all, s)
		delete(m.sockets, k)
	}
	m.mu.Unlock()
	for _, s := range all {
		s.listener.Close()
	}
}
func appDataKey(id string) string { h := sha256.Sum256([]byte(id)); return hex.EncodeToString(h[:16]) }

func (m *AppDataSocketManager) serve(s *appDataSocket) {
	for {
		c, err := s.listener.Accept()
		if err != nil {
			if m.ctx.Err() == nil && m.logger != nil {
				m.logger.Warn("app data socket accept failed", zap.Error(err))
			}
			return
		}
		if !m.admit(s) {
			// The refusal is written from the accept loop, so it must not be
			// able to block it: a peer that connects and never reads would
			// otherwise stall every later accept, which is the same denial the
			// cap exists to prevent.
			_ = c.SetWriteDeadline(time.Now().Add(dataRefusalWriteTimeout))
			_ = writeDataFrame(c, dataAck{Version: 1, State: "rejected", Error: "too many open connections for this app"})
			_ = c.Close()
			continue
		}
		go func() {
			defer m.finish(s)
			m.serveConn(s, c)
		}()
	}
}

// admit reserves one of the app's live-connection slots, or reports that the
// app is already at dataMaxLiveConnectionsPerApp. The over-cap warning is
// logged at most once per dataRefusalLogInterval per app.
func (m *AppDataSocketManager) admit(s *appDataSocket) bool {
	now := time.Now()
	m.mu.Lock()
	if s.live >= dataMaxLiveConnectionsPerApp {
		warn := s.lastRefusalLog.IsZero() || now.Sub(s.lastRefusalLog) >= dataRefusalLogInterval
		if warn {
			s.lastRefusalLog = now
		}
		live := s.live
		appID := s.appID
		m.mu.Unlock()
		if warn && m.logger != nil {
			m.logger.Warn("app data socket refused connection over the per-app limit",
				zap.String("app_id", appID),
				zap.Int("live_connections", live),
				zap.Int("limit", dataMaxLiveConnectionsPerApp))
		}
		return false
	}
	s.live++
	m.mu.Unlock()
	return true
}

// finish returns the slot admit reserved.
func (m *AppDataSocketManager) finish(s *appDataSocket) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.live > 0 {
		s.live--
	}
}

// verifyPeer binds a connection to the app the socket belongs to using the
// peer's kernel-reported credentials.
//
// Identity achieved: the accepted connection's peer PID is read via
// SO_PEERCRED, and that PID's cgroup is compared against the app the socket was
// created for. Every app container runs in a systemd scope named by client.go
// as "<systemd-service>-<appID>[@<service>].scope", so a process that belongs
// to a different app is refused: it cannot write records attributed to an app
// it is not.
//
// Fail-open is confined to the case where the connection structurally carries
// no peer to read: not a unix socket, an unsupported platform, or the seam
// being disabled (see errPeerCredUnavailable). In those cases the group-2000
// gate and the 0750 app-private socket directory remain the baseline. A
// SO_PEERCRED lookup that fails for any other reason fails closed.
//
// Once a peer pid IS known, verification fails CLOSED: any inability to
// positively attribute that pid to this app (its /proc cgroup is unreadable,
// carries no wendy app scope, or names a different app) is a refusal. This is
// deliberate. The SO_PEERCRED pid is captured by the kernel at connect, but the
// cgroup is read from /proc by that pid afterwards, so a peer that exits between
// accept and the read (or whose pid is recycled onto a non-app process) would,
// under a fail-open policy, have records it buffered before exiting attributed
// to this app. Refusing an unattributable-but-credentialed peer closes that
// forgery path; no legitimate caller connects to an app data socket except that
// app's own containers, whose cgroup always resolves.
//
// Residual gaps:
//   - Granularity is per app, not per service: an app's services legitimately
//     share one socket, so the service suffix is stripped before comparison.
//   - The peer UID is read but not used as the discriminator, because app
//     containers run as UID 0 by default; the cgroup is the identity.
//   - A pid recycled within the accept-to-read window onto a process in the
//     SAME app's scope still verifies. Closing that last window requires a
//     kernel primitive that pins the peer identity across the read
//     (SO_PEERPIDFD, Linux 6.5+); it is left as a follow-up.
func (m *AppDataSocketManager) verifyPeer(appID string, c net.Conn) error {
	if m.peerCred == nil {
		return nil
	}
	creds, err := m.peerCred(c)
	if err != nil {
		if errors.Is(err, errPeerCredUnavailable) {
			// The connection carries no peer identity at all (not a unix
			// socket, or an unsupported platform). Fall back to the
			// group/directory gate.
			return nil
		}
		// SO_PEERCRED was available in principle and failed anyway. Refusing
		// keeps the identity check from being silently skipped by whatever
		// made it fail.
		return fmt.Errorf("reading peer credentials failed; cannot attribute the connection to app %q: %w", appID, err)
	}
	if m.cgroupOfPID == nil {
		return nil
	}
	// From here the peer is a real local process with a kernel-attested pid.
	// Anything short of a positive match to this app is a refusal (fail closed).
	cgroup, err := m.cgroupOfPID(creds.PID)
	if err != nil || cgroup == "" {
		return fmt.Errorf("peer (pid %d, uid %d) cgroup is unreadable; cannot attribute to app %q", creds.PID, creds.UID, appID)
	}
	peerApp, ok := appIDFromCgroup(cgroup)
	if !ok {
		return fmt.Errorf("peer (pid %d, uid %d) is not in a wendy app scope; cannot attribute to app %q", creds.PID, creds.UID, appID)
	}
	if peerApp != appID {
		return fmt.Errorf("peer (pid %d, uid %d) belongs to app %q, not %q", creds.PID, creds.UID, peerApp, appID)
	}
	return nil
}

// appIDFromCgroup extracts the wendy app identity from a /proc/<pid>/cgroup
// body. Container scopes are named "<systemd-service>-<appID>[@<service>].scope"
// (see client.go's CgroupsPath assignment). The service suffix is stripped so
// the result is the app identity the socket is keyed by. It returns false when
// no such scope component is present, which is the signal that the peer is not
// a wendy-managed app container.
func appIDFromCgroup(cgroup string) (string, bool) {
	svc := sharedenv.SystemdServiceName()
	prefix := svc + "-"
	for _, line := range strings.Split(cgroup, "\n") {
		for _, segment := range strings.Split(line, "/") {
			segment = strings.TrimSuffix(segment, ".scope")
			id := ""
			switch {
			case strings.HasPrefix(segment, prefix):
				// systemd cgroup driver: runc translated the
				// "system.slice:{svc}:{suffix}" CgroupsPath into a
				// "{svc}-{suffix}.scope" unit.
				id = strings.TrimPrefix(segment, prefix)
			case strings.HasPrefix(segment, "system.slice:"+svc+":"):
				// cgroupfs cgroup driver: the same CgroupsPath is taken as a
				// literal directory name, colons and all, so the whole
				// "system.slice:{svc}:{suffix}" string is one path segment.
				id = strings.TrimPrefix(segment, "system.slice:"+svc+":")
			default:
				continue
			}
			if at := strings.IndexByte(id, '@'); at >= 0 {
				id = id[:at]
			}
			if id != "" {
				return id, true
			}
		}
	}
	return "", false
}

type dataAck struct {
	Version int    `json:"version"`
	State   string `json:"state"`
	Error   string `json:"error,omitempty"`
}

func (m *AppDataSocketManager) serveConn(s *appDataSocket, c net.Conn) {
	defer c.Close()
	appID := s.appID
	if err := m.verifyPeer(appID, c); err != nil {
		if m.logger != nil {
			m.logger.Warn("app data socket refused connection with mismatched peer", zap.String("app_id", appID), zap.Error(err))
		}
		_ = writeDataFrame(c, dataAck{Version: 1, State: "rejected", Error: "peer credentials do not match this app"})
		return
	}
	r := bufio.NewReader(c)
	for {
		// The token bucket lives on the socket, so every connection the app
		// opens draws from one per-app budget.
		if !s.limiter.allow(time.Now()) {
			_ = writeDataFrame(c, dataAck{Version: 1, State: "rejected", Error: "rate limit exceeded"})
			return
		}
		payload, err := readDataFrame(r)
		if err != nil {
			return
		}
		var rec data.ApplicationRecord
		if err = json.Unmarshal(payload, &rec); err != nil {
			_ = writeDataFrame(c, dataAck{Version: 1, State: "rejected", Error: "invalid JSON"})
			continue
		}
		if err = validateApplicationRecord(rec); err != nil {
			_ = writeDataFrame(c, dataAck{Version: 1, State: "rejected", Error: err.Error()})
			continue
		}
		state, err := m.capture.RecordApplication(appID, rec)
		ack := dataAck{Version: 1, State: state}
		if err != nil {
			ack.State = "rejected"
			ack.Error = err.Error()
		}
		if writeDataFrame(c, ack) != nil {
			return
		}
	}
}

func readDataFrame(r io.Reader) ([]byte, error) {
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return nil, err
	}
	if n == 0 || n > dataProtocolMaxRecord {
		return nil, errors.New("record exceeds 64 KiB")
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return b, err
}
func writeDataFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > dataProtocolMaxRecord {
		return errors.New("response too large")
	}
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(b)))
	if _, err = w.Write(h[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// recordKindValidator checks the kind-specific required fields of a record.
type recordKindValidator func(data.ApplicationRecord) error

// recordKind describes one accepted record kind: how to validate its own
// fields, and whether it may bind itself to the harness samples it was computed
// from. Sample references are a property of the kind, not of the protocol, so a
// kind that cannot have inputs rejects them instead of silently storing them.
type recordKind struct {
	validate     recordKindValidator
	allowsInputs bool
}

// applicationRecordKinds is the registry of accepted record kinds. Adding a
// new kind is a one-entry addition here; an unknown kind is rejected with a
// clean ack rather than killing the connection.
var applicationRecordKinds = map[string]recordKind{
	"event": {validate: func(r data.ApplicationRecord) error {
		if r.Name == "" {
			return errors.New("event name is required")
		}
		return nil
	}},
	"prediction": {
		validate: func(r data.ApplicationRecord) error {
			if r.Model == "" {
				return errors.New("prediction model is required")
			}
			return nil
		},
		// A prediction is the outcome the harness exists to correlate: it may
		// name the sample or samples the model consumed.
		allowsInputs: true,
	},
}

func validateApplicationRecord(r data.ApplicationRecord) error {
	if r.Version != 1 {
		return fmt.Errorf("unsupported protocol version %d", r.Version)
	}
	kind, ok := applicationRecordKinds[r.Type]
	if !ok {
		return fmt.Errorf("unknown record kind %q", r.Type)
	}
	if err := kind.validate(r); err != nil {
		return err
	}
	if len(r.Inputs) > 0 {
		if !kind.allowsInputs {
			return fmt.Errorf("record kind %q cannot reference input samples", r.Type)
		}
		if err := data.ValidateSampleRefs(r.Inputs); err != nil {
			return err
		}
	}
	if len(r.Attributes) > 128 {
		return errors.New("too many attributes")
	}
	for k := range r.Attributes {
		if k == "" || len(k) > 128 {
			return errors.New("invalid attribute key")
		}
	}
	return nil
}
