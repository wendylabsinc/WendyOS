package netgossip

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
)

func noPayloads(t *testing.T, p *Pool) {
	t.Helper()
	s := p.PayloadStats()
	if s.Objects != 0 || s.ObjectBytes != 0 || s.Downloads != 0 || s.DownloadBytes != 0 || s.Uploads != 0 || s.UploadBytes != 0 {
		t.Fatalf("retained expired payload holders: %+v", s)
	}
}

func TestZeroCreditPollCannotRetainExpiredUpload(t *testing.T) {
	now := time.Now()
	pool := NewPool()
	source := pool.NewSession(Manifest)
	defer source.Close()
	have := source.Offer(bytes.Repeat([]byte("x"), MaxObject), now.Add(time.Second), now)
	if _, _, err := source.ReceiveDeferred(*message("want", have.ID(), 1), now); err != nil {
		t.Fatal(err)
	}
	source.Poll(now.Add(time.Second), 0)
	if len(source.out) != 0 || pool.serving != 0 {
		t.Fatalf("expired upload retained at zero output credit: uploads=%d, serving=%d", len(source.out), pool.serving)
	}
}

func TestIdleSweepFreesUploadsAtOriginalDeadlineWithZeroCredit(t *testing.T) {
	now := time.Now()
	pool := NewPool()
	source := pool.NewSession(Manifest)
	receiver := NewPool().NewSession(Manifest)
	defer receiver.Close()
	data := bytes.Repeat([]byte("signed body and signature"), 1000)
	deadline := now.Add(time.Second)
	have := source.Offer(data, deadline, now)
	want, _, err := receiver.Receive(*have, now)
	if err != nil || want == nil {
		t.Fatal("missing WANT", err)
	}
	if _, _, err = source.ReceiveDeferred(*want, now); err != nil {
		t.Fatal(err)
	}
	if len(source.Poll(now, 0)) != 0 || len(source.out) != 1 || pool.serving != 1 {
		t.Fatal("deferred transfer was not retained under zero credit")
	}
	// A repeated exact publication cannot extend either retained object's or
	// transfer's deadline, even if a caller supplies a later expiration.
	again := source.Offer(data, now.Add(time.Hour), now.Add(time.Millisecond))
	if again.Deadline() != deadline || source.out[0].expires != deadline {
		t.Fatal("duplicate extended the original lease")
	}
	x := source.out[0]
	pool.Sweep(deadline)
	noPayloads(t, pool)
	if x.data != nil || len(source.out) != 0 || !have.Expired(deadline) {
		t.Fatal("idle sweep left upload bytes or a live queued control")
	}
	if len(source.Poll(deadline, 0)) != 0 || source.Lookup(have.ID(), deadline) != nil {
		t.Fatal("expired artifact remained serveable without new input")
	}
	reply, _, err := source.Receive(*want, deadline)
	if err != nil || reply == nil || reply.Op != "unavailable" {
		t.Fatal("expired artifact served a later request", err)
	}
	source.Close()
	if pool.PayloadStats().Sessions != 0 {
		t.Fatal("closed session retained by pool")
	}
}

func TestForgetDropsQueuedChunksAndCannotReloadExpiredReceipt(t *testing.T) {
	now := time.Now()
	for _, forget := range []bool{false, true} {
		t.Run(fmt.Sprint(forget), func(t *testing.T) {
			pool := NewPool()
			source := pool.NewSession(Manifest)
			defer source.Close()
			body := []byte("immutable verified signed artifact")
			entered, release := make(chan struct{}), make(chan struct{})
			have := source.Offer(body, now.Add(time.Second), now, func(time.Time) []byte {
				close(entered)
				<-release
				return body
			})
			pool.mu.Lock()
			o := pool.objects[have.ID()]
			pool.bytes -= len(o.data)
			o.data = nil
			pool.objects[have.ID()] = o
			pool.mu.Unlock()
			done := make(chan []byte, 1)
			go func() { done <- source.Lookup(have.ID(), now) }()
			<-entered
			if forget {
				pool.Forget(have.ID())
			} else {
				pool.Sweep(now.Add(time.Second))
			}
			close(release)
			if data := <-done; data != nil {
				t.Fatal("in-flight loader resurrected an expired/forgotten artifact")
			}
			noPayloads(t, pool)
		})
	}

	pool := NewPool()
	source := pool.NewSession(Manifest)
	receiver := NewPool().NewSession(Manifest)
	defer source.Close()
	defer receiver.Close()
	have := source.Offer(bytes.Repeat([]byte("x"), MaxObject), now.Add(time.Minute), now)
	want, _, _ := receiver.Receive(*have, now)
	source.ReceiveDeferred(*want, now)
	x := source.out[0]
	pool.Forget(have.ID())
	noPayloads(t, pool)
	if x.data != nil || len(source.Poll(now, 0)) != 0 {
		t.Fatal("Forget retained a zero-credit upload")
	}
}

func TestIncompleteDownloadHasFixedResourceDeadline(t *testing.T) {
	now := time.Now()
	source := NewPool().NewSession(Manifest)
	defer source.Close()
	pool := NewPool()
	receiver := pool.NewSession(Manifest)
	defer receiver.Close()
	have := source.Offer(bytes.Repeat([]byte("x"), MaxObject), now.Add(3*time.Minute), now)
	want, _, err := receiver.Receive(*have, now)
	if err != nil || want == nil || want.Deadline() != now.Add(HintLifetime) {
		t.Fatal("missing bounded download", err)
	}
	frame, _, _ := source.Receive(*want, now)
	for i := 1; i <= 5; i++ {
		at := now.Add(time.Duration(i) * 20 * time.Second)
		if _, data, err := receiver.Receive(*frame, at); err != nil || data != nil {
			t.Fatal("partial transfer failed", err)
		}
		receiver.Receive(*have, at) // Fresh hints and progress cannot extend the claim.
		frame = source.Poll(at, 1)[0]
	}
	if pool.PayloadStats().DownloadBytes == 0 {
		t.Fatal("fixture did not retain partial download")
	}
	pool.Sweep(now.Add(HintLifetime))
	noPayloads(t, pool)
	if len(receiver.Poll(now.Add(HintLifetime), 0)) != 0 {
		t.Fatal("zero-credit expiration started another request")
	}
}

func TestRepeatedIdleExpiryAndCloseReturnAllPayloadCountsToZero(t *testing.T) {
	// UTC is deliberately unrelated to this receiver-local elapsed clock.
	mono := time.Unix(1, 0)
	wall := time.Unix(1800000000, 0)
	pool := NewPool()
	source := pool.NewSession(Manifest)
	receiver := NewPool().NewSession(Manifest)
	defer receiver.Close()
	for i := range 250 {
		wall = wall.Add(time.Duration(1-2*(i%2)) * 365 * 24 * time.Hour)
		_ = wall // Never supplied to the gossip scheduler.
		body := bytes.Repeat(fmt.Appendf(nil, "signed publication%d;", i), 100)
		have := source.Offer(body, mono.Add(time.Second), mono)
		want := message("want", have.ID(), uint64(i+1))
		if _, _, err := source.ReceiveDeferred(*want, mono); err != nil {
			t.Fatal(err)
		}
		frames := source.Poll(mono, 1)
		if len(frames) != 1 || frames[0].Deadline() != mono.Add(time.Second) {
			t.Fatal("generated chunk lost the original local deadline")
		}
		if len(frames[0].Data) != ChunkSize || cap(frames[0].Data) > 2*ChunkSize {
			t.Fatal("bounded chunk retained a complete publication backing array")
		}
		mono = mono.Add(time.Second)
		pool.Sweep(mono)
		noPayloads(t, pool)
	}
	source.Close()
	if pool.PayloadStats().Sessions != 0 {
		t.Fatal("closed session leaked")
	}
}

func TestLocalDeadlineMetadataDoesNotChangeWire(t *testing.T) {
	now := time.Now()
	s := NewPool().NewSession(Manifest)
	defer s.Close()
	have := s.Offer([]byte("signed immutable bytes"), now.Add(time.Minute), now)
	withMetadata, err := netwire.Marshal(have)
	if err != nil {
		t.Fatal(err)
	}
	plain := *have
	plain.deadline = time.Time{}
	withoutMetadata, err := netwire.Marshal(plain)
	if err != nil || !bytes.Equal(withMetadata, withoutMetadata) {
		t.Fatal("local scheduling metadata changed signed control bytes", err)
	}
	var decoded Message
	if err = netwire.Unmarshal(withMetadata, &decoded); err != nil || !decoded.Deadline().IsZero() {
		t.Fatal("wire decode invented a receiver deadline", err)
	}
}
