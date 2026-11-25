package unitree

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestMegaphoneBlocksReassembleTheClip(t *testing.T) {
	if !bytes.HasPrefix(barkWAV, []byte("RIFF")) || !bytes.Equal(barkWAV[8:12], []byte("WAVE")) {
		t.Fatal("embedded bark is not a WAV file")
	}
	blocks := megaphoneBlocks(barkWAV)
	var encoded string
	for i, raw := range blocks {
		var block struct {
			Size    int    `json:"current_block_size"`
			Content string `json:"block_content"`
			Index   int    `json:"current_block_index"`
			Total   int    `json:"total_block_number"`
		}
		if err := json.Unmarshal([]byte(raw), &block); err != nil {
			t.Fatal(err)
		}
		if block.Index != i+1 || block.Total != len(blocks) || block.Size != len(block.Content) || block.Size > megaphoneBlockSize || block.Size == 0 {
			t.Fatalf("block %d: index=%d total=%d size=%d content=%d", i, block.Index, block.Total, block.Size, len(block.Content))
		}
		encoded += block.Content
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || !bytes.Equal(decoded, barkWAV) {
		t.Fatalf("blocks do not reassemble the clip: %v", err)
	}
}
