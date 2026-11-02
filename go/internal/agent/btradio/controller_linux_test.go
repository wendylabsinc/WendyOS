//go:build linux

package btradio

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
)

func complete(op uint16, data ...byte) []byte {
	return append([]byte{4, 14, byte(4 + len(data)), 1, byte(op), byte(op >> 8), 0}, data...)
}
func managementReply(op uint16, data []byte) []byte {
	p := make([]byte, 9+len(data))
	binary.LittleEndian.PutUint16(p, 1)
	binary.LittleEndian.PutUint16(p[4:], uint16(3+len(data)))
	binary.LittleEndian.PutUint16(p[6:], op)
	copy(p[9:], data)
	return p
}
func fakeController(t *testing.T, send func([]byte) [][]byte) {
	t.Helper()
	old := openController
	openController = func(index uint16, management bool) (*controllerIO, error) {
		if index != 0 {
			t.Fatalf("wrong adapter %d", index)
		}
		var queue [][]byte
		return &controllerIO{close: func() {}, send: func(p []byte) error { queue = append(queue, send(p)...); return nil }, read: func(ctx context.Context) ([]byte, error) {
			if len(queue) == 0 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			p := queue[0]
			queue = queue[1:]
			return p, nil
		}}, nil
	}
	t.Cleanup(func() { openController = old })
}
func TestMeshPHYSelectionPreservesOtherPHYsAndChecksReadback(t *testing.T) {
	selected := uint32(0x1fff)
	writes := 0
	fakeController(t, func(p []byte) [][]byte {
		op := binary.LittleEndian.Uint16(p)
		if op == 0x45 {
			writes++
			if len(p) != 10 {
				t.Fatal(p)
			}
			selected = binary.LittleEndian.Uint32(p[6:])
			return [][]byte{managementReply(op, nil)}
		}
		if op != 0x44 {
			t.Fatal(op)
		}
		data := make([]byte, 12)
		binary.LittleEndian.PutUint32(data, 0x7fff)
		binary.LittleEndian.PutUint32(data[4:], 0x79fe)
		binary.LittleEndian.PutUint32(data[8:], selected)
		// An unrelated short broadcast must not abort a command response.
		return [][]byte{{4, 0, 0, 0, 0, 0}, managementReply(op, data)}
	})
	ctx := context.Background()
	if err := CodedPHYAvailable(ctx, "/org/bluez/hci0"); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMeshPHY(ctx, "/org/bluez/hci0", "coded-s2"); err != nil {
		t.Fatal(err)
	}
	if selected != 0x7fff || writes != 1 {
		t.Fatal(selected, writes)
	}
	if err := ApplyMeshPHY(ctx, "/org/bluez/hci0", "coded-s8"); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatal("unnecessary controller write")
	}
	if err := ApplyMeshPHY(ctx, "/org/bluez/hci0", "normal"); err != nil {
		t.Fatal(err)
	}
	if selected != 0x1fff {
		t.Fatal("unrelated/default PHYs changed", selected)
	}
}
func TestMeshLinkCodingPreferenceRequiresObservedPHY(t *testing.T) {
	for _, tc := range []struct {
		mode   string
		option byte
		want   byte
	}{{"normal", 0, 1}, {"coded-s2", 1, 3}, {"coded-s8", 2, 3}} {
		t.Run(tc.mode, func(t *testing.T) {
			resetPolicy(t)
			meshMode = tc.mode
			loaded = true
			fakeController(t, func(p []byte) [][]byte {
				op := binary.LittleEndian.Uint16(p[1:3])
				switch op {
				case 0x2032:
					if len(p) != 11 || p[4] != 7 || p[5] != 0 || p[9] != tc.option {
						t.Fatal(p)
					}
					return [][]byte{{4, 15, 4, 0, 1, 0x32, 0x20}}
				case 0x2030:
					return [][]byte{complete(op, 7, 0, tc.want, tc.want)}
				}
				t.Fatal(op)
				return nil
			})
			if err := TuneMeshLink(context.Background(), "/org/bluez/hci0", 7); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestLinkPowerReadbackRetainsUnknownAndHandleOwnership(t *testing.T) {
	meshLinks = map[string]map[uint16]int{}
	first := TrackMeshLink("/org/bluez/hci0", 7)
	second := TrackMeshLink("/org/bluez/hci0", 7)
	first()
	fakeController(t, func(p []byte) [][]byte {
		op := binary.LittleEndian.Uint16(p[1:3])
		if op == 0x2030 {
			return [][]byte{complete(op, 7, 0, 3, 3)}
		}
		if op == 0x2076 {
			return [][]byte{{4, 14, 4, 1, 0x76, 0x20, 1}}
		}
		if op == 0x0c2d {
			power := byte(127)
			if p[6] == 1 {
				power = 8
			}
			return [][]byte{complete(op, 7, 0, power)}
		}
		t.Fatal(op)
		return nil
	})
	links := LinkStatus(context.Background(), "/org/bluez/hci0")
	if len(links) != 1 || links[0].TxPhy != 3 || links[0].CurrentDbm != nil || links[0].GetMaximumDbm() != 8 {
		t.Fatal(links)
	}
	second()
	links = LinkStatus(context.Background(), "/org/bluez/hci0")
	if len(links) != 0 {
		t.Fatal(links)
	}
}
func TestHCIRejectionIsNotReportedAsPHYSuccess(t *testing.T) {
	resetPolicy(t)
	meshMode = "coded-s8"
	loaded = true
	fakeController(t, func(p []byte) [][]byte { return [][]byte{{4, 15, 4, 0x11, 1, 0x32, 0x20}} })
	if err := TuneMeshLink(context.Background(), "/org/bluez/hci0", 7); err == nil {
		t.Fatal("controller rejection ignored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ApplyMeshPHY(ctx, "/org/bluez/hci0", "normal"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestNormalModeOnKernelWithoutPHYControl(t *testing.T) {
	fakeController(t, func(p []byte) [][]byte {
		response := managementReply(0x44, nil)
		response[8] = 0x01 // Unknown Command
		return [][]byte{response}
	})
	if err := ApplyMeshPHY(context.Background(), "/org/bluez/hci0", "normal"); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMeshPHY(context.Background(), "/org/bluez/hci0", "coded-s2"); err == nil {
		t.Fatal("unsupported Coded request accepted")
	}
	if err := CodedPHYAvailable(context.Background(), "/org/bluez/hci0"); err == nil {
		t.Fatal("unsupported capability reported")
	}
}

func TestReservedPowerReadbacksAreUnknown(t *testing.T) {
	for _, raw := range []byte{120, 127, 128} {
		if knownPower(raw) != nil {
			t.Fatal(raw)
		}
	}
	for _, value := range []int8{-127, -40, 0, 6, 20} {
		if got := knownPower(byte(value)); got == nil || *got != int32(value) {
			t.Fatal(value, got)
		}
	}
}
func TestEnhancedPowerReadbackUsesCodedPreference(t *testing.T) {
	resetPolicy(t)
	loaded = true
	meshMode = "coded-s2"
	meshLinks = map[string]map[uint16]int{}
	defer TrackMeshLink("/org/bluez/hci0", 7)()
	fakeController(t, func(p []byte) [][]byte {
		op := binary.LittleEndian.Uint16(p[1:3])
		if op == 0x2030 {
			return [][]byte{complete(op, 7, 0, 3, 3)}
		}
		if op == 0x2076 {
			if p[6] != 4 {
				t.Fatal("S2 readback must use PHY 4", p)
			}
			return [][]byte{complete(op, 7, 0, 4, 6, 6)}
		}
		t.Fatal(op)
		return nil
	})
	links := LinkStatus(context.Background(), "/org/bluez/hci0")
	if len(links) != 1 || links[0].GetCurrentDbm() != 6 || links[0].GetMaximumDbm() != 6 || links[0].Error != "" {
		t.Fatal(links)
	}
}
