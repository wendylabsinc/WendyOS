// Package devwatch reports camera device nodes (videoN) appearing in and
// disappearing from a directory such as /dev.
package devwatch

import (
	"errors"
	"regexp"
)

// ErrUnsupported is returned on every platform other than Linux.
var ErrUnsupported = errors.New("watching device nodes needs Linux")

// Event is one videoN node created or removed.
type Event struct {
	Name    string
	Created bool
}

var videoNode = regexp.MustCompile(`^video[0-9]+$`)
