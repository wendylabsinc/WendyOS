// Package devwatch reports camera device nodes (videoN) appearing in and
// disappearing from a directory such as /dev.
package devwatch

import (
	"errors"
	"regexp"
)

// ErrUnsupported is returned on every platform other than Linux.
var ErrUnsupported = errors.New("watching device nodes needs Linux")

// Event is one videoN node created or removed, or, with Overflow set, word
// that the kernel dropped events because its queue overflowed: some nodes may
// have come and gone unreported, so a caller that tracks them checks again.
// An overflow event has no Name.
type Event struct {
	Name     string
	Created  bool
	Overflow bool
}

var videoNode = regexp.MustCompile(`^video[0-9]+$`)
