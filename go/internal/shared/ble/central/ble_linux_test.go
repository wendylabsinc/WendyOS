//go:build linux

package central

import (
	"reflect"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

func TestParseBTAddr(t *testing.T) {
	t.Run("byte order is MSB-first, as SockaddrL2 wants", func(t *testing.T) {
		// The kernel's bdaddr_t is little-endian, but x/sys/unix reverses
		// SockaddrL2.Addr while marshalling, so it takes the human order and
		// converts. Pre-reversing here cancels that out and aims the connect at
		// a byte-reversed peer that never answers — which is exactly how this
		// failed: every PSM timed out instead of being refused.
		got, err := parseBTAddr("AA:BB:CC:DD:EE:FF")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := [6]byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF}
		if got != want {
			t.Errorf("parseBTAddr = %v, want %v", got, want)
		}
	})

	t.Run("lowercase is accepted", func(t *testing.T) {
		lower, err := parseBTAddr("aa:bb:cc:dd:ee:ff")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		upper, _ := parseBTAddr("AA:BB:CC:DD:EE:FF")
		if lower != upper {
			t.Errorf("case changed the result: %v vs %v", lower, upper)
		}
	})

	t.Run("digits parse as hex, not decimal", func(t *testing.T) {
		got, err := parseBTAddr("10:20:30:40:50:60")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := [6]byte{0x10, 0x20, 0x30, 0x40, 0x50, 0x60}
		if got != want {
			t.Errorf("parseBTAddr = %v, want %v", got, want)
		}
	})

	for _, tc := range []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"too short", "AA:BB:CC:DD:EE"},
		{"too long", "AA:BB:CC:DD:EE:FF:00"},
		{"no separators", "AABBCCDDEEFF"},
		{"wrong separator", "AA-BB-CC-DD-EE-FF"},
		{"non-hex bytes", "ZZ:BB:CC:DD:EE:FF"},
		{"a CoreBluetooth UUID, which is what macOS would pass", "7565E9EB-4C20-4B67-9272-D708B397B631"},
	} {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			if _, err := parseBTAddr(tc.in); err == nil {
				t.Errorf("parseBTAddr(%q) succeeded, want an error", tc.in)
			}
		})
	}
}

func TestAddressTypeFromProps(t *testing.T) {
	tests := []struct {
		name     string
		props    map[string]dbus.Variant
		wantType uint8
		wantOK   bool
	}{
		{
			name:     "public",
			props:    map[string]dbus.Variant{"AddressType": dbus.MakeVariant("public")},
			wantType: unix.BDADDR_LE_PUBLIC,
			wantOK:   true,
		},
		{
			name:     "random",
			props:    map[string]dbus.Variant{"AddressType": dbus.MakeVariant("random")},
			wantType: unix.BDADDR_LE_RANDOM,
			wantOK:   true,
		},
		{
			name:     "case is not significant",
			props:    map[string]dbus.Variant{"AddressType": dbus.MakeVariant("Random")},
			wantType: unix.BDADDR_LE_RANDOM,
			wantOK:   true,
		},
		{
			// A BR/EDR-only device, or a property BlueZ did not report. Not
			// knowing must not read as "public", or OpenL2CAP would commit to a
			// guess instead of trying both.
			name:     "missing property is unknown, not public",
			props:    map[string]dbus.Variant{},
			wantType: unix.BDADDR_LE_PUBLIC,
			wantOK:   false,
		},
		{
			name:     "unrecognized value is unknown",
			props:    map[string]dbus.Variant{"AddressType": dbus.MakeVariant("bredr")},
			wantType: unix.BDADDR_LE_PUBLIC,
			wantOK:   false,
		},
		{
			name:     "wrong type is unknown",
			props:    map[string]dbus.Variant{"AddressType": dbus.MakeVariant(int32(1))},
			wantType: unix.BDADDR_LE_PUBLIC,
			wantOK:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotType, gotOK := addressTypeFromProps(tc.props)
			if gotType != tc.wantType || gotOK != tc.wantOK {
				t.Errorf("addressTypeFromProps = (%d, %v), want (%d, %v)", gotType, gotOK, tc.wantType, tc.wantOK)
			}
		})
	}
}

func TestConnectRejectsBadAddressWithoutLeakingASocket(t *testing.T) {
	// The parse happens before the socket, so a bad address costs no fd.
	if _, err := Connect("not-an-address", 10); err == nil {
		t.Fatal("Connect accepted an address that cannot be parsed")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	// The bug this guards: Close used to leave c.fd set, so a second call would
	// close whatever descriptor had since taken the number.
	conn, err := Connect("AA:BB:CC:DD:EE:FF", 10)
	if err != nil {
		t.Skipf("cannot create an AF_BLUETOOTH socket here: %v", err)
	}
	conn.Close()
	if conn.fd != -1 {
		t.Fatalf("fd = %d after Close, want -1", conn.fd)
	}
	conn.Close()
	conn.Close()

	// Operations on a closed connection report it rather than acting on a
	// recycled descriptor.
	if err := conn.L2CAPSend([]byte{1}); err == nil {
		t.Error("L2CAPSend on a closed connection succeeded")
	}
	if _, err := conn.L2CAPRecv(1); err == nil {
		t.Error("L2CAPRecv on a closed connection succeeded")
	}
	if err := conn.OpenL2CAP(128, 1); err == nil {
		t.Error("OpenL2CAP on a closed connection succeeded")
	}
}

func TestConnectDefaultsToPublicButUnknown(t *testing.T) {
	conn, err := Connect("AA:BB:CC:DD:EE:FF", 10)
	if err != nil {
		t.Skipf("cannot create an AF_BLUETOOTH socket here: %v", err)
	}
	defer conn.Close()

	if conn.addrKnown {
		// Connect touches neither the radio nor D-Bus, so it cannot know.
		t.Error("Connect claimed to know the address type")
	}
	if conn.address != "AA:BB:CC:DD:EE:FF" {
		t.Errorf("address = %q, want the uppercased form", conn.address)
	}
	if conn.g != nil {
		t.Error("Connect allocated a GATT session; it must stay nil until a GATT call")
	}
}

func TestL2CAPSendChunksToNegotiatedMTU(t *testing.T) {
	// A socketpair gives the same SOCK_SEQPACKET semantics the L2CAP socket has:
	// one write is one datagram, and an oversized write fails rather than being
	// split. That is what makes chunking necessary in the first place.
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Skipf("socketpair unavailable: %v", err)
	}
	defer unix.Close(fds[1]) //nolint:errcheck

	const mtu = 16
	conn := &Connection{fd: fds[0], sndMTU: mtu}
	defer conn.Close()

	payload := make([]byte, 40)
	for i := range payload {
		payload[i] = byte(i)
	}
	if err := conn.L2CAPSend(payload); err != nil {
		t.Fatalf("L2CAPSend: %v", err)
	}

	// Expect 16 + 16 + 8, in order, reassembling to the original bytes.
	var got []byte
	for _, want := range []int{mtu, mtu, 8} {
		buf := make([]byte, 64)
		n, err := unix.Read(fds[1], buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if n != want {
			t.Errorf("SDU length %d, want %d", n, want)
		}
		got = append(got, buf[:n]...)
	}
	if !reflect.DeepEqual(got, payload) {
		t.Error("reassembled payload does not match what was sent")
	}
}

func TestL2CAPSendWithoutNegotiatedMTUFallsBackToMinimum(t *testing.T) {
	// sndMTU is 0 before a channel is open. Falling back to 0 would busy-loop
	// on a zero-length chunk, so it has to become the LE CoC minimum.
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Skipf("socketpair unavailable: %v", err)
	}
	defer unix.Close(fds[1]) //nolint:errcheck

	conn := &Connection{fd: fds[0], sndMTU: 0}
	defer conn.Close()

	done := make(chan error, 1)
	go func() { done <- conn.L2CAPSend(make([]byte, l2capMinMTU+5)) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("L2CAPSend: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("L2CAPSend hung, likely looping on a zero-length chunk")
	}

	buf := make([]byte, 64)
	n, err := unix.Read(fds[1], buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != l2capMinMTU {
		t.Errorf("first SDU is %d bytes, want the %d-byte LE CoC minimum", n, l2capMinMTU)
	}
}

func TestConnectBindsSocketAsLE(t *testing.T) {
	// The bind is what puts the channel into LE credit-based flow control.
	// Without it the kernel stays in basic L2CAP mode and omits the 2-byte
	// SDU-length header from every K-frame, so the peer reads the first two
	// bytes of payload as the SDU length and drops the channel. Nothing about
	// the local API changes, which is exactly why this needs a test.
	conn, err := Connect("AA:BB:CC:DD:EE:FF", 10)
	if err != nil {
		t.Skipf("cannot create an AF_BLUETOOTH socket here: %v", err)
	}
	defer conn.Close()

	sa, err := unix.Getsockname(conn.fd)
	if err != nil {
		t.Fatalf("Getsockname: %v", err)
	}
	l2, ok := sa.(*unix.SockaddrL2)
	if !ok {
		t.Fatalf("socket is not bound to an L2CAP address: %T", sa)
	}
	if l2.AddrType != unix.BDADDR_LE_PUBLIC {
		t.Errorf("bound source address type is %d, want BDADDR_LE_PUBLIC (%d) — "+
			"a non-LE source type leaves the channel in basic mode",
			l2.AddrType, unix.BDADDR_LE_PUBLIC)
	}
}
