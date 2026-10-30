package t234

import (
	"bytes"
	"fmt"
	"slices"
	"testing"
)

const mib = 1 << 20

func TestStaleExtents(t *testing.T) {
	cases := []struct {
		name              string
		partitions, wrote []Extent
		want              []Extent
	}{
		{
			name:       "an empty partition loses its head and tail, not its middle",
			partitions: []Extent{{100 * mib, 200 * mib}},
			want:       []Extent{{100 * mib, 108 * mib}, {198 * mib, 200 * mib}},
		},
		{
			name:       "a partition smaller than both windows is cleared whole",
			partitions: []Extent{{1 * mib, 6 * mib}},
			want:       []Extent{{1 * mib, 6 * mib}},
		},
		{
			name:       "written bytes are never cleared",
			partitions: []Extent{{0, 64 * mib}},
			wrote:      []Extent{{0, 3 * mib}, {5 * mib, 6 * mib}, {63 * mib, 64 * mib}},
			want:       []Extent{{3 * mib, 5 * mib}, {6 * mib, 8 * mib}, {62 * mib, 63 * mib}},
		},
		{
			name:       "a fully written partition needs nothing",
			partitions: []Extent{{10 * mib, 20 * mib}},
			wrote:      []Extent{{10 * mib, 20 * mib}},
		},
		{
			name:       "unsorted partitions and writes, with neighbours merged",
			partitions: []Extent{{20 * mib, 21 * mib}, {0, 10 * mib}, {10 * mib, 20 * mib}},
			wrote:      []Extent{{20 * mib, 21 * mib}, {2 * mib, 4 * mib}},
			want:       []Extent{{0, 2 * mib}, {4 * mib, 20 * mib}},
		},
		{
			name:       "empty partitions are skipped",
			partitions: []Extent{{5 * mib, 5 * mib}, {9 * mib, 4 * mib}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StaleExtents(tc.partitions, tc.wrote); !slices.Equal(got, tc.want) {
				t.Fatalf("StaleExtents = %s, want %s", mibs(got), mibs(tc.want))
			}
		})
	}
}

func mibs(es []Extent) string {
	var b bytes.Buffer
	for _, e := range es {
		fmt.Fprintf(&b, "[%g, %g) ", float64(e.Start)/mib, float64(e.End)/mib)
	}
	return b.String()
}

// recordingDevice is an in-memory disk that refuses what a raw device refuses:
// a write that is not whole, aligned sectors.
type recordingDevice struct {
	buf    []byte
	writes int
}

func (d *recordingDevice) WriteAt(p []byte, off int64) (int, error) {
	if off%sectorSize != 0 || len(p)%sectorSize != 0 {
		return 0, fmt.Errorf("unaligned write of %d bytes at %d", len(p), off)
	}
	d.writes++
	return copy(d.buf[off:], p), nil
}

// TestZeroExtentsWholeSectors pins that ZeroExtents writes only whole aligned
// sectors, in bounded chunks, and never rounds an extent out over a partial
// sector it was not asked to clear.
func TestZeroExtentsWholeSectors(t *testing.T) {
	dev := &recordingDevice{buf: bytes.Repeat([]byte{0xA5}, 4*mib)}
	extents := []Extent{
		{sectorSize, 3 * sectorSize},             // whole sectors
		{5*sectorSize + 100, 8*sectorSize + 100}, // ragged ends: sectors 6-7 only
		{mib, 3*mib + sectorSize},                // more than one chunk
	}
	if err := ZeroExtents(dev, extents, sectorSize); err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte{0xA5}, 4*mib)
	clear(want[sectorSize : 3*sectorSize])
	clear(want[6*sectorSize : 8*sectorSize])
	clear(want[mib : 3*mib+sectorSize])
	if !bytes.Equal(dev.buf, want) {
		t.Fatal("ZeroExtents cleared other bytes than the whole sectors of its extents")
	}
	if dev.writes != 5 {
		t.Errorf("ZeroExtents made %d writes, want 5 (one per short extent, then 1 MiB + 1 MiB + a sector)", dev.writes)
	}
}
