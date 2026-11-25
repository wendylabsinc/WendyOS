package tui

import (
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/shared/terminaloutput"
)

// RunProgram gives the renderer exclusive terminal ownership until Run has
// restored terminal state, including on cancellation, errors and panics.
// Background diagnostics must use terminaloutput.Writer or the default logger;
// user-visible progress and errors belong in the model's View.
func RunProgram(p *tea.Program) (tea.Model, error) {
	release := terminaloutput.Suspend()
	defer release()
	return p.Run()
}

// inlineBacklog bounds the lines waiting for the event loop. Beyond it, writes
// are deferred until the program exits, as under RunProgram.
const inlineBacklog = 256

// RunInlineProgram is RunProgram for an inline (not alt-screen) program that
// can stay up for a whole session. Background stderr diagnostics print above
// it as they arrive instead of waiting for it to exit. Lines still queued for
// the event loop at exit are replayed after it; one already handed to Send as
// the program shuts down can be lost, as with tea.Program.Println.
func RunInlineProgram(p *tea.Program) (tea.Model, error) {
	lines := make(chan []byte, inlineBacklog)
	var mu sync.Mutex
	closed := false
	release := terminaloutput.SuspendInline(func(b []byte) bool {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return false
		}
		select {
		case lines <- append([]byte(nil), b...):
			return true
		default:
			// Refuse from now on: later lines would otherwise print ahead
			// of the deferred ones.
			closed = true
			return false
		}
	})
	stop := make(chan struct{})
	forwarded := make(chan struct{})
	go func() {
		defer close(forwarded)
		for {
			select {
			case line := <-lines:
				// Send, unlike Println, returns once the program has exited.
				p.Send(tea.Println(strings.TrimSuffix(string(line), "\n"))())
			case <-stop:
				return
			}
		}
	}()
	defer func() {
		mu.Lock()
		closed = true
		mu.Unlock()
		close(stop)
		<-forwarded
		var unprinted [][]byte
		for len(lines) > 0 {
			unprinted = append(unprinted, <-lines)
		}
		release(unprinted)
	}()
	return p.Run()
}
