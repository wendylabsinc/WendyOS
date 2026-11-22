package episodeexport

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestSnapshotMovieKeepsTheCapturedKeyframe(t *testing.T) {
	// A 32×24 solid red frame encoded by x264, including its parameter sets.
	raw, _ := hex.DecodeString("000000016764100aacb92fcb808800000300080000030190200000000168ee0f2c8b00000165888404bffee8c9fccb2b2fe3d437b34629b349a71bb0702fe1")
	movie, err := SnapshotMovie(raw)
	if err != nil {
		t.Fatal(err)
	}
	units := splitAnnexB(raw)
	if len(units) != 3 || !bytes.Contains(movie, concat(u32(uint32(len(units[2]))), units[2])) || !bytes.Contains(movie, []byte("avcC")) {
		t.Fatal("snapshot picture or decoder configuration lost")
	}
	for _, bad := range [][]byte{nil, raw[:30], append(append([]byte{}, raw...), 0, 0, 1, 0x41, 0xfe)} {
		if _, err := SnapshotMovie(bad); err == nil {
			t.Fatal("accepted a partial snapshot or multiple pictures")
		}
	}
}
