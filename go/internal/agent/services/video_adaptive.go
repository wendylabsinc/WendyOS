package services

import "time"

// rawFramePacer controls delivery, never the shared camera's capture rate.
// Send blocks once gRPC's transport window fills. Leave headroom rather than
// filling it again immediately, and increase slowly when writes stay cheap.
type rawFramePacer struct {
	fps     float64
	next    time.Time
	probeAt time.Time
}

func (p *rawFramePacer) allow(now time.Time) bool {
	if p.fps == 0 {
		p.fps = 5
		p.probeAt = now.Add(5 * time.Second)
	}
	return !now.Before(p.next)
}

func (p *rawFramePacer) sent(start, end time.Time) {
	interval := time.Duration(float64(time.Second) / p.fps)
	busy := end.Sub(start)
	if busy > interval/2 {
		// A blocked write is stronger evidence than previous quick writes
		// into a buffer. Cut promptly and postpone the next upward probe.
		p.fps = max(1, min(p.fps*0.7, float64(time.Second)/float64(2*busy)))
		p.probeAt = end.Add(10 * time.Second)
	} else if !end.Before(p.probeAt) {
		p.fps = min(30, p.fps+1)
		p.probeAt = end.Add(5 * time.Second)
	}
	p.next = start.Add(time.Duration(float64(time.Second) / p.fps))
	if p.next.Before(end) {
		p.next = end
	}
}
