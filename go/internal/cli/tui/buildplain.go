package tui

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// BuildTally counts cached vs rebuilt Dockerfile steps for the summary line.
type BuildTally struct {
	Cached  int
	Rebuilt int
}

// BuildTallyCounter counts each Dockerfile step once, by the last cached/done
// status it reported. BuildKit can finish a step more than once (a lazily
// pulled base image reports DONE, pulls its layers, then reports DONE again),
// and it is still one step in the summary.
type BuildTallyCounter struct {
	last map[string]BuildStepStatus
}

// Observe records e when it finishes a Dockerfile step.
func (c *BuildTallyCounter) Observe(e BuildStepEvent) {
	if e.Kind != BuildVertexStep || (e.Status != BuildStepCached && e.Status != BuildStepDone) {
		return
	}
	if c.last == nil {
		c.last = map[string]BuildStepStatus{}
	}
	c.last[e.ID] = e.Status
}

// Tally returns the counts observed so far.
func (c *BuildTallyCounter) Tally() BuildTally {
	var t BuildTally
	for _, s := range c.last {
		if s == BuildStepCached {
			t.Cached++
		} else {
			t.Rebuilt++
		}
	}
	return t
}

// PlainHeartbeatInterval is how often the non-interactive renderer reports on
// steps that are still running. Without it a ten-minute compile is a silent gap
// in CI logs, which reads as a hang.
const PlainHeartbeatInterval = 15 * time.Second

// formatDuration returns a human-readable duration string with at least one
// decimal place (e.g., "2.0s", "4.3s").
func formatDuration(d time.Duration) string {
	return fmt.Sprintf("%.1fs", d.Round(time.Millisecond).Seconds())
}

type plainRunningStep struct {
	display   string
	detail    string
	started   time.Time
	lastPrint time.Time
}

// NewBuildPlainRenderer returns an emit callback (to pass to NewBuildParser)
// that writes one concise line per completed step to w, a tally accessor for the
// final summary, and a stop function that must be called when the build ends.
// It is the non-interactive (CI / piped) renderer.
//
// While a step runs it stays quiet, except for a periodic heartbeat carrying the
// step's latest progress detail so long steps visibly advance.
func NewBuildPlainRenderer(w io.Writer) (emit func(BuildStepEvent), tally func() BuildTally, stop func()) {
	return newBuildPlainRenderer(w, PlainHeartbeatInterval)
}

func newBuildPlainRenderer(w io.Writer, heartbeat time.Duration) (func(BuildStepEvent), func() BuildTally, func()) {
	var (
		mu      sync.Mutex
		t       BuildTallyCounter
		running = map[string]*plainRunningStep{}
		order   []string
		listed  = map[string]bool{}
	)

	emitFn := func(e BuildStepEvent) {
		mu.Lock()
		defer mu.Unlock()
		switch e.Status {
		case BuildStepRunning:
			// A step that finished and was reopened starts a fresh entry, so its
			// heartbeat counts from the reopen.
			s := running[e.ID]
			if s == nil {
				s = &plainRunningStep{display: e.Display, started: time.Now(), lastPrint: time.Now()}
				running[e.ID] = s
				if !listed[e.ID] {
					listed[e.ID] = true
					order = append(order, e.ID)
				}
			}
			if d := progressDetailOf(e); d != "" {
				s.detail = d
			}
		case BuildStepCached:
			delete(running, e.ID)
			t.Observe(e)
			fmt.Fprintf(w, "  cached  %s\n", e.Display)
		case BuildStepDone:
			delete(running, e.ID)
			t.Observe(e)
			fmt.Fprintf(w, "  done    %s  %s\n", e.Display, formatDuration(e.Dur))
		case BuildStepFailed:
			delete(running, e.ID)
			fmt.Fprintf(w, "  FAILED  %s\n", e.Display)
		}
	}

	done := make(chan struct{})
	var stopOnce sync.Once
	stopFn := func() { stopOnce.Do(func() { close(done) }) }

	if heartbeat > 0 {
		go func() {
			ticker := time.NewTicker(heartbeat)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case now := <-ticker.C:
					mu.Lock()
					for _, id := range order {
						s := running[id]
						if s == nil || now.Sub(s.lastPrint) < heartbeat {
							continue
						}
						s.lastPrint = now
						elapsed := formatDuration(now.Sub(s.started))
						if s.detail != "" {
							fmt.Fprintf(w, "  ...     %s  %s  (%s)\n", s.display, s.detail, elapsed)
						} else {
							fmt.Fprintf(w, "  ...     %s  (%s)\n", s.display, elapsed)
						}
					}
					mu.Unlock()
				}
			}
		}()
	}

	return emitFn, func() BuildTally { mu.Lock(); defer mu.Unlock(); return t.Tally() }, stopFn
}

// ProgressDetail renders an event's tool progress and byte counters as one
// string ("[525/1027] 51% Compiling WendyKit  61%  128MB/210MB  9.4MB/s"), or
// "" when the event carries neither. Renderers outside this package use it to
// show live movement for a running step.
func ProgressDetail(e BuildStepEvent) string { return joinProgress(e.Detail, e.Bytes) }

func progressDetailOf(e BuildStepEvent) string { return ProgressDetail(e) }

// joinProgress combines a tool's own progress text with BuildKit's transfer
// counters, dropping whichever half is absent.
func joinProgress(detail string, b ByteProgress) string {
	bs := b.String()
	switch {
	case detail != "" && bs != "":
		return detail + "  " + bs
	case bs != "":
		return bs
	default:
		return detail
	}
}
