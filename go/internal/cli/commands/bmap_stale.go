package commands

import (
	"io"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/t234"
)

// A block-map write skips the image's holes, so it leaves whatever the drive
// held there before. A partition the image ships empty is one big hole: the
// Jetson NVMe image's `data` and UDA map no bytes at all. WendyOS puts `data`
// at the same offset on every release and grows it back over the old
// filesystem on first boot, so after a reflash the board would come back with
// the previous installation's /data and its device identity. A full write of
// the image puts zeros there; the block-map write now does the same at the
// start and end of every partition, which is where the signatures that make
// first boot reuse a partition live (see t234.StaleExtents).
//
// Only holes are cleared, and a hole reads as zeros from the image, so in
// those windows the drive ends up byte-for-byte as a full write leaves it.
// Nothing the image writes is touched, and nothing past the image's end.

// clearStalePartitions zeroes the start and end of each partition in layout,
// the image's GPT, wherever the block map b leaves the image empty. dst is
// the drive as the image was written to it, already clipped to the drive's
// end. An image without a GPT the writer can read (an MBR image) is left as
// written.
func clearStalePartitions(dst io.WriterAt, layout *imageGPT, b *Bmap) error {
	if layout == nil {
		return nil
	}
	var parts, mapped []t234.Extent
	for _, p := range layout.table.Partitions {
		parts = append(parts, t234.Extent{
			Start: int64(p.Start) * layout.sectorSize,
			End:   min((int64(p.End)+1)*layout.sectorSize, b.ImageSize),
		})
	}
	for _, r := range b.Ranges {
		mapped = append(mapped, t234.Extent{
			Start: r.First * b.BlockSize,
			End:   min((r.Last+1)*b.BlockSize, b.ImageSize),
		})
	}
	return t234.ZeroExtents(dst, t234.StaleExtents(parts, mapped), layout.sectorSize)
}

// headCapture keeps the first imageHeadBytes written to it, so a streamed
// image's GPT can be read once the stream has been consumed.
type headCapture struct{ buf []byte }

func (h *headCapture) Write(p []byte) (int, error) {
	if room := imageHeadBytes - len(h.buf); room > 0 {
		h.buf = append(h.buf, p[:min(len(p), room)]...)
	}
	return len(p), nil
}
