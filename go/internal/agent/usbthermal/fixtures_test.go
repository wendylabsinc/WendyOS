package usbthermal

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures are real bytes from a TC001 Max on exxon-01 (2026-10-30), cut
// from a 3.5 MB bulk capture with the camera looking at a room:
//
//   - tc001max-block-window.bin.gz: one whole thermal block with 4 KiB of the
//     visible camera's bytes on either side of it, exactly as they sat in the
//     stream. The block is the first of its composite frame (counter 21901).
//   - tc001max-header-window.bin: 2 KiB centred on a composite-frame header
//     (the header is at offset 1024), with visible bytes around it.
//
// Both are stored base64-encoded (".b64") so that the change stays reviewable
// as text; the bytes themselves are untouched.
const (
	windowMargin   = 4096 // visible bytes before and after the block in the window
	headerAt       = 1024 // offset of the header in the header window
	fixtureCounter = 21901
)

var tc001Max = models[0].Layout

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	text, err := os.ReadFile(filepath.Join("testdata", name+".b64"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(text)), ""))
	if err != nil {
		t.Fatalf("decoding fixture %s: %v", name, err)
	}
	if filepath.Ext(name) != ".gz" {
		return b
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gunzip %s: %v", name, err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip %s: %v", name, err)
	}
	return out
}

// blockWindow returns the real stream excerpt: margin, block, margin.
func blockWindow(t *testing.T) []byte {
	t.Helper()
	w := readFixture(t, "tc001max-block-window.bin.gz")
	if want := 2*windowMargin + tc001Max.BlockBytes(); len(w) != want {
		t.Fatalf("block window is %d bytes, want %d", len(w), want)
	}
	return w
}

// realBlock returns the real thermal block alone.
func realBlock(t *testing.T) []byte {
	t.Helper()
	return blockWindow(t)[windowMargin : windowMargin+tc001Max.BlockBytes()]
}

// withCounter returns a copy of a block window whose block carries a different
// frame counter, standing in for the camera's next frame.
func withCounter(window []byte, seq uint16) []byte {
	out := bytes.Clone(window)
	off := windowMargin + tc001Max.MetadataOffset() + 2*metadataSequenceWord
	binary.LittleEndian.PutUint16(out[off:], seq)
	return out
}

// feedInChunks feeds data to p in pieces of n bytes and collects the blocks.
func feedInChunks(p *Parser, data []byte, n int) [][]byte {
	var got [][]byte
	for len(data) > 0 {
		k := min(n, len(data))
		p.Feed(data[:k], func(b []byte) { got = append(got, b) })
		data = data[k:]
	}
	return got
}
