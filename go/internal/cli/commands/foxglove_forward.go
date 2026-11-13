package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// openFoxgloveConnection reaches the bridge's loopback listener through the
// already selected agent connection. Non-TTY exec preserves WebSocket bytes
// and keeps socat's diagnostics separate from the connection's data.
func openFoxgloveConnection(ctx context.Context, client agentpb.WendyContainerServiceClient) (net.Conn, error) {
	return openContainerConnection(ctx, client, foxgloveAppID, []string{"socat", "STDIO", "TCP:127.0.0.1:8765"}, "Foxglove relay")
}

// openContainerConnection adapts non-TTY exec stdin/stdout to a byte stream.
// Callers supply a fixed command; stderr never enters the forwarded protocol.
func openContainerConnection(ctx context.Context, client agentpb.WendyContainerServiceClient, app string, command []string, label string) (net.Conn, error) {
	execCtx, cancel := context.WithCancel(ctx)
	stream, err := client.ExecContainer(execCtx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("opening %s: %w", label, err)
	}
	if err := stream.Send(&agentpb.ExecContainerRequest{
		RequestType: &agentpb.ExecContainerRequest_Start{Start: &agentpb.ExecContainerRequest_ExecStart{
			AppName: app,
			Command: command,
			Tty:     false,
		}},
	}); err != nil {
		cancel()
		return nil, fmt.Errorf("starting %s: %w", label, err)
	}

	// Only the receive pump accesses stderr. Retain a bounded tail so a failed
	// exec reports its diagnostics without accumulating arbitrary process output.
	const stderrLimit = 4096
	var stderr []byte
	relayError := func(err error) error {
		if detail := strings.TrimSpace(string(stderr)); detail != "" {
			return fmt.Errorf("%s: %w: %s", label, err, detail)
		}
		return fmt.Errorf("%s: %w", label, err)
	}
	tunnel := pipeBrokerTunnel(func() ([]byte, bool, error) {
		for {
			resp, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					return nil, false, relayError(io.ErrUnexpectedEOF)
				}
				return nil, false, relayError(err)
			}
			switch value := resp.GetResponseType().(type) {
			case *agentpb.ExecContainerResponse_StdoutData:
				return value.StdoutData, false, nil
			case *agentpb.ExecContainerResponse_StderrData:
				data := value.StderrData
				if len(data) >= stderrLimit {
					stderr = append(stderr[:0], data[len(data)-stderrLimit:]...)
				} else {
					if extra := len(stderr) + len(data) - stderrLimit; extra > 0 {
						stderr = stderr[extra:]
					}
					stderr = append(stderr, data...)
				}
			case *agentpb.ExecContainerResponse_ExitCode:
				if value.ExitCode != 0 {
					return nil, false, relayError(fmt.Errorf("exited with code %d", value.ExitCode))
				}
				return nil, true, nil
			}
		}
	}, func(data []byte, halfClose bool) error {
		if halfClose {
			// runTunnelUplink calls CloseSend after this marker and any queued
			// data. It is the sole sender, including the half-close operation.
			return nil
		}
		return stream.Send(&agentpb.ExecContainerRequest{
			RequestType: &agentpb.ExecContainerRequest_StdinData{StdinData: data},
		})
	}, stream.CloseSend)
	return &foxgloveExecConnection{Conn: tunnel, cancel: cancel}, nil
}

type foxgloveExecConnection struct {
	net.Conn
	cancel context.CancelFunc
}

func (c *foxgloveExecConnection) Close() error {
	c.cancel()
	return c.Conn.Close()
}

// The shared pipe preserves its terminal error using a broker-specific wrapper.
// This connection uses agent exec, so expose its original relay error instead.
func foxgloveConnectionError(err error) error {
	var closed *clouddefaults.TunnelClosedError
	if errors.As(err, &closed) {
		return closed.Reason
	}
	return err
}

func (c *foxgloveExecConnection) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	return n, foxgloveConnectionError(err)
}

func (c *foxgloveExecConnection) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	return n, foxgloveConnectionError(err)
}

// serveFoxgloveForward owns listener until it returns. Each local connection
// has one exec stream; closing either side tears down both copy directions.
func serveFoxgloveForward(ctx context.Context, listener net.Listener, client agentpb.WendyContainerServiceClient) error {
	ctx, cancel := context.WithCancel(ctx)
	var active sync.WaitGroup
	stopClose := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer func() {
		cancel()
		_ = listener.Close()
		stopClose()
		active.Wait()
	}()
	for {
		local, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accepting Foxglove connection: %w", err)
		}
		active.Add(1)
		go func() {
			defer active.Done()
			if err := forwardFoxgloveConnection(ctx, local, client); err != nil && ctx.Err() == nil {
				cliLogln("Foxglove connection failed: %v", err)
			}
		}()
	}
}

func forwardFoxgloveConnection(ctx context.Context, local net.Conn, client agentpb.WendyContainerServiceClient) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer local.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = local.Close() })
	defer stopClose()
	remote, err := openFoxgloveConnection(ctx, client)
	if err != nil {
		return err
	}
	defer remote.Close()
	done := make(chan error, 2)
	go func() { _, err := io.Copy(remote, local); done <- err }()
	go func() { _, err := io.Copy(local, remote); done <- err }()
	err = <-done
	cancel()
	_ = local.Close()
	_ = remote.Close()
	<-done
	return err
}
