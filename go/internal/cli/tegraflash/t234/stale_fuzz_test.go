package t234

import "testing"

// Model a disk as 64 one-MiB cells. The oracle marks each cell independently,
// without reproducing the interval sorting/subtraction used by StaleExtents.
func FuzzStaleExtentsPreserveWrittenBytes(f *testing.F) {
	f.Add([]byte{0, 64}, []byte{0, 3, 5, 6, 63, 64})
	f.Add([]byte{20, 21, 0, 10, 10, 20}, []byte{20, 21, 2, 4})
	f.Add([]byte{8, 40}, []byte{})
	f.Fuzz(func(t *testing.T, partitions, writes []byte) {
		decode := func(raw []byte) []Extent {
			var out []Extent
			for i := 0; i+1 < min(len(raw), 128); i += 2 {
				start, end := int64(raw[i]%65), int64(raw[i+1]%65)
				if start < end {
					out = append(out, Extent{start * mib, end * mib})
				}
			}
			return out
		}
		parts, wrote := decode(partitions), decode(writes)
		var want, got [64]bool
		for i := range want {
			pos := int64(i) * mib
			for _, p := range parts {
				if pos >= p.Start && pos < p.End && (pos < p.Start+8*mib || pos >= p.End-2*mib) {
					want[i] = true
				}
			}
			for _, w := range wrote {
				if pos >= w.Start && pos < w.End {
					want[i] = false
				}
			}
		}
		for _, extent := range StaleExtents(parts, wrote) {
			if extent.Start < 0 || extent.End > 64*mib || extent.Start >= extent.End || extent.Start%mib != 0 || extent.End%mib != 0 {
				t.Fatalf("invalid clear range: %+v", extent)
			}
			for i := extent.Start / mib; i < extent.End/mib; i++ {
				got[i] = true
			}
		}
		if got != want {
			t.Fatalf("clear cells=%v, want=%v; partitions=%v writes=%v", got, want, parts, wrote)
		}
	})
}
