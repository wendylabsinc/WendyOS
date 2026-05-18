package main

import (
	"fmt"
	"net"
	"sync"
)

// listenDualStackLoopback binds to both 127.0.0.1:port (IPv4) and [::1]:port
// (IPv6) loopback addresses and returns a single net.Listener that accepts
// connections from either. If IPv6 is unavailable the IPv4-only listener is
// returned. Fails only when IPv4 also fails.
func listenDualStackLoopback(port string) (net.Listener, error) {
	lis4, err4 := net.Listen("tcp4", "127.0.0.1:"+port)
	lis6, err6 := net.Listen("tcp6", "[::1]:"+port)

	switch {
	case err4 != nil && err6 != nil:
		return nil, fmt.Errorf("failed to listen on loopback: %v; %v", err4, err6)
	case err4 != nil:
		return lis6, nil
	case err6 != nil:
		return lis4, nil
	}
	return newDualStackListener(lis4, lis6), nil
}

// dualStackListener multiplexes two underlying listeners (IPv4 and IPv6) into
// a single net.Listener. Accept returns the next connection from either.
type dualStackListener struct {
	listeners [2]net.Listener
	connCh    chan net.Conn
	once      sync.Once
	done      chan struct{}
}

func newDualStackListener(lis4, lis6 net.Listener) *dualStackListener {
	l := &dualStackListener{
		listeners: [2]net.Listener{lis4, lis6},
		connCh:    make(chan net.Conn, 64),
		done:      make(chan struct{}),
	}
	go l.acceptLoop(lis4)
	go l.acceptLoop(lis6)
	return l
}

func (l *dualStackListener) acceptLoop(lis net.Listener) {
	for {
		conn, err := lis.Accept()
		if err != nil {
			return // listener closed or fatal; other side keeps going
		}
		select {
		case l.connCh <- conn:
		case <-l.done:
			conn.Close()
			return
		}
	}
}

func (l *dualStackListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connCh:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *dualStackListener) Close() error {
	l.once.Do(func() {
		close(l.done)
		for _, lis := range l.listeners {
			lis.Close()
		}
	})
	return nil
}

// Addr returns the address of the IPv4 listener.
func (l *dualStackListener) Addr() net.Addr {
	return l.listeners[0].Addr()
}
