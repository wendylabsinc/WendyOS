package meshoperatorlink

import (
	"context"
	"crypto/tls"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

func TestOperatorDeadlineNeverReextends(t *testing.T) {
	base := time.Now()
	var clock atomic.Value
	clock.Store(base)
	a, b := net.Pipe()
	defer b.Close()
	l, e := readyWithClock(context.Background(), tls.Client(a, &tls.Config{InsecureSkipVerify: true}), netip.Addr{}, false, base.Add(2*time.Second), func() time.Time { return clock.Load().(time.Time) })
	if e != nil {
		t.Fatal(e)
	}
	defer func() { l.Close(); l.Wait() }()
	clock.Store(base.Add(1900 * time.Millisecond))
	short := l.currentExpiry()
	clock.Store(base)
	after := l.currentExpiry()
	if after.After(short.Add(10 * time.Millisecond)) {
		t.Fatalf("shortened deadline reextended by %s", after.Sub(short))
	}
}
func TestOperatorInvalidLifetimeCannotResurrect(t *testing.T) {
	base := time.Now()
	var clock atomic.Value
	clock.Store(base)
	a, b := net.Pipe()
	defer b.Close()
	l, e := readyWithClock(context.Background(), tls.Client(a, &tls.Config{InsecureSkipVerify: true}), netip.Addr{}, false, base.Add(2*time.Second), func() time.Time { return clock.Load().(time.Time) })
	if e != nil {
		t.Fatal(e)
	}
	defer func() { l.Close(); l.Wait() }()
	clock.Store(base.Add(time.Hour))
	if l.Live() {
		t.Fatal("expired lease stayed live")
	}
	clock.Store(base)
	if l.Live() {
		t.Fatal("expired lease resurrected on backward clock before watcher")
	}
}
