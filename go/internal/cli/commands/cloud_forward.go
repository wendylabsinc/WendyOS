package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func newCloudTunnelCmd() *cobra.Command {
	var cloudGRPC string
	var deviceName string
	var brokerURL string

	cmd := &cobra.Command{
		Use:   "tunnel [<local-port>:<remote-port>[/udp]]",
		Short: "Forward a local TCP or UDP port to a port on a cloud-enrolled device",
		Long: "Listens on <local-port> and forwards each connection through the Wendy Cloud tunnel broker to <remote-port> on the target device.\n\n" +
			"Without a port mapping, prompts for one interactively. Non-interactive and --json invocations require an explicit port mapping.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var localPort, remotePort uint32
			var udp bool
			if len(args) > 0 {
				var err error
				if localPort, remotePort, udp, err = parseTunnelArg(args[0]); err != nil {
					return err
				}
			} else if jsonOutput || !isInteractiveTerminal() {
				return usageErrorf("provide a port mapping: 'wendy cloud tunnel <local-port>:<remote-port>[/udp]'")
			}
			// Resolve the session before prompting, so a missing login is
			// reported before the user types a mapping.
			auth, err := pickAuthEntry(cloudGRPC)
			if err != nil {
				return err
			}
			if len(args) == 0 {
				if localPort, remotePort, udp, err = promptTunnelMapping(cmd); err != nil {
					return err
				}
			}
			return cloudTunnelCommand(cmd.Context(), auth, effectiveDeviceName(deviceName), brokerURL, localPort, remotePort, udp)
		},
	}

	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint (optional when a default session is set via 'wendy auth use')")
	cmd.Flags().StringVar(&deviceName, "device", "", "Device name (skips interactive picker)")
	cmd.Flags().StringVar(&brokerURL, "broker-url", os.Getenv("WENDY_BROKER_URL"), "Tunnel broker host:port (default: cloud :443 endpoint, otherwise <cloud-host>:50052)")

	return cmd
}

// promptTunnelMapping asks for a port mapping until one parses. Ctrl+C, or a
// signal while the prompt is open, cancels quietly, as the device picker and
// the running tunnel do.
func promptTunnelMapping(cmd *cobra.Command) (localPort, remotePort uint32, udp bool, err error) {
	ctx := cmd.Context()
	mapping, err := tui.PromptText(
		"Port mapping",
		"local:remote or port (e.g. 8080:3000); add /udp for UDP",
		validateTunnelMapping,
		tea.WithContext(ctx),
		tea.WithInput(cmd.InOrStdin()),
		tea.WithOutput(cmd.OutOrStdout()),
	)
	if ctx.Err() != nil || errors.Is(err, tui.ErrCancelled) || errors.Is(err, tea.ErrInterrupted) {
		return 0, 0, false, ErrUserCancelled
	}
	if err != nil {
		return 0, 0, false, err
	}
	return parseTunnelArg(mapping)
}

// validateTunnelMapping is the prompt's validator. An empty submission asks
// for a mapping rather than reporting an invalid empty port.
func validateTunnelMapping(value string) error {
	if value == "" {
		return errors.New("enter a port mapping, e.g. 8080:3000")
	}
	_, _, _, err := parseTunnelArg(value)
	return err
}

// parseTunnelArg parses "localPort:remotePort" or "port", with an optional
// docker-style "/udp" (or explicit "/tcp") protocol suffix.
func parseTunnelArg(arg string) (localPort, remotePort uint32, udp bool, err error) {
	if i := strings.LastIndex(arg, "/"); i >= 0 {
		switch strings.ToLower(arg[i+1:]) {
		case "udp":
			udp = true
		case "tcp":
		default:
			return 0, 0, false, fmt.Errorf("unknown protocol %q (use tcp or udp)", arg[i+1:])
		}
		arg = arg[:i]
	}
	parts := strings.SplitN(arg, ":", 2)
	parse := func(s string) (uint32, error) {
		n, e := strconv.ParseUint(s, 10, 32)
		if e != nil || n == 0 || n > 65535 {
			return 0, fmt.Errorf("invalid port %q", s)
		}
		return uint32(n), nil
	}
	if len(parts) == 1 {
		p, e := parse(parts[0])
		return p, p, udp, e
	}
	lp, e := parse(parts[0])
	if e != nil {
		return 0, 0, false, e
	}
	rp, e := parse(parts[1])
	return lp, rp, udp, e
}

func cloudTunnelCommand(ctx context.Context, auth *config.AuthConfig, deviceName, brokerURL string, localPort, remotePort uint32, udp bool) error {
	cliLogln("Fetching device list from cloud...")
	asset, err := pickCloudDiscoveryDevice(ctx, auth, deviceName, brokerURL)
	if err != nil {
		return err
	}

	var brokerConn *grpc.ClientConn
	if asset.legacy != nil {
		brokerConn, err = clouddefaults.DialBroker(auth, brokerURL)
		if err != nil {
			return err
		}
		defer brokerConn.Close()
	} else {
		if brokerURL != "" {
			return fmt.Errorf("Cloud selects the authorized relay; --broker-url is supported only for legacy sessions")
		}
	}

	if udp {
		pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(localPort)})
		if err != nil {
			return fmt.Errorf("listening on udp 127.0.0.1:%d: %w", localPort, err)
		}
		defer pc.Close()
		if err := pc.SetWriteBuffer(65507); err != nil {
			return fmt.Errorf("configuring UDP forwarding buffer: %w", err)
		}
		session, err := asset.openDatagram(ctx, brokerConn, auth, remotePort)
		if err != nil {
			return datagramOpenError(err, asset.GetName())
		}
		defer session.close()
		cliSuccess("Forwarding udp 127.0.0.1:%d → %s:%d (via cloud)", localPort, asset.GetName(), remotePort)
		cliLogln("Press Ctrl+C to stop.")
		go func() { <-ctx.Done(); pc.Close() }()
		udpErr := serveUDPForward(ctx, pc, session, remotePort, udpFlowIdleTimeout)
		if ctx.Err() != nil {
			return nil
		}
		return datagramOpenError(udpErr, asset.GetName())
	}

	listenAddr := fmt.Sprintf("127.0.0.1:%d", localPort)
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", listenAddr, err)
	}
	defer ln.Close()

	cliSuccess("Forwarding %s → %s:%d (via cloud)", listenAddr, asset.GetName(), remotePort)
	cliLogln("Press Ctrl+C to stop.")

	return asset.serveTunnelForward(ctx, ln, brokerConn, auth, remotePort, func(err error) {
		cliLogln("Cloud tunnel failed: %v", err)
	}, newUnansweredNotice(func() {
		cliLogln("The device closed a connection to port %d without sending anything. Nothing may be listening on 127.0.0.1:%d on the device; an app that listens only on another address cannot be reached through the tunnel.", remotePort, remotePort)
	}))
}

// serveTunnelForward relays every connection accepted on ln through its own
// tunnel to remotePort. The connections share one TunnelSession: each still
// requests its own grant, over the session's authorization connection.
func (d cloudDiscoveryDevice) serveTunnelForward(ctx context.Context, ln net.Listener, brokerConn *grpc.ClientConn, auth *config.AuthConfig, remotePort uint32, dialFailed func(error), unanswered *unansweredNotice) error {
	var session TunnelSession
	session.SetActive(true)
	defer session.SetActive(false)
	return serveTCPForward(session.Context(ctx), ln, func(ctx context.Context) (net.Conn, error) {
		return d.openTunnel(ctx, brokerConn, auth, remotePort)
	}, dialFailed, unanswered)
}

// serveTCPForward accepts connections on ln until ctx ends and relays each one
// through its own tunnel connection from dial. It closes ln when ctx ends and
// then returns nil; any other accept failure is returned. A failed dial drops
// that one local connection and is reported to dialFailed. A connection the
// device closes without an answer is reported to unanswered, when one is
// given. `wendy cloud tunnel` and the tunnel `wendy run` opens to a cloud app
// (cloudAppTunnel) both forward through it.
func serveTCPForward(ctx context.Context, ln net.Listener, dial func(context.Context) (net.Conn, error), dialFailed func(error), unanswered *unansweredNotice) error {
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	for {
		tcpConn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accepting connection: %w", err)
		}
		go serveForwardedConn(ctx, tcpConn, dial, dialFailed, unanswered)
	}
}

func serveForwardedConn(ctx context.Context, tcpConn net.Conn, dial func(context.Context) (net.Conn, error), dialFailed func(error), unanswered *unansweredNotice) {
	defer tcpConn.Close()
	tunnel, err := dial(ctx)
	if err != nil {
		dialFailed(err)
		return
	}
	if unanswered == nil {
		relayTunnel(ctx, tcpConn, tunnel)
		return
	}
	opened := unanswered.now()
	if relayTunnel(ctx, tcpConn, tunnel) && unanswered.now().Sub(opened) <= unanswered.window {
		unanswered.report()
	}
}

const (
	// unansweredWindow is how soon after its tunnel opens a forwarded
	// connection must end to be reported as unanswered. A device with nothing
	// on the port ends it within moments; a later end is more likely the
	// app's own, such as an idle timeout.
	unansweredWindow = 15 * time.Second
	// unansweredNoticeInterval spaces the notices, so a client that retries
	// or opens several connections at once gets one line, not one each.
	unansweredNoticeInterval = 10 * time.Second
)

// unansweredNotice tells the user about forwarded connections that end the
// way they do when nothing listens on the device's port: the device ends the
// tunnel within window of it opening, without sending a byte, while the local
// client still waits. The device's own reason stays in its log, so the notice
// names a likely cause, at most once per interval.
type unansweredNotice struct {
	window, interval time.Duration
	now              func() time.Time
	notify           func()

	mu       sync.Mutex
	notified time.Time // when notify last ran; zero until it has
}

func newUnansweredNotice(notify func()) *unansweredNotice {
	return &unansweredNotice{window: unansweredWindow, interval: unansweredNoticeInterval, now: time.Now, notify: notify}
}

// report notifies of one unanswered connection, unless a notice went out less
// than interval ago.
func (u *unansweredNotice) report() {
	now := u.now()
	u.mu.Lock()
	due := u.notified.IsZero() || now.Sub(u.notified) >= u.interval
	if due {
		u.notified = now
	}
	u.mu.Unlock()
	if due {
		u.notify()
	}
}

func serveTunnelConn(ctx context.Context, tcpConn net.Conn, brokerConn *grpc.ClientConn, auth *config.AuthConfig, assetID int32, remotePort uint32) {
	defer tcpConn.Close()

	tunnelConn, err := openBrokerTunnel(ctx, brokerConn, auth, assetID, remotePort)
	if err != nil {
		return
	}
	relayTunnel(ctx, tcpConn, tunnelConn)
}

// relayTunnel copies between a forwarded local connection and its tunnel
// until the tunnel's side ends or ctx does. The local client finishing its
// sending does not end the relay: the device may still answer. The tunnel
// ending does: nothing more can come back, and a tunnel whose device side has
// ended takes nothing more either, so the local connection is closed at once
// rather than left open until its client gives up waiting.
//
// It reports whether the relay ended the way it does when nothing answers on
// the device: the tunnel ended cleanly, without a byte from the device, while
// the local client was still connected.
func relayTunnel(ctx context.Context, tcpConn, tunnelConn io.ReadWriteCloser) (unanswered bool) {
	defer tcpConn.Close()
	defer tunnelConn.Close()
	local := &endNotingReader{Reader: tcpConn}
	go func() { _, _ = io.Copy(tunnelConn, local) }()
	type copied struct {
		n   int64
		err error
	}
	fromTunnel := make(chan copied, 1)
	go func() {
		n, err := io.Copy(tcpConn, tunnelConn)
		fromTunnel <- copied{n, err}
	}()
	select {
	case <-ctx.Done():
		return false
	case c := <-fromTunnel:
		// A tunnel that ctx ended also reads as a clean end.
		return c.n == 0 && c.err == nil && !local.ended.Load() && ctx.Err() == nil
	}
}

// endNotingReader notes when its source has ended, by EOF or by an error.
type endNotingReader struct {
	io.Reader
	ended atomic.Bool
}

func (r *endNotingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil {
		r.ended.Store(true)
	}
	return n, err
}
