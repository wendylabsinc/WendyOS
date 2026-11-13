package linklocal

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// Windows reports Winsock codes, which syscall.ECONNREFUSED does not match.
func TestDialReturnsAWinsockRefusalWithoutWaiting(t *testing.T) {
	stubSeams(t, []link{linkA, linkB}, failPlain(t),
		func(ctx context.Context, l link, _ string) (net.Conn, error) {
			if l.ifi.Name == linkB.ifi.Name {
				return nil, &net.OpError{Op: "dial", Net: "tcp4", Err: windows.WSAECONNREFUSED}
			}
			<-ctx.Done()
			return nil, ctx.Err()
		})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	conn, err := Dial(ctx, deviceAddr)
	if conn != nil || !errors.Is(err, windows.WSAECONNREFUSED) || time.Since(start) > time.Second {
		t.Fatalf("Dial = %v, %v after %v; want an immediate WSAECONNREFUSED", conn, err, time.Since(start))
	}
}
