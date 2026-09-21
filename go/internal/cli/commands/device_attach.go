package commands

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"golang.org/x/term"
)

// buildExecStart builds the first ExecContainer frame. An empty command defaults
// to `claude` (the claude-on-device app's purpose); callers pass an explicit
// command after `--` to run something else.
func buildExecStart(app string, cmd []string, rows, cols uint32) *agentpb.ExecContainerRequest {
	if len(cmd) == 0 {
		cmd = []string{"claude"}
	}
	return &agentpb.ExecContainerRequest{RequestType: &agentpb.ExecContainerRequest_Start{
		Start: &agentpb.ExecContainerRequest_ExecStart{
			AppName:  app,
			Command:  cmd,
			Tty:      true,
			TermSize: &agentpb.WindowSize{Rows: rows, Cols: cols},
		},
	}}
}

func winSizeFrame(rows, cols uint32) *agentpb.ExecContainerRequest {
	return &agentpb.ExecContainerRequest{RequestType: &agentpb.ExecContainerRequest_Resize{
		Resize: &agentpb.WindowSize{Rows: rows, Cols: cols},
	}}
}

// termSize returns the current terminal size as (rows, cols), defaulting to a
// sane 24x80 when stdin is not a sized terminal.
func termSize(fd int) (rows, cols uint32) {
	w, h, err := term.GetSize(fd)
	if err != nil || w == 0 || h == 0 {
		return 24, 80
	}
	return uint32(h), uint32(w)
}

func newDeviceAttachCmd() *cobra.Command {
	return &cobra.Command{
		Hidden: true,
		Use:    "attach <app> [-- command...]",
		Short:  "Attach an interactive PTY to a running app's container",
		Long: "Attach an interactive terminal to a running app's container, running\n" +
			"`claude` by default (or a command given after `--`). Used to drive the\n" +
			"claude-on-device app, but works for execing into any running container.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := args[0]
			var execCmd []string
			if d := cmd.ArgsLenAtDash(); d >= 0 {
				execCmd = args[d:]
			}
			return runDeviceAttach(cmd, app, execCmd)
		},
	}
}

func runDeviceAttach(cmd *cobra.Command, app string, execCmd []string) error {
	ctx := cmd.Context()
	conn, err := connectToAgent(ctx, SuppressProvisioningHint())
	if err != nil {
		return err
	}
	defer conn.Close()

	stream, err := conn.ContainerService.ExecContainer(ctx)
	if err != nil {
		return fmt.Errorf("opening exec stream: %w", err)
	}

	// gRPC forbids concurrent SendMsg on one stream, so the initial start frame,
	// the resize watcher, and the stdin pump all send through this lock.
	var sendMu sync.Mutex
	send := func(req *agentpb.ExecContainerRequest) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(req)
	}

	fd := int(os.Stdin.Fd())
	rows, cols := termSize(fd)
	if err := send(buildExecStart(app, execCmd, rows, cols)); err != nil {
		return fmt.Errorf("sending exec start: %w", err)
	}

	if term.IsTerminal(fd) {
		oldState, rawErr := term.MakeRaw(fd)
		if rawErr == nil {
			defer func() { _ = term.Restore(fd, oldState) }()
		}
	}

	// Terminal resize -> resize frames. Only wired up on Unix (SIGWINCH);
	// a no-op on Windows, which has no equivalent signal.
	winch := make(chan os.Signal, 1)
	stopResize := notifyTerminalResize(winch)
	defer stopResize()
	go func() {
		for range winch {
			r, c := termSize(fd)
			_ = send(winSizeFrame(r, c))
		}
	}()

	// stdin -> stream.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := os.Stdin.Read(buf)
			if n > 0 {
				_ = send(&agentpb.ExecContainerRequest{
					RequestType: &agentpb.ExecContainerRequest_StdinData{StdinData: append([]byte(nil), buf[:n]...)},
				})
			}
			if rerr != nil {
				sendMu.Lock()
				_ = stream.CloseSend()
				sendMu.Unlock()
				return
			}
		}
	}()

	// stream -> stdout/stderr; exit on the final exit_code frame.
	for {
		resp, rerr := stream.Recv()
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
		switch {
		case len(resp.GetStdoutData()) > 0:
			_, _ = os.Stdout.Write(resp.GetStdoutData())
		case len(resp.GetStderrData()) > 0:
			_, _ = os.Stderr.Write(resp.GetStderrData())
		default:
			if _, ok := resp.GetResponseType().(*agentpb.ExecContainerResponse_ExitCode); ok {
				if code := resp.GetExitCode(); code != 0 {
					return fmt.Errorf("remote process exited with code %d", code)
				}
				return nil
			}
		}
	}
}
