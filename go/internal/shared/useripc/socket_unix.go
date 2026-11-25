//go:build !windows && !ios

package useripc

import (
	"context"
	"net"
	"os"
)

func Listen(path string) (net.Listener, error) {
	if inProcess.Load() {
		return memoryListen(path)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}
func Dial(ctx context.Context, path string) (net.Conn, error) {
	if inProcess.Load() {
		return memoryDial(ctx, path)
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}
