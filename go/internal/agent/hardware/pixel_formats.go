package hardware

import (
	"slices"
	"strings"
)

const (
	// v4l2PixFmtZ16 is V4L2_PIX_FMT_Z16 in Video for Linux 2 (V4L2), the
	// four-character code (fourcc) 'Z','1','6',' ': 16-bit depth in
	// device-specific units, the format depth cameras such as the Intel
	// RealSense expose on their depth node. Defined here rather than shared
	// with the services package's V4L2 constants because services imports
	// hardware, so the reverse import would be a cycle.
	v4l2PixFmtZ16 uint32 = 0x2036315A

	// maxListedPixelFormats bounds the pixel_formats property. Real nodes
	// advertise a handful; the bound keeps a misbehaving driver from bloating
	// every hardware listing.
	maxListedPixelFormats = 16
)

// enumeratePixelFormats lists the capture pixel formats (V4L2 fourccs) a video
// node advertises through VIDIOC_ENUM_FMT. Behind a var so discovery tests can describe a camera without a
// V4L2 node, in the spirit of the services package's enumerateRawFrameSizes.
// An error means the node could not be opened or refused the ioctl.
var enumeratePixelFormats = v4l2PixelFormats

// addPixelFormatProperties records what a node's formats say about it:
// "pixel_formats" is the sorted, de-duplicated, trimmed fourcc list (at most
// maxListedPixelFormats entries), and "depth" is "z16" when the node offers
// Z16 depth frames.
func addPixelFormatProperties(props map[string]string, formats []uint32) {
	var names []string
	for _, f := range formats {
		if f == v4l2PixFmtZ16 {
			props["depth"] = "z16"
		}
		if name, ok := fourccName(f); ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	if len(names) > maxListedPixelFormats {
		names = names[:maxListedPixelFormats]
	}
	if len(names) > 0 {
		props["pixel_formats"] = strings.Join(names, ",")
	}
}

// fourccName renders a V4L2 fourcc (little-endian, first character in the low
// byte) with trailing padding trimmed. A code with a byte outside printable
// ASCII, such as one carrying the big-endian flag in bit 31, is skipped rather
// than rendered as garbage.
func fourccName(f uint32) (string, bool) {
	b := []byte{byte(f), byte(f >> 8), byte(f >> 16), byte(f >> 24)}
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return "", false
		}
	}
	name := strings.TrimSpace(string(b))
	return name, name != ""
}
