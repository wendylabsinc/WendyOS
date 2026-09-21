package services

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// burstReader hands out chunks with no delay, the way a pipe drains one encoded
// frame that is larger than the read buffer.
type burstReader struct {
	chunks [][]byte
	i      int
}

func (r *burstReader) Read(p []byte) (int, error) {
	if r.i >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.i])
	if n < len(r.chunks[r.i]) {
		r.chunks[r.i] = r.chunks[r.i][n:] // short buffer: hand back the rest next Read
		return n, nil
	}
	r.i++
	return n, nil
}

// Dropping a chunk does not cost a frame: it truncates whichever access unit was
// mid-flight, and the decoder renders its surviving prefix plus garbage.
func TestPumpEncodedStreamDeliversEveryByteOfABurst(t *testing.T) {
	chunks := [][]byte{
		bytes.Repeat([]byte{0xA1}, 4096),
		bytes.Repeat([]byte{0xB2}, 4096),
		bytes.Repeat([]byte{0xC3}, 4096),
		bytes.Repeat([]byte{0xD4}, 4096),
		bytes.Repeat([]byte{0xE5}, 4096),
	}
	var want []byte
	for _, c := range chunks {
		want = append(want, c...)
	}

	var got []byte
	broadcast := func(data []byte, _ uint64, _ agentpb.VideoCodec) bool {
		got = append(got, data...)
		return true
	}

	err := pumpEncodedStream(context.Background(), &burstReader{chunks: chunks},
		agentpb.VideoCodec_VIDEO_CODEC_H264, broadcast, func() {})
	if err != nil {
		t.Fatalf("pumpEncodedStream returned %v, want nil", err)
	}

	if len(got) != len(want) {
		t.Fatalf("delivered %d bytes, want %d — %d bytes of the stream were dropped",
			len(got), len(want), len(want)-len(got))
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("delivered bytes differ from the source stream")
	}
}

// Enforcing the first-frame timeout kills gst, which ends the read in io.EOF —
// indistinguishable from a clean exit, so the timeout must be consulted first.
func TestGstStreamEndPrefersTheTimeoutOverACleanRead(t *testing.T) {
	err := gstStreamEnd(nil, true, false, nil)
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("got %v (code %s), want DeadlineExceeded", err, status.Code(err))
	}
}

func TestGstStreamEndClassifiesTheOtherOutcomes(t *testing.T) {
	cancelled := context.Canceled
	readFailed := errors.New("read failed")

	for _, tc := range []struct {
		name     string
		readErr  error
		timedOut bool
		sawChunk bool
		ctxErr   error
		want     codes.Code
		wantErr  error
	}{
		{name: "clean end", want: codes.OK},
		// The timer can fire just as the first chunk lands; frames were delivered,
		// so this is a normal end of stream and not a timeout.
		{name: "timer raced the first chunk", timedOut: true, sawChunk: true, want: codes.OK},
		{name: "caller cancelled", readErr: cancelled, ctxErr: cancelled, wantErr: cancelled},
		{name: "read failed", readErr: readFailed, want: codes.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := gstStreamEnd(tc.readErr, tc.timedOut, tc.sawChunk, tc.ctxErr)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("got %v, want %v", err, tc.wantErr)
				}
				return
			}
			if status.Code(err) != tc.want {
				t.Fatalf("got %v (code %s), want %s", err, status.Code(err), tc.want)
			}
		})
	}
}

func TestPumpEncodedStreamSignalsTheFirstChunkExactlyOnce(t *testing.T) {
	chunks := [][]byte{{1}, {2}, {3}}
	calls := 0
	broadcast := func([]byte, uint64, agentpb.VideoCodec) bool { return true }

	err := pumpEncodedStream(context.Background(), &burstReader{chunks: chunks},
		agentpb.VideoCodec_VIDEO_CODEC_H264, broadcast, func() { calls++ })
	if err != nil {
		t.Fatalf("pumpEncodedStream returned %v, want nil", err)
	}
	if calls != 1 {
		t.Fatalf("onFirstChunk fired %d times, want 1", calls)
	}
}

// io.Reader may return bytes and an error from the same Read, which is how a
// pipe reports the tail of a stream whose writer has just died. Those bytes must
// still be forwarded, and the error must still reach the caller to classify.
func TestPumpEncodedStreamForwardsTheTailThatArrivesWithAnError(t *testing.T) {
	want := errors.New("pipe broke")
	var got []byte
	broadcast := func(data []byte, _ uint64, _ agentpb.VideoCodec) bool {
		got = append(got, data...)
		return true
	}

	err := pumpEncodedStream(context.Background(), &dyingReader{err: want, tail: []byte{0x42}},
		agentpb.VideoCodec_VIDEO_CODEC_H264, broadcast, func() {})
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
	if !bytes.Equal(got, []byte{0x42}) {
		t.Fatalf("delivered %v, want the tail 0x42 to survive the error", got)
	}
}

// dyingReader hands back its tail and its error in a single Read.
type dyingReader struct {
	err  error
	tail []byte
}

func (r *dyingReader) Read(p []byte) (int, error) {
	return copy(p, r.tail), r.err
}

func TestPumpEncodedStreamStopsWhenTheHubHasNoSubscribers(t *testing.T) {
	chunks := [][]byte{{1}, {2}, {3}}
	sent := 0
	broadcast := func([]byte, uint64, agentpb.VideoCodec) bool {
		sent++
		return false // no subscribers left
	}

	err := pumpEncodedStream(context.Background(), &burstReader{chunks: chunks},
		agentpb.VideoCodec_VIDEO_CODEC_H264, broadcast, func() {})
	if err != nil {
		t.Fatalf("pumpEncodedStream returned %v, want nil", err)
	}
	if sent != 1 {
		t.Fatalf("broadcast called %d times, want 1 (stop on the first refusal)", sent)
	}
}

func TestPumpEncodedStreamReturnsTheContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	broadcast := func([]byte, uint64, agentpb.VideoCodec) bool { return true }

	err := pumpEncodedStream(ctx, &burstReader{chunks: [][]byte{{1}}},
		agentpb.VideoCodec_VIDEO_CODEC_H264, broadcast, func() {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}
