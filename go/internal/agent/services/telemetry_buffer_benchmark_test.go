//go:build linux || darwin

package services

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/protobuf/proto"
)

// Exercise publication with an empty or nearly full default retention window.
// Historical files are sparse because publication only inspects their metadata.
// Writes, rotation, quota enforcement and broadcaster fanout are production code.
// Compare with a fixed iteration count, such as -benchtime=4000x, so both
// revisions perform the same writes and traverse the same retention growth.
func BenchmarkTelemetryBufferPublishLogs(b *testing.B) {
	for _, retained := range []int{0, 24} {
		for _, payload := range []int{256, 4096} {
			b.Run(fmt.Sprintf("segments=%d/payload=%d", retained, payload), func(b *testing.B) {
				dir := b.TempDir()
				for i := 1; i <= retained; i++ {
					f, err := os.Create(filepath.Join(dir, segmentFilename(SignalLogs, i)))
					if err != nil {
						b.Fatal(err)
					}
					err = f.Truncate(defaultSegmentBytes)
					closeErr := f.Close()
					if err != nil || closeErr != nil {
						b.Fatalf("prepare history: %v, %v", err, closeErr)
					}
				}
				core, warnings := observer.New(zap.WarnLevel)
				buffer := NewTelemetryBuffer(TelemetryBufferConfig{Dir: dir}, NewTelemetryBroadcaster(), zap.New(core))
				b.Cleanup(func() {
					for _, writer := range buffer.writers {
						if writer != nil {
							_ = writer.f.Close()
						}
					}
				})
				request := makeLogReq(strings.Repeat("x", payload))
				frameBytes := int64(proto.Size(request) + 4)
				if buffer.writers[SignalLogs] == nil || warnings.Len() != 0 {
					b.Fatal("telemetry writer setup failed")
				}
				b.ReportAllocs()
				b.SetBytes(int64(payload))
				b.ResetTimer()
				cpu := telemetryBenchmarkCPU(b)
				for i := 0; i < b.N; i++ {
					buffer.PublishLogs(request)
				}
				cpu = telemetryBenchmarkCPU(b) - cpu
				b.StopTimer()
				b.ReportMetric(float64(cpu)/float64(b.N), "cpu-ns/op")
				b.ReportMetric(float64(frameBytes), "disk-B/op")
				// PublishLogs reports failures through logging rather than returning
				// them. Verify successful writes so failures cannot look faster.
				writer := buffer.writers[SignalLogs]
				if writer == nil || warnings.Len() != 0 {
					b.Fatal("telemetry publication logged a failure")
				}
				perSegment := defaultSegmentBytes / frameBytes
				first := 1
				if retained > 0 {
					first = retained + 1
				}
				wantSequence := first + int(int64(b.N-1)/perSegment)
				wantBytes := (int64(b.N-1)%perSegment + 1) * frameBytes
				info, err := writer.f.Stat()
				if err != nil || writer.seqNum != wantSequence || info.Size() != wantBytes {
					b.Fatalf("unexpected final segment: sequence=%d want=%d bytes=%d err=%v", writer.seqNum, wantSequence, wantBytes, err)
				}
				if buffer.totalSegmentSize() > defaultMaxTotalBytes {
					b.Fatal("telemetry retention exceeded its quota")
				}
			})
		}
	}
}

func telemetryBenchmarkCPU(b *testing.B) int64 {
	b.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		b.Fatal(err)
	}
	return usage.Utime.Nano() + usage.Stime.Nano()
}
