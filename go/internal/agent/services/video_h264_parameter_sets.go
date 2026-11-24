package services

import (
	"bytes"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
)

// h264ParameterSetCacheBytes bounds what one capture remembers. A camera's SPS
// and PPS together are tens to a few hundred bytes; the bound only stops a
// misbehaving device from making the agent hold, and add to every keyframe,
// megabytes of something labelled a parameter set.
const h264ParameterSetCacheBytes = 64 << 10

// h264MaxPPSID is the largest pic_parameter_set_id H.264 allows.
const h264MaxPPSID = 255

// h264ParameterSets remembers the parameter sets one native V4L2 H.264 capture
// has sent and puts them ahead of any IDR that arrives without them.
//
// A decoder can start on an IDR only if it has the SPS and PPS the picture
// refers to. A camera may send those once, when the stream starts, and never
// again; some UVC H.264 firmware does. The capture is shared by every consumer
// of its hub, so whoever joins later (a second viewer, an episode recording, a
// pre-roll ring, the loopback pump after a rejoin) would wait for an IDR it
// can start on and never get one. The GStreamer and IP camera producers repeat
// the sets with `h264parse config-interval=-1`; this does the same for the one
// producer that has no parser in it.
//
// It belongs to one capture. nativeH264Emit makes a new one per capture, so a
// producer restart or another camera starts with nothing remembered, and only
// that capture's goroutine touches it: it is not hub state and needs no lock.
type h264ParameterSets struct {
	// sps is the most recent SPS, and pps the most recent PPS for each
	// pic_parameter_set_id, as NAL units without start codes. Only one SPS is
	// kept: consumers start a unit at the last SPS ahead of the IDR (see
	// h264RandomAccessOffset), so a second one in front would be cut off.
	sps []byte
	pps map[int][]byte
	// size is the bytes held in sps and pps, within h264ParameterSetCacheBytes.
	size int
	// annexB is what gets inserted: sps, then each pps. Built on first use and
	// discarded whenever a set changes.
	annexB []byte
}

// h264Inlining says what inline did with a buffer.
type h264Inlining int

const (
	// h264Unchanged: the buffer goes on as it came. It holds no IDR, its IDR
	// already follows an SPS and a PPS, or it is not a whole access unit.
	h264Unchanged h264Inlining = iota
	// h264Inlined: the remembered parameter sets were put ahead of the IDR.
	h264Inlined
	// h264MissingParameterSets: an IDR came without parameter sets and could
	// not be given them, so it goes on as it came.
	h264MissingParameterSets
)

// inline takes one buffer dequeued from the camera and returns it with the
// remembered parameter sets inserted when it is an IDR access unit lacking
// them. whole is false for a buffer the maxFrameBytes cap cut short.
//
// Every other buffer comes back as the same slice, so a camera that already
// sends SPS and PPS with each IDR is forwarded exactly as before. "Lacking" is
// the consumers' own test, h264RandomAccessOffset: no SPS followed by a PPS
// ahead of the IDR.
func (p *h264ParameterSets) inline(au []byte, whole bool) ([]byte, h264Inlining) {
	// insertAt is where the sets would go: behind the last access unit
	// delimiter, SPS or PPS, else ahead of the first NAL unit. A delimiter has
	// to stay first and SEI can depend on the SPS, so the sets go between them.
	insertAt := -1
	haveSPS, havePPS := false, false // an SPS, and a PPS since the last SPS
	start, header, ok := h264NextNAL(au, 0)
	for ok {
		if insertAt < 0 {
			insertAt = start
		}
		// The walk ends at the first slice, before its payload is searched for
		// a further start code: V4L2 delivers one picture per buffer, so what
		// follows is the rest of that picture and by far most of the bytes.
		nalType := au[header] & 0x1f
		switch nalType {
		case h264NALIDR:
			if (haveSPS && havePPS) || !whole {
				return au, h264Unchanged
			}
			return p.insert(au, insertAt)
		case 1, 2, 3, 4: // a slice of a picture that is not an IDR
			return au, h264Unchanged
		}
		next, nextHeader, more := h264NextNAL(au, header+1)
		end := len(au)
		if more {
			end = next
		}
		switch nalType {
		case h264NALSPS, h264NALPPS:
			switch {
			case more:
				// Kept exactly as the camera delimited it, any padding ahead of
				// the next start code included, so that the copy inserted later
				// is byte for byte the unit a reader split out the first time.
				// The episode export refuses a clip whose SPS or PPS changes.
				p.remember(nalType, au[header:end])
			case whole:
				// Nothing delimits the buffer's last unit. Zero bytes at its
				// end are padding: a NAL unit never ends in one.
				p.remember(nalType, bytes.TrimRight(au[header:end], "\x00"))
			}
			// Otherwise it is the last unit of a truncated buffer and may
			// itself be cut short, so it is not remembered.
			if nalType == h264NALSPS {
				haveSPS, havePPS = true, false
			} else {
				havePPS = true
			}
			insertAt = end
		case h264NALAUD:
			insertAt = end
		}
		start, header, ok = next, nextHeader, more
	}
	return au, h264Unchanged
}

// insert returns a copy of au with the remembered sets at offset at. au itself
// is left alone: the capture loop owns it.
func (p *h264ParameterSets) insert(au []byte, at int) ([]byte, h264Inlining) {
	sets := p.asAnnexB()
	// The hub drops a frame over maxFrameBytes. Growing an IDR past that would
	// take a keyframe away from the viewers who can already decode it.
	if sets == nil || len(au)+len(sets) > maxFrameBytes {
		return au, h264MissingParameterSets
	}
	out := make([]byte, 0, len(au)+len(sets))
	out = append(out, au[:at]...)
	out = append(out, sets...)
	return append(out, au[at:]...), h264Inlined
}

// remember records an SPS or PPS the camera sent, in place of the one it
// supersedes.
func (p *h264ParameterSets) remember(nalType byte, nal []byte) {
	if nalType == h264NALSPS {
		if !bytes.Equal(p.sps, nal) {
			p.sps = p.replace(p.sps, nal)
		}
		return
	}
	id, ok := h264PPSID(nal)
	if !ok {
		return
	}
	old := p.pps[id]
	if bytes.Equal(old, nal) {
		return
	}
	if kept := p.replace(old, nal); kept != nil {
		if p.pps == nil {
			p.pps = make(map[int][]byte)
		}
		p.pps[id] = kept
	} else {
		delete(p.pps, id)
	}
}

// replace returns a copy of nal to hold in place of old, or nil when it would
// not fit the bound. old is given up either way: the camera has replaced it,
// so it is no longer what a decoder should be handed.
func (p *h264ParameterSets) replace(old, nal []byte) []byte {
	p.annexB = nil
	p.size -= len(old)
	if p.size+len(nal) > h264ParameterSetCacheBytes {
		return nil
	}
	p.size += len(nal)
	return bytes.Clone(nal)
}

// asAnnexB returns the remembered sets as a byte stream: the SPS, then each PPS
// in id order, behind the four-byte start codes that a parameter set and the
// first NAL unit of an access unit require. It is nil until there is both an
// SPS and a PPS, since decoding cannot start with only one of them.
func (p *h264ParameterSets) asAnnexB() []byte {
	if p.sps == nil || len(p.pps) == 0 {
		return nil
	}
	if p.annexB == nil {
		startCode := []byte{0, 0, 0, 1}
		p.annexB = append(append(p.annexB, startCode...), p.sps...)
		for id := 0; id <= h264MaxPPSID; id++ {
			if nal := p.pps[id]; nal != nil {
				p.annexB = append(append(p.annexB, startCode...), nal...)
			}
		}
	}
	return p.annexB
}

// h264ExpGolomb reads one unsigned Exp-Golomb code of body, starting at bit,
// and returns the bit behind it. The bits are read as they lie: the fields read
// this way sit directly behind a NAL header and are too short to hold an
// emulation prevention byte, which needs two zero bytes ahead of it.
func h264ExpGolomb(body []byte, bit int) (value, next int, ok bool) {
	readBit := func() (int, bool) {
		if bit >= len(body)*8 {
			return 0, false
		}
		b := int(body[bit/8]>>(7-bit%8)) & 1
		bit++
		return b, true
	}
	zeros := 0
	for {
		b, more := readBit()
		if !more || zeros > 16 {
			return 0, 0, false
		}
		if b == 1 {
			break
		}
		zeros++
	}
	for i := 0; i < zeros; i++ {
		b, more := readBit()
		if !more {
			return 0, 0, false
		}
		value = value<<1 | b
	}
	return 1<<zeros - 1 + value, bit, true
}

// h264PPSID reads pic_parameter_set_id, the first field of a PPS NAL unit.
func h264PPSID(nal []byte) (id int, ok bool) {
	if len(nal) < 2 {
		return 0, false
	}
	id, _, ok = h264ExpGolomb(nal[1:], 0)
	if !ok || id > h264MaxPPSID {
		return 0, false
	}
	return id, true
}

// h264PictureIsBidirectional reports whether an access unit's picture is coded
// in B slices, read off its first slice header (first_mb_in_slice, then
// slice_type). A decoder hands such a picture back before pictures it was given
// earlier.
func h264PictureIsBidirectional(au []byte) bool {
	for _, header, ok := h264NextNAL(au, 0); ok; _, header, ok = h264NextNAL(au, header+1) {
		switch au[header] & 0x1f {
		case 1, 2, h264NALIDR:
			_, bit, ok := h264ExpGolomb(au[header+1:], 0)
			if !ok {
				return false
			}
			sliceType, _, ok := h264ExpGolomb(au[header+1:], bit)
			return ok && sliceType%5 == 1
		}
	}
	return false
}

// nativeH264Emit returns the emit callback for one native V4L2 H.264 capture.
// Each dequeued buffer goes to broadcast as it came, except an IDR that lacks
// parameter sets, which is first given the ones the camera sent earlier in the
// same capture (see h264ParameterSets).
//
// What it remembers lives in the returned callback, which the capture loop
// calls from its own goroutine only. Make one per capture and never share it
// between captures.
func (s *VideoService) nativeH264Emit(path string, broadcast func([]byte, frameTimestamp, agentpb.VideoCodec) bool) func([]byte, frameTimestamp) bool {
	var (
		sets            h264ParameterSets
		inlined, missed bool // each is logged once per capture, not once per keyframe
	)
	return func(data []byte, stamp frameTimestamp) bool {
		data, result := sets.inline(data, stamp.auAligned)
		switch {
		case result == h264Inlined && !inlined:
			inlined = true
			s.logger.Info("camera sends H.264 keyframes without SPS/PPS; repeating the ones it sent earlier so late viewers and recordings can start",
				zap.String("device", path))
		case result == h264MissingParameterSets && !missed:
			missed = true
			s.logger.Warn("H.264 keyframe has no SPS/PPS and none could be added; a viewer or recording that joins now cannot start decoding",
				zap.String("device", path))
		}
		stamp.native = stamp.auAligned
		return broadcast(data, stamp, agentpb.VideoCodec_VIDEO_CODEC_H264)
	}
}
