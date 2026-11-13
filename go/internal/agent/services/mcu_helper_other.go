//go:build !unix

package services

import (
	"context"
	"errors"
	"io"
)

func runMCUHelper(context.Context, ...string) (io.ReadCloser, func() error, error) {
	return nil, nil, errors.New("microcontroller flashing needs a Unix host")
}
