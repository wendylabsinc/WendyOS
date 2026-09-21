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

// shellNeedsTTY reports whether the invocation is a bare login shell, which
// only makes sense against a local terminal. An explicit `-- command` is a
// legitimate batch invocation and runs fine without one: the device-side
// server always allocates a PTY, so its output arrives CRLF-terminated with
// stderr merged into stdout.
func shellNeedsTTY(shellCmd []string) bool {
	return len(shellCmd) == 0
}

// buildShellStart builds the first HostShell frame. An empty command tells the
// agent to resolve and run the login shell; a command after `--` runs that argv.
func buildShellStart(cmd []string, rows, cols uint32) *agentpb.HostShellRequest {
	return &agentpb.HostShellRequest{RequestType: &agentpb.HostShellRequest_Start_{
		Start: &agentpb.HostShellRequest_Start{
			Command:  cmd,
			TermSize: &agentpb.WindowSize{Rows: rows, Cols: cols},
		},
	}}
}

func shellWinSizeFrame(rows, cols uint32) *agentpb.HostShellRequest {
	return &agentpb.HostShellRequest{RequestType: &agentpb.HostShellRequest_Resize{
		Resize: &agentpb.WindowSize{Rows: rows, Cols: cols},
	}}
}

func newDeviceShellCmd() *cobra.Command {
	return &cobra.Command{
		Hidden: true,
		Use:    "shell [-- command...]",
		Short:  "Open an interactive shell on the device host",
		Long: "Open a full interactive TTY on the device host (the device's root\n" +
			"filesystem, not a container), running the login shell by default or a\n" +
			"command given after `--`. Uses the existing mTLS/PKI trust; the shell\n" +
			"runs as root.\n\n" +
			"With `--`, no local terminal is required: the command can be run\n" +
			"non-interactively (e.g. piped or scripted). The device still runs it\n" +
			"in a PTY, so output is CRLF-terminated and stderr is merged into\n" +
			"stdout.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var shellCmd []string
			if d := cmd.ArgsLenAtDash(); d >= 0 {
				shellCmd = args[d:]
			}
			return runDeviceShell(cmd, shellCmd)
		},
	}
}

func runDeviceShell(cmd *cobra.Command, shellCmd []string) error {
	fd := int(os.Stdin.Fd())
	interactive := term.IsTerminal(fd)
	if !interactive && shellNeedsTTY(shellCmd) {
		return fmt.Errorf("interactive device shell requires a terminal; run a command instead: wendy device shell -- <command>")
	}

	ctx := cmd.Context()
	conn, err := connectToAgent(ctx, SuppressProvisioningHint())
	if err != nil {
		return err
	}
	defer conn.Close()

	stream, err := conn.ShellService.HostShell(ctx)
	if err != nil {
		return fmt.Errorf("opening shell stream: %w", err)
	}

	// gRPC forbids concurrent SendMsg on one stream; the start frame, resize
	// watcher, and stdin pump all send through this lock.
	var sendMu sync.Mutex
	send := func(req *agentpb.HostShellRequest) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(req)
	}

	rows, cols := termSize(fd)
	if err := send(buildShellStart(shellCmd, rows, cols)); err != nil {
		return fmt.Errorf("sending shell start: %w", err)
	}

	if interactive {
		oldState, rawErr := term.MakeRaw(fd)
		if rawErr == nil {
			defer func() { _ = term.Restore(fd, oldState) }()
		}

		// Terminal resize -> resize frames (SIGWINCH on Unix; no-op on
		// Windows). Only meaningful when we actually own a terminal.
		winch := make(chan os.Signal, 1)
		stopResize := notifyTerminalResize(winch)
		defer stopResize()
		go func() {
			for range winch {
				r, c := termSize(fd)
				_ = send(shellWinSizeFrame(r, c))
			}
		}()
	}

	// stdin -> stream.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := os.Stdin.Read(buf)
			if n > 0 {
				_ = send(&agentpb.HostShellRequest{
					RequestType: &agentpb.HostShellRequest_StdinData{StdinData: append([]byte(nil), buf[:n]...)},
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

	// stream -> stdout; exit on the final exit_code frame.
	for {
		resp, rerr := stream.Recv()
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
		if len(resp.GetStdoutData()) > 0 {
			_, _ = os.Stdout.Write(resp.GetStdoutData())
			continue
		}
		if _, ok := resp.GetResponseType().(*agentpb.HostShellResponse_ExitCode); ok {
			if code := resp.GetExitCode(); code != 0 {
				return fmt.Errorf("remote shell exited with code %d", code)
			}
			return nil
		}
	}
}
