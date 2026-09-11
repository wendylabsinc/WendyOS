package commands

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func newCloudTunnelCmd() *cobra.Command {
	var cloudGRPC string
	var deviceName string
	var brokerURL string

	cmd := &cobra.Command{
		Use:   "tunnel <local-port>:<remote-port>[/udp]",
		Short: "Forward a local TCP or UDP port to a port on a cloud-enrolled device",
		Long:  "Listens on <local-port> and forwards each connection through the Wendy Cloud tunnel broker to <remote-port> on the target device.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			localPort, remotePort, udp, err := parseTunnelArg(args[0])
			if err != nil {
				return err
			}
			return cloudTunnelCommand(cmd.Context(), cloudGRPC, effectiveDeviceName(deviceName), brokerURL, localPort, remotePort, udp)
		},
	}

	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint (optional when a default session is set via 'wendy auth use')")
	cmd.Flags().StringVar(&deviceName, "device", "", "Device name (skips interactive picker)")
	cmd.Flags().StringVar(&brokerURL, "broker-url", os.Getenv("WENDY_BROKER_URL"), "Tunnel broker host:port (default: cloud :443 endpoint, otherwise <cloud-host>:50052)")

	return cmd
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

func cloudTunnelCommand(ctx context.Context, cloudGRPC, deviceName, brokerURL string, localPort, remotePort uint32, udp bool) error {
	auth, err := pickAuthEntry(cloudGRPC)
	if err != nil {
		return err
	}

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
		if udp {
			return fmt.Errorf("Cloud's authorized service catalog does not expose UDP forwarding")
		}
		if remotePort != 22 && remotePort != 50052 {
			return fmt.Errorf("Cloud's authorized service catalog has no service for port %d", remotePort)
		}
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
		session, err := openDatagramSession(ctx, brokerConn, auth, asset.legacy.GetId())
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

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		tcpConn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accepting connection: %w", err)
		}
		go serveSelectedTunnelConn(ctx, tcpConn, brokerConn, auth, asset, remotePort)
	}
}

func serveSelectedTunnelConn(ctx context.Context, tcpConn net.Conn, brokerConn *grpc.ClientConn, auth *config.AuthConfig, asset cloudDiscoveryDevice, remotePort uint32) {
	defer tcpConn.Close()
	tunnel, err := asset.openTunnel(ctx, brokerConn, auth, remotePort)
	if err != nil {
		cliLogln("Cloud tunnel failed: %v", err)
		return
	}
	defer tunnel.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(tunnel, tcpConn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(tcpConn, tunnel); done <- struct{}{} }()
	select {
	case <-ctx.Done():
	case <-done:
	}
	_ = tcpConn.Close()
	_ = tunnel.Close()
}

func serveTunnelConn(ctx context.Context, tcpConn net.Conn, brokerConn *grpc.ClientConn, auth *config.AuthConfig, assetID int32, remotePort uint32) {
	defer tcpConn.Close()

	tunnelConn, err := openBrokerTunnel(ctx, brokerConn, auth, assetID, remotePort)
	if err != nil {
		return
	}
	defer tunnelConn.Close()

	done := make(chan struct{}, 2)
	relay := func(dst io.Writer, src io.Reader) {
		defer func() { done <- struct{}{} }()
		_, _ = io.Copy(dst, src)
	}
	go relay(tunnelConn, tcpConn)
	go relay(tcpConn, tunnelConn)
	<-done
	<-done // wait for both directions before closing connections
}
