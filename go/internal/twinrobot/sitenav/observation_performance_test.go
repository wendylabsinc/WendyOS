package sitenav

import (
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"

	"github.com/pierrec/lz4/v4"
)

func performanceCloudFrame(t testing.TB, raw []byte, now time.Time, origin []float64) []byte {
	t.Helper()
	compressed := make([]byte, lz4.CompressBlockBound(len(raw)))
	n, err := lz4.CompressBlock(raw, compressed, nil)
	if err != nil || n == 0 {
		t.Fatalf("compress synthetic cloud: size=%d err=%v", n, err)
	}
	header, err := json.Marshal(map[string]any{"topic": CloudTopic, "data": map[string]any{
		"src_size": len(raw), "origin": origin, "resolution": .05, "frame_id": "odom", "stamp": float64(now.UnixNano()) / 1e9,
	}})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, 4)
	binary.LittleEndian.PutUint16(out, uint16(len(header)))
	return append(append(out, header...), compressed[:n]...)
}

func TestCloudPointLimitCountsOnlyRetainedVoxels(t *testing.T) {
	now := time.Unix(1700000000, 125000000)
	// More than 500,000 occupied voxels, all within the decoder's byte limit.
	raw := make([]byte, 32*2048)
	for i := range raw {
		raw[i] = 255
	}
	for _, outside := range []bool{false, true} {
		origin := []float64{-3, -3, 0}
		if outside {
			origin = []float64{1000, 1000, 0}
		}
		o := Observations{Pose: Pose{Height: .3, Received: now, Captured: now}}
		err := o.Receive(CloudTopic, performanceCloudFrame(t, raw, now, origin), true, now, now)
		if outside {
			if err != nil || o.FloorFault == "" || len(o.Landmarks) != 0 || len(o.Grid.Floor) != 0 {
				t.Fatalf("out-of-range voxels counted toward limit or invented geometry: err=%v floorFault=%q", err, o.FloorFault)
			}
		} else if err == nil || !o.CloudAt.IsZero() {
			t.Fatalf("oversized retained cloud did not fail closed: %v", err)
		}
	}
}

func BenchmarkDenseObservationCloud(b *testing.B) {
	now := time.Unix(1700000000, 125000000)
	raw := make([]byte, 32*2048)
	// Synthetic floor, perimeter walls and occupied clutter. No device payload
	// or room geometry is needed to reproduce allocation regressions.
	for z := 0; z < 32; z++ {
		for y := 0; y < 128; y++ {
			for x := 0; x < 128; x++ {
				if z == 0 || x == 0 || x == 127 || y == 0 || y == 127 || z < 12 && x < 32 && y < 32 {
					raw[z*2048+y*16+x/8] |= 1 << uint(7-x%8)
				}
			}
		}
	}
	packet := performanceCloudFrame(b, raw, now, []float64{-3, -3, 0})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		o := Observations{Pose: Pose{Height: .3, Received: now, Captured: now}}
		if err := o.Receive(CloudTopic, packet, true, now, now); err != nil || o.FloorFault != "" {
			b.Fatalf("synthetic cloud invalid: %v %s", err, o.FloorFault)
		}
	}
}
