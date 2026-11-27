package unitree

// Megaphone wire shape from unitree_webrtc_connect@e0abc5780761539eff89a382a79319e5cf6ad1f4
// webrtc_audiohub.py: enter (4001), 1-based base64 WAV blocks of 4096
// characters (4003), exit (4002). Firmware acceptance is unverified.
import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

// CommandBark is UnitreeCommand 7. service-protos does not name it yet;
// proto3 enums are open, so an agent without it answers InvalidArgument.
const CommandBark = 7

// BarkTimeout covers every block acknowledgement plus playback.
const BarkTimeout = 20 * time.Second

const audioHubTopic = "rt/api/audiohub/request"
const megaphoneBlockSize = 4096

// The reference client converts its input to 44.1 kHz before uploading, so the
// clip ships at that rate. Source and licence: Examples/Go2CameraSearch/audio.
//
//go:embed bark.wav
var barkWAV []byte

// Exiting megaphone mode early may cut the clip short on the robot.
const barkPlayback = 1700 * time.Millisecond

func megaphoneBlocks(wav []byte) []string {
	encoded := base64.StdEncoding.EncodeToString(wav)
	total := (len(encoded) + megaphoneBlockSize - 1) / megaphoneBlockSize
	blocks := make([]string, 0, total)
	for i := 0; i < total; i++ {
		block := encoded[i*megaphoneBlockSize : min((i+1)*megaphoneBlockSize, len(encoded))]
		parameter, _ := json.Marshal(map[string]any{"current_block_size": len(block), "block_content": block,
			"current_block_index": i + 1, "total_block_number": total})
		blocks = append(blocks, string(parameter))
	}
	return blocks
}

// Bark plays one short clip through Go2's speaker. It sends no motion, posture
// or mode request, and each block waits for the robot's acknowledgement.
func (c *Connection) Bark(ctx context.Context) error {
	if err := c.apiRequest(ctx, audioHubTopic, 4001, "{}"); err != nil {
		return errors.New("Go2 did not enter megaphone mode")
	}
	// Leave megaphone mode even when the upload or the caller's deadline fails.
	defer func() {
		exit, cancel := context.WithTimeout(context.Background(), PostureAckTimeout)
		defer cancel()
		_ = c.apiRequest(exit, audioHubTopic, 4002, "{}")
	}()
	for _, block := range megaphoneBlocks(barkWAV) {
		if err := c.apiRequest(ctx, audioHubTopic, 4003, block); err != nil {
			return errors.New("Go2 did not accept the bark audio")
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return errors.New("Go2 disconnected")
	case <-time.After(barkPlayback):
		return nil
	}
}
