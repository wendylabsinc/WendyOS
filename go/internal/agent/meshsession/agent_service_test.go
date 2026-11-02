package meshsession

import (
	"errors"
	"net"
	"testing"
)

func TestAgentServiceAllowsOnlyConfiguredTLSAndAuthorizedApps(t *testing.T) {
	auth := WithAgentService(allowPort(func(port uint16) bool { return port == 28081 }), 50052)
	for _, test := range []struct {
		port    uint16
		address string
	}{{50052, "127.0.0.1:50052"}, {28081, "127.0.0.1:28081"}} {
		called := false
		_, err := auth.DialAuthorized(test.port, func(address string) (net.Conn, error) {
			called = true
			if address != test.address {
				t.Fatal(address)
			}
			return nil, nil
		})
		if err != nil || !called {
			t.Fatal(test.port, err)
		}
	}
	for _, port := range []uint16{0, 50051, 22, 28082} {
		if _, err := auth.DialAuthorized(port, func(string) (net.Conn, error) { t.Fatal("denied port dialed"); return nil, nil }); !errors.Is(err, ErrDenied) {
			t.Fatal(port, err)
		}
	}
}
