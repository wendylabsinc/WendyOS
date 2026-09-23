//go:build !linux

package localmesh

import (
	"context"
	"errors"
)

func RunConfiguredTCP(context.Context, string, TCPIdentity) error {
	return errors.New("local mesh TCP topology requires Linux")
}
