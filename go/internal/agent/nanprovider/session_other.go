//go:build !linux

package nanprovider

import (
	"context"
	"errors"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"go.uber.org/zap"
)

type LinkNode interface {
	AttachWithCost(context.Context, int32, *quic.Conn, uint16) error
}

const NANLinkCost uint16 = 512

type Provider struct {
	Credentials *localmesh.Credentials
	Node        LinkNode
	Logger      *zap.Logger
}

func (Provider) Run(context.Context) error {
	return errors.New("NAN provider requires Linux")
}
