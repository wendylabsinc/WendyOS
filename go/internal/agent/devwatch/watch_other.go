//go:build !linux

package devwatch

import "context"

// Watch is unsupported off Linux.
func Watch(ctx context.Context, dir string, onEvent func(Event)) error {
	return ErrUnsupported
}
