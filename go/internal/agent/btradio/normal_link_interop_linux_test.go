//go:build linux

package btradio

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

func configuredLinkFixture(t *testing.T, mode string) {
	t.Helper()
	resetPolicy(t)
	if err := SetRadio(context.Background(), nil, mode); err != nil {
		t.Fatal(err)
	}
	oldLinks := meshLinks
	meshLinks = map[string]map[uint16]int{}
	t.Cleanup(func() { meshLinks = oldLinks })
}

func linkPHYCapabilities() []byte {
	data := make([]byte, 12)
	binary.LittleEndian.PutUint32(data, 0x7fff)
	binary.LittleEndian.PutUint32(data[4:], 0x79fe)
	return managementReply(0x44, data)
}

func phyCommandStatus(status byte) []byte {
	return []byte{4, 15, 4, status, 1, 0x32, 0x20}
}

// This is the actual admission/controller seam: the local controller supports
// Coded, but its normal-only remote would reject a redundant Set PHY procedure.
func TestNormalOnlyRemoteAlready1MDoesNotRequireSetPHY(t *testing.T) {
	configuredLinkFixture(t, "normal")
	const handle uint16 = 0x123
	const other uint16 = 0x124
	adapter := "/org/bluez/hci0"
	existing := TrackMeshLink(adapter, handle)
	defer existing()
	otherOwner := TrackMeshLink(adapter, other)
	defer otherOwner()
	reads, sets := 0, 0
	fakeController(t, func(p []byte) [][]byte {
		if binary.LittleEndian.Uint16(p) == 0x44 {
			return [][]byte{linkPHYCapabilities()}
		}
		op := binary.LittleEndian.Uint16(p[1:3])
		if binary.LittleEndian.Uint16(p[4:6]) != handle {
			t.Fatalf("touched another connection handle: %x", p)
		}
		switch op {
		case 0x2030:
			reads++
			return [][]byte{complete(op, byte(handle&255), byte(handle>>8), 1, 1)}
		case 0x2032:
			sets++
			return [][]byte{phyCommandStatus(0x1a)}
		}
		t.Fatalf("unexpected command: %x", p)
		return nil
	})
	untrack, err := TrackAndTuneMeshLink(context.Background(), adapter, handle)
	if err != nil || untrack == nil {
		t.Fatalf("already-normal remote failed admission: %v", err)
	}
	if reads != 1 || sets != 0 || meshLinks[adapter][handle] != 2 || meshLinks[adapter][other] != 1 {
		t.Fatalf("wrong commands or ownership: reads=%d sets=%d links=%v", reads, sets, meshLinks)
	}
	untrack()
	untrack()
	if meshLinks[adapter][handle] != 1 || meshLinks[adapter][other] != 1 {
		t.Fatalf("cleanup removed another owner: %v", meshLinks)
	}
}

func TestNormalModeStillAppliesAndObservesChangedPHY(t *testing.T) {
	for _, before := range [][2]byte{{3, 3}, {1, 3}, {3, 1}, {2, 2}} {
		t.Run(string([]byte{'0' + before[0], '0' + before[1]}), func(t *testing.T) {
			configuredLinkFixture(t, "normal")
			reads, sets := 0, 0
			fakeController(t, func(p []byte) [][]byte {
				op := binary.LittleEndian.Uint16(p[1:3])
				if binary.LittleEndian.Uint16(p[4:6]) != 7 {
					t.Fatalf("wrong handle: %x", p)
				}
				switch op {
				case 0x2030:
					reads++
					if reads <= 2 {
						return [][]byte{complete(op, 7, 0, before[0], before[1])}
					}
					return [][]byte{complete(op, 7, 0, 1, 1)}
				case 0x2032:
					sets++
					if reads != 1 || len(p) != 11 || p[6] != 0 || p[7] != 1 || p[8] != 1 || p[9] != 0 || p[10] != 0 {
						t.Fatalf("wrong normal procedure: reads=%d packet=%x", reads, p)
					}
					return [][]byte{phyCommandStatus(0)}
				}
				t.Fatalf("unexpected command: %x", p)
				return nil
			})
			if err := TuneMeshLink(context.Background(), "/org/bluez/hci0", 7); err != nil {
				t.Fatal(err)
			}
			if reads != 3 || sets != 1 {
				t.Fatalf("normal transition not observed: reads=%d sets=%d", reads, sets)
			}
		})
	}
}

func TestNormalPHYReadbackAndProcedureFailuresStayFatal(t *testing.T) {
	for _, tc := range []struct {
		name      string
		initial   []byte
		setStatus byte
		post      []byte
		wantSets  int
	}{
		{name: "read rejected", initial: []byte{4, 14, 4, 1, 0x30, 0x20, 0x02}},
		{name: "short read", initial: complete(0x2030, 7, 0, 1)},
		{name: "wrong initial handle", initial: complete(0x2030, 8, 0, 1, 1)},
		{name: "remote rejection is fatal when still coded", initial: complete(0x2030, 7, 0, 3, 3), setStatus: 0x1a, wantSets: 1},
		{name: "other rejection remains fatal", initial: complete(0x2030, 7, 0, 3, 3), setStatus: 0x0c, wantSets: 1},
		{name: "wrong post procedure handle", initial: complete(0x2030, 7, 0, 3, 3), post: complete(0x2030, 8, 0, 1, 1), wantSets: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configuredLinkFixture(t, "normal")
			sets := 0
			fakeController(t, func(p []byte) [][]byte {
				if binary.LittleEndian.Uint16(p) == 0x44 {
					return [][]byte{linkPHYCapabilities()}
				}
				op := binary.LittleEndian.Uint16(p[1:3])
				switch op {
				case 0x2030:
					if sets != 0 {
						return [][]byte{tc.post}
					}
					return [][]byte{tc.initial}
				case 0x2032:
					sets++
					return [][]byte{phyCommandStatus(tc.setStatus)}
				}
				t.Fatalf("unexpected command: %x", p)
				return nil
			})
			untrack, err := TrackAndTuneMeshLink(context.Background(), "/org/bluez/hci0", 7)
			if err == nil || untrack != nil || sets != tc.wantSets || len(meshLinks["/org/bluez/hci0"]) != 0 {
				t.Fatalf("failure accepted or ownership leaked: sets=%d err=%v links=%v", sets, err, meshLinks)
			}
		})
	}
}

func TestCodedRemoteRejectionDoesNotUseNormalShortcut(t *testing.T) {
	for _, mode := range []string{"coded-s2", "coded-s8"} {
		t.Run(mode, func(t *testing.T) {
			configuredLinkFixture(t, mode)
			sets := 0
			fakeController(t, func(p []byte) [][]byte {
				if binary.LittleEndian.Uint16(p[1:3]) != 0x2032 || binary.LittleEndian.Uint16(p[4:6]) != 7 || p[7] != 4 || p[8] != 4 {
					t.Fatalf("Coded request used normal shortcut: %x", p)
				}
				sets++
				return [][]byte{phyCommandStatus(0x1a)}
			})
			untrack, err := TrackAndTuneMeshLink(context.Background(), "/org/bluez/hci0", 7)
			if err == nil || untrack != nil || sets != 1 || len(meshLinks["/org/bluez/hci0"]) != 0 {
				t.Fatalf("failed Coded procedure accepted: sets=%d err=%v links=%v", sets, err, meshLinks)
			}
		})
	}
}

func TestNormalModeStillFailsWhenPHYNeverChanges(t *testing.T) {
	configuredLinkFixture(t, "normal")
	fakeController(t, func(p []byte) [][]byte {
		switch binary.LittleEndian.Uint16(p[1:3]) {
		case 0x2030:
			return [][]byte{complete(0x2030, 7, 0, 3, 3)}
		case 0x2032:
			return [][]byte{phyCommandStatus(0)}
		}
		t.Fatalf("unexpected command: %x", p)
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if err := TuneMeshLink(ctx, "/org/bluez/hci0", 7); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unobserved normal PHY reported success: %v", err)
	}
}
