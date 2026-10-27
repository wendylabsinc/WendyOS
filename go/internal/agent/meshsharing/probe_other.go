//go:build !linux

package meshsharing

import (
	"context"
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func NewLinux(context.Context, *localmesh.Credentials, Node) (*Controller, error) {
	return nil, errors.New("wendy net Internet sharing requires Linux")
}
