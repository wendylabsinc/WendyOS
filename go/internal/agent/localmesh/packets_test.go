package localmesh

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func TestIPFragmentsReorderingLossAndLimits(t *testing.T) {
	p := make([]byte, TunnelMTU)
	p[0] = 0x60
	binary.BigEndian.PutUint16(p[4:6], uint16(len(p)-40))
	for i := 40; i < len(p); i++ {
		p[i] = byte(i)
	}
	frags, err := EncodeIP(1, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(frags) != 2 {
		t.Fatal("expected two fragments")
	}
	var a IPAssembler
	now := time.Now()
	if got, err := a.Receive(frags[1], now); err != nil || got != nil {
		t.Fatal(err)
	}
	got, err := a.Receive(frags[0], now)
	if err != nil || !bytes.Equal(got, p) {
		t.Fatal("reassembly failed", err)
	}
	for i := uint32(0); i < 100; i++ {
		ds, _ := EncodeIP(i, p)
		if _, err := a.Receive(ds[0], now); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.pending) != 32 {
		t.Fatal("reassembly not bounded")
	}
	ds, _ := EncodeIP(101, p)
	a.Receive(ds[0], now.Add(2*time.Second))
	if len(a.pending) != 1 {
		t.Fatal("incomplete packet expiry failed")
	}
}

func TestIPFragmentsRejectConflicts(t *testing.T) {
	p := make([]byte, TunnelMTU)
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	ds, _ := EncodeIP(1, p)
	var a IPAssembler
	now := time.Now()
	a.Receive(ds[0], now)
	ds[0][40] ^= 1
	if _, err := a.Receive(ds[0], now); err == nil {
		t.Fatal("conflicting duplicate accepted")
	}
	if _, err := EncodeIP(1, make([]byte, 20)); err == nil {
		t.Fatal("non-IP accepted")
	}
}
