//go:build !linux

package hardware

import "errors"

// v4l2PixelFormats has nothing to ask off Linux: there are no V4L2 nodes.
func v4l2PixelFormats(string) ([]uint32, error) {
	return nil, errors.New("pixel format enumeration requires Linux V4L2")
}
