//go:build !darwin && !windows

package commands

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const realtimeAudioAvailable = false

func playRealtimeAudio(_ context.Context, _ interface {
	Recv() (*agentpb.AudioChunk, error)
}, _, _, _ uint32) error {
	return fmt.Errorf("audio playback is not available in this CLI build; use --stdout to write raw PCM audio")
}
