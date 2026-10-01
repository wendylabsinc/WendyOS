//go:build !js

package browserauth

import (
	"context"
	"github.com/coder/websocket"
	"net"
	"net/http"
)

func dialTestRelay(ctx context.Context, endpoint string) (net.Conn, error) {
	ws, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"http://localhost:5173"}}})
	if err != nil {
		return nil, err
	}
	return websocket.NetConn(ctx, ws, websocket.MessageBinary), nil
}
