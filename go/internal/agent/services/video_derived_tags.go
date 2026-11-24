package services

import (
	"sync"
	"time"
)

const (
	// GStreamer video encoders move every timestamp up by a constant when the
	// first one they see is below 1000 hours. Tags start there, so they come
	// back as they were sent.
	derivedTagBase = uint64(1000 * time.Hour)

	derivedTagTableFrames = 64
)

// derivedFrameTags remembers which source frame each timestamp fed to a child
// pipeline stands for. A child drops the tag of a repeated timestamp and clamps
// one that goes backward, so every tag is above the one before it.
type derivedFrameTags struct {
	mu      sync.Mutex
	sources map[uint64]*videoFrame
	oldest  []uint64
	last    uint64
	unknown uint64
}

func (t *derivedFrameTags) assign(source *videoFrame, sinceChildStart time.Duration) (tag uint64) {
	identity := *source
	identity.data = nil

	t.mu.Lock()
	defer t.mu.Unlock()
	tag = derivedTagBase + uint64(max(sinceChildStart, 0))
	if tag <= t.last {
		tag = t.last + 1
	}
	t.last = tag
	if t.sources == nil {
		t.sources = make(map[uint64]*videoFrame, derivedTagTableFrames)
	}
	if len(t.oldest) == derivedTagTableFrames {
		delete(t.sources, t.oldest[0])
		t.oldest = t.oldest[1:]
	}
	t.sources[tag] = &identity
	t.oldest = append(t.oldest, tag)
	return tag
}

func (t *derivedFrameTags) source(outputTimestamp, shift uint64) (*videoFrame, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	source, known := t.sources[outputTimestamp-shift]
	if !known || outputTimestamp < shift {
		t.unknown++
		return nil, false
	}
	return source, true
}

func (t *derivedFrameTags) clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sources, t.oldest, t.last = nil, nil, 0
}

func (t *derivedFrameTags) unknownOutputs() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.unknown
}
