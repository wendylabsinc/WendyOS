//go:build linux

package audioloop

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

func defaultDeps() deps {
	return deps{
		modprobe:  modprobeSndAloop,
		newWriter: newAplayWriter,
	}
}

// modprobeSndAloop loads snd-aloop. Mirrors ipcam.modprobeLoopback's
// idempotent shape: modprobe itself already treats an already-loaded module
// as success, so this is a single shell-out with no special-casing needed.
func modprobeSndAloop(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "modprobe", "snd-aloop").CombinedOutput()
	if err != nil {
		return fmt.Errorf("modprobe snd-aloop: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// aplayWriter is an AudioWriter backed by a running `aplay` process fed over
// a pipe.
type aplayWriter struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	closed bool
}

func newAplayWriter(ctx context.Context, hwID string, f PCMFormat) (AudioWriter, error) {
	cmd := exec.CommandContext(ctx, "aplay", aplayArgs(hwID, f)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("aplay stdin pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting aplay: %w", err)
	}
	return &aplayWriter{cmd: cmd, stdin: stdin}, nil
}

func (w *aplayWriter) WritePCM(pcm []byte) error {
	_, err := w.stdin.Write(pcm)
	return err
}

func (w *aplayWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	_ = w.stdin.Close()
	return w.cmd.Wait()
}
