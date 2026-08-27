package commands

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshname"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
)

const maxCloudAssets = 10_000

// The tunneled agent conn's keepalive is a slow END-TO-END BACKSTOP, not the
// liveness probe: its PINGs are payload bytes inside the broker stream, subject
// to the same flow control as a multi-GB chunk upload, so during a push their
// RTT measures queue drain, not liveness. Link liveness/NAT warmth belongs to
// the broker conn (real TCP, stays at 30s/20s), and a dead tunnel already fails
// fast via broker-stream errors closing the pipe. Ping stays ≥ the agent's
// enforced MinTime (10s, mtls/server.go:108). Numbers worth measuring against
// broker behavior before shipping.
const (
	tunneledKeepalivePing       = 5 * time.Minute
	tunneledKeepaliveACKTimeout = 1 * time.Minute
)

type closeFunc func()

func (f closeFunc) Close() error {
	f()
	return nil
}

func certXFCC(cert config.CertificateInfo) string {
	if cert.UserID != "" {
		return fmt.Sprintf("URI=urn:wendy:org:%d:user:%s", cert.OrganizationID, cert.UserID)
	}
	if cert.AssetID != 0 {
		return fmt.Sprintf("URI=urn:wendy:org:%d:asset:%d", cert.OrganizationID, cert.AssetID)
	}
	return ""
}

func cloudContext(ctx context.Context, auth *config.AuthConfig) (context.Context, error) {
	if len(auth.Certificates) == 0 {
		return ctx, nil
	}
	cert := auth.Certificates[0]
	md := metadata.MD{}
	if auth.HasAPIKey() {
		bearerToken, err := auth.BearerToken()
		if err != nil {
			return nil, fmt.Errorf("loading API token: %w", err)
		}
		md.Set("authorization", "Bearer "+bearerToken)
	}
	certHeader := certXFCC(cert)
	if certHeader != "" {
		md.Set("x-wendy-client-cert", certHeader)
		md.Set("x-forwarded-client-cert", certHeader)
	}
	return metadata.NewOutgoingContext(ctx, md), nil
}

func connectToCloudAgent(ctx context.Context, cloudGRPC, deviceName, brokerURL string) (*grpcclient.AgentConnection, error) {
	auth, err := pickAuthEntry(cloudGRPC)
	if err != nil {
		return nil, err
	}

	asset, err := pickCloudDevice(ctx, auth, deviceName, brokerURL)
	if err != nil {
		return nil, err
	}
	cliLogln("Connecting to %s via cloud tunnel...", asset.GetName())

	return connectCloudAsset(ctx, auth, asset, brokerURL)
}

// detachedTunnelContext returns the context to open a connection-scoped broker
// tunnel stream on, given the context that bounds only the dial.
//
// A dial context routinely outlives its usefulness the moment the connection is
// handed to the caller: the reconnect helpers (waitForCloudAgentRestart,
// waitForUpdatedAgentReady, runAgentConnectionSpinner) each wrap the dial in a
// timeout they cancel as soon as the connection is returned. A tunnel stream
// opened directly on the dial context dies with that cancel, so every
// reconnected cloud connection arrived dead — its liveness probe passed only
// because it ran before the cancel, and the next RPC failed with
// "error reading from server: EOF".
//
// The returned context therefore does not inherit dialCtx's cancellation (its
// values are kept). Until handoff is called, cancelling dialCtx still ends it,
// so an abandoned dial attempt cannot leak a live stream; handoff severs that
// link at the successful hand-over, after which only cancel — which the caller
// must wire into the connection's Close path — ends the context.
func detachedTunnelContext(dialCtx context.Context) (tunnelCtx context.Context, handoff func(), cancel context.CancelFunc) {
	tunnelCtx, tunnelCancel := context.WithCancel(context.WithoutCancel(dialCtx))
	stop := context.AfterFunc(dialCtx, tunnelCancel)
	return tunnelCtx, func() { stop() }, tunnelCancel
}

func connectCloudAsset(ctx context.Context, auth *config.AuthConfig, asset *cloudpb.Asset, brokerURL string) (*grpcclient.AgentConnection, error) {
	brokerConn, err := clouddefaults.DialBroker(auth, brokerURL)
	if err != nil {
		return nil, err
	}

	cleanupBroker := true
	defer func() {
		if cleanupBroker {
			_ = brokerConn.Close()
		}
	}()

	// The tunnel stream lives exactly as long as the context it is opened on,
	// and ctx here may be a short per-attempt dial timeout the caller cancels
	// right after we return (see detachedTunnelContext). Open the stream on a
	// context detached at handoff and ended by Close instead.
	tunnelCtx, tunnelHandoff, tunnelCancel := detachedTunnelContext(ctx)
	handedOff := false
	defer func() {
		if !handedOff {
			tunnelHandoff()
			tunnelCancel()
		}
	}()

	// Provisioned agents serve mTLS on agentPort+1 (50052) for remote clients; the
	// plaintext port (50051) is shut down after provisioning. (On-device containers
	// with the admin entitlement can reach the agent via the local unix socket.)
	tunnelConn, err := openBrokerTunnel(tunnelCtx, brokerConn, auth, asset.GetId(), defaultAgentPort+1)
	if err != nil {
		return nil, fmt.Errorf("opening cloud tunnel to %s: %w", asset.GetName(), err)
	}

	dialOpt, closeTunnel := tunnelDialer(tunnelConn)

	cert := auth.Certificates[0]
	keyPEM, err := cert.PrivateKeyPEM()
	if err != nil {
		closeTunnel()
		return nil, fmt.Errorf("loading client key: %w", err)
	}
	x509Cert, err := tls.X509KeyPair([]byte(cert.PemCertificate), []byte(keyPEM))
	if err != nil {
		closeTunnel()
		return nil, fmt.Errorf("loading agent mTLS cert: %w", err)
	}
	verifyConn, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{
		ChainPEM:      cert.PemCertificateChain,
		ExpectedOrgID: int32(cert.OrganizationID),
	})
	if err != nil {
		closeTunnel()
		return nil, fmt.Errorf("building TLS verifier: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates:       []tls.Certificate{x509Cert},
		InsecureSkipVerify: true, //nolint:gosec — hostname bypass only; VerifyConnection validates server cert against Wendy PKI
		VerifyConnection:   verifyConn,
		MinVersion:         tls.VersionTLS12,
	}

	grpcConn, err := grpc.NewClient(
		"passthrough:///cloud-tunnel",
		dialOpt,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithInitialWindowSize(8*1024*1024),
		grpc.WithInitialConnWindowSize(16*1024*1024),
		grpc.WithReadBufferSize(256*1024),
		grpc.WithWriteBufferSize(256*1024),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                tunneledKeepalivePing,
			Timeout:             tunneledKeepaliveACKTimeout,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		closeTunnel()
		return nil, fmt.Errorf("creating tunnelled gRPC connection: %w", err)
	}

	agentConn := grpcclient.NewFromConn(grpcConn)
	agentConn.Host = asset.GetName()
	agentConn.MeshHost = meshname.Device(asset.GetId())
	agentConn.IsMTLS = true
	agentConn.CertInfo = &cert
	agentConn.RegistryDialer = func(ctx context.Context, port int) (net.Conn, error) {
		return openBrokerTunnel(ctx, brokerConn, auth, asset.GetId(), uint32(port))
	}
	// Pin reconnect to this exact asset (by id) so a post-restart reconnect
	// can't drift to a different cloud device — the asset name may be empty or
	// ambiguous, and re-running device discovery while the agent is mid-restart
	// can match whichever other device happens to be reachable.
	agentConn.Reconnect = func(rctx context.Context) (*grpcclient.AgentConnection, error) {
		return waitForCloudAgentRestart(rctx, auth, asset, brokerURL)
	}
	agentConn.ExtraClosers = append(agentConn.ExtraClosers, closeFunc(closeTunnel), brokerConn, closeFunc(tunnelCancel))
	cleanupBroker = false
	tunnelHandoff()
	handedOff = true
	return agentConn, nil
}

func waitForCloudAgentRestart(ctx context.Context, auth *config.AuthConfig, asset *cloudpb.Asset, brokerURL string) (*grpcclient.AgentConnection, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	restartErr := func() error {
		return fmt.Errorf("timed out waiting for %s (id=%d) to restart", asset.GetName(), asset.GetId())
	}
	// Give the agent a moment to begin shutdown.
	select {
	case <-time.After(time.Second):
	case <-ctx.Done():
		return nil, restartErr()
	}
	for {
		select {
		case <-ctx.Done():
			return nil, restartErr()
		default:
		}
		attemptCtx, attemptCancel := context.WithTimeout(ctx, 10*time.Second)
		conn, err := connectCloudAsset(attemptCtx, auth, asset, brokerURL)
		if err != nil {
			attemptCancel()
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return nil, restartErr()
			}
			continue
		}
		probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Second)
		resp, probeErr := conn.AgentService.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
		probeCancel()
		attemptCancel()
		if probeErr == nil {
			conn.CacheAgentVersion(resp)
			return conn, nil
		}
		conn.Close()
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return nil, restartErr()
		}
	}
}

func openBrokerTunnel(ctx context.Context, brokerConn *grpc.ClientConn, auth *config.AuthConfig, assetID int32, remotePort uint32) (net.Conn, error) {
	client := cloudpb.NewTunnelBrokerServiceClient(brokerConn)

	cloudCtx, err := cloudContext(ctx, auth)
	if err != nil {
		return nil, err
	}
	stream, err := client.ClientTunnel(cloudCtx)
	if err != nil {
		return nil, fmt.Errorf("opening tunnel stream: %w", err)
	}

	if err := stream.Send(&cloudpb.ClientTunnelMessage{
		Content: &cloudpb.ClientTunnelMessage_Open{
			Open: &cloudpb.ClientTunnelOpen{
				AssetId: assetID,
				Host:    "localhost",
				Port:    remotePort,
			},
		},
	}); err != nil {
		return nil, fmt.Errorf("sending tunnel open: %w", err)
	}

	local, remote := net.Pipe()

	go func() {
		defer remote.Close()
		for {
			msg, err := stream.Recv()
			if err != nil {
				if tlsDebug := os.Getenv("WENDY_TLS_DEBUG") != ""; tlsDebug {
					fmt.Fprintf(os.Stderr, "[tunnel-debug] broker stream closed: %v\n", err)
				}
				break
			}
			if len(msg.Payload) > 0 {
				if _, err := remote.Write(msg.Payload); err != nil {
					break
				}
			}
			if msg.HalfClose {
				break
			}
		}
	}()

	go runTunnelUplink(remote, func(payload []byte, halfClose bool) error {
		return stream.Send(&cloudpb.ClientTunnelMessage{
			Content: &cloudpb.ClientTunnelMessage_Data{
				Data: &cloudpb.TunnelData{Payload: payload, HalfClose: halfClose},
			},
		})
	}, stream.CloseSend)

	return local, nil
}

// tunnelUplinkQueueSlots bounds the uplink queue: reads are ≤256KiB, so 128
// slots ≈ 32MB — 2× the tunneled conn's 16MB connection window, more than gRPC
// can have in flight before its own flow control pushes back. Derived from the
// window, not tuned: the queue only guarantees pipe writes (data AND the
// keepalive PING/ACK control frames queued behind them) keep completing while
// stream.Send is momentarily blocked on broker flow control.
const tunnelUplinkQueueSlots = 128

// tunnelUplinkItem is one unit of queued uplink work: either a data payload
// (halfClose false) or the terminal half-close marker (payload nil,
// halfClose true) — never both, mirroring net.Pipe's Read contract of never
// returning n>0 together with an error.
type tunnelUplinkItem struct {
	payload   []byte
	halfClose bool
}

// runTunnelUplink pumps remote→broker through a bounded queue, decoupling
// remote.Read from send. Previously one goroutine did both, so a Send stalled
// on broker flow control blocked net.Pipe writes, starving the tunneled
// transport's keepalive until the ACK window tore the connection down
// mid-push (WDY-2433). Closes remote on send failure; closeSend after EOF
// drain.
//
// runTunnelUplink itself is the sender — the sole toucher of send/closeSend,
// which also fixes a latent Send/CloseSend concurrency hazard from the old
// single-goroutine pump. It spawns one more goroutine, the reader — the sole
// toucher of remote.Read — which feeds the bounded queue. A full queue blocks
// the reader (and, transitively, remote.Read): that backpressure is
// intentional, not a bug.
func runTunnelUplink(remote net.Conn, send func(payload []byte, halfClose bool) error, closeSend func() error) {
	q := make(chan tunnelUplinkItem, tunnelUplinkQueueSlots)

	go func() {
		buf := make([]byte, 256*1024)
		for {
			n, readErr := remote.Read(buf)
			if n > 0 {
				payload := make([]byte, n)
				copy(payload, buf[:n])
				q <- tunnelUplinkItem{payload: payload}
			}
			if readErr != nil {
				if readErr == io.EOF {
					q <- tunnelUplinkItem{halfClose: true}
				}
				close(q)
				return
			}
		}
	}()

	defer closeSend()
	for item := range q {
		if err := send(item.payload, item.halfClose); err != nil {
			// Sole toucher of remote.Close() on this path: unblocks any
			// caller mid-Write on the local pipe end instead of leaving it
			// hanging forever behind a reader that has nowhere left to send,
			// and unblocks the reader itself so it can close q and let this
			// drain loop (and the deferred closeSend) finish.
			remote.Close()
			for range q {
			}
			return
		}
	}
}

func fetchCloudAssets(ctx context.Context, auth *config.AuthConfig) ([]*cloudpb.Asset, error) {
	assets, err := fetchCloudAssetsFiltered(ctx, auth, true)
	if err != nil {
		return nil, err
	}
	seedPinsFromAssetsBestEffort(auth, assets)
	return assets, nil
}

// errNoCloudDevicesEnrolled is the "no --device given, org has zero enrolled
// devices" miss. Kept as a plain sentinel error (not errCloudDeviceNotFound)
// because it isn't about one specific device by name; upgradeOfflineResolveErr
// still recognizes it via errors.Is to offer the "all N devices offline"
// upgrade when an offline-inclusive re-query finds devices after all.
var errNoCloudDevicesEnrolled = errors.New("no enrolled devices found for this org; enroll a device with 'wendy device enroll' first")

// errCloudDeviceNotFound is returned by resolveCloudAsset when deviceName
// was given (or the asset list was empty despite a name being given) but no
// matching device was found in the queried asset list. It is typed so
// upgradeOfflineResolveErr can distinguish "not found in this list" (which
// may just mean "found in the offline-inclusive list instead") from other
// resolveCloudAsset errors such as ambiguity, which should pass through
// unchanged.
type errCloudDeviceNotFound struct{ name string }

func (e *errCloudDeviceNotFound) Error() string {
	return fmt.Sprintf("no device named or with id %q found; run 'wendy cloud discover --json' to list ids", e.name)
}

func resolveCloudAsset(assets []*cloudpb.Asset, deviceName string) (*cloudpb.Asset, error) {
	if len(assets) == 0 {
		if deviceName != "" {
			return nil, &errCloudDeviceNotFound{name: deviceName}
		}
		return nil, errNoCloudDevicesEnrolled
	}
	if deviceName != "" {
		lower := strings.ToLower(deviceName)
		var matched *cloudpb.Asset
		for _, a := range assets {
			if strings.ToLower(a.GetName()) == lower {
				if matched != nil {
					return nil, fmt.Errorf("multiple devices match %q; use a more specific name", deviceName)
				}
				matched = a
			}
		}
		if matched != nil {
			return matched, nil
		}
		// Numeric asset-id fallback: allows targeting unnamed devices.
		if id, err := strconv.Atoi(strings.TrimSpace(deviceName)); err == nil {
			for _, a := range assets {
				if a.GetId() == int32(id) {
					return a, nil
				}
			}
		}
		return nil, &errCloudDeviceNotFound{name: deviceName}
	}
	if len(assets) == 1 {
		return assets[0], nil
	}
	var b strings.Builder
	for i, a := range assets {
		if i > 0 {
			b.WriteString(", ")
		}
		name := a.GetName()
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Fprintf(&b, "%d=%s", a.GetId(), name)
	}
	return nil, fmt.Errorf("multiple cloud devices found; rerun with --device <id|name> (%s)", b.String())
}

// upgradeOfflineResolveErr re-checks a resolveCloudAsset miss against the
// full (offline-inclusive) asset list, so an enrolled device that merely had
// a heartbeat flap reads as "offline" rather than "nonexistent". fetchAll is
// injected (rather than calling fetchCloudAssetsFiltered directly) so this
// stays a pure function for testing; a fetchAll failure or a still-missing
// device both keep resolveErr unchanged. Errors that aren't a not-found miss
// (e.g. ambiguity) pass through without invoking fetchAll at all.
func upgradeOfflineResolveErr(resolveErr error, deviceName string, fetchAll func() ([]*cloudpb.Asset, error)) error {
	var notFound *errCloudDeviceNotFound
	switch {
	case errors.As(resolveErr, &notFound):
		allAssets, err := fetchAll()
		if err != nil {
			return resolveErr
		}
		if clouddefaults.FindAssetByNameOrID(allAssets, deviceName) == nil {
			return resolveErr
		}
		return fmt.Errorf("device %q is enrolled but currently reported offline; check the device's power and network connection, then retry ('wendy cloud discover --all --json' lists all enrolled devices)", deviceName)
	case errors.Is(resolveErr, errNoCloudDevicesEnrolled):
		allAssets, err := fetchAll()
		if err != nil {
			return resolveErr
		}
		if len(allAssets) == 0 {
			return resolveErr
		}
		return fmt.Errorf("all %d enrolled devices are currently reported offline; check their power and network connections, then retry ('wendy cloud discover --all --json' lists all enrolled devices)", len(allAssets))
	default:
		return resolveErr
	}
}

func pickCloudDevice(ctx context.Context, auth *config.AuthConfig, deviceName, brokerURL string) (*cloudpb.Asset, error) {
	return pickCloudDeviceWithRelogin(ctx, auth, deviceName, brokerURL, true)
}

// pickCloudDeviceWithRelogin lists the org's cloud devices and lets the user
// pick one. When the cloud rejects the session as unauthenticated and allowRelogin
// is set, it offers to log in again (the spinner has already exited, so the
// terminal is free for the prompt) and retries once with the fresh credentials.
func pickCloudDeviceWithRelogin(ctx context.Context, auth *config.AuthConfig, deviceName, brokerURL string, allowRelogin bool) (*cloudpb.Asset, error) {
	if len(auth.Certificates) == 0 {
		return nil, fmt.Errorf("auth entry has no certificates; re-run 'wendy auth login'")
	}

	retryAfterRelogin := func(cause error) (*cloudpb.Asset, error, bool) {
		if !allowRelogin || !offerReloginOnUnauthenticated(ctx, auth, cause) {
			return nil, nil, false
		}
		fresh := reloadAuthEntry(auth)
		if fresh == nil {
			return nil, nil, false
		}
		asset, err := pickCloudDeviceWithRelogin(ctx, fresh, deviceName, brokerURL, false)
		return asset, err, true
	}

	var assets []*cloudpb.Asset
	if isInteractiveTerminal() {
		prog := tui.NewProgressProgram(tui.NewSpinner("Fetching devices from cloud..."))
		var fetchErr error
		go func() {
			assets, fetchErr = fetchCloudAssets(ctx, auth)
			prog.Send(tui.SpinnerDoneMsg{})
		}()
		finalModel, err := prog.Run()
		if err != nil {
			return nil, fmt.Errorf("spinner: %w", err)
		}
		if sm, ok := finalModel.(tui.SpinnerModel); ok && !sm.Done() {
			return nil, ErrUserCancelled
		}
		if fetchErr != nil {
			if asset, err, handled := retryAfterRelogin(fetchErr); handled {
				return asset, err
			}
			return nil, fetchErr
		}
	} else {
		var err error
		assets, err = fetchCloudAssets(ctx, auth)
		if err != nil {
			if asset, rerr, handled := retryAfterRelogin(err); handled {
				return asset, rerr
			}
			return nil, err
		}
	}

	// When running interactively with no --device and multiple assets, skip
	// resolveCloudAsset (which now returns an enumerated error) and fall
	// straight through to the interactive picker.
	if isInteractiveTerminal() && deviceName == "" && len(assets) > 1 {
		// fall through to picker below
	} else {
		asset, err := resolveCloudAsset(assets, deviceName)
		if err != nil {
			// The miss may just mean the device is offline: fetchCloudAssets
			// above only queried online assets. Re-check against the full
			// (offline-inclusive) list before reporting the device nonexistent.
			// Bypasses fetchCloudAssets (not fetchCloudAssetsFiltered directly)
			// so this offline re-query doesn't re-run pin-seeding.
			return nil, upgradeOfflineResolveErr(err, deviceName, func() ([]*cloudpb.Asset, error) {
				return fetchCloudAssetsFiltered(ctx, auth, false)
			})
		}
		if asset != nil {
			// With no --device, resolveCloudAsset picks the org's only enrolled
			// device without asking. Say so, rather than leaving the target
			// implicit. Note this is not the configured default device: the
			// cloud path does not consult that setting.
			if deviceName == "" {
				noteImplicitDevice(asset.GetName(), implicitSoleCloudDevice)
			}
			return asset, nil
		}
	}

	m := newCloudDiscoverModel(ctx, auth, brokerURL, false, true, assets)
	p := tea.NewProgram(m)
	finalModel, err := p.Run()
	if err != nil {
		return nil, fmt.Errorf("device picker: %w", err)
	}
	cm := finalModel.(cloudDiscoverModel)
	if cm.quitting && cm.selected == nil {
		return nil, ErrUserCancelled
	}
	if cm.selected == nil {
		return nil, fmt.Errorf("no device selected")
	}
	return cm.selected, nil
}

func boolPtr(b bool) *bool { return &b }

func int32Ptr(i int32) *int32 { return &i }

func dialCloudGRPC(auth *config.AuthConfig) (*grpc.ClientConn, error) {
	if len(auth.Certificates) == 0 {
		return nil, fmt.Errorf("auth entry has no certificates; re-run 'wendy auth login'")
	}
	cert := auth.Certificates[0]
	var transport grpc.DialOption
	if clouddefaults.UsesPublicCA(auth.CloudGRPC) {
		keyPEM, err := cert.PrivateKeyPEM()
		if err != nil {
			return nil, fmt.Errorf("loading client key: %w", err)
		}
		tlsCfg, err := certs.LoadTLSConfig(
			cert.PemCertificate,
			cert.PemCertificateChain,
			keyPEM,
			"",
		)
		if err != nil {
			return nil, fmt.Errorf("loading TLS config: %w", err)
		}
		transport = grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))
	} else {
		transport = grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	conn, err := grpc.NewClient(auth.CloudGRPC,
		transport,
		grpc.WithInitialWindowSize(8*1024*1024),
		grpc.WithInitialConnWindowSize(16*1024*1024),
		grpc.WithReadBufferSize(256*1024),
		grpc.WithWriteBufferSize(256*1024),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                clouddefaults.KeepalivePing,
			Timeout:             clouddefaults.KeepaliveACKTimeout,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to cloud: %w", err)
	}
	return conn, nil
}

func tunnelDialer(tunnelConn net.Conn) (grpc.DialOption, func()) {
	var once sync.Once
	return grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
		return tunnelConn, nil
	}), func() { once.Do(func() { tunnelConn.Close() }) }
}
