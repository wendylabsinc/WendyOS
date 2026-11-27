package spatialsim

import (
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
)

func BenchmarkLidarMinute(b *testing.B) {
	s := officeSite(b)
	for i := 0; i < b.N; i++ {
		n := 0
		for range s.Stream(s.At(0), s.At(time.Minute), Filter{Devices: []spatial.DeviceID{Rover1}, Kinds: []spatial.SensorKind{spatial.KindLidar2D}}) {
			n++
		}
		if n != 601 { // one manifest and 600 sweeps
			b.Fatal(n)
		}
	}
}

func BenchmarkEverythingTenSeconds(b *testing.B) {
	s := officeSite(b)
	for i := 0; i < b.N; i++ {
		for range s.Stream(s.At(0), s.At(10*time.Second), Filter{}) {
		}
	}
}

func BenchmarkCamerasTenSeconds(b *testing.B) {
	s := officeSite(b)
	for i := 0; i < b.N; i++ {
		for range s.Stream(s.At(0), s.At(10*time.Second), Filter{Kinds: []spatial.SensorKind{spatial.KindCamera}}) {
		}
	}
}
