package meshingress

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestReleaseRevokesExistingTCPWithoutClosingOtherOwner(t *testing.T) {
	r := NewRegistry()
	for owner, port := range map[string]uint16{"camera": 18080, "other": 18081} {
		if err := r.Claim(owner, port); err != nil {
			t.Fatal(err)
		}
	}
	dial := func(port uint16) (net.Conn, net.Conn) {
		t.Helper()
		a, b := net.Pipe()
		conn, err := r.DialAuthorized(port, func() (net.Conn, error) { return a, nil })
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close(); b.Close() })
		return conn, b
	}
	camera, remoteCamera := dial(18080)
	other, remoteOther := dial(18081)
	r.Release("camera")
	_ = remoteCamera.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var got [1]byte
	if _, err := remoteCamera.Read(got[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("revoked TCP remained usable/open: %v", err)
	}
	write := make(chan error, 1)
	go func() { _, err := other.Write([]byte{42}); write <- err }()
	_ = remoteOther.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(remoteOther, got[:]); err != nil || got[0] != 42 {
		t.Fatalf("other app affected: %v %v", got, err)
	}
	if err := <-write; err != nil {
		t.Fatal(err)
	}
	if _, err := r.DialAuthorized(18080, func() (net.Conn, error) { t.Fatal("revoked port dialed"); return nil, nil }); !errors.Is(err, ErrPortDenied) {
		t.Fatal(err)
	}
	// Reclaiming the same owner must not let a late close of its old connection
	// release the replacement's live connection.
	if err := r.Claim("camera", 18080); err != nil {
		t.Fatal(err)
	}
	replacement, remoteReplacement := dial(18080)
	_ = camera.Close()
	go func() { _, err := replacement.Write([]byte{43}); write <- err }()
	_ = remoteReplacement.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(remoteReplacement, got[:]); err != nil || got[0] != 43 {
		t.Fatalf("old close revoked replacement: %v %v", got, err)
	}
	if err := <-write; err != nil {
		t.Fatal(err)
	}
}
