// Package terminaloutput defers background diagnostics while a TUI owns the terminal.
//
// Deferred writes live in memory until the last view exits. A process that
// ends while a view is active (os.Exit, or an unrecovered panic in another
// goroutine) loses them, so fatal paths should release the view first.
package terminaloutput

import (
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"sync"
)

const maxPendingBytes = 64 << 10

var output outputState

type outputState struct {
	sync.Mutex
	owners       int
	blocking     int        // owners without a printer
	printers     []*printer // inline owners, newest last
	bytes        int
	droppedBytes int
	dropped      int
	pending      []write
}

var lifecycle sync.Mutex
var loggerOutput io.Writer

type write struct {
	dst  io.Writer
	data []byte
}

// printer prints a diagnostic above an inline view. It reports false when it
// cannot take the write, which is then deferred as under Suspend.
type printer struct{ print func([]byte) bool }

type writer struct{ dst io.Writer }

// Writer protects diagnostics written to dst. Renderer output must bypass it.
func Writer(dst io.Writer) io.Writer {
	if _, ok := dst.(writer); ok {
		return dst
	}
	if _, ok := dst.(stderrWriter); ok {
		dst = os.Stderr
	}
	return writer{dst}
}

func (w writer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	output.Lock()
	if print := output.inlinePrinter(w.dst); print != nil {
		output.Unlock()
		if print(p) {
			return len(p), nil
		}
		output.Lock()
	}
	defer output.Unlock()
	if output.owners == 0 {
		return w.dst.Write(p)
	}
	output.enqueue(w.dst, p)
	return len(p), nil
}

// inlinePrinter returns the printer for a stderr write while only inline views
// are active. Callers hold output's lock.
func (o *outputState) inlinePrinter(dst io.Writer) func([]byte) bool {
	if o.owners == 0 || o.blocking > 0 || len(o.printers) == 0 || dst != io.Writer(os.Stderr) {
		return nil
	}
	return o.printers[len(o.printers)-1].print
}

// enqueue defers a write. Callers hold output's lock.
func (o *outputState) enqueue(dst io.Writer, p []byte) {
	// Keep the newest writes: the ones just before the view exits usually
	// explain its outcome, and evicting from the front keeps the replay free
	// of gaps.
	o.pending = append(o.pending, write{dst, append([]byte(nil), p...)})
	o.bytes += len(p)
	for o.bytes > maxPendingBytes {
		evicted := o.pending[0]
		o.pending[0] = write{}
		o.pending = o.pending[1:]
		o.bytes -= len(evicted.data)
		o.droppedBytes += len(evicted.data)
		o.dropped++
	}
}

type stderrWriter struct{}

func (stderrWriter) Write(p []byte) (int, error) { return Writer(os.Stderr).Write(p) }

// Stderr follows os.Stderr and defers writes until all active views have exited.
var Stderr io.Writer = stderrWriter{}

// Suspend returns an idempotent release function. Standard-library logging is
// included because discovery and third-party libraries use the default logger.
// The bounded queue avoids unbounded memory use during a long-running view.
func Suspend() func() {
	release := suspend(nil)
	return func() { release(nil) }
}

// SuspendInline is Suspend for an inline view that can print lines above
// itself, such as a prompt that stays up for a whole session. While only
// inline views are active, stderr diagnostics go to print as they arrive
// instead of waiting for the view to exit. Writes to other destinations, and
// any write print refuses, are deferred as under Suspend. The idempotent
// release takes the stderr writes print accepted but never showed, oldest
// first; they are replayed ahead of anything deferred.
func SuspendInline(print func([]byte) bool) func(unprinted [][]byte) {
	return suspend(&printer{print})
}

func suspend(inline *printer) func(unprinted [][]byte) {
	lifecycle.Lock()
	output.Lock()
	first := output.owners == 0
	output.owners++
	if inline != nil {
		output.printers = append(output.printers, inline)
	} else {
		output.blocking++
	}
	output.Unlock()
	if first {
		loggerOutput = log.Writer()
		log.SetOutput(Writer(loggerOutput))
	}
	lifecycle.Unlock()
	var once sync.Once
	return func(unprinted [][]byte) {
		once.Do(func() {
			lifecycle.Lock()
			defer lifecycle.Unlock()
			output.Lock()
			if len(unprinted) > 0 {
				queued := output.pending
				output.pending, output.bytes = nil, 0
				for _, data := range unprinted {
					output.enqueue(os.Stderr, data)
				}
				for _, entry := range queued {
					output.enqueue(entry.dst, entry.data)
				}
			}
			output.owners--
			if inline != nil {
				output.printers = slices.DeleteFunc(output.printers, func(p *printer) bool { return p == inline })
			} else {
				output.blocking--
			}
			last := output.owners == 0
			var handOff []write
			if last {
				if output.dropped > 0 {
					_, _ = fmt.Fprintf(os.Stderr, "wendy: omitted %d earlier background diagnostic writes (%d bytes) while the TUI was active\n", output.dropped, output.droppedBytes)
				}
				for _, entry := range output.pending {
					_, _ = entry.dst.Write(entry.data)
				}
				output.pending = nil
				output.bytes, output.dropped, output.droppedBytes = 0, 0, 0
			} else if output.inlinePrinter(os.Stderr) != nil {
				// The last full-screen view left an inline one: its deferred
				// stderr writes can be shown now rather than at session end.
				handOff, output.pending, output.bytes = output.pending, nil, 0
			}
			output.Unlock()
			for _, entry := range handOff {
				_, _ = Writer(entry.dst).Write(entry.data)
			}
			if last {
				log.SetOutput(loggerOutput)
			}
		})
	}
}
