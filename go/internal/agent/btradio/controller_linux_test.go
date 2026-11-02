//go:build linux

package btradio

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"
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

// Pause controller admission so a newer policy can finish before a queued link
// submits its first HCI command.
type controllerAdmissionContext struct {
	context.Context
	entered chan struct{}
	release chan struct{}
}

func (c controllerAdmissionContext) Deadline() (time.Time, bool) {
	close(c.entered)
	<-c.release
	return c.Context.Deadline()
}

func TestMeshLinkTuneReadsPolicyAfterControllerAdmission(t *testing.T) {
	resetPolicy(t)
	if err := SetRadio(context.Background(), nil, "coded-s2"); err != nil {
		t.Fatal(err)
	}
	var selected byte
	fakeController(t, func(p []byte) [][]byte {
		op := binary.LittleEndian.Uint16(p[1:3])
		if op == 0x2032 {
			selected = p[7]
			return [][]byte{{4, 15, 4, 0, 1, 0x32, 0x20}}
		}
		if op == 0x2030 {
			phy := byte(1)
			if selected == 4 {
				phy = 3
			}
			return [][]byte{complete(op, 7, 0, phy, phy)}
		}
		t.Fatal(op)
		return nil
	})
	ctx := controllerAdmissionContext{Context: context.Background(), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- TuneMeshLink(ctx, "/org/bluez/hci0", 7) }()
	<-ctx.entered
	if err := SetRadio(context.Background(), nil, "normal"); err != nil {
		close(ctx.release)
		<-done
		t.Fatal(err)
	}
	close(ctx.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if selected != 1 {
		t.Fatalf("queued link applied superseded policy: TX PHY mask=%d, saved mode=normal", selected)
	}
}

// Model the exact incoming/outgoing admission order where raw CoC establishment
// predates a profile change but TrackMeshLink executes after its tracked pass.
func TestLateCodedLinkAdmissionObservesCompletedNormalProfile(t *testing.T) {
	resetPolicy(t)
	adapter := "/org/bluez/hci0"
	if err := SetRadio(context.Background(), nil, "coded-s2"); err != nil {
		t.Fatal(err)
	}
	selected := uint32(0x7fff)
	observed := byte(3) // established raw CoC was Coded before the normal RPC
	fakeController(t, func(p []byte) [][]byte {
		mgmt := binary.LittleEndian.Uint16(p)
		if mgmt == 0x44 {
			data := make([]byte, 12)
			binary.LittleEndian.PutUint32(data, 0x7fff)
			binary.LittleEndian.PutUint32(data[4:], 0x79fe)
			binary.LittleEndian.PutUint32(data[8:], selected)
			return [][]byte{managementReply(mgmt, data)}
		}
		if mgmt == 0x45 {
			selected = binary.LittleEndian.Uint32(p[6:])
			return [][]byte{managementReply(mgmt, nil)}
		}
		op := binary.LittleEndian.Uint16(p[1:3])
		switch op {
		case 0x2032:
			if p[7] == 1 {
				observed = 1
			} else {
				observed = 3
			}
			return [][]byte{{4, 15, 4, 0, 1, 0x32, 0x20}}
		case 0x2030:
			return [][]byte{complete(op, 7, 0, observed, observed)}
		}
		t.Fatal(op)
		return nil
	})
	// Set normal has finished its existing-link pass; this raw CoC is not tracked yet.
	if err := SetRadioAndApply(context.Background(), nil, "normal", func(ctx context.Context) error {
		if err := ApplyMeshPHY(ctx, adapter, "normal"); err != nil {
			return err
		}
		return ApplyTrackedMeshPHY(ctx, adapter)
	}); err != nil {
		t.Fatal(err)
	}
	if selected&codedPHYMask != 0 {
		t.Fatal("normal controller PHYs were not applied")
	}
	untrack, err := TrackAndTuneMeshLink(context.Background(), adapter, 7)
	if err != nil {
		t.Fatal(err)
	}
	defer untrack()
	if observed != 1 {
		t.Fatalf("late admitted link remains on Coded PHY=%d after completed normal policy", observed)
	}
}

func TestNormalLinkAdmissionPreservesDefaultAndLegacyControllers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured bool
		status     byte
		wantError  bool
	}{
		{name: "fresh default"},
		{name: "legacy unknown PHY command", configured: true, status: 0x01},
		{name: "no configurable Coded PHY", configured: true},
		{name: "capability failure is visible", configured: true, status: 0x0a, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetPolicy(t)
			meshLinks = map[string]map[uint16]int{}
			if tc.configured {
				if err := SetRadio(context.Background(), nil, "normal"); err != nil {
					t.Fatal(err)
				}
			}
			commands := 0
			fakeController(t, func(p []byte) [][]byte {
				commands++
				if !tc.configured || binary.LittleEndian.Uint16(p) != 0x44 {
					t.Fatalf("normal admission submitted unsupported controller command: %x", p)
				}
				data := make([]byte, 12)
				response := managementReply(0x44, data)
				response[8] = tc.status
				return [][]byte{response}
			})
			untrack, err := TrackAndTuneMeshLink(context.Background(), "/org/bluez/hci0", 7)
			if tc.wantError {
				if err == nil || untrack != nil || len(meshLinks["/org/bluez/hci0"]) != 0 {
					t.Fatalf("failed admission leaked ownership: untrack=%t err=%v links=%v", untrack != nil, err, meshLinks)
				}
				return
			}
			if err != nil || untrack == nil {
				t.Fatalf("normal admission failed: %v", err)
			}
			untrack()
			if !tc.configured && commands != 0 {
				t.Fatal("fresh default required PHY I/O")
			}
		})
	}
}

func TestFailedCodedLinkAdmissionReleasesTracking(t *testing.T) {
	resetPolicy(t)
	meshLinks = map[string]map[uint16]int{}
	if err := SetRadio(context.Background(), nil, "coded-s8"); err != nil {
		t.Fatal(err)
	}
	fakeController(t, func(p []byte) [][]byte { return [][]byte{{4, 15, 4, 0x11, 1, 0x32, 0x20}} })
	untrack, err := TrackAndTuneMeshLink(context.Background(), "/org/bluez/hci0", 7)
	if err == nil || untrack != nil || len(meshLinks["/org/bluez/hci0"]) != 0 {
		t.Fatalf("failed coded admission leaked ownership: untrack=%t err=%v links=%v", untrack != nil, err, meshLinks)
	}
}

func TestCanceledDefaultLinkAdmissionDoesNotAcquireTracking(t *testing.T) {
	resetPolicy(t)
	meshLinks = map[string]map[uint16]int{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	untrack, err := TrackAndTuneMeshLink(ctx, "/org/bluez/hci0", 7)
	if !errors.Is(err, context.Canceled) || untrack != nil || len(meshLinks["/org/bluez/hci0"]) != 0 {
		t.Fatalf("canceled default admission acquired ownership: untrack=%t err=%v links=%v", untrack != nil, err, meshLinks)
	}
}
