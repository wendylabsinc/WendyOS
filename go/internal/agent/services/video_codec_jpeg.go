package services

import agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"

// Not a value of the VideoCodec wire enum: it must never be written to a
// VideoFrame that leaves the agent.
const videoCodecMJPEG agentpb.VideoCodec = 3

func codecIndexName(codec agentpb.VideoCodec) string {
	if codec == videoCodecMJPEG {
		return "VIDEO_CODEC_MJPEG"
	}
	return codec.String()
}
