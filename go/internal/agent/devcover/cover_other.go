//go:build !linux

package devcover

import "time"

// Open is unsupported off Linux.
func Open(pid uint32) (*Namespace, error) { return nil, ErrUnsupported }

func openPath(nsPath string, pid uint32) (*Namespace, error) { return nil, ErrUnsupported }

// Inode is unsupported off Linux.
func (n *Namespace) Inode() (uint64, error) { return 0, ErrUnsupported }

// Close is a no-op off Linux.
func (n *Namespace) Close() error { return nil }

// Cover is unsupported off Linux.
func Cover(n *Namespace, src, dst string, timeout time.Duration) error {
	return ErrUnsupported
}

// Covered is unsupported off Linux.
func Covered(n *Namespace, src, dst string) (bool, error) { return false, ErrUnsupported }
