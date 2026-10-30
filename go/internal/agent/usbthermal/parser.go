package usbthermal

import (
	"bytes"
	"encoding/binary"
)

// Parser reassembles thermal blocks from the camera's bulk stream.
//
// What the stream looks like (TC001 Max, measured on exxon-01): one endless
// run of 512-byte USB packets carrying two things at once, the visible camera
// and the thermal core. Every 1,777,664 bytes a 12-byte UVC-style payload
// header (Header) starts a "composite" frame; between two headers sit a visible
// YUYV picture and, wherever the camera's DMA happened to put them, two whole
// thermal blocks of Layout.BlockBytes (197,632) bytes each. There are no end
// markers.
//
// The header therefore does NOT say where a thermal frame starts: the blocks
// float. In one capture they sat 451,584 and 1,451,008 bytes after one header
// and 566,272 and 1,074,176 bytes after the next. Taking "the bytes at a fixed
// offset from the header" is how a first decode read a metadata row as
// 240 degC pixels.
//
// So the parser synchronises on the block itself, which is self-delimiting:
//
//   - it ends with a row of 0xFF 0x00 repeated (the end-of-block row);
//   - it is exactly BlockBytes long, so its start is end - BlockBytes;
//   - its first Height rows are a grey picture whose every odd byte is 0x80.
//
// A candidate is accepted only when all three hold. The third check is what
// makes a wrong boundary fail loudly instead of decoding: a block cut short by
// a lost packet, or taken from the wrong offset, starts in visible-camera
// bytes or temperature words, and neither has exactly 0x80 in 49,152 places.
//
// One subtlety: the metadata row before the end-of-block row ends in 0xFF 0x00
// pairs too (73 of them on the TC001 Max), so the end row is found as the END
// of a run of such pairs, not its first 512 bytes. Should the visible picture
// after a block happen to begin with the same pair, the shorter candidates in
// the run are tried as well; only the true end passes the picture check.
//
// Headers are still parsed, at packet boundaries in the phase the last block
// established, and counted (Stats.Headers): two thermal frames per composite
// frame is the healthy ratio, and the header is where reassembly of the visible
// picture will start when that is added.
//
// Resync is implicit: whatever happens to the stream (a lost packet, a camera
// restart, the agent joining mid-frame), the next intact block is found by its
// own structure, and everything before it is discarded. The parser holds at most
// about one block plus compactThreshold bytes of history.
//
// A Parser is not safe for concurrent use.
type Parser struct {
	layout   Layout
	endRow   []byte // one row of 0xFF 0x00
	maxRun   int    // longest run of 0xFF 0x00 that can still end a block
	buf      []byte // undiscarded stream bytes; buf[0] is stream offset base
	base     int64
	scan     int   // index into buf where the next end-row search starts
	nextHead int64 // stream offset of the next packet boundary to check for a header; -1 = phase unknown
	haveSeq  bool
	lastSeq  uint16
	stats    Stats
}

// Stats counts what a Parser has seen. Everything here is diagnostics: none of
// it changes which frames are delivered.
type Stats struct {
	Bytes  uint64 // stream bytes fed
	Frames uint64 // thermal blocks that passed validation
	// Rejected counts end-of-block rows whose block failed validation: a block
	// cut short, corrupted, or a picture whose chroma is not the neutral grey
	// this driver was measured with.
	Rejected uint64
	// Partial counts end-of-block rows whose block began before the parser's
	// first byte: the stream was joined mid-frame. Expected once per start.
	Partial uint64
	// Lost counts frames the camera's own counter says were sent but never
	// arrived whole (gaps in RawFrame.Sequence).
	Lost uint64
	// Headers counts composite-frame headers seen at packet boundaries.
	Headers    uint64
	LastHeader Header
}

// Header is the 12-byte payload header that starts each composite frame. Its
// shape is a UVC payload header: bHeaderLength 12, bmHeaderInfo with end-of-
// header, presentation time and source clock flags set (0x8C, plus the frame-id
// and end-of-frame bits), a 32-bit presentation time (always 0 so far), a
// 32-bit source clock and an 11-bit USB frame number.
//
// Two consecutive headers from exxon-01:
//
//	0c 8c 00000000 1950db01 1c00
//	0c 8c 00000000 d4c3eb01 6400
type Header struct {
	Info           byte   // bmHeaderInfo, 0x8C..0x8F
	PresentationTS uint32 // dwPresentationTime
	SourceClock    uint32 // scrSourceClock, clock part
	FrameNumber    uint16 // scrSourceClock, USB frame number (0..2047)
}

const (
	// headerLength is bHeaderLength: the header is 12 bytes.
	headerLength = 12
	// usbPacketBytes is the bulk endpoint's wMaxPacketSize. Blocks and headers
	// both start on packet boundaries, which is what gives the header check its
	// phase.
	usbPacketBytes = 512
	// compactThreshold is how much consumed history the parser lets accumulate
	// before moving the live tail down. Moving on every read would copy a whole
	// block's worth of history per 16 KiB read, ~0.3 GB/s at full rate; moving
	// once per MiB costs a fifth of the stream's own rate.
	compactThreshold = 1 << 20
)

// ParseHeader reads a composite-frame header from the first 12 bytes of b.
//
// The signature is short (two fixed bytes), so the 11-bit frame number is
// checked too: picture and temperature bytes almost never have both its top
// five bits clear and the right two leading bytes. It is still only safe at a
// packet boundary, which is the only place the parser looks.
func ParseHeader(b []byte) (Header, bool) {
	if len(b) < headerLength || b[0] != headerLength || b[1]&0xFC != 0x8C {
		return Header{}, false
	}
	h := Header{
		Info:           b[1],
		PresentationTS: binary.LittleEndian.Uint32(b[2:]),
		SourceClock:    binary.LittleEndian.Uint32(b[6:]),
		FrameNumber:    binary.LittleEndian.Uint16(b[10:]),
	}
	if h.FrameNumber >= 2048 {
		return Header{}, false
	}
	return h, true
}

// NewParser returns a parser for blocks of the given layout.
func NewParser(layout Layout) *Parser {
	endRow := make([]byte, layout.RowBytes())
	for i := 0; i < len(endRow); i += 2 {
		endRow[i] = 0xFF
	}
	return &Parser{
		layout:   layout,
		endRow:   endRow,
		maxRun:   4 * layout.RowBytes(),
		nextHead: -1,
	}
}

// Stats returns what the parser has counted so far.
func (p *Parser) Stats() Stats { return p.stats }

// Feed adds stream bytes and calls emit once for every complete, valid thermal
// block they finish, in stream order. The block passed to emit is a fresh copy
// of Layout.BlockBytes bytes that the parser never touches again.
func (p *Parser) Feed(chunk []byte, emit func(block []byte)) {
	p.stats.Bytes += uint64(len(chunk))
	p.buf = append(p.buf, chunk...)
	rowBytes, blockBytes := p.layout.RowBytes(), p.layout.BlockBytes()
	for {
		i := bytes.Index(p.buf[p.scan:], p.endRow)
		if i < 0 {
			// The end row may straddle this chunk and the next.
			p.scan = max(p.scan, len(p.buf)-rowBytes+1)
			break
		}
		m := p.scan + i
		// Find the end of the run of 0xFF 0x00 pairs; the block ends there.
		e := m + rowBytes
		for e+1 < len(p.buf) && p.buf[e] == 0xFF && p.buf[e+1] == 0x00 && e-m < p.maxRun {
			e += 2
		}
		if e-m >= p.maxRun {
			// Far longer than a metadata tail plus one end row: not a block end.
			p.stats.Rejected++
			p.scan = e
			continue
		}
		if e+1 >= len(p.buf) {
			// The run may continue into bytes not read yet.
			p.scan = m
			break
		}
		start, ok := p.findBlock(m, e)
		if !ok {
			p.scan = e
			continue
		}
		end := start + blockBytes
		p.countHeaders(p.base + int64(start))
		block := make([]byte, blockBytes)
		copy(block, p.buf[start:end])
		p.noteSequence(block)
		p.stats.Frames++
		// The block ends on a packet boundary, so the next one fixes the phase
		// headers are looked for at.
		p.nextHead = p.base + int64(end)
		p.scan = end
		emit(block)
	}
	p.countHeaders(p.base + int64(len(p.buf)) - headerLength + 1)
	p.compact()
}

// findBlock tries each place the block could end within the run [m, e), the
// run's own end first, and returns the start of the first candidate that
// validates.
func (p *Parser) findBlock(m, e int) (int, bool) {
	rowBytes, blockBytes := p.layout.RowBytes(), p.layout.BlockBytes()
	for end := e; end >= m+rowBytes; end -= 2 {
		start := end - blockBytes
		if start < 0 {
			// Only possible before the first compaction: the stream was joined
			// partway through this block.
			p.stats.Partial++
			return 0, false
		}
		if p.pictureIsGrey(p.buf[start : start+p.layout.PictureBytes()]) {
			return start, true
		}
	}
	p.stats.Rejected++
	return 0, false
}

// pictureIsGrey reports whether every chroma byte of a picture is 0x80.
func (p *Parser) pictureIsGrey(picture []byte) bool {
	for i := 1; i < len(picture); i += 2 {
		if picture[i] != 0x80 {
			return false
		}
	}
	return true
}

// noteSequence tracks the camera's frame counter to count lost frames. A jump
// of 64 or more (over two seconds of frames) is a camera restart, not a loss.
func (p *Parser) noteSequence(block []byte) {
	seq := blockSequence(p.layout, block)
	if p.haveSeq {
		if gap := seq - p.lastSeq - 1; gap != 0 && gap < 64 {
			p.stats.Lost += uint64(gap)
		}
	}
	p.haveSeq, p.lastSeq = true, seq
}

// blockSequence reads the frame counter from a block's metadata row.
func blockSequence(layout Layout, block []byte) uint16 {
	off := layout.MetadataOffset() + metadataSequenceWord*2
	return binary.LittleEndian.Uint16(block[off:])
}

// metadataSequenceWord is the uint16 index of the frame counter within the
// metadata row. Measured: 21901, 21902, 21903, 21904 on four consecutive
// blocks, with word 180 a second counter moving in step (1737..1740).
const metadataSequenceWord = 64

// countHeaders checks every packet boundary from the current phase up to (not
// including) stream offset upto for a composite-frame header.
func (p *Parser) countHeaders(upto int64) {
	if p.nextHead < 0 {
		return
	}
	for ; p.nextHead < upto; p.nextHead += usbPacketBytes {
		idx := p.nextHead - p.base
		if idx < 0 || idx+headerLength > int64(len(p.buf)) {
			continue
		}
		if h, ok := ParseHeader(p.buf[idx:]); ok {
			p.stats.Headers++
			p.stats.LastHeader = h
		}
	}
}

// compact discards history no future block can reach: a block ends at least
// one end row past the search position, so it starts no earlier than
// BlockBytes minus one row before it.
func (p *Parser) compact() {
	keep := p.scan - (p.layout.BlockBytes() - p.layout.RowBytes())
	if keep < compactThreshold {
		return
	}
	n := copy(p.buf, p.buf[keep:])
	p.buf = p.buf[:n]
	p.base += int64(keep)
	p.scan -= keep
}
