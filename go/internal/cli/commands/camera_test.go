package commands

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

type mockVideoStream struct {
	frames []*agentpb.VideoFrame
	idx    int
}

func (m *mockVideoStream) Recv() (*agentpb.VideoFrame, error) {
	if m.idx >= len(m.frames) {
		return nil, io.EOF
	}
	f := m.frames[m.idx]
	m.idx++
	return f, nil
}

func TestPipeVideoToStdout_WritesAllFrames(t *testing.T) {
	stream := &mockVideoStream{
		frames: []*agentpb.VideoFrame{
			{Data: []byte{0x00, 0x00, 0x00, 0x01}},
			{Data: []byte{0x41, 0x42, 0x43}},
		},
	}
	var buf bytes.Buffer
	if err := pipeVideoToStdout(stream, &buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []byte{0x00, 0x00, 0x00, 0x01, 0x41, 0x42, 0x43}
	if !bytes.Equal(buf.Bytes(), expected) {
		t.Errorf("got %v, want %v", buf.Bytes(), expected)
	}
}

func TestPipeVideoToStdout_EmptyStream(t *testing.T) {
	stream := &mockVideoStream{}
	var buf bytes.Buffer
	if err := pipeVideoToStdout(stream, &buf); err != nil {
		t.Fatalf("unexpected error for empty stream: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected empty output, got %d bytes", buf.Len())
	}
}

func TestPlaybackPipelineArgs_H264UsesTypefindNotBareCaps(t *testing.T) {
	args := playbackPipelineArgs(agentpb.VideoCodec_VIDEO_CODEC_H264)
	joined := strings.Join(args, " ")

	// Regression: a bare "video/x-h264" capsfilter directly after fdsrc cannot
	// fixate caps onto fdsrc's untyped buffers and fails to preroll with
	// "Output caps are unfixed". typefind must classify the stream instead.
	if !strings.Contains(joined, "fdsrc fd=0 ! typefind ! h264parse") {
		t.Errorf("H264 pipeline must route fdsrc through typefind into h264parse, got: %v", args)
	}
	if strings.Contains(joined, "! video/x-h264 !") {
		t.Errorf("H264 pipeline must not use a bare video/x-h264 capsfilter after fdsrc, got: %v", args)
	}
}

func TestPlaybackPipelineArgs_VP8UsesMatroskademux(t *testing.T) {
	args := playbackPipelineArgs(agentpb.VideoCodec_VIDEO_CODEC_VP8)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "fdsrc fd=0 ! matroskademux ! vp8dec") {
		t.Errorf("VP8 pipeline must demux WebM via matroskademux into vp8dec, got: %v", args)
	}
}

func TestPlayVideoWithGStreamer_MissingGStreamer(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir — no executables

	stream := &mockVideoStream{}
	err := playVideoWithGStreamer(context.Background(), stream)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "gst-launch-1.0 not found") {
		t.Errorf("expected 'gst-launch-1.0 not found' error, got: %v", err)
	}
}
