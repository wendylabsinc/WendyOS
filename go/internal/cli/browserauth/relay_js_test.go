//go:build js && wasm

package browserauth

import (
	"context"
	"github.com/coder/websocket"
	"net"
)

func dialTestRelay(ctx context.Context, endpoint string) (net.Conn, error) {
	ws, _, err := websocket.Dial(ctx, endpoint, nil)
	if err != nil {
		return nil, err
	}
	return websocket.NetConn(ctx, ws, websocket.MessageBinary), nil
}
