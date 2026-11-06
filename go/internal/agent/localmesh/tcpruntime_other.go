//go:build !linux

package localmesh

import (
	"context"
	"errors"
)

func RunConfiguredTCP(context.Context, string, TCPIdentity) error {
	return errors.New("wendy net TCP topology requires Linux")
}

func RunConfiguredTCPObserved(context.Context, string, TCPIdentity, func(func() NodeSnapshot)) error {
	return errors.New("wendy net TCP topology requires Linux")
}
