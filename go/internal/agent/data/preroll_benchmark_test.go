//go:build linux || darwin

package data

import (
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func preRollBenchmarkCPU(b testing.TB) int64 {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		b.Fatal(err)
	}
	return usage.Utime.Nano() + usage.Stime.Nano()
}

// A mature five-minute loss history: each iteration ingests another loss and
// expires its oldest predecessor. The application-byte budget is driven through
// Manager's real eviction function, with synthetic clock advancement only.
func BenchmarkPreRollEviction(b *testing.B) {
	for _, history := range []int{0, 10000, 100000} {
		b.Run(fmt.Sprint(history), func(b *testing.B) {
			m := &Manager{}
			now := (10 * time.Minute).Nanoseconds()
			step := preRollWindow.Nanoseconds() / int64(max(history, 1))
			for i := 0; i < history; i++ {
				m.preRollEvicted = append(m.preRollEvicted, now-preRollWindow.Nanoseconds()+int64(i)*step)
			}
			one := make([]bufferedRecord, 1)
			payload := []byte("sample")
			b.ReportAllocs()
			b.ResetTimer()
			cpu := preRollBenchmarkCPU(b)
			for i := 0; i < b.N; i++ {
				now += step
				if history != 0 {
					one[0] = bufferedRecord{receiptNanos: now, encoded: payload}
					m.preRoll, m.preRollBytes = one, preRollLimit+1
				}
				m.evictPreRoll(now)
			}
			cpu = preRollBenchmarkCPU(b) - cpu
			b.StopTimer()
			b.ReportMetric(float64(cpu)/float64(b.N), "cpu-ns/op")
			b.ReportMetric(float64(cap(m.preRollEvicted)*8), "history-cap-B")
			if len(m.preRollEvicted) > history+1 {
				b.Fatal("history accumulated outside its window")
			}
		})
	}
}

// After a gap in ingestion, many losses can expire together. Include the same
// receipt-slice reset in both revisions and retain its capacity between runs.
func BenchmarkPreRollExpiryBurst(b *testing.B) {
	for _, history := range []int{10000, 100000} {
		for _, expiredPercent := range []int{25, 100} {
			b.Run(fmt.Sprintf("history=%d/expired=%d", history, expiredPercent), func(b *testing.B) {
				m := &Manager{}
				seed := make([]int64, history)
				for i := range seed {
					seed[i] = int64(i + 1)
				}
				m.preRollEvicted = append(m.preRollEvicted, seed...)
				expired := history * expiredPercent / 100
				now := preRollWindow.Nanoseconds() + int64(expired) + 1
				b.ReportAllocs()
				b.ResetTimer()
				cpu := preRollBenchmarkCPU(b)
				for i := 0; i < b.N; i++ {
					m.preRollEvicted = append(m.preRollEvicted[:0], seed...)
					m.evictPreRoll(now)
				}
				cpu = preRollBenchmarkCPU(b) - cpu
				b.StopTimer()
				b.ReportMetric(float64(cpu)/float64(b.N), "cpu-ns/op")
				if len(m.preRollEvicted) != history-expired {
					b.Fatal("wrong loss expiry count")
				}
			})
		}
	}
}

// Include timestamping, boot identity lookup, JSON encoding, pre-roll insertion
// and the shared manager lock. Fixed iteration counts make pressure runs compare
// the same number of ingested records across revisions.
func BenchmarkPreRollApplicationIngest(b *testing.B) {
	for _, history := range []int{0, 10000, 100000} {
		b.Run(fmt.Sprint(history), func(b *testing.B) {
			m, err := NewManager(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			now, err := readBootTime()
			if err != nil {
				b.Fatal(err)
			}
			for i := 0; i < history; i++ {
				m.preRollEvicted = append(m.preRollEvicted, now-time.Minute.Nanoseconds()+int64(i))
			}
			payload := strings.Repeat("x", 1024)
			if history != 0 {
				// Seed actual logical bytes at the 50 MiB budget. Seed payloads can
				// share immutable bytes; timed records own their JSON encoding.
				encoded := []byte(payload)
				for i := 0; i < preRollLimit/len(encoded); i++ {
					m.preRoll = append(m.preRoll, bufferedRecord{receiptNanos: now, encoded: encoded})
					m.preRollBytes += len(encoded)
				}
			} else {
				payload = "idle-control"
			}
			record := ApplicationRecord{Version: 1, Type: "prediction", Model: "detector", Attributes: map[string]any{"sample": payload}}
			b.ReportAllocs()
			b.ResetTimer()
			cpu := preRollBenchmarkCPU(b)
			for i := 0; i < b.N; i++ {
				if state, err := m.RecordApplication("test.app", record); err != nil || state != "buffered" {
					b.Fatalf("ingest: %s %v", state, err)
				}
			}
			cpu = preRollBenchmarkCPU(b) - cpu
			b.StopTimer()
			b.ReportMetric(float64(cpu)/float64(b.N), "cpu-ns/op")
			b.ReportMetric(float64(len(m.preRollEvicted)), "losses")
			b.ReportMetric(float64(cap(m.preRollEvicted)*8), "history-cap-B")
		})
	}
}

// Report live Go heap after aging half a 50 MiB pre-roll input. This measures
// retained payloads, not allocations per operation or process resident memory.
// Keep the manager alive across GC so its remaining slice owns the backing
// array while references to expired records are inspected by the collector.
func TestPreRollRetainedHeapMeasurement(t *testing.T) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	m := &Manager{preRoll: make([]bufferedRecord, 800)}
	now := (10 * time.Minute).Nanoseconds()
	for i := range m.preRoll {
		receipt := now
		if i < len(m.preRoll)/2 {
			receipt -= preRollWindow.Nanoseconds() + 1
		}
		payload := make([]byte, 64<<10)
		payload[0] = byte(i)
		m.preRoll[i] = bufferedRecord{receiptNanos: receipt, encoded: payload}
		m.preRollBytes += len(payload)
	}
	m.evictPreRoll(now)
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(m)
	if len(m.preRoll) != 400 || m.preRollBytes != 25<<20 {
		t.Fatal("unexpected retained pre-roll window")
	}
	t.Logf("PREROLL_HEAP live_heap_delta_bytes=%d active_payload_bytes=%d records=%d", int64(after.HeapAlloc)-int64(before.HeapAlloc), m.preRollBytes, len(m.preRoll))
}
