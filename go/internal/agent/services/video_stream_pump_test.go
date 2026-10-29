package services

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

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
	broadcast := func(data []byte, _ frameTimestamp, _ agentpb.VideoCodec) bool {
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
		{name: "stalled after first chunk", timedOut: true, sawChunk: true, want: codes.DeadlineExceeded},
		{name: "cancelled during timeout", timedOut: true, sawChunk: true, ctxErr: cancelled, wantErr: cancelled},
		{name: "cancelled with clean read", ctxErr: cancelled, wantErr: cancelled},
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

func TestCameraPipeReaderTimesOutWithoutOutput(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	reader := &cameraPipeReader{pipe: r, timeout: 30 * time.Millisecond, idleTimeout: time.Second}
	if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read error = %v, want timeout", err)
	}
}

func TestCameraPipeReaderRefreshesDeadlineAndDetectsLaterStall(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	reader := &cameraPipeReader{pipe: r, timeout: time.Second, idleTimeout: 100 * time.Millisecond}
	// Keep sending beyond one idle timeout, then leave the writer open and
	// silent, as gst does when the USB driver stops supplying capture buffers.
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 8; i++ {
			if _, err := w.Write([]byte{byte(i)}); err != nil {
				done <- err
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		done <- nil
	}()
	var got []byte
	err = pumpEncodedStream(context.Background(), reader, agentpb.VideoCodec_VIDEO_CODEC_H264,
		func(b []byte, _ frameTimestamp, _ agentpb.VideoCodec) bool {
			got = append(got, b...)
			return true
		}, func() {})
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read error = %v, want timeout after output stopped", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte{0, 1, 2, 3, 4, 5, 6, 7}) {
		t.Fatalf("stream stopped while frames were arriving: %v", got)
	}
}

func TestCameraPipelineReleasesStalledProducerAfterFirstChunk(t *testing.T) {
	program := filepath.Join(t.TempDir(), "gst-test")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf encoded\nexec /bin/sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cameraIdleTimeout+5*time.Second)
	defer cancel()
	svc := &VideoService{logger: zap.NewNop()}
	var received bytes.Buffer
	err := svc.runCameraPipeline(ctx, func(b []byte, _ frameTimestamp, _ agentpb.VideoCodec) bool {
		received.Write(b)
		return true
	}, "/dev/video4", gstEncoderResult{element: "x264enc"}, gstPipelinePlan{args: []string{program}}, nil, nil, nil)
	if ctx.Err() != nil {
		t.Fatal("stalled producer survived until caller cancellation")
	}
	if status.Code(err) != codes.DeadlineExceeded || !strings.Contains(err.Error(), "stopped producing video") {
		t.Fatalf("error = %v, want capture stall timeout", err)
	}
	if received.String() != "encoded" {
		t.Fatalf("received %q, want initial video chunk", received.String())
	}
}

func TestPumpEncodedStreamSignalsTheFirstChunkExactlyOnce(t *testing.T) {
	chunks := [][]byte{{1}, {2}, {3}}
	calls := 0
	broadcast := func([]byte, frameTimestamp, agentpb.VideoCodec) bool { return true }

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
	broadcast := func(data []byte, _ frameTimestamp, _ agentpb.VideoCodec) bool {
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
	broadcast := func([]byte, frameTimestamp, agentpb.VideoCodec) bool {
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
	broadcast := func([]byte, frameTimestamp, agentpb.VideoCodec) bool { return true }

	err := pumpEncodedStream(ctx, &burstReader{chunks: [][]byte{{1}}},
		agentpb.VideoCodec_VIDEO_CODEC_H264, broadcast, func() {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}
