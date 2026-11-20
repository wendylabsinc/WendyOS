package rawvideo

import "encoding/binary"

// StretchGray16 maps a GRAY16_LE picture onto 8 bits for viewing, writing the
// result into dst (reallocated only when too small) and returning it. src holds
// little-endian 16-bit pixels; the result has one byte per pixel.
//
// The mapping is a per-frame linear min/max stretch: the darkest pixel of the
// frame becomes 0 and the brightest 255. That is the choice that works for the
// sources this exists for. A thermal or depth sensor puts its useful signal in a
// narrow band somewhere inside the 16-bit range (a room at 20-30 °C is a few
// hundred counts out of 65536), so a fixed mapping such as "keep the high byte"
// shows a uniform grey with nothing in it. The cost is that the picture's
// brightness is relative, not absolute: a hot object entering the scene darkens
// everything else. Anyone who needs absolute values reads the raw frames, not the
// encoded preview.
//
// A frame with a single value everywhere maps to mid grey (128) rather than
// black, so a flat scene does not look like a dead camera.
func StretchGray16(dst, src []byte) []byte {
	n := len(src) / 2
	if cap(dst) < n {
		dst = make([]byte, n)
	}
	dst = dst[:n]
	if n == 0 {
		return dst
	}
	lo, hi := uint16(0xffff), uint16(0)
	for i := 0; i < n; i++ {
		v := binary.LittleEndian.Uint16(src[2*i:])
		lo = min(lo, v)
		hi = max(hi, v)
	}
	if lo == hi {
		for i := range dst {
			dst[i] = 128
		}
		return dst
	}
	span := uint32(hi - lo)
	for i := 0; i < n; i++ {
		v := uint32(binary.LittleEndian.Uint16(src[2*i:]) - lo)
		dst[i] = byte((v*255 + span/2) / span)
	}
	return dst
}
