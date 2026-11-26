//go:build !windows

package commands

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// A04: decline a real controlling-terminal prompt from all three valid states.
// Interactive EOF is deliberately not claimed: closing a PTY delivers SIGHUP,
// which is not evidence that the prompt itself handled EOF.
func TestAuthModesA04TerminalDecline(t *testing.T) {
	for _, action := range []string{"close", "open", "remove"} {
		t.Run(action, func(t *testing.T) {
			c := authComputerFor(t)
			d := authDevice(t)
			args := []string{"device", "access", action}
			if action != "close" {
				c.ok(t, d.Addr, "Closed.", "device", "access", "close", "--yes")
				d.Restart(t)
			}
			if action == "remove" {
				other := authComputerFor(t)
				authAdd(t, d, c, other, "viewer")
				args = []string{"device", "keys", "remove", other.fingerprint}
			}
			before := authSnapshot(t, d)
			closed := d.Keys.Closed()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, c.binary, append([]string{"--json=false", "--device", d.Addr}, args...)...)
			cmd.Env = append(c.environment(), "TERM=xterm-256color")
			terminal, err := pty.Start(cmd)
			if err != nil {
				t.Fatal(err)
			}
			defer terminal.Close()
			transcript := make(chan []byte, 1)
			go func() {
				var b bytes.Buffer
				buf := make([]byte, 1024)
				answered := false
				for {
					n, e := terminal.Read(buf)
					if n > 0 {
						b.Write(buf[:n])
						if !answered && (strings.Contains(b.String(), "[y/N]") || strings.Contains(b.String(), "(y/n)")) {
							answered = true
							_, _ = io.WriteString(terminal, "n\r")
						}
					}
					if e != nil {
						break
					}
				}
				transcript <- b.Bytes()
			}()
			err = cmd.Wait()
			out := <-transcript
			if ctx.Err() != nil {
				t.Fatalf("terminal decline did not terminate before deadline: %q", out)
			}
			if err != nil || !bytes.Contains(out, []byte("Aborted; nothing was changed")) {
				t.Fatalf("decline: %v %q", err, out)
			}
			if d.Keys.Closed() != closed || !bytes.Equal(before, authSnapshot(t, d)) {
				t.Fatal("decline changed shape or keys")
			}
			t.Logf("%s: observed terminal prompt, explicit No, successful abort, unchanged state", action)
		})
	}
}
