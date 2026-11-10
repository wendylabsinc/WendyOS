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
	"sync"
)

const maxPendingBytes = 64 << 10

var output struct {
	sync.Mutex
	owners       int
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
	defer output.Unlock()
	if output.owners == 0 {
		return w.dst.Write(p)
	}
	// Keep the newest writes: the ones just before the view exits usually
	// explain its outcome, and evicting from the front keeps the replay free
	// of gaps.
	output.pending = append(output.pending, write{w.dst, append([]byte(nil), p...)})
	output.bytes += len(p)
	for output.bytes > maxPendingBytes {
		evicted := output.pending[0]
		output.pending[0] = write{}
		output.pending = output.pending[1:]
		output.bytes -= len(evicted.data)
		output.droppedBytes += len(evicted.data)
		output.dropped++
	}
	return len(p), nil
}

type stderrWriter struct{}

func (stderrWriter) Write(p []byte) (int, error) { return Writer(os.Stderr).Write(p) }

// Stderr follows os.Stderr and defers writes until all active views have exited.
var Stderr io.Writer = stderrWriter{}

// Suspend returns an idempotent release function. Standard-library logging is
// included because discovery and third-party libraries use the default logger.
// The bounded queue avoids unbounded memory use during a long-running view.
func Suspend() func() {
	lifecycle.Lock()
	output.Lock()
	first := output.owners == 0
	output.owners++
	output.Unlock()
	if first {
		loggerOutput = log.Writer()
		log.SetOutput(Writer(loggerOutput))
	}
	lifecycle.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			lifecycle.Lock()
			defer lifecycle.Unlock()
			output.Lock()
			output.owners--
			last := output.owners == 0
			if last {
				if output.dropped > 0 {
					_, _ = fmt.Fprintf(os.Stderr, "wendy: omitted %d earlier background diagnostic writes (%d bytes) while the TUI was active\n", output.dropped, output.droppedBytes)
				}
				for _, entry := range output.pending {
					_, _ = entry.dst.Write(entry.data)
				}
				output.pending = nil
				output.bytes, output.dropped, output.droppedBytes = 0, 0, 0
			}
			output.Unlock()
			if last {
				log.SetOutput(loggerOutput)
			}
		})
	}
}
