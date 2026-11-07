package services

// webmInitialization retains only the EBML and Segment metadata preceding the
// first Cluster. A model joining an existing producer needs these bytes to
// initialize its demuxer. Payload bytes and sample identities stay unchanged.
// The hub mutex protects this state. See the WebM container guidelines:
// https://www.webmproject.org/docs/container/
type webmInitialization struct {
	pending []byte
	header  []byte
	offset  int
	segment bool
	failed  bool
}

const maxWebMInitialization = 1 << 20

func (w *webmInitialization) feed(data []byte) []byte {
	if w.header != nil || w.failed {
		return w.header
	}
	// Only retain a bounded prefix, even if the first read contains a large Cluster.
	remaining := maxWebMInitialization - len(w.pending)
	if len(data) > remaining {
		data = data[:remaining]
	}
	w.pending = append(w.pending, data...)
	for {
		id, n, _, ok := webmVint(w.pending[w.offset:], true)
		if !ok {
			break
		}
		if w.offset == 0 && id != 0x1a45dfa3 {
			w.failed = true
			break
		}
		if w.segment && id == 0x1f43b675 {
			w.header = append([]byte(nil), w.pending[:w.offset]...)
			w.pending = nil
			return w.header
		}
		size, m, unknown, ok := webmVint(w.pending[w.offset+n:], false)
		if !ok {
			break
		}
		end := w.offset + n + m
		if id == 0x18538067 && !w.segment {
			// A resumed stream has a different byte length; declare an unknown-sized
			// Segment, preserving the encoded size's width.
			w.pending[w.offset+n] = byte((1 << (9 - m)) - 1)
			for i := w.offset + n + 1; i < end; i++ {
				w.pending[i] = 0xff
			}
			w.segment = true
			w.offset = end
			continue
		}
		if unknown || size > uint64(maxWebMInitialization-end) {
			w.failed = true
			break
		}
		if size > uint64(len(w.pending)-end) {
			break
		}
		w.offset = end + int(size)
	}
	if len(w.pending) == maxWebMInitialization {
		w.failed = true
	}
	if w.failed {
		w.pending = nil
	}
	return nil
}

// webmVint reads an EBML identifier or data size without allocating. Identifier
// marker bits remain part of the value; size marker bits do not.
func webmVint(b []byte, id bool) (value uint64, width int, unknown, ok bool) {
	if len(b) == 0 || b[0] == 0 {
		return
	}
	marker := byte(0x80)
	width = 1
	for b[0]&marker == 0 {
		marker >>= 1
		width++
	}
	if (id && width > 4) || len(b) < width {
		return 0, 0, false, false
	}
	value = uint64(b[0])
	if !id {
		value &= uint64(marker - 1)
	}
	for _, v := range b[1:width] {
		value = value<<8 | uint64(v)
	}
	unknown = !id && value == (uint64(1)<<(7*width))-1
	return value, width, unknown, true
}

// WebM element identifiers the join walker steps into or inspects.
const (
	webmIDSegment     = 0x18538067
	webmIDCluster     = 0x1f43b675
	webmIDBlockGroup  = 0xa0
	webmIDBlock       = 0xa1
	webmIDSimpleBlock = 0xa3
)

// webmJoinPoint is where, within one VP8 frame, a viewer that joined after the
// stream began can start: it is sent prefix, then data[offset:]. prefix is the
// WebM initialization followed by any of that Cluster's opening bytes carried
// over from earlier reads. The zero value passes the frame through unchanged.
type webmJoinPoint struct {
	prefix []byte
	offset int
}

// webmJoinAsIs marks the stream's first frame, which carries its own
// initialization, and every frame once the structure could not be followed.
var webmJoinAsIs = &webmJoinPoint{}

// join returns the bytes a late viewer is sent for the frame data carrying p.
func (p *webmJoinPoint) join(data []byte) []byte {
	if p.prefix == nil {
		return data[p.offset:]
	}
	out := make([]byte, 0, len(p.prefix)+len(data)-p.offset)
	return append(append(out, p.prefix...), data[p.offset:]...)
}

// webmJoins follows a WebM stream element by element, across arbitrary pipe
// reads, to find where a viewer joining mid-stream can start: each Cluster
// whose first frame is a VP8 key frame. The stream is one container, not a
// sequence of frames, so bytes cut from its middle can be neither demuxed (no
// EBML header or Tracks) nor decoded (delta frames first). Segment, Cluster
// and BlockGroup are stepped into, since streamable WebM leaves their sizes
// unknown; every other element is skipped by its size, so payload bytes are
// never read as identifiers. The hub mutex protects this state.
type webmJoins struct {
	hdr     [12]byte // the element header being read; ID and size may span reads
	hdrN    int
	skip    uint64   // payload bytes of the current element still to pass over
	block   [12]byte // the opening bytes of the open Cluster's first block
	blockN  int
	peek    bool   // the element being skipped is that first block
	open    bool   // a Cluster has begun and its first block is still unread
	carry   []byte // the open Cluster's bytes from earlier reads
	started bool
	failed  bool
}

// feed advances over one read of the stream and returns its join point, or nil
// when a late viewer cannot start in it. init is the initialization
// webmInitialization reported for the same read. A stream whose structure
// cannot be followed passes every frame through as before, rather than holding
// late viewers back for good.
func (w *webmJoins) feed(data, init []byte) *webmJoinPoint {
	if w.failed {
		return webmJoinAsIs
	}
	var point *webmJoinPoint
	if !w.started {
		w.started = true
		point = webmJoinAsIs
	}
	from := 0 // where the open Cluster's bytes begin in data
	for i := 0; i < len(data); {
		if w.skip > 0 || w.peek {
			n := int(min(w.skip, uint64(len(data)-i)))
			if w.peek {
				w.blockN += copy(w.block[w.blockN:], data[i:i+n])
			}
			i += n
			w.skip -= uint64(n)
			if w.peek {
				key, known := vp8BlockKeyframe(w.block[:w.blockN])
				if !known && w.skip > 0 {
					continue
				}
				if key && point == nil {
					if init == nil {
						return w.fail()
					}
					point = &webmJoinPoint{prefix: init, offset: from}
					if len(w.carry) > 0 {
						point.prefix = append(append([]byte(nil), init...), w.carry...)
					}
				}
				w.peek, w.open, w.carry = false, false, nil
			}
			continue
		}
		w.hdr[w.hdrN] = data[i]
		w.hdrN++
		i++
		id, idN, _, ok := webmVint(w.hdr[:w.hdrN], true)
		if !ok {
			if w.hdr[0] < 0x10 { // a zero byte, or an identifier wider than four bytes
				return w.fail()
			}
			continue
		}
		size, _, unknown, ok := webmVint(w.hdr[idN:w.hdrN], false)
		if !ok {
			if w.hdrN > idN && w.hdr[idN] == 0 { // a size wider than eight bytes
				return w.fail()
			}
			continue
		}
		start := i - w.hdrN // negative when the header began in an earlier read
		switch id {
		case webmIDCluster:
			w.open, w.carry, from = true, nil, max(start, 0)
			if start < 0 {
				w.carry = append(w.carry, w.hdr[:-start]...)
			}
		case webmIDSegment, webmIDBlockGroup:
			// Masters whose children are walked in place.
		default:
			if unknown {
				return w.fail()
			}
			w.skip = size
			w.peek, w.blockN = w.open && (id == webmIDSimpleBlock || id == webmIDBlock), 0
		}
		w.hdrN = 0
	}
	if w.open {
		w.carry = append(w.carry, data[from:]...)
		if len(w.carry) > maxWebMInitialization {
			w.peek, w.open, w.carry = false, false, nil
		}
	}
	return point
}

func (w *webmJoins) fail() *webmJoinPoint {
	w.failed, w.carry = true, nil
	return webmJoinAsIs
}

// vp8BlockKeyframe reads a block's opening bytes: track number, 16-bit timecode
// and flags, then the VP8 frame tag, whose low bit is clear on a key frame
// (RFC 6386 section 9.1). A laced block is not a clean start. known is false
// while b is too short to tell.
func vp8BlockKeyframe(b []byte) (key, known bool) {
	if len(b) > 0 && b[0] == 0 {
		return false, true
	}
	_, n, _, ok := webmVint(b, false)
	if !ok || len(b) < n+4 {
		return false, false
	}
	return b[n+2]&0x06 == 0 && b[n+3]&0x01 == 0, true
}
