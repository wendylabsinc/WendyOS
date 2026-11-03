// Package asyncdiag isolates best-effort diagnostics from routing and policy
// lifecycles. A process has one worker and at most 64 queued observations. A
// blocked sink can lose diagnostics, but cannot delay withdrawal or shutdown.
package asyncdiag

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const capacity = 64
const maxFields = 24

// MaxMessageBytes is the existing diagnostic record limit. Callers encoding a
// structured line can bound the complete line before enqueueing it.
const MaxMessageBytes = 4096
const maxString = MaxMessageBytes

type record struct {
	logger  *zap.Logger
	level   zapcore.Level
	message string
	fields  []zap.Field
	barrier chan struct{}
}

type dispatcher struct {
	queue   chan record
	dropped atomic.Uint64
}

func newDispatcher() *dispatcher {
	d := &dispatcher{queue: make(chan record, capacity)}
	go func() {
		for r := range d.queue {
			if r.barrier != nil {
				close(r.barrier)
				continue
			}
			if r.logger == nil {
				log.Print(r.message)
			} else {
				r.logger.Log(r.level, r.message, r.fields...)
			}
		}
	}()
	return d
}

var process = newDispatcher()

func bounded(s string) string {
	if len(s) > maxString {
		return s[:maxString] + "[truncated]"
	}
	return strings.Clone(s)
}

// Log copies scalar observations before enqueueing. It does not call the sink,
// its Enabled method, or retain arbitrary errors, objects or mutable slices.
// Calls never wait for queue space; overflow is deliberately discarded.
func Log(logger *zap.Logger, level zapcore.Level, message string, fields ...zap.Field) {
	process.log(logger, level, message, fields...)
}

func (d *dispatcher) log(logger *zap.Logger, level zapcore.Level, message string, fields ...zap.Field) {
	if logger == nil {
		return
	}
	if len(fields) > maxFields {
		fields = fields[:maxFields]
	}
	copied := make([]zap.Field, 0, len(fields))
	for _, f := range fields {
		f.Key = bounded(f.Key)
		switch f.Type {
		case zapcore.ErrorType:
			if e, ok := f.Interface.(error); ok {
				f = zap.String(f.Key, bounded(e.Error()))
			} else {
				continue
			}
		case zapcore.StringType:
			f.String = bounded(f.String)
			f.Interface = nil
		case zapcore.BoolType, zapcore.Int64Type, zapcore.Int32Type, zapcore.Int16Type, zapcore.Int8Type, zapcore.Uint64Type, zapcore.Uint32Type, zapcore.Uint16Type, zapcore.Uint8Type, zapcore.UintptrType, zapcore.Float64Type, zapcore.Float32Type, zapcore.DurationType:
			f.Interface = nil
		case zapcore.TimeType:
			if _, ok := f.Interface.(*time.Location); f.Interface != nil && !ok {
				continue
			}
		case zapcore.TimeFullType:
			if _, ok := f.Interface.(time.Time); !ok {
				continue
			}
		case zapcore.ReflectType:
			// Only already serialized, bounded JSON snapshots are accepted; never
			// retain application objects or execute their marshalers in the worker.
			raw, ok := f.Interface.(json.RawMessage)
			if !ok || len(raw) > maxString {
				continue
			}
			f.Interface = append(json.RawMessage(nil), raw...)
		case zapcore.SkipType:
			continue
		default:
			// Object/array/reflection marshalers may retain application state and may
			// execute arbitrary code in the sink. These diagnostics use scalars only.
			continue
		}
		copied = append(copied, f)
	}
	d.try(record{logger: logger, level: level, message: bounded(message), fields: copied})
}

// Print queues a bounded already-formatted public diagnostic line.
func Print(message string) { process.try(record{message: bounded(message)}) }

func (d *dispatcher) try(r record) bool {
	select {
	case d.queue <- r:
		return true
	default:
		d.dropped.Add(1)
		return false
	}
}

// Flush waits for previously queued observations, bounded by ctx. Routing,
// cleanup and shutdown must never call it; it is for diagnostic consumers/tests.
func Flush(ctx context.Context) error {
	done := make(chan struct{})
	select {
	case process.queue <- record{barrier: done}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
