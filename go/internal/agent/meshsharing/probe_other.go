//go:build !linux

package meshsharing

import (
	"context"
	"errors"
)

func NewLinux(context.Context, int32, int32, Node) (*Controller, error) {
	return nil, errors.New("mesh Internet sharing requires Linux")
}
