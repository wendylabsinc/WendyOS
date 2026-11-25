package asyncdiag

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestBoundedQueueCopiesOnlyScalarFieldsAndDropsOverflow(t *testing.T) {
	d := &dispatcher{queue: make(chan record, capacity)}
	fields := []zap.Field{zap.String("state", "before"), zap.Error(errors.New("failure")), zap.Any("mutable", []string{"secret"}), zap.String("long", strings.Repeat("x", maxString+100))}
	for range capacity + 1000 {
		d.log(zap.NewNop(), zap.InfoLevel, "test", fields...)
	}
	fields[0] = zap.String("state", "after")
	if len(d.queue) != capacity || d.dropped.Load() != 1000 {
		t.Fatalf("queue=%d dropped=%d", len(d.queue), d.dropped.Load())
	}
	r := <-d.queue
	if len(r.fields) != 3 || r.fields[0].String != "before" || r.fields[1].Type != zapcore.StringType || r.fields[1].String != "failure" || len(r.fields[2].String) > maxString+len("[truncated]") {
		t.Fatalf("unbounded/mutable fields: %+v", r.fields)
	}
}

func TestJSONAndNumericSnapshotsRemainImmutable(t *testing.T) {
	d := &dispatcher{queue: make(chan record, capacity)}
	raw := json.RawMessage(`["before"]`)
	d.log(zap.NewNop(), zap.InfoLevel, "snapshot", zap.Reflect("paths", raw), zap.Uint16("slot", 5), zap.Int32("count", 7))
	copy(raw, `["after!"]`)
	r := <-d.queue
	if len(r.fields) != 3 || string(r.fields[0].Interface.(json.RawMessage)) != `["before"]` {
		t.Fatalf("mutable or omitted snapshot: %+v", r.fields)
	}
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range r.fields {
		f.AddTo(enc)
	}
	if enc.Fields["slot"] != uint16(5) || enc.Fields["count"] != int32(7) {
		t.Fatalf("scalar fields lost: %+v", enc.Fields)
	}
}
