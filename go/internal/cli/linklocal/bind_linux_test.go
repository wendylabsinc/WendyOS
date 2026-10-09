package linklocal

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"
)

func loopbackLink(t *testing.T) link {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagLoopback != 0 {
			return link{ifi: ifi, ip: net.ParseIP("127.0.0.1")}
		}
	}
	t.Skip("no loopback interface")
	return link{}
}

func TestDialOnLinkConnectsThroughTheBoundInterface(t *testing.T) {
	ln, err := listenOnLink(context.Background(), loopbackLink(t), "127.0.0.1:0")
	if errors.Is(err, syscall.EPERM) {
		t.Skip("SO_BINDTODEVICE needs Linux 5.7+ or CAP_NET_RAW")
	}
	if err != nil {
		t.Fatalf("listenOnLink: %v", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialOnLink(ctx, loopbackLink(t), ln.Addr().String())
	if err != nil {
		t.Fatalf("dialOnLink: %v", err)
	}
	conn.Close()
}

func TestDialOnLinkReportsAnUnbindableInterfaceAsABindError(t *testing.T) {
	missing := link{ifi: net.Interface{Index: 1 << 20, Name: "wendynone0"}}
	conn, err := dialOnLink(context.Background(), missing, "127.0.0.1:9")
	var be *bindError
	if conn != nil || !errors.As(err, &be) {
		t.Fatalf("dialOnLink = %v, %v; want a *bindError", conn, err)
	}
}
