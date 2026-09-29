package mcp

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type runProgressKey struct{}

// withRunProgress attaches a sink that receives the run child's output.
func withRunProgress(ctx context.Context, sink *runProgress) context.Context {
	return context.WithValue(ctx, runProgressKey{}, sink)
}

func runProgressFrom(ctx context.Context) *runProgress {
	sink, _ := ctx.Value(runProgressKey{}).(*runProgress)
	return sink
}

const (
	runProgressInterval   = time.Second
	runProgressMaxMessage = 200
	runProgressMaxPartial = 4096
)

// runProgress forwards the newest complete output line as an MCP progress
// notification, at most once per interval. Values stay below 1 and strictly
// increase (n/(n+1)), so handleRun's final progress of 1 is still the largest,
// as the MCP progress contract requires.
type runProgress struct {
	mu       sync.Mutex
	partial  []byte
	sent     int
	last     time.Time
	interval time.Duration
	now      func() time.Time
	notify   func(progress float64, message string)
}

func newRunProgress(notify func(progress float64, message string)) *runProgress {
	return &runProgress{interval: runProgressInterval, now: time.Now, notify: notify}
}

func (p *runProgress) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.partial = append(p.partial, b...)
	end := bytes.LastIndexByte(p.partial, '\n')
	if end < 0 {
		if len(p.partial) > runProgressMaxPartial {
			p.partial = append(p.partial[:0], p.partial[len(p.partial)-runProgressMaxPartial:]...)
		}
		return len(b), nil
	}
	line := runProgressLine(string(p.partial[:end]))
	p.partial = append(p.partial[:0], p.partial[end+1:]...)
	if line == "" || p.now().Sub(p.last) < p.interval {
		return len(b), nil
	}
	p.last = p.now()
	p.sent++
	p.notify(float64(p.sent)/float64(p.sent+1), line)
	return len(b), nil
}

func runProgressLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if len(line) > runProgressMaxMessage {
			cut := runProgressMaxMessage
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			line = line[:cut]
		}
		return strings.ToValidUTF8(line, "�")
	}
	return ""
}
