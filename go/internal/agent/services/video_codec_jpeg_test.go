package services

import (
	"testing"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestVideoCodecMJPEGIsNotAWireEnumValue(t *testing.T) {
	if name, defined := agentpb.VideoCodec_name[int32(videoCodecMJPEG)]; defined {
		t.Fatalf("videoCodecMJPEG (%d) collides with wire enum value %q", int32(videoCodecMJPEG), name)
	}
}

func TestCodecIndexName(t *testing.T) {
	for codec, want := range map[agentpb.VideoCodec]string{
		videoCodecMJPEG:                     "VIDEO_CODEC_MJPEG",
		agentpb.VideoCodec_VIDEO_CODEC_H264: "VIDEO_CODEC_H264",
		agentpb.VideoCodec_VIDEO_CODEC_VP8:  "VIDEO_CODEC_VP8",
		agentpb.VideoCodec_VIDEO_CODEC_RAW:  "VIDEO_CODEC_RAW",
	} {
		if got := codecIndexName(codec); got != want {
			t.Errorf("codecIndexName(%d) = %q, want %q", int32(codec), got, want)
		}
	}
}
