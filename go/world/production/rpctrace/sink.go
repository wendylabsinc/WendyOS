package rpctrace

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"sync/atomic"
)

const closedBit uint64 = 1 << 63

type counts struct {
	started, finished, enqueued, enqueuedBytes atomic.Uint64
	written, writtenBytes                      atomic.Uint64
	droppedInvalid, droppedInFlight            atomic.Uint64
	droppedClosed, droppedBusy                 atomic.Uint64
	droppedQueueFull, droppedBudget            atomic.Uint64
	writerErrors                               atomic.Uint64
}

type Sink struct {
	cfg     Config
	queue   chan []byte
	active  atomic.Int64
	count   counts
	running atomic.Bool
	// The high bit seals submission; low bits count already reserved emits.
	// Close never takes the writer or submit mutex and never waits for either.
	gate        atomic.Uint64
	settled     chan struct{}
	settledOnce sync.Once
	// Submit uses TryLock only. Contention drops diagnostics, never work.
	submitMu sync.Mutex
	sequence uint64
}

func New(cfg Config) (*Sink, error) {
	if cfg.Capacity < 0 || cfg.Capacity > MaxCapacity || cfg.MaxEvents < 0 || cfg.MaxEvents > MaxEvents || cfg.MaxBytes < 0 || cfg.MaxBytes > MaxBytes {
		return nil, ErrConfig
	}
	if cfg.Capacity == 0 {
		cfg.Capacity = MaxCapacity
	}
	if cfg.MaxEvents == 0 {
		cfg.MaxEvents = MaxEvents
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = MaxBytes
	}
	return &Sink{cfg: cfg, queue: make(chan []byte, cfg.Capacity), settled: make(chan struct{})}, nil
}

func (s *Sink) reserveCollector() bool {
	for {
		if s.gate.Load()&closedBit != 0 {
			s.count.droppedClosed.Add(1)
			return false
		}
		n := s.active.Load()
		if n >= int64(s.cfg.Capacity) {
			s.count.droppedInFlight.Add(1)
			return false
		}
		if s.active.CompareAndSwap(n, n+1) {
			// Seal may race the reservation; it never waits for a live RPC.
			if s.gate.Load()&closedBit != 0 {
				s.active.Add(-1)
				s.count.droppedClosed.Add(1)
				return false
			}
			return true
		}
	}
}

func (s *Sink) enterEmit() bool {
	for {
		old := s.gate.Load()
		if old&closedBit != 0 {
			s.count.droppedClosed.Add(1)
			return false
		}
		if s.gate.CompareAndSwap(old, old+1) {
			return true
		}
	}
}
func (s *Sink) leaveEmit() {
	if s.gate.Add(^uint64(0)) == closedBit {
		s.settledOnce.Do(func() { close(s.settled) })
	}
}
func (s *Sink) emit(event Event) {
	if !s.enterEmit() {
		return
	}
	defer s.leaveEmit()
	if !s.submitMu.TryLock() {
		s.count.droppedBusy.Add(1)
		return
	}
	defer s.submitMu.Unlock()
	if s.count.enqueued.Load() >= uint64(s.cfg.MaxEvents) {
		s.count.droppedBudget.Add(1)
		return
	}
	s.sequence++
	event.Sequence = s.sequence
	raw, err := json.Marshal(event)
	if err != nil { // Event's fixed scalar shape cannot currently fail encoding.
		s.count.droppedInvalid.Add(1)
		return
	}
	raw = append(raw, '\n')
	if uint64(len(raw)) > uint64(s.cfg.MaxBytes)-s.count.enqueuedBytes.Load() {
		s.count.droppedBudget.Add(1)
		return
	}
	select {
	case s.queue <- raw:
		s.count.enqueued.Add(1)
		s.count.enqueuedBytes.Add(uint64(len(raw)))
	default:
		s.count.droppedQueueFull.Add(1)
	}
}

// Close seals immediately. It neither waits for live collectors nor drains or
// joins a writer. Events already in submission may finish; later Finish calls
// are dropped. Run independently drains those reserved/queued events.
func (s *Sink) Close() {
	if s == nil || s.settled == nil {
		return
	}
	old := s.gate.Or(closedBit)
	if old&^closedBit == 0 {
		s.settledOnce.Do(func() { close(s.settled) })
	}
}

// Run is the only writer path. The caller starts it outside RPC/authority
// locks and owns the writer. Run may be invoked once, including after Close.
// Cancellation seals the sink and stops between writes. An arbitrary blocked
// io.Writer cannot be interrupted by context; close it externally if needed.
// Neither that blockage nor a write error can block an RPC or Close.
func (s *Sink) Run(ctx context.Context, writer io.Writer) error {
	if s == nil || s.queue == nil || s.settled == nil || ctx == nil || writer == nil || !s.running.CompareAndSwap(false, true) {
		return ErrWriter
	}
	defer s.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case raw := <-s.queue:
			if err := s.write(writer, raw); err != nil {
				return err
			}
		case <-s.settled:
			// All pre-seal emit reservations are complete. No sender remains.
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				select {
				case raw := <-s.queue:
					if err := s.write(writer, raw); err != nil {
						return err
					}
				default:
					return nil
				}
			}
		}
	}
}

func (s *Sink) write(writer io.Writer, raw []byte) error {
	n, err := writer.Write(raw)
	if n < 0 || n > len(raw) {
		n = 0
		err = io.ErrShortWrite
	}
	s.count.writtenBytes.Add(uint64(n))
	if err != nil || n != len(raw) {
		s.count.writerErrors.Add(1)
		return ErrWriter // Do not expose path/token-bearing writer error text.
	}
	s.count.written.Add(1)
	return nil
}

// Stats is a bounded concurrent snapshot; counters may advance between fields.
func (s *Sink) Stats() Stats {
	if s == nil {
		return Stats{}
	}
	c := &s.count
	return Stats{Started: c.started.Load(), Finished: c.finished.Load(), Enqueued: c.enqueued.Load(), EnqueuedBytes: c.enqueuedBytes.Load(), Written: c.written.Load(), WrittenBytes: c.writtenBytes.Load(), DroppedInvalid: c.droppedInvalid.Load(), DroppedInFlight: c.droppedInFlight.Load(), DroppedClosed: c.droppedClosed.Load(), DroppedBusy: c.droppedBusy.Load(), DroppedQueueFull: c.droppedQueueFull.Load(), DroppedBudget: c.droppedBudget.Load(), WriterErrors: c.writerErrors.Load(), Active: s.active.Load(), Queued: len(s.queue), Closed: s.gate.Load()&closedBit != 0}
}
