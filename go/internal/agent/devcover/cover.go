// Package devcover places a host path (for example a per-app virtual camera
// node) over a path inside a running container's mount namespace, without
// touching the container image or the agent's own namespace.
package devcover

import "errors"

// ErrTimeout is returned by Cover when the cover was not applied before the
// deadline. A timed-out cover is never applied later.
var ErrTimeout = errors.New("cover did not finish in time")

// ErrUnsupported is returned on every platform other than Linux.
var ErrUnsupported = errors.New("covering a path inside a container needs Linux")

// Namespace holds a container's mount namespace open. While the descriptor is
// held the namespace stays pinned, so a later Cover can never land in an
// unrelated process that reused the pid.
type Namespace struct {
	fd  int
	pid uint32
}

// PID is the process the namespace was opened through.
func (n *Namespace) PID() uint32 { return n.pid }
