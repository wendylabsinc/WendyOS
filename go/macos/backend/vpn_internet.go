package backend

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/macos/vpn"
)

func openVPNInternet(ctx context.Context, selector string) (*vpn.Internet, error) {
	if _, matched, err := sessionbroker.ParseMeshSelector(selector); err != nil || !matched {
		return nil, errors.New("choose a nearby mesh device as the internet gateway")
	}
	setup, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	stream, err := sessionbroker.OpenOperatorInternet(setup, selector)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(setup, func() { stream.Close() })
	defer stop()
	config, err := vpn.ReadInternetConfig(stream)
	if err != nil {
		stream.Close()
		return nil, err
	}
	return &vpn.Internet{Config: config, Stream: stream, DNS: func(ctx context.Context) (net.Conn, error) { return sessionbroker.DialOperatorTCP(ctx, selector, 53) }}, nil
}
