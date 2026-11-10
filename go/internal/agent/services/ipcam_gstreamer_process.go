package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/agent/executil"
	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
)

const ipCameraGStreamerUtility = "ipcam-gstreamer"

// newIPCameraGStreamerCommand builds the credential-safe helper invocation.
// The pipeline itself is sent over stdin after Start, so neither the RTSP URL
// nor its password can appear in argv or the environment.
func newIPCameraGStreamerCommand(ctx context.Context, executable string) *exec.Cmd {
	return exec.CommandContext(ctx, executable, "utils", ipCameraGStreamerUtility)
}

// gstreamerFrames runs the in-process GStreamer API inside a short-lived copy of
// the agent executable. This keeps plugins isolated from the server process while
// moving the credential-bearing pipeline over a private stdin pipe instead of a
// process argument.
//
// Helper stderr is returned, but only after redaction: a GStreamer diagnostic may
// repeat element property values, and the location property carries the camera
// password. Discarding it instead, as this did originally, collapses every
// distinct failure — no route to the camera, wrong password, missing plugin —
// into one message that names none of them.
func (s *VideoService) gstreamerFrames(ctx context.Context, args []string, onFrame func([]byte)) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating GStreamer helper: %w", err)
	}
	cmd := newIPCameraGStreamerCommand(ctx, executable)
	executil.ResetOOMScore(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("creating GStreamer input pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("creating GStreamer output pipe: %w", err)
	}
	// Bounded so a misbehaving child cannot exhaust the heap through stderr.
	diagnostics := &limitedBuffer{limit: maxStderrBytes}
	cmd.Stderr = diagnostics
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting GStreamer helper: %w", err)
	}

	encodeErr := json.NewEncoder(stdin).Encode(args)
	closeErr := stdin.Close()
	if encodeErr != nil {
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
		return errors.New("sending GStreamer pipeline failed")
	}
	if closeErr != nil {
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
		return errors.New("closing GStreamer pipeline input failed")
	}

	buf := make([]byte, 64*1024)
	for {
		n, readErr := stdout.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			onFrame(chunk)
		}
		if readErr != nil {
			if readErr != io.EOF && ctx.Err() == nil {
				cmd.Process.Kill() //nolint:errcheck
				cmd.Wait()         //nolint:errcheck
				return errors.New("reading GStreamer output failed")
			}
			break
		}
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return nil
	}
	if waitErr != nil {
		return gstreamerFailure(diagnostics, args)
	}
	return nil
}

// gstreamerFailure builds the capture error, carrying the helper's own
// diagnostic when there is one and it survives redaction.
func gstreamerFailure(diagnostics *limitedBuffer, args []string) error {
	detail := ipcam.RedactText(strings.TrimSpace(diagnostics.buf.String()), ipcam.SecretsIn(args)...)
	// Collapse to a single line: this is joined into a gRPC status message, and a
	// multi-line status renders badly wherever it is finally printed.
	detail = strings.Join(strings.Fields(detail), " ")
	if detail == "" {
		return errors.New("GStreamer capture pipeline failed")
	}
	return fmt.Errorf("GStreamer capture pipeline failed: %s", detail)
}
