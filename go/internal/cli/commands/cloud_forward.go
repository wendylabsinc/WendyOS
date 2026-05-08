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

	"github.com/wendylabsinc/wendy/internal/shared/config"
)

func newCloudTunnelCmd() *cobra.Command {
	var cloudGRPC string
	var deviceName string
	var brokerURL string

	cmd := &cobra.Command{
		Use:   "tunnel <local-port>:<remote-port>",
		Short: "Forward a local TCP port to a port on a cloud-enrolled device",
		Long:  "Listens on <local-port> and forwards each connection through the Wendy Cloud tunnel broker to <remote-port> on the target device.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			localPort, remotePort, err := parseTunnelArg(args[0])
			if err != nil {
				return err
			}
			return cloudTunnelCommand(cmd.Context(), cloudGRPC, deviceName, brokerURL, localPort, remotePort)
		},
	}

	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint (required when multiple auth sessions exist)")
	cmd.Flags().StringVar(&deviceName, "device", "", "Device name (skips interactive picker)")
	cmd.Flags().StringVar(&brokerURL, "broker-url", os.Getenv("WENDY_BROKER_URL"), "Tunnel broker host:port (default: cloud :443 endpoint, otherwise <cloud-host>:50052)")

	return cmd
}

// parseTunnelArg parses "localPort:remotePort" or just "port" (same for both sides).
func parseTunnelArg(arg string) (localPort, remotePort uint32, err error) {
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
		return p, p, e
	}
	lp, e := parse(parts[0])
	if e != nil {
		return 0, 0, e
	}
	rp, e := parse(parts[1])
	return lp, rp, e
}

func cloudTunnelCommand(ctx context.Context, cloudGRPC, deviceName, brokerURL string, localPort, remotePort uint32) error {
	auth, err := pickAuthEntry(cloudGRPC)
	if err != nil {
		return err
	}

	cliLogln("Fetching device list from cloud...")
	asset, err := pickCloudDevice(ctx, auth, deviceName, brokerURL)
	if err != nil {
		return err
	}

	brokerConn, err := dialCloudBroker(auth, brokerURL)
	if err != nil {
		return err
	}
	defer brokerConn.Close()

	listenAddr := fmt.Sprintf("127.0.0.1:%d", localPort)
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", listenAddr, err)
	}
	defer ln.Close()

	cliSuccess("Forwarding %s → %s (cloud) → localhost:%d", listenAddr, asset.GetName(), remotePort)
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
		go serveTunnelConn(ctx, tcpConn, brokerConn, auth, asset.GetId(), remotePort)
	}
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
}
