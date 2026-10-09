//go:build linux

package bleprovider

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
)

func TestCancelPendingACLOnlyForOwnTimedOutDial(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		dialErr, providerErr error
		index                int
		want                 bool
	}{
		{"socket deadline", context.DeadlineExceeded, nil, 0, true},
		{"wrapped deadline", errors.Join(errors.New("connect"), context.DeadlineExceeded), nil, 1, true},
		{"provider stopped", context.DeadlineExceeded, context.Canceled, 0, false},
		{"socket cancelled", context.Canceled, nil, 0, false},
		{"connection refused", errors.New("refused"), nil, 0, false},
		{"unknown adapter", context.DeadlineExceeded, nil, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldCancelPendingACL(tc.dialErr, tc.providerErr, tc.index); got != tc.want {
				t.Fatalf("shouldCancelPendingACL = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestMgmtDisconnectScopesAdapterAndPeer(t *testing.T) {
	got, err := encodeMgmtDisconnect(2, "E4:5F:01:0D:81:46", "random")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x14, 0x00, 0x02, 0x00, 0x07, 0x00, 0x46, 0x81, 0x0d, 0x01, 0x5f, 0xe4, btAddrLERandom}
	if !bytes.Equal(got, want) {
		t.Fatalf("management packet = %x, want %x", got, want)
	}
	got, err = encodeMgmtDisconnect(0, "12:34:56:78:9A:BC", "public")
	if err != nil {
		t.Fatal(err)
	}
	if got[12] != btAddrLEPublic || binary.LittleEndian.Uint16(got[2:4]) != 0 {
		t.Fatalf("wrong public address or adapter: %x", got)
	}
	for _, tc := range []struct {
		index                int
		address, addressType string
	}{
		{-1, "12:34:56:78:9A:BC", "public"},
		{0xffff, "12:34:56:78:9A:BC", "public"},
		{0, "12:34:56:78:9A", "public"},
		{0, "12:34:56:78:9A:BC", "bredr"},
	} {
		if _, err := encodeMgmtDisconnect(tc.index, tc.address, tc.addressType); err == nil {
			t.Fatalf("accepted invalid peer %+v", tc)
		}
	}
}

func TestMgmtDisconnectReplyRequiresMatchingCommandAndAdapter(t *testing.T) {
	for _, tc := range []struct {
		name    string
		packet  []byte
		matched bool
		status  byte
		bad     bool
	}{
		{"complete", []byte{1, 0, 2, 0, 3, 0, 0x14, 0, 0}, true, 0, false},
		{"not connected", []byte{1, 0, 2, 0, 3, 0, 0x14, 0, 2}, true, 2, false},
		{"disconnected pending", []byte{1, 0, 2, 0, 3, 0, 0x14, 0, 0x0e}, true, 0x0e, false},
		{"status", []byte{2, 0, 2, 0, 3, 0, 0x14, 0, 3}, true, 3, false},
		{"other adapter", []byte{1, 0, 1, 0, 3, 0, 0x14, 0, 0}, false, 0, false},
		{"other command", []byte{1, 0, 2, 0, 3, 0, 0x15, 0, 0}, false, 0, false},
		{"broadcast event", []byte{3, 0, 2, 0, 3, 0, 0x14, 0, 0}, false, 0, false},
		{"short header", []byte{1, 0}, false, 0, true},
		{"truncated", []byte{1, 0, 2, 0, 7, 0, 0x14, 0, 0}, false, 0, true},
		{"short command", []byte{1, 0, 2, 0, 2, 0, 0x14, 0}, false, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			matched, status, err := decodeMgmtDisconnectReply(tc.packet, 2)
			if (err != nil) != tc.bad || matched != tc.matched || status != tc.status {
				t.Fatalf("decode = %t, %#x, %v", matched, status, err)
			}
		})
	}
}

func TestMgmtDisconnectAcceptedReplyStatuses(t *testing.T) {
	for _, status := range []byte{mgmtStatusSuccess, mgmtStatusNotConnected, mgmtStatusDisconnected} {
		if !mgmtDisconnectReplyAccepted(status) {
			t.Fatalf("status %#x should acknowledge the management request", status)
		}
	}
	for _, status := range []byte{0x03, 0x0a, 0x0b, 0x0f} {
		if mgmtDisconnectReplyAccepted(status) {
			t.Fatalf("status %#x must remain an error", status)
		}
	}
}

func TestMgmtDisconnectHonorsCancelledContextBeforeOpeningSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := disconnectLEPeer(ctx, 0, "12:34:56:78:9A:BC", "public"); err == nil {
		t.Fatal("cancelled context unexpectedly succeeded")
	}
}
