package commands

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// cloudAppTunnelHost is the host an attached run uses for an app it reaches
// through a cloud app tunnel: postStart hooks expand WENDY_HOSTNAME to it and
// the announced URL names it. The tunnel listens on 127.0.0.1 only;
// loopbackPortInUse keeps "localhost" from also meaning another listener.
const cloudAppTunnelHost = "localhost"

// cloudAppTunnel is a loopback-only TCP forward from this machine to one port
// on a cloud device. An attached `wendy run` opens it when the device has no
// route from here (no active Wendy Mesh, no reachable LAN address), so the
// readiness probe, the announced URL and the postStart hooks still reach the
// app. It relays through the connection's cloud port dialer
// (AgentConnection.RegistryDialer) with serveTCPForward, the loop
// `wendy cloud tunnel` uses, and closes when the context it was started with
// ends: the attached run, or the watch session.
type cloudAppTunnel struct {
	LocalPort  int // the listening port on 127.0.0.1
	RemotePort int // the app's port on the device
	stop       context.CancelFunc
	done       chan struct{}
	// relay opens one Wendy Cloud relay to RemotePort, the dial every
	// forwarded connection makes; relayFailed reports a failed one. The
	// readiness probe opens its relays with them directly (probeRelay).
	relay       func(context.Context) (net.Conn, error)
	relayFailed func(error)
	// probing is held while a readiness probe's relay is open, so the
	// readiness waits on this tunnel never hold more than one at a time.
	probing sync.Mutex
}

// Close stops accepting connections, ends the forwarded ones, and waits for
// the accept loop to exit. It is safe on a tunnel whose context already ended.
func (t *cloudAppTunnel) Close() {
	if t == nil {
		return
	}
	if t.stop != nil {
		t.stop()
	}
	if t.done != nil {
		<-t.done
	}
}

// open reports whether the tunnel still accepts connections.
func (t *cloudAppTunnel) open() bool {
	if t.done == nil {
		return true
	}
	select {
	case <-t.done:
		return false
	default:
		return true
	}
}

// localURL points raw — a hook URL expanded with cloudAppTunnelHost — at the
// tunnel's local port. ok is false when raw does not address the device port
// the tunnel forwards; it is then returned unchanged.
func (t *cloudAppTunnel) localURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != cloudAppTunnelHost || urlPort(u) != t.RemotePort {
		return raw, false
	}
	if t.LocalPort == t.RemotePort {
		return raw, true
	}
	u.Host = net.JoinHostPort(cloudAppTunnelHost, strconv.Itoa(t.LocalPort))
	return u.String(), true
}

// rootURL is the tunnel's local end as a URL: https when the app's URL is,
// http otherwise.
func (t *cloudAppTunnel) rootURL(scheme string) string {
	if scheme != "https" {
		scheme = "http"
	}
	return scheme + "://" + net.JoinHostPort(cloudAppTunnelHost, strconv.Itoa(t.LocalPort))
}

// startCloudAppTunnel opens a cloudAppTunnel to remotePort on conn's cloud
// device, open until ctx ends or Close. Indirected so tests can inject one.
var startCloudAppTunnel = openCloudAppTunnel

func openCloudAppTunnel(ctx context.Context, conn *grpcclient.AgentConnection, remotePort int) (*cloudAppTunnel, error) {
	if conn == nil || conn.RegistryDialer == nil {
		return nil, errors.New("this connection cannot forward ports through Wendy Cloud")
	}
	if remotePort <= 0 || remotePort > 65535 {
		return nil, fmt.Errorf("invalid port %d", remotePort)
	}
	ln, err := listenCloudAppTunnel(remotePort)
	if err != nil {
		return nil, err
	}
	dial, device := conn.RegistryDialer, conn.Host
	tunnelCtx, cancel := context.WithCancel(ctx)
	t := &cloudAppTunnel{
		LocalPort:  ln.Addr().(*net.TCPAddr).Port,
		RemotePort: remotePort,
		stop:       cancel,
		done:       make(chan struct{}),
		relay: func(ctx context.Context) (net.Conn, error) {
			return dial(ctx, remotePort)
		},
	}
	// The readiness probe retries its relay while the app boots, so report a
	// failing relay once per distinct error, not per try.
	var mu sync.Mutex
	var lastErr string
	dialFailed := func(err error) {
		if tunnelCtx.Err() != nil {
			return
		}
		mu.Lock()
		repeat := err.Error() == lastErr
		lastErr = err.Error()
		mu.Unlock()
		if !repeat {
			cliNotice("Wendy Cloud tunnel to port %d on %s failed: %v", remotePort, device, err)
		}
	}
	t.relayFailed = dialFailed
	go func() {
		defer close(t.done)
		defer ln.Close()
		err := serveTCPForward(tunnelCtx, ln, t.relay, dialFailed)
		if err != nil && tunnelCtx.Err() == nil {
			cliNotice("Wendy Cloud tunnel on localhost:%d stopped: %v", t.LocalPort, err)
		}
	}()
	return t, nil
}

// listenCloudAppTunnel binds the loopback end of a cloud app tunnel: the
// app's own port when nothing on this machine uses it, so URLs and hooks read
// the same as on the device's network, otherwise a free ephemeral port. It
// never binds a non-loopback address.
func listenCloudAppTunnel(port int) (net.Listener, error) {
	if !loopbackPortInUse(port) {
		if ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err == nil {
			return ln, nil
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listening on 127.0.0.1: %w", err)
	}
	return ln, nil
}

// loopbackPortInUse reports whether something on this machine already
// accepts connections on port at 127.0.0.1 or [::1]. A successful bind on
// 127.0.0.1 is not enough: "localhost" may resolve to [::1] first (where a
// local dev server often listens), and some systems let a specific bind
// coexist with another process's wildcard listener on the same port.
func loopbackPortInUse(port int) bool {
	for _, host := range []string{"127.0.0.1", "::1"} {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), 250*time.Millisecond)
		if err == nil {
			c.Close()
			return true
		}
	}
	return false
}

// appTunnelSet holds the cloud app tunnels one attached run, or one watch
// session, has open: one per device port, shared by every hook path that
// forwards that port. The zero value is ready to use.
type appTunnelSet struct {
	mu      sync.Mutex
	tunnels map[int]*cloudAppTunnel
}

// forward returns the set's open tunnel to remotePort, or opens one that
// lives until ctx ends; opened reports a new tunnel. A nil set opens an
// unshared tunnel, which only ctx closes.
func (s *appTunnelSet) forward(ctx context.Context, conn *grpcclient.AgentConnection, remotePort int) (t *cloudAppTunnel, opened bool, err error) {
	if s == nil {
		t, err = startCloudAppTunnel(ctx, conn, remotePort)
		return t, err == nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.tunnels[remotePort]; t != nil && t.open() {
		return t, false, nil
	}
	t, err = startCloudAppTunnel(ctx, conn, remotePort)
	if err != nil {
		return nil, false, err
	}
	if s.tunnels == nil {
		s.tunnels = map[int]*cloudAppTunnel{}
	}
	s.tunnels[remotePort] = t
	return t, true, nil
}

// closeAll closes every tunnel in the set and waits for each to stop.
func (s *appTunnelSet) closeAll() {
	if s == nil {
		return
	}
	s.mu.Lock()
	tunnels := s.tunnels
	s.tunnels = nil
	s.mu.Unlock()
	for _, t := range tunnels {
		t.Close()
	}
}

// cloudAppTunnelPort picks the device port a cloud app tunnel forwards: the
// one the run presents, chosen the way reachableAppURL chooses it — the port
// of a postStart openURL whose host is WENDY_HOSTNAME, else the http
// entitlement's, else the readiness tcpSocket's. It is 0 unless a host-side
// action addresses the device: an openURL or cli hook using WENDY_HOSTNAME,
// or an http entitlement (which synthesizes an openURL). Readiness alone is no
// reason to forward: through a tunnel readiness is probed with a request
// (tunnelReadinessProbe), which a non-HTTP port need not answer.
func cloudAppTunnelPort(appCfg *appconfig.AppConfig) int {
	if appCfg == nil {
		return 0
	}
	hooks := synthesizedOpenURLHook(appCfg)
	if hooks == nil || hooks.PostStart == nil {
		return 0
	}
	hook := hooks.PostStart
	if !strings.Contains(hook.OpenURL, "WENDY_HOSTNAME") && !strings.Contains(hook.CLI, "WENDY_HOSTNAME") {
		return 0
	}
	if strings.Contains(hook.OpenURL, "WENDY_HOSTNAME") {
		u, err := url.Parse(expandHookEnv(hook.OpenURL, cloudAppTunnelHost, appCfg.AppID, appCfg.ServiceName))
		if err == nil && u.Hostname() == cloudAppTunnelHost {
			if port := urlPort(u); port != 0 {
				return port
			}
		}
	}
	if port, ok := httpEntitlementPort(appCfg.Entitlements); ok && port <= 65535 {
		return port
	}
	if r := effectiveReadiness(appCfg); r != nil && r.TCPSocket != nil && r.TCPSocket.Port > 0 && r.TCPSocket.Port <= 65535 {
		return r.TCPSocket.Port
	}
	return 0
}

// urlPort is u's explicit port, or its scheme's default for http and https;
// 0 when neither applies.
func urlPort(u *url.URL) int {
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return 0
		}
		return n
	}
	switch u.Scheme {
	case "http":
		return 80
	case "https":
		return 443
	}
	return 0
}

// hookRoute is where this machine's readiness probe and postStart hook reach
// an app: a device address (host), or the loopback end of a cloud app tunnel
// (host is cloudAppTunnelHost and tunnel is set).
type hookRoute struct {
	host       string
	tunnel     *cloudAppTunnel
	browserURL string // selected operator viewer; empty for physical/Cloud paths
}

// appURL is the URL to show for appCfg on this route: reachableAppURLForHost
// for a device address; through a tunnel, that URL on the tunnel's local
// port, or the tunnel's root when the app's URL names another port.
func (r hookRoute) appURL(appCfg *appconfig.AppConfig) string {
	raw := reachableAppURLForHost(appCfg, r.host)
	if r.tunnel == nil {
		return raw
	}
	if local, ok := r.tunnel.localURL(raw); ok {
		return local
	}
	scheme := ""
	if u, err := url.Parse(raw); err == nil {
		scheme = u.Scheme
	}
	return r.tunnel.rootURL(scheme)
}

// waitForReadiness is waitForAttachedReadiness on this route. When Wendy
// Cloud refuses the tunnel's relays (errCloudRelayRefused), it closes the
// tunnel, whose URL cannot reach the app, and says the host-side lifecycle is
// skipped; callers then skip it without a readiness warning.
func (r hookRoute) waitForReadiness(ctx context.Context, conn *grpcclient.AgentConnection, cfg *appconfig.AppConfig, timeoutOverride time.Duration) error {
	if r.tunnel == nil {
		return waitForAttachedReadiness(ctx, conn, cfg, r.host, timeoutOverride)
	}
	err := waitForTunneledReadiness(ctx, conn, cfg, r.tunnel, timeoutOverride)
	if errors.Is(err, errCloudRelayRefused) {
		r.tunnel.Close()
		cliNotice("Could not forward port %d to %s: %v", r.tunnel.RemotePort, containerDisplayName(cfg), err)
		cliNotice("%s", cloudHostSkipNotice(cfg))
	}
	return err
}

// resolveHookRoute is resolveHookHost for the attached hook paths
// (runPostStartIfReady, serviceHookRunner.runOne). When this machine has a
// route to the device — Wendy Mesh, a reachable LAN address, a direct or USB
// target, a VM — it is resolveHookHost exactly. For a cloud device without
// one, it forwards the app's presented port (cloudAppTunnelPort) through
// Wendy Cloud from tunnels, tied to hookCtx, and routes the probe and hooks
// through it. ok=false means the host-side lifecycle is skipped; the reason
// has been printed unless ctx ended.
func resolveHookRoute(ctx, hookCtx context.Context, conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, tunnels *appTunnelSet, watch bool) (hookRoute, bool) {
	if host, ok := resolveHookHost(ctx, conn, appCfg); ok {
		return hookRoute{host: host}, true
	}
	if ctx.Err() != nil {
		return hookRoute{}, false
	}
	port := cloudAppTunnelPort(appCfg)
	if port == 0 {
		cliNotice("%s", cloudHostSkipNotice(appCfg))
		return hookRoute{}, false
	}
	t, opened, err := tunnels.forward(hookCtx, conn, port)
	if err != nil {
		cliNotice("Could not open a Wendy Cloud tunnel to %s port %d: %v", containerDisplayName(appCfg), port, err)
		cliNotice("%s", cloudHostSkipNotice(appCfg))
		cliNotice("%s", manualCloudTunnelHint(conn, appCfg, port))
		return hookRoute{}, false
	}
	route := hookRoute{host: cloudAppTunnelHost, tunnel: t}
	appURL := route.appURL(appCfg)
	if opened {
		announceCloudAppTunnel(conn, appCfg, t, appURL, watch)
	}
	cliLogln("App reachable at %s", tui.Value(appURL))
	return route, true
}

// announceCloudAppTunnel prints the one line that says a run opened a cloud
// app tunnel, where it listens, and how long it lasts.
func announceCloudAppTunnel(conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, t *cloudAppTunnel, appURL string, watch bool) {
	until := "closes when wendy run exits"
	if watch {
		until = "closes when the watch stops"
	}
	device := conn.Host
	if device == "" {
		device = "the device"
	}
	scheme := ""
	if u, err := url.Parse(appURL); err == nil {
		scheme = u.Scheme
	}
	root := t.rootURL(scheme)
	if t.LocalPort == t.RemotePort {
		cliSuccess("Forwarding %s → %s on %s through Wendy Cloud (%s)", root, containerDisplayName(appCfg), device, until)
		return
	}
	cliSuccess("Forwarding %s → %s (port %d) on %s through Wendy Cloud; port %d is in use on this machine (%s)", root, containerDisplayName(appCfg), t.RemotePort, device, t.RemotePort, until)
}

// manualCloudTunnelHint tells the developer how to reach port on conn's cloud
// device from this machine themselves: the `wendy cloud tunnel` command and
// the URL to open once it runs.
func manualCloudTunnelHint(conn *grpcclient.AgentConnection, appCfg *appconfig.AppConfig, port int) string {
	same := &cloudAppTunnel{LocalPort: port, RemotePort: port}
	appURL := hookRoute{host: cloudAppTunnelHost, tunnel: same}.appURL(appCfg)
	return fmt.Sprintf("To open %s from this machine, run %s and open %s", containerDisplayName(appCfg), tui.Command(cloudTunnelCommandLine(conn.Host, port)), tui.Value(appURL))
}

// cloudTunnelCommandLine is the `wendy cloud tunnel` command that forwards
// port on the named cloud device to the same port on this machine.
func cloudTunnelCommandLine(device string, port int) string {
	cmd := fmt.Sprintf("wendy cloud tunnel %d:%d", port, port)
	if device != "" {
		cmd += " --device " + doubleQuoteArg(device)
	}
	return cmd
}

// doubleQuoteArg double-quotes s for a copy-pasted command line, which POSIX
// shells, PowerShell and cmd.exe all read as one argument, escaping the
// characters a POSIX shell still interprets inside double quotes.
func doubleQuoteArg(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		if strings.ContainsRune("\"\\$`", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// Readiness through a cloud app tunnel. A connect to the tunnel's local end
// always succeeds, so an attempt has to reach the app through a Wendy Cloud
// relay, and on the v2 relay each relay is a separate authorization request to
// Wendy Cloud: opening one per attempt cost an app that loads a model for two
// minutes over a hundred of them. So the wait first asks the agent, over the
// run's existing connection, whether the app listens on the forwarded port
// (devicePortListening). Once it does, the app is ready as a TCP readiness
// check on the device's network would find it, and one relay that opens
// confirms Wendy Cloud forwards the port. Only when the agent cannot see the
// socket does a relay have to carry a request the app answers, spaced by a
// backoff.
const (
	// tunnelReadinessAttemptTimeout bounds one readiness relay: the relay
	// setup plus the device's dial to the app.
	tunnelReadinessAttemptTimeout = 5 * time.Second
	// tunnelRelayBackoff and tunnelRelayBackoffMax space the readiness relays
	// when the agent cannot say whether the port listens, or says it does and
	// the relay did not open: the first at once, the next 1 s after it, the
	// gap doubling to 5 s. A failure never shortens the gap.
	tunnelRelayBackoff    = time.Second
	tunnelRelayBackoffMax = 5 * time.Second
	// tunnelSafetyRelay and tunnelSafetyRelayMax space the relays opened while
	// the agent reports the port is not listening: 5 s into the wait, the gap
	// doubling to 30 s. They keep an agent that cannot see the socket from
	// failing an app that answers: the macOS agent lists the sockets of the
	// app's main process only, and the forwarded port may be served by a
	// process outside the app.
	tunnelSafetyRelay    = 5 * time.Second
	tunnelSafetyRelayMax = 30 * time.Second
	// devicePortsPollInterval spaces the agent calls, which the agent logs;
	// devicePortsCallTimeout bounds one.
	devicePortsPollInterval = time.Second
	devicePortsCallTimeout  = 3 * time.Second
)

// waitForTunneledReadiness is waitForAttachedReadiness for an app reached
// through a cloud app tunnel. It checks the forwarded port, the one the
// browser reaches, even when the app's readiness tcpSocket names another
// device port, which the tunnel does not carry. The readiness timeout and the
// slow-start observation are unchanged; tunnelReadiness decides which attempts
// open a relay. A relay Wendy Cloud refuses (errCloudRelayRefused) ends the
// wait at once with that error, since no later attempt can pass.
func waitForTunneledReadiness(ctx context.Context, conn *grpcclient.AgentConnection, cfg *appconfig.AppConfig, t *cloudAppTunnel, timeoutOverride time.Duration) error {
	readiness := effectiveReadiness(cfg)
	if readiness == nil || readiness.TCPSocket == nil {
		return nil
	}
	probe := &appconfig.ReadinessConfig{
		TCPSocket:      &appconfig.TCPSocketProbe{Port: t.LocalPort},
		TimeoutSeconds: readiness.TimeoutSeconds,
	}
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	r := newTunnelReadiness(time.Now, devicePortListening(conn, cfg.AppID, t.RemotePort), t.probeRelay)
	attempt := func(ctx context.Context) error {
		err := r.attempt(ctx)
		if errors.Is(err, errCloudRelayRefused) {
			stop(err)
		}
		return err
	}
	err := waitForAttachedReadinessWith(ctx, conn, cfg, probe, cloudAppTunnelHost, 0, tunnelReadinessProbe(attempt), timeoutOverride)
	if cause := context.Cause(ctx); errors.Is(cause, errCloudRelayRefused) {
		return cause
	}
	return err
}

// tunnelReadinessProbe is makeReadinessProbe for a cloud app tunnel: it
// reports the tunnel's local end and tries with attempt. The readiness wait
// builds its probe twice, for the timed wait and the slow-start observation;
// both share one tunnelReadiness, so neither the backoff nor the relay count
// starts over.
func tunnelReadinessProbe(attempt func(context.Context) error) readinessProbeFactory {
	return func(hostname string, port int, _ bool) (string, func(context.Context) error, func()) {
		return net.JoinHostPort(hostname, strconv.Itoa(port)), attempt, func() {}
	}
}

// devicePortListening asks the agent, over conn's existing gRPC connection
// and so without a relay, whether appID's processes hold a TCP socket
// listening on port. GetContainerPorts reads the app's own sockets on the
// device; it backs the ports panel of `wendy device top`. nil when conn
// cannot ask.
func devicePortListening(conn *grpcclient.AgentConnection, appID string, port int) func(context.Context) (bool, error) {
	if conn == nil || conn.ContainerService == nil || appID == "" {
		return nil
	}
	return func(ctx context.Context) (bool, error) {
		ctx, cancel := context.WithTimeout(ctx, devicePortsCallTimeout)
		defer cancel()
		resp, err := conn.ContainerService.GetContainerPorts(ctx, &agentpb.GetContainerPortsRequest{AppName: appID})
		if err != nil {
			return false, err
		}
		for _, p := range resp.GetPorts() {
			if strings.HasPrefix(p.GetProtocol(), "tcp") && int(p.GetPort()) == port {
				return true, nil
			}
		}
		return false, nil
	}
}

var (
	errTunnelPortNotListening = errors.New("the app is not listening on the forwarded port yet")
	errTunnelRelayDeferred    = errors.New("waiting before the next Wendy Cloud relay")
	// errCloudRelayRefused wraps a relay failure that every later attempt
	// would repeat (permanentRelayError).
	errCloudRelayRefused = errors.New("Wendy Cloud refused the relay")
)

// permanentRelayError reports whether a failure to open a Wendy Cloud relay
// would repeat on every later attempt: Cloud would not authorize it, or does
// not offer the port's service. A relay to an app that is still starting does
// not fail to open: the device joins it, then closes it when its dial to the
// app fails, so the probe reads EOF.
func permanentRelayError(err error) bool {
	switch status.Code(err) {
	case codes.PermissionDenied, codes.Unauthenticated, codes.NotFound, codes.InvalidArgument, codes.Unimplemented:
		return true
	}
	return false
}

// tunnelReadiness decides, attempt by attempt, whether a readiness wait
// through a cloud app tunnel opens a relay. One wait owns it and calls attempt
// from one goroutine.
type tunnelReadiness struct {
	now func() time.Time
	// listening asks the agent whether the app listens on the forwarded port
	// (devicePortListening): nil when the connection cannot ask, or once the
	// agent answered Unimplemented.
	listening func(context.Context) (bool, error)
	// relay is one relay attempt (cloudAppTunnel.probeRelay): with reply it
	// passes when the app answers a request, otherwise once the relay opens.
	relay func(ctx context.Context, reply bool) error

	started  bool
	polled   time.Time // the last call to listening
	answered bool      // the last call answered
	listens  bool      // what it answered
	confirm  bool      // the agent has reported the port listening
	backoff  relaySchedule
	safety   relaySchedule
}

func newTunnelReadiness(now func() time.Time, listening func(context.Context) (bool, error), relay func(ctx context.Context, reply bool) error) *tunnelReadiness {
	return &tunnelReadiness{
		now:       now,
		listening: listening,
		relay:     relay,
		backoff:   relaySchedule{gap: tunnelRelayBackoff, max: tunnelRelayBackoffMax},
		safety:    relaySchedule{gap: tunnelSafetyRelay, max: tunnelSafetyRelayMax},
	}
}

// relaySchedule is when the next relay may open and the gap after it.
type relaySchedule struct {
	next     time.Time
	gap, max time.Duration
}

func (s *relaySchedule) due(now time.Time) bool { return !now.Before(s.next) }

// opened records a relay attempt that ended at now: the next waits gap, and
// the gap doubles up to max.
func (s *relaySchedule) opened(now time.Time) {
	s.next = now.Add(s.gap)
	s.gap = min(2*s.gap, s.max)
}

// attempt is one readiness attempt. It asks the agent first, at most once per
// devicePortsPollInterval. While the agent reports the port is not listening,
// only the spaced safety relays open, and they pass only when the app answers
// a request. The first time it reports the port listening, a relay opens at
// once and passes when it opens: the agent has seen the socket, and the app
// need not speak HTTP. Later ones follow the backoff. When the agent cannot
// answer, relays follow the backoff and need the app to answer.
func (r *tunnelReadiness) attempt(ctx context.Context) error {
	now := r.now()
	if !r.started {
		r.started = true
		r.safety.next = now.Add(r.safety.gap)
	}
	if r.listening != nil && (r.polled.IsZero() || now.Sub(r.polled) >= devicePortsPollInterval) {
		r.polled = now
		listens, err := r.listening(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		switch {
		case status.Code(err) == codes.Unimplemented:
			r.listening, r.answered = nil, false
		case err != nil:
			r.answered = false
		default:
			r.answered, r.listens = true, listens
			if listens && !r.confirm {
				r.confirm = true
				r.backoff.next = now
			}
		}
	}
	schedule := &r.backoff
	if r.answered && !r.listens {
		schedule = &r.safety
	}
	if !schedule.due(now) {
		if schedule == &r.safety {
			return errTunnelPortNotListening
		}
		return errTunnelRelayDeferred
	}
	err := r.relay(ctx, !(r.answered && r.listens))
	schedule.opened(r.now())
	return err
}

// probeRelay is one readiness attempt through t: it opens a relay to the
// app's port and, with reply, sends an HTTP HEAD request and passes on any
// reply, an HTTP response or the bytes a TLS or banner-first server sends
// back. Without reply it passes once the relay opens; the device joins a relay
// before it dials the app, so that alone does not show the app listens, and
// callers pass reply=false only once the agent has reported the socket. When
// the device cannot reach the app's port the relay fails or closes and nothing
// comes back. It opens the relay itself, not through the local listener, so
// the relay closes when the attempt ends, answered, refused or timed out, and
// only one probe relay is open on t at a time.
func (t *cloudAppTunnel) probeRelay(ctx context.Context, reply bool) error {
	if !t.open() {
		return errors.New("the Wendy Cloud tunnel has closed")
	}
	if t.relay == nil {
		return errors.New("this tunnel cannot open a Wendy Cloud relay")
	}
	if !t.probing.TryLock() {
		return errors.New("another readiness probe is using the Wendy Cloud tunnel")
	}
	defer t.probing.Unlock()
	ctx, cancel := context.WithTimeout(ctx, tunnelReadinessAttemptTimeout)
	defer cancel()
	c, err := t.relay(ctx)
	if err != nil {
		if permanentRelayError(err) {
			// hookRoute.waitForReadiness reports it, with what is skipped.
			return fmt.Errorf("%w: %w", errCloudRelayRefused, err)
		}
		if ctx.Err() == nil && t.relayFailed != nil {
			t.relayFailed(err)
		}
		return err
	}
	defer c.Close()
	if !reply {
		return nil
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	host := net.JoinHostPort(cloudAppTunnelHost, strconv.Itoa(t.LocalPort))
	if _, err := fmt.Fprintf(c, "HEAD / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", host); err != nil {
		return err
	}
	var b [1]byte
	if _, err := c.Read(b[:]); err != nil {
		return fmt.Errorf("no reply through the Wendy Cloud tunnel: %w", err)
	}
	return nil
}
