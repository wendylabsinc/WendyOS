//go:build !linux

package audioloop

import (
	"context"
	"errors"
)

var errLinuxOnly = errors.New("snd-aloop mic mount requires Linux")

func defaultDeps() deps {
	return deps{
		modprobe: func(context.Context) error {
			return errLinuxOnly
		},
		newWriter: func(_ context.Context, hwID string, f PCMFormat) (AudioWriter, error) {
			return nil, errLinuxOnly
		},
	}
}
