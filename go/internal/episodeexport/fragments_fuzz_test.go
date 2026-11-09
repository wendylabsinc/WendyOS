package episodeexport

import (
	"bytes"
	"testing"
)

// Vary transport boundaries without varying the valid multi-slice bitstream.
// The byte and timestamp oracle is independent of the assembler's NAL parser.
func FuzzH264ChunkBoundaries(f *testing.F) {
	for _, sizes := range [][]byte{{}, {0}, {255}, {1, 2, 3, 4}, {8, 0, 5, 1, 255}} {
		f.Add(sizes)
	}
	f.Fuzz(func(t *testing.T, sizes []byte) {
		want := [][]byte{
			annexB([]byte{7, 0x42}, []byte{8, 0x80}, []byte{0x65, 0xB8, 0xAA}, []byte{0x65, 0x4C, 0xBB}),
			annexB([]byte{9, 0xF0}, []byte{0x41, 0xC0, 0xCC}),
			annexB([]byte{0x41, 0xC0, 0xDD}),
		}
		stream := bytes.Join(want, nil)
		var chunks [][]byte
		var starts []int
		for pos := 0; pos < len(stream); {
			n := len(stream)
			if len(sizes) > 0 {
				n = int(sizes[len(chunks)%len(sizes)]) + 1
			}
			end := min(pos+n, len(stream))
			starts = append(starts, pos)
			chunks = append(chunks, stream[pos:end])
			pos = end
		}
		got, stamps, _, skipped := assembleTestChunks(t, chunks)
		if len(got) != len(want) || skipped != 0 {
			t.Fatalf("pictures=%d skipped=%d, want %d intact pictures", len(got), skipped, len(want))
		}
		pos := 0
		for i, unit := range want {
			chunk := 0
			for chunk+1 < len(starts) && starts[chunk+1] <= pos {
				chunk++
			}
			if !bytes.Equal(got[i], unit) || stamps[i] != int64(chunk)*1000 {
				t.Fatalf("picture %d: bytes=%x stamp=%d; want %x at chunk %d", i, got[i], stamps[i], unit, chunk)
			}
			pos += len(unit)
		}
	})
}
