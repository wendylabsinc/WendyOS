package services

import (
	"bytes"
	"fmt"
	"runtime"
	"testing"
	"time"
)

func TestPreRollRestartReleasesFramesWithoutChangingSharedPayload(t *testing.T) {
	for _, reason := range []string{"bytes", "age"} {
		t.Run(reason, func(t *testing.T) {
			r := &cameraPreRollRing{buffer: time.Second, limitBytes: preRollCameraLimitBytes}
			first := preRollFrame(0, 1, true)
			first.data = bytes.Clone(first.data)
			sharedPayload := bytes.Clone(first.data)
			r.add(first)
			last := preRollFrame(int64(9*time.Second), 2, false)
			if reason == "bytes" {
				r.limitBytes = len(first.data)
				last.receiptBootNanos = int64(time.Millisecond)
			}
			r.add(last)
			if len(r.frames) != 0 || r.bytes != 0 || !r.droppedForBytes || !r.nextResetSegment {
				t.Fatalf("ring did not restart: %+v", r)
			}
			for i, retained := range r.frames[:cap(r.frames)] {
				if retained.frame != nil {
					t.Fatalf("discarded frame %d still reachable through ring backing array", i)
				}
			}
			if !bytes.Equal(first.data, sharedPayload) || first.sampleID != 1 {
				t.Fatal("restart changed a frame still held by another subscriber")
			}
			r.add(preRollFrame(int64(10*time.Second), 3, false))
			if len(r.frames) != 0 {
				t.Fatal("restarted ring accepted an inter frame before a keyframe")
			}
			r.add(preRollFrame(int64(11*time.Second), 4, true))
			if len(r.frames) != 1 || !r.frames[0].resetSegment || r.frames[0].frame.sampleID != 4 {
				t.Fatal("restarted ring did not resume at the next keyframe with a segment reset")
			}
		})
	}
}

// This is retained Go heap after collection, not process RSS or allocation
// traffic. Keep the armed ring alive while its producer waits for a keyframe.
func TestPreRollRestartRetainedHeap(t *testing.T) {
	for _, reason := range []string{"bytes", "age"} {
		t.Run(reason, func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			r := restartedMemoryRing(reason)
			runtime.GC()
			runtime.ReadMemStats(&after)
			if len(r.frames) != 0 || r.bytes != 0 {
				t.Fatal("fixture did not reach restart")
			}
			t.Logf("retained_heap_bytes=%d ring_capacity=%d active_payload_bytes=%d", int64(after.HeapAlloc)-int64(before.HeapAlloc), cap(r.frames), r.bytes)
			runtime.KeepAlive(r)
		})
	}
}

func restartedMemoryRing(reason string) *cameraPreRollRing {
	r := &cameraPreRollRing{buffer: time.Second, limitBytes: preRollCameraLimitBytes}
	frames := 513 // 64 MiB plus the 128 KiB frame that crosses the cap.
	if reason == "age" {
		frames = 256 // 32 MiB, within the production byte cap.
	}
	for i := range frames {
		stamp := int64(i) * int64(time.Millisecond)
		if reason == "age" && i == frames-1 {
			stamp = int64(9 * time.Second)
		}
		f := preRollFrame(stamp, uint64(i+1), i == 0)
		payload := make([]byte, 128<<10)
		copy(payload, f.data)
		f.data = payload
		r.add(f)
	}
	return r
}

// Include refilling the existing metadata slice so this measures a reusable
// ring, with shared immutable frame payloads and no allocation in either path.
func BenchmarkPreRollRestart(b *testing.B) {
	for _, count := range []int{256, 513, 4096} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			frames := make([]bufferedCameraFrame, count)
			for i := range frames {
				frames[i] = bufferedCameraFrame{frame: preRollFrame(int64(i), uint64(i+1), i == 0)}
			}
			r := &cameraPreRollRing{frames: make([]bufferedCameraFrame, count)}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				r.frames = r.frames[:count]
				copy(r.frames, frames)
				r.restart()
			}
			runtime.KeepAlive(r)
		})
	}
}
