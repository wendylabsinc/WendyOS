//go:build !darwin && !windows

package commands

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func playRealtimeAudio(_ context.Context, _ interface {
	Recv() (*agentpb.AudioChunk, error)
}, _, _ uint32) error {
	return fmt.Errorf("audio playback is not available in this CLI build; use --stdout to write raw PCM audio")
}
