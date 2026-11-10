// Package terminaloutput defers background diagnostics while a TUI owns the terminal.
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
	owners  int
	bytes   int
	dropped int
	pending []write
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
	if len(p) <= maxPendingBytes-output.bytes {
		output.pending = append(output.pending, write{w.dst, append([]byte(nil), p...)})
		output.bytes += len(p)
	} else {
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
				for _, entry := range output.pending {
					_, _ = entry.dst.Write(entry.data)
				}
				if output.dropped > 0 {
					_, _ = fmt.Fprintf(os.Stderr, "wendy: omitted %d background diagnostic writes while the TUI was active\n", output.dropped)
				}
				output.pending = nil
				output.bytes, output.dropped = 0, 0
			}
			output.Unlock()
			if last {
				log.SetOutput(loggerOutput)
			}
		})
	}
}
