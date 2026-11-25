package rpctrace

import (
	"context"
	"math"
	"strings"
	"sync"
	"time"
)

type contextKey struct{}
type counter struct {
	count    uint64
	sum, max int64
}
type collector struct {
	mu                           sync.Mutex
	sink                         *Sink
	labels                       Labels
	start                        time.Time
	finished                     bool
	stages                       [stageCount]counter
	refusalPoint                 RefusalPoint
	refusalReason, watcherReason RefusalReason
	pinInterruptions             [len(interruptionPoints)]uint64
}

func noop()         {}
func noFinish(Code) {}

// Begin installs one bounded collector. Nil sinks are a no-op: no clock read,
// context allocation or background work. Invalid labels, exhausted capacity
// and closed sinks also leave the caller's context and behavior unchanged.
// Finish must run once after the RPC's authority/storage locks are released.
// It never calls a writer or waits for queue capacity. Further calls are safe.
func Begin(ctx context.Context, sink *Sink, labels Labels) (context.Context, func(Code)) {
	if sink == nil {
		return ctx, noFinish
	}
	if ctx == nil || !validLabels(labels) {
		sink.count.droppedInvalid.Add(1)
		return ctx, noFinish
	}
	if ctx.Value(contextKey{}) != nil {
		sink.count.droppedInvalid.Add(1)
		return ctx, noFinish
	}
	if !sink.reserveCollector() {
		return ctx, noFinish
	}
	labels.App, labels.Service = strings.Clone(labels.App), strings.Clone(labels.Service)
	c := &collector{sink: sink, labels: labels, start: time.Now()}
	sink.count.started.Add(1)
	return context.WithValue(ctx, contextKey{}, c), c.finish
}

// Start measures local monotonic elapsed time. Stop is idempotent and safe
// across goroutines. Total is reserved for Begin/Finish and cannot be added.
func Start(ctx context.Context, stage Stage) func() {
	c := fromContext(ctx)
	if c == nil || stage >= StageTotal {
		return noop
	}
	start := time.Now()
	var once sync.Once
	return func() { once.Do(func() { c.add(stage, time.Since(start)) }) }
}

// Add records an already measured local duration, such as a mutex wait.
// Negative durations, invalid stages and work ending after Finish are ignored.
func Add(ctx context.Context, stage Stage, elapsed time.Duration) {
	if c := fromContext(ctx); c != nil && stage < StageTotal && elapsed >= 0 {
		c.add(stage, elapsed)
	}
}

func fromContext(ctx context.Context) *collector {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(contextKey{}).(*collector)
	return c
}

func (c *collector) add(stage Stage, elapsed time.Duration) {
	if elapsed < 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return
	}
	v := &c.stages[stage]
	if v.count < math.MaxUint64 {
		v.count++
	}
	ns := int64(elapsed)
	if ns > math.MaxInt64-v.sum {
		v.sum = math.MaxInt64
	} else {
		v.sum += ns
	}
	if ns > v.max {
		v.max = ns
	}
}

func (c *collector) finish(code Code) {
	c.mu.Lock()
	if c.finished {
		c.mu.Unlock()
		return
	}
	c.finished = true
	ns := max(int64(0), int64(time.Since(c.start)))
	c.stages[StageTotal] = counter{count: 1, sum: ns, max: ns}
	if uint64(code) >= uint64(len(codeNames)) {
		code = Unknown
	}
	event := Event{Version: 2, Method: methodNames[c.labels.Method], App: c.labels.App, Service: c.labels.Service, Generation: c.labels.Generation, Code: codeNames[code], Inclusive: true, Stages: make([]Measurement, 0, stageCount)}
	if c.refusalReason != 0 {
		event.Refusal = &Refusal{Point: pointNames[c.refusalPoint], Reason: reasonNames[c.refusalReason], WatcherReason: reasonNames[c.watcherReason]}
	}
	for i, count := range c.pinInterruptions {
		if count != 0 {
			event.PinInterruptions = append(event.PinInterruptions, PinInterruption{Point: pointNames[interruptionPoints[i]], Count: count})
		}
	}
	for stage, value := range c.stages {
		if value.count != 0 {
			event.Stages = append(event.Stages, Measurement{Stage: stageNames[stage], Count: value.count, SumNS: value.sum, MaxNS: value.max})
		}
	}
	c.mu.Unlock()
	defer c.sink.active.Add(-1)
	c.sink.count.finished.Add(1)
	c.sink.emit(event)
}
