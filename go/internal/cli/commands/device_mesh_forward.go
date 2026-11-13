package commands

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshappstream"
)

const meshForwardFlows = 8

var dialLiteMeshApp = sessionbroker.DialOperatorApp

func newDeviceMeshForwardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "forward <local-port>:<remote-port>",
		Short: "Forward a local TCP port to a running mesh app",
		Long:  "Listen on loopback and open an authorized app stream through the selected device's authenticated agent connection, or native app TLS for a routed Lite service. Mesh selectors reuse their retained operator router. Lite ports must be declared in the current signed manifest; destination WAS1 admission still checks the running app. Each connection owns its own stream; the listener supports up to eight simultaneous connections.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			localPort, remotePort, udp, err := parseTunnelArg(args[0])
			if err != nil {
				return err
			}
			if udp {
				return fmt.Errorf("mesh forward currently supports TCP")
			}
			target, err := resolveTarget(cmd.Context(), SuppressUpdateCheck())
			if err != nil {
				return err
			}
			defer target.Close()
			dial, label, err := meshAppForwardDialer(target, uint16(remotePort))
			if err != nil {
				return err
			}
			listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
			if err != nil {
				return err
			}
			defer listener.Close()
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Forwarding %s to running app port %d on %s\n", listener.Addr(), remotePort, label); err != nil {
				return err
			}
			return serveMeshForward(cmd.Context(), listener, func(ctx context.Context) (net.Conn, error) {
				connection, err := dial(ctx)
				if err != nil {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Mesh app connection failed: %v\n", err)
				}
				return connection, err
			})
		},
	}
}

func meshAppForwardDialer(target *SelectedDevice, port uint16) (func(context.Context) (net.Conn, error), string, error) {
	if target.External != nil && target.External.ProviderKey == "wendy-lite" {
		selector := target.External.ConnectionInfo["selector"]
		if _, matched, err := sessionbroker.ParseMeshSelector(selector); err != nil || !matched {
			return nil, "", fmt.Errorf("Lite app forwarding requires a routed mesh://tenant/device selector")
		}
		return func(ctx context.Context) (net.Conn, error) { return dialLiteMeshApp(ctx, selector, port) }, selector, nil
	}
	if target.Agent == nil || target.Agent.Conn == nil || !target.Agent.IsMTLS {
		return nil, "", fmt.Errorf("mesh app forwarding requires an authenticated WendyOS agent or routed Lite service")
	}
	return func(ctx context.Context) (net.Conn, error) { return meshappstream.Dial(ctx, target.Agent.Conn, port) }, target.Agent.Host, nil
}

// A full listener refuses another accepted connection rather than creating a
// goroutine or queue for it. Ctrl+C closes the listener and every owned flow.
func serveMeshForward(ctx context.Context, listener net.Listener, dial func(context.Context) (net.Conn, error)) error {
	owner, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(owner, func() { _ = listener.Close() })
	defer stop()
	var flows sync.WaitGroup
	defer flows.Wait()
	admission := make(chan struct{}, meshForwardFlows)
	for {
		local, err := listener.Accept()
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case admission <- struct{}{}:
		default:
			_ = local.Close()
			continue
		}
		flows.Add(1)
		go func() {
			defer flows.Done()
			defer func() { <-admission }()
			defer local.Close()
			setup, stop := context.WithTimeout(owner, meshsession.TLSAppSessionSetupTimeout)
			remote, err := dial(setup)
			stop()
			if err != nil {
				return
			}
			defer remote.Close()
			relayMeshForward(owner, local, remote)
		}()
	}
}

func relayMeshForward(ctx context.Context, local, remote net.Conn) {
	stop := context.AfterFunc(ctx, func() { _ = local.Close(); _ = remote.Close() })
	defer stop()
	completed := make(chan error, 2)
	copyDirection := func(destination, source net.Conn) {
		_, err := io.CopyBuffer(destination, source, make([]byte, meshappstream.ChunkSize))
		if err == nil {
			if half, ok := destination.(interface{ CloseWrite() error }); ok {
				err = half.CloseWrite()
			}
		}
		completed <- err
	}
	go copyDirection(remote, local)
	go copyDirection(local, remote)
	for count := 0; count < 2; count++ {
		select {
		case err := <-completed:
			if err != nil {
				_ = local.Close()
				_ = remote.Close()
			}
		case <-ctx.Done():
			_ = local.Close()
			_ = remote.Close()
			// Join both copies after closing their actual owned sockets.
			for ; count < 2; count++ {
				<-completed
			}
			return
		}
	}
}
