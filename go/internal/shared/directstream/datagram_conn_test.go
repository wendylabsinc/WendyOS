package directstream

import (
	"bytes"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestDatagramConnRecoveryAndBoundedDrops(t *testing.T) {
	budget, _ := NewDatagramBudget(6 * 1024)
	a, _ := budget.NewConn(7, 1024, nil, nil)
	b, _ := budget.NewConn(7, 1024, nil, nil)
	defer a.Close()
	defer b.Close()
	if _, err := budget.NewConn(9, 1, nil, nil); err != ErrCapacity {
		t.Fatal("unbounded reservation", err)
	}
	if budget.Used() != 6144 {
		t.Fatal("incorrect budget charge")
	}
	if err := a.Bind(1, 1); err != nil {
		t.Fatal(err)
	}
	_ = b.Bind(1, 1)
	transfer := func() bool {
		t.Helper()
		var wire [MaxFlowFrame]byte
		n, g, id, offset, err := a.Prepare(wire[:])
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return false
		}
		f, err := DecodeFlowFrame(wire[:n])
		if err != nil {
			t.Fatal(err)
		}
		if err = b.Receive(g, f); err != nil {
			t.Fatal(err)
		}
		if err = a.Accepted(g, id, offset); err != nil {
			t.Fatal(err)
		}
		return true
	}
	if _, err := a.Write(bytes.Repeat([]byte{1}, 800)); err != nil {
		t.Fatal(err)
	}
	if !transfer() {
		t.Fatal("missing first fragment")
	}
	a.Suspend()
	b.Suspend()
	if n, err := a.Write([]byte("lost during outage")); n != 18 || err != nil {
		t.Fatal(n, err)
	}
	_ = a.Bind(2, 1)
	_ = b.Bind(2, 1)
	if transfer() {
		t.Fatal("partial UDP replayed")
	}
	_, _ = a.Write([]byte("new"))
	_, _ = a.Write([]byte("drop when busy"))
	for transfer() {
	}
	_, _ = a.Write([]byte("drop full receive slot"))
	for transfer() {
	}
	buffer := make([]byte, 20)
	n, err := b.Read(buffer)
	if err != nil || string(buffer[:n]) != "new" {
		t.Fatalf("%q %v", buffer[:n], err)
	}
	tx, _ := a.Drops()
	_, rx := b.Drops()
	if tx != 3 || rx != 2 {
		t.Fatal("drop accounting", tx, rx)
	}
	_, _ = a.Write(nil)
	for transfer() {
	}
	n, err = b.Read(buffer)
	if n != 0 || err != nil {
		t.Fatal("empty UDP lost", n, err)
	}
	_, _ = a.Write([]byte("truncate"))
	for transfer() {
	}
	n, err = b.Read(buffer[:2])
	if n != 2 || err != nil || string(buffer[:2]) != "tr" {
		t.Fatal(n, err)
	}
	_ = b.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	if _, err = b.Read(buffer); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("truncation remainder leaked", err)
	}
	if _, err = a.Write(make([]byte, 1025)); !errors.Is(err, syscall.EMSGSIZE) {
		t.Fatal("oversized datagram accepted", err)
	}
	_ = a.Close()
	_ = a.Close()
	if budget.Used() != 3072 {
		t.Fatal("budget not released exactly once")
	}
	c, err := budget.NewConn(9, 1024, nil, nil)
	if err != nil {
		t.Fatal("budget not reusable", err)
	}
	_ = c.Close()
}
func TestDatagramConnReadWakesForDeadlineAndClose(t *testing.T) {
	for _, closeConn := range []bool{false, true} {
		budget, _ := NewDatagramBudget(24)
		c, _ := budget.NewConn(7, 8, nil, nil)
		done := make(chan error, 1)
		go func() { _, err := c.Read(make([]byte, 8)); done <- err }()
		if closeConn {
			_ = c.Close()
		} else {
			_ = c.SetReadDeadline(time.Now().Add(-time.Second))
		}
		select {
		case err := <-done:
			want := error(os.ErrDeadlineExceeded)
			if closeConn {
				want = net.ErrClosed
			}
			if !errors.Is(err, want) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("blocked read did not wake")
		}
		_ = c.Close()
		if budget.Used() != 0 {
			t.Fatal("budget leaked")
		}
	}
}
