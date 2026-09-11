package services

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/audio"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
)

// fakeAudioStream yields a fixed sequence of PCM chunks and then EOF.
type fakeAudioStream struct {
	chunks [][]byte
	i      int
}

func (f *fakeAudioStream) Read(b []byte) (int, error) {
	if f.i >= len(f.chunks) {
		return 0, io.EOF
	}
	n := copy(b, f.chunks[f.i])
	f.i++
	return n, nil
}
func (f *fakeAudioStream) Close()      {}
func (f *fakeAudioStream) Err() string { return "" }

// pcmChunk is silence of an arbitrary length. Anything shorter than
// audioChunkBytes is a short read: the capture loop offers a 9600-byte buffer
// and the fake, like a real capture pipe, fills only part of it. Hardware does
// exactly this, delivering 8192 bytes per read on a Jetson Orin Nano.
func pcmChunk(n int) []byte { return make([]byte, n) }

// loudPCMChunk is a chunk of near full-scale s16 samples, well above any
// sensible level threshold.
func loudPCMChunk(n int) []byte {
	b := make([]byte, n)
	for i := 0; i+1 < len(b); i += 2 {
		binary.LittleEndian.PutUint16(b[i:], 0x7FFF)
	}
	return b
}

func silentChunk() []byte { return pcmChunk(audioChunkBytes) }

func loudChunk() []byte { return loudPCMChunk(audioChunkBytes) }

// fillOneSegment returns the chunks that fill exactly one rotating segment,
// opening with a read of firstBytes so a test can tell which chunk started
// which segment. The total is exact, so the next chunk after these begins a new
// segment.
func fillOneSegment(firstBytes int) [][]byte {
	chunks := [][]byte{pcmChunk(firstBytes)}
	remaining := int(audioSegmentBytes) - firstBytes
	for remaining >= audioChunkBytes {
		chunks = append(chunks, pcmChunk(audioChunkBytes))
		remaining -= audioChunkBytes
	}
	if remaining > 0 {
		chunks = append(chunks, pcmChunk(remaining))
	}
	return chunks
}

// audioReceipt is one scripted CLOCK_BOOTTIME bracket: the reads either side of
// the instant the capture stamps a segment with.
type audioReceipt struct{ before, mid, after int64 }

// bracket is the half-width canonicalTime derives from the bracket, which is
// the clock-read jitter alone.
func (r audioReceipt) bracket() int64 { return (r.after - r.before + 1) / 2 }

// bootReceipt builds a bracket of the given half-width centred on mid. Callers
// pass instants at least a second past boot, as CLOCK_BOOTTIME always is by the
// time audio is flowing, so the backdating and the episode-offset subtraction
// are exercised against realistic values rather than against zero.
func bootReceipt(mid, halfWidth time.Duration) audioReceipt {
	return audioReceipt{before: int64(mid - halfWidth), mid: int64(mid), after: int64(mid + halfWidth)}
}

// scriptedReceipts drives the captureReceipt seam, handing out one bracket per
// beginSegment call and repeating the last once the script runs out, so a test
// scripts only the segments it asserts on.
func scriptedReceipts(brackets ...audioReceipt) func() (int64, int64, int64, error) {
	var i int
	return func() (int64, int64, int64, error) {
		r := brackets[i]
		if i < len(brackets)-1 {
			i++
		}
		return r.before, r.mid, r.after, nil
	}
}

// audioRun scripts one capture: the PCM the fake stream yields, the clock the
// capture reads, and the instant the episode was requested.
type audioRun struct {
	capture          *data.SourceCapture
	chunks           [][]byte
	receipts         func() (int64, int64, int64, error)
	requestBootNanos int64
}

// runAudioCaptureWith drives the adapter with a scripted run and returns the
// capture result plus the audio output directory.
func runAudioCaptureWith(t *testing.T, run audioRun) (data.CaptureResult, string) {
	t.Helper()
	sessionDir := t.TempDir()
	source := data.Source{ID: "audio:0", Kind: "audio", ClockDomain: "TEST_CAPTURE/AGENT_RECEIPT", Healthy: true, Detail: "test mic", Capture: run.capture}
	adapter := &audioDataAdapter{
		audio: &AudioService{},
		openStream: func(context.Context, uint32) (audioStream, error) {
			return &fakeAudioStream{chunks: run.chunks}, nil
		},
		captureReceipt: run.receipts,
	}
	session := data.CaptureSession{ID: "ep", Directory: sessionDir, RequestBootNanos: run.requestBootNanos}
	running, err := adapter.Start(context.Background(), session, []data.Source{source})
	if err != nil {
		t.Fatalf("adapter start: %v", err)
	}
	if running == nil {
		t.Fatal("adapter returned no running capture")
	}
	results, err := running.Stop(context.Background())
	if err != nil {
		t.Fatalf("adapter stop: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 capture result, got %d", len(results))
	}
	return results[0], filepath.Join(sessionDir, "audio", safeCaptureName(source.ID))
}

// runAudioCapture drives the adapter with a scripted PCM stream over the real
// clock, for tests that assert on sealing behaviour rather than on stamps.
func runAudioCapture(t *testing.T, capture *data.SourceCapture, chunks [][]byte) (data.CaptureResult, string) {
	t.Helper()
	return runAudioCaptureWith(t, audioRun{capture: capture, chunks: chunks})
}

// readAudioIndex parses index.jsonl, one record per sealed segment.
func readAudioIndex(t *testing.T, dir string) []audioIndexRecord {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var records []audioIndexRecord
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var record audioIndexRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("index line %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func wavFragments(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var wavs []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".wav" {
			wavs = append(wavs, filepath.Join(dir, e.Name()))
		}
	}
	return wavs
}

// wavDataBytes reads the PCM payload size recorded in a WAV file's data chunk
// header and confirms it matches the bytes actually on disk.
func wavDataBytes(t *testing.T, path string) int64 {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < wavHeaderBytes || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		t.Fatalf("%s is not a WAV file", path)
	}
	declared := int64(binary.LittleEndian.Uint32(b[40:44]))
	if onDisk := int64(len(b)) - wavHeaderBytes; onDisk != declared {
		t.Fatalf("%s: header declares %d PCM bytes but file holds %d", path, declared, onDisk)
	}
	return declared
}

// TestAudioThresholdSealsFragmentOfConfiguredDuration verifies a level crossing
// seals exactly one fragment of the configured duration.
func TestAudioThresholdSealsFragmentOfConfiguredDuration(t *testing.T) {
	// fragment 1s = 10 chunks of ~100ms.
	chunks := [][]byte{silentChunk(), silentChunk()}
	for i := 0; i < 10; i++ {
		chunks = append(chunks, loudChunk())
	}
	chunks = append(chunks, silentChunk(), silentChunk())

	result, dir := runAudioCapture(t, &data.SourceCapture{Mode: "threshold", Trigger: "level_db > -20", Fragment: "1s"}, chunks)

	wavs := wavFragments(t, dir)
	if len(wavs) != 1 {
		t.Fatalf("expected exactly 1 sealed fragment, got %d (%v)", len(wavs), wavs)
	}
	if got := wavDataBytes(t, wavs[0]); got != int64(audioBytesPerSecond) {
		t.Fatalf("fragment PCM bytes = %d, want %d (1 second)", got, audioBytesPerSecond)
	}
	if result.Count != 1 {
		t.Fatalf("result.Count = %d, want 1 sealed fragment", result.Count)
	}
	if result.Drops == nil || *result.Drops != 0 {
		t.Fatalf("expected 0 missed crossings, got %v", result.Drops)
	}
	if result.DropAccounting != "missed_threshold_crossings_during_seal" {
		t.Fatalf("drop accounting = %q", result.DropAccounting)
	}
}

// TestAudioSubThresholdSealsNothing verifies audio that never crosses the
// threshold produces no fragment.
func TestAudioSubThresholdSealsNothing(t *testing.T) {
	chunks := [][]byte{silentChunk(), silentChunk(), silentChunk(), silentChunk()}
	result, dir := runAudioCapture(t, &data.SourceCapture{Mode: "threshold", Trigger: "level_db > -20", Fragment: "1s"}, chunks)

	if wavs := wavFragments(t, dir); len(wavs) != 0 {
		t.Fatalf("expected no fragments for sub-threshold audio, got %v", wavs)
	}
	if result.Count != 0 {
		t.Fatalf("result.Count = %d, want 0", result.Count)
	}
}

// TestAudioThresholdCountsMissedCrossingsDuringSeal verifies a second crossing
// while a fragment is still sealing is counted as a missed crossing rather than
// starting an overlapping fragment.
func TestAudioThresholdCountsMissedCrossingsDuringSeal(t *testing.T) {
	// A long fragment (3s = 30 chunks) so the seal is still in progress when a
	// second crossing occurs. Script: loud (start seal), silent (drop below),
	// loud (a new crossing mid-seal -> missed), then enough loud to finish.
	chunks := [][]byte{loudChunk(), silentChunk(), loudChunk()}
	for i := 0; i < 30; i++ {
		chunks = append(chunks, loudChunk())
	}
	result, dir := runAudioCapture(t, &data.SourceCapture{Mode: "threshold", Trigger: "level_db > -20", Fragment: "3s"}, chunks)

	if wavs := wavFragments(t, dir); len(wavs) != 1 {
		t.Fatalf("expected 1 fragment, got %v", wavs)
	}
	if result.Drops == nil || *result.Drops != 1 {
		t.Fatalf("expected exactly 1 missed crossing during the seal, got %v", result.Drops)
	}
}

// TestAudioSegmentStampBackdatesChunkAndPipeline pins the whole stamping
// arithmetic on one segment. The clock is read only after a chunk has been
// pulled off the capture pipe, so the receipt describes the moment the chunk
// arrived, not the moment its first sample was captured. The segment stamp
// labels that first sample, so it has to be walked back past the chunk and past
// the delay still outstanding behind it, and the published uncertainty has to
// widen to cover that delay rather than reporting clock-read jitter alone.
func TestAudioSegmentStampBackdatesChunkAndPipeline(t *testing.T) {
	const (
		chunkBytes    = 4800 // 50 ms of s16le 48 kHz mono
		chunkNanos    = int64(50 * time.Millisecond)
		requestBoot   = int64(500 * time.Millisecond)
		clockHalfWide = 3 * time.Microsecond
	)
	stamp := bootReceipt(5*time.Second, clockHalfWide)

	result, dir := runAudioCaptureWith(t, audioRun{
		chunks:           [][]byte{pcmChunk(chunkBytes)},
		receipts:         scriptedReceipts(stamp),
		requestBootNanos: requestBoot,
	})

	records := readAudioIndex(t, dir)
	if len(records) != 1 {
		t.Fatalf("got %d index records, want 1: %+v", len(records), records)
	}
	record := records[0]

	wantCanonical := stamp.mid - chunkNanos - int64(audioPipelineDelayEstimate) - requestBoot
	if record.CanonicalEpisodeNanos != wantCanonical {
		t.Errorf("canonical_episode_nanos = %d, want %d (receipt %d less the %v chunk, the %v pipeline delay and the %v episode offset)",
			record.CanonicalEpisodeNanos, wantCanonical, stamp.mid,
			time.Duration(chunkNanos), audioPipelineDelayEstimate, time.Duration(requestBoot))
	}
	if record.AgentReceiptBootNanos != stamp.mid {
		t.Errorf("agent_receipt_boottime_nanos = %d, want the raw receipt %d", record.AgentReceiptBootNanos, stamp.mid)
	}
	wantUncertainty := stamp.bracket() + int64(audioPipelineDelayBound)
	if record.CanonicalUncertaintyNanos != wantUncertainty {
		t.Errorf("canonical_uncertainty_nanos = %d, want %d (clock bracket %d plus the %v delay bound)",
			record.CanonicalUncertaintyNanos, wantUncertainty, stamp.bracket(), audioPipelineDelayBound)
	}
	if record.MappingSegment != "receipt-minus-pipeline-v1" {
		t.Errorf("mapping_segment = %q, want %q so a reader can tell corrected stamps from legacy ones",
			record.MappingSegment, "receipt-minus-pipeline-v1")
	}
	if record.DurationNanos != chunkNanos {
		t.Errorf("duration_nanos = %d, want %d", record.DurationNanos, chunkNanos)
	}
	if result.ActualOffset == nil || *result.ActualOffset != wantCanonical {
		t.Errorf("result.ActualOffset = %v, want %d", result.ActualOffset, wantCanonical)
	}
	if result.MappingError == nil || *result.MappingError != wantUncertainty {
		t.Errorf("result.MappingError = %v, want %d so the campaign summary agrees with the segment",
			result.MappingError, wantUncertainty)
	}
}

// TestAudioShortReadStampAndDurationUseBytes proves every duration comes from
// the bytes actually read rather than from audioChunkBytes. A pipe read returns
// whatever the capture process has produced, and on hardware that is 8192 bytes
// rather than the 9600 the buffer holds; assuming the buffer size would put
// every stamp out by the difference.
func TestAudioShortReadStampAndDurationUseBytes(t *testing.T) {
	const (
		firstBytes  = 3000
		secondBytes = 5000
		// 3000 bytes at 96000 bytes per second is 31.25 ms.
		firstNanos  = int64(31_250_000)
		requestBoot = int64(1500 * time.Millisecond)
	)
	if got := pcmDurationNanos(firstBytes); got != firstNanos {
		t.Fatalf("pcmDurationNanos(%d) = %d, want %d", firstBytes, got, firstNanos)
	}
	stamp := bootReceipt(7*time.Second, 2*time.Microsecond)

	_, dir := runAudioCaptureWith(t, audioRun{
		chunks:           [][]byte{pcmChunk(firstBytes), pcmChunk(secondBytes)},
		receipts:         scriptedReceipts(stamp),
		requestBootNanos: requestBoot,
	})

	records := readAudioIndex(t, dir)
	if len(records) != 1 {
		t.Fatalf("got %d index records, want 1: %+v", len(records), records)
	}
	record := records[0]

	wantCanonical := stamp.mid - firstNanos - int64(audioPipelineDelayEstimate) - requestBoot
	if record.CanonicalEpisodeNanos != wantCanonical {
		t.Errorf("canonical_episode_nanos = %d, want %d (backdated by the 3000 bytes actually read, not by the 9600-byte buffer)",
			record.CanonicalEpisodeNanos, wantCanonical)
	}
	if record.ByteSize != firstBytes+secondBytes {
		t.Errorf("byte_size = %d, want %d", record.ByteSize, firstBytes+secondBytes)
	}
	// 8000 bytes at 96000 bytes per second is 83.333333 ms.
	if want := int64(83_333_333); record.DurationNanos != want {
		t.Errorf("duration_nanos = %d, want %d (the total bytes sealed)", record.DurationNanos, want)
	}
}

// TestAudioThresholdFragmentStampBackdates covers the threshold path, where the
// segment opens on the chunk that crossed the level rather than on the first
// chunk of the stream. The crossing chunk is short, so an assumed chunk length
// would misplace the fragment start.
func TestAudioThresholdFragmentStampBackdates(t *testing.T) {
	const (
		crossingBytes = 4800 // 50 ms, exactly the configured fragment
		crossingNanos = int64(50 * time.Millisecond)
		requestBoot   = int64(2 * time.Second)
	)
	stamp := bootReceipt(9*time.Second, 4*time.Microsecond)

	result, dir := runAudioCaptureWith(t, audioRun{
		capture:          &data.SourceCapture{Mode: "threshold", Trigger: "level_db > -20", Fragment: "50ms"},
		chunks:           [][]byte{silentChunk(), loudPCMChunk(crossingBytes), silentChunk()},
		receipts:         scriptedReceipts(stamp),
		requestBootNanos: requestBoot,
	})

	if result.Count != 1 {
		t.Fatalf("result.Count = %d, want 1 sealed fragment", result.Count)
	}
	records := readAudioIndex(t, dir)
	if len(records) != 1 {
		t.Fatalf("got %d index records, want 1: %+v", len(records), records)
	}
	record := records[0]

	wantCanonical := stamp.mid - crossingNanos - int64(audioPipelineDelayEstimate) - requestBoot
	if record.CanonicalEpisodeNanos != wantCanonical {
		t.Errorf("canonical_episode_nanos = %d, want %d (backdated by the crossing chunk, not by the silent chunk before it)",
			record.CanonicalEpisodeNanos, wantCanonical)
	}
	if record.CanonicalUncertaintyNanos != stamp.bracket()+int64(audioPipelineDelayBound) {
		t.Errorf("canonical_uncertainty_nanos = %d, want %d", record.CanonicalUncertaintyNanos, stamp.bracket()+int64(audioPipelineDelayBound))
	}
	if record.Mode != "threshold" || record.TriggerLevelDb == nil {
		t.Errorf("record does not describe a threshold fragment: %+v", record)
	}
	if record.MappingSegment != "receipt-minus-pipeline-v1" {
		t.Errorf("mapping_segment = %q, want %q", record.MappingSegment, "receipt-minus-pipeline-v1")
	}
	if record.DurationNanos != crossingNanos {
		t.Errorf("duration_nanos = %d, want %d", record.DurationNanos, crossingNanos)
	}
}

// TestAudioSegmentRotationRestampsPerSegment proves the correction is applied
// per segment rather than once at capture start. Each rotation opens on a chunk
// of a different length, and each must be stamped from its own receipt and its
// own opening chunk.
func TestAudioSegmentRotationRestampsPerSegment(t *testing.T) {
	const requestBoot = int64(3 * time.Second)
	firstBytes := []int{4800, 2400, 3000}
	firstNanos := []int64{50_000_000, 25_000_000, 31_250_000}
	stamps := []audioReceipt{
		bootReceipt(11*time.Second, 1*time.Microsecond),
		bootReceipt(21*time.Second, 5*time.Microsecond),
		bootReceipt(31*time.Second, 9*time.Microsecond),
	}

	var chunks [][]byte
	chunks = append(chunks, fillOneSegment(firstBytes[0])...)
	chunks = append(chunks, fillOneSegment(firstBytes[1])...)
	chunks = append(chunks, pcmChunk(firstBytes[2]))

	result, dir := runAudioCaptureWith(t, audioRun{
		chunks:           chunks,
		receipts:         scriptedReceipts(stamps...),
		requestBootNanos: requestBoot,
	})

	if result.Count != 3 {
		t.Fatalf("result.Count = %d, want 3 (two rotations plus the segment sealed at stop)", result.Count)
	}
	records := readAudioIndex(t, dir)
	if len(records) != 3 {
		t.Fatalf("got %d index records, want 3: %+v", len(records), records)
	}
	for i, record := range records {
		wantCanonical := stamps[i].mid - firstNanos[i] - int64(audioPipelineDelayEstimate) - requestBoot
		if record.CanonicalEpisodeNanos != wantCanonical {
			t.Errorf("segment %d canonical_episode_nanos = %d, want %d (its own receipt less its own opening chunk)",
				i+1, record.CanonicalEpisodeNanos, wantCanonical)
		}
		if record.AgentReceiptBootNanos != stamps[i].mid {
			t.Errorf("segment %d agent_receipt_boottime_nanos = %d, want %d", i+1, record.AgentReceiptBootNanos, stamps[i].mid)
		}
		wantUncertainty := stamps[i].bracket() + int64(audioPipelineDelayBound)
		if record.CanonicalUncertaintyNanos != wantUncertainty {
			t.Errorf("segment %d canonical_uncertainty_nanos = %d, want %d", i+1, record.CanonicalUncertaintyNanos, wantUncertainty)
		}
	}
	for i := 0; i < 2; i++ {
		if records[i].ByteSize != audioSegmentBytes {
			t.Errorf("segment %d byte_size = %d, want a full segment of %d", i+1, records[i].ByteSize, audioSegmentBytes)
		}
		if want := int64(audioSegmentDuration); records[i].DurationNanos != want {
			t.Errorf("segment %d duration_nanos = %d, want %d", i+1, records[i].DurationNanos, want)
		}
	}
	if records[2].ByteSize != int64(firstBytes[2]) {
		t.Errorf("final segment byte_size = %d, want %d", records[2].ByteSize, firstBytes[2])
	}
}

// jetsonArecordFixture is the verbatim "arecord -l" output from
// wendyos-hubert.local, a Jetson Orin Nano: one real USB microphone on card 0
// and the twenty Audio DMA Interface front-ends of the Tegra Audio Processing
// Engine on card 2. Every one of the twenty opens, streams and returns pure
// digital silence; a one-second capture from plughw:2,0 yields 192044 bytes
// with zero non-zero samples, while the same capture from the C920 on
// plughw:0,0 yields 159601 non-zero bytes.
const jetsonArecordFixture = `**** List of CAPTURE Hardware Devices ****
card 0: C920 [HD Pro Webcam C920], device 0: USB Audio [USB Audio]
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 0: fe.admaif@290f000.ADMAIF1 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 1: fe.admaif@290f000.ADMAIF2 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 2: fe.admaif@290f000.ADMAIF3 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 3: fe.admaif@290f000.ADMAIF4 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 4: fe.admaif@290f000.ADMAIF5 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 5: fe.admaif@290f000.ADMAIF6 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 6: fe.admaif@290f000.ADMAIF7 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 7: fe.admaif@290f000.ADMAIF8 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 8: fe.admaif@290f000.ADMAIF9 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 9: fe.admaif@290f000.ADMAIF10 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 10: fe.admaif@290f000.ADMAIF11 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 11: fe.admaif@290f000.ADMAIF12 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 12: fe.admaif@290f000.ADMAIF13 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 13: fe.admaif@290f000.ADMAIF14 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 14: fe.admaif@290f000.ADMAIF15 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 15: fe.admaif@290f000.ADMAIF16 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 16: fe.admaif@290f000.ADMAIF17 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 17: fe.admaif@290f000.ADMAIF18 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 18: fe.admaif@290f000.ADMAIF19 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 19: fe.admaif@290f000.ADMAIF20 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
`

// jetsonAplayFixture is the playback half from the same device. Card 1 is the
// HDMI codec; the APE card exposes the playback direction of the same twenty
// ADMAIF front-ends. Sinks are skipped by Discover, so these only prove the
// health check never reaches them.
const jetsonAplayFixture = `**** List of PLAYBACK Hardware Devices ****
card 1: HDA [NVIDIA Jetson Orin Nano HDA], device 3: HDMI 0 [HDMI 0]
  Subdevices: 1/1
  Subdevice #0: subdevice #0
card 2: APE [NVIDIA Jetson Orin Nano APE], device 0: fe.admaif@290f000.ADMAIF1 (*) []
  Subdevices: 1/1
  Subdevice #0: subdevice #0
`

// discoverWithAlsaFixtures runs Discover over the ALSA fallback path with the
// given aplay/arecord output, which is the path a WendyOS device actually takes
// (the agent has no PipeWire session, so its sources report the
// ALSA_CAPTURE/AGENT_RECEIPT clock domain).
func discoverWithAlsaFixtures(t *testing.T, aplayOut, arecordOut string) []data.Source {
	t.Helper()
	origAvailable, origAplay, origArecord := audio.Available, audio.AplayListRun, audio.ArecordListRun
	t.Cleanup(func() {
		audio.Available, audio.AplayListRun, audio.ArecordListRun = origAvailable, origAplay, origArecord
	})
	audio.Available = func() bool { return false }
	audio.AplayListRun = func(context.Context) ([]byte, error) { return []byte(aplayOut), nil }
	audio.ArecordListRun = func(context.Context) ([]byte, error) { return []byte(arecordOut), nil }

	adapter := &audioDataAdapter{audio: &AudioService{}}
	return adapter.Discover(context.Background())
}

// TestIsAudioHubDMAEndpointOnRealDeviceStrings exercises the detection function
// against the exact strings the kernel produced on hardware, since it parses a
// driver-formatted name and nothing else pins that format.
func TestIsAudioHubDMAEndpointOnRealDeviceStrings(t *testing.T) {
	unrouted := []string{
		"APE [NVIDIA Jetson Orin Nano APE], device 0: fe.admaif@290f000.ADMAIF1 (*) [] plughw:2,0",
		"APE [NVIDIA Jetson Orin Nano APE], device 19: fe.admaif@290f000.ADMAIF20 (*) [] plughw:2,19",
		// /proc/asound/card2/pcm0c/info reports the id without the empty name,
		// so the check must not depend on the trailing "[]".
		"fe.admaif@290f000.ADMAIF1 (*)",
	}
	for _, detail := range unrouted {
		if !isAudioHubDMAEndpoint(detail) {
			t.Errorf("isAudioHubDMAEndpoint(%q) = false, want true", detail)
		}
	}

	physical := []string{
		"C920 [HD Pro Webcam C920], device 0: USB Audio [USB Audio] plughw:0,0",
		"HDA [NVIDIA Jetson Orin Nano HDA], device 3: HDMI 0 [HDMI 0] plughw:1,3",
		"tegra-hsp [tegra-hsp], device 0: ADMA Interface [ADMA] plughw:3,0",
	}
	for _, detail := range physical {
		if isAudioHubDMAEndpoint(detail) {
			t.Errorf("isAudioHubDMAEndpoint(%q) = true, want false", detail)
		}
	}
}

// TestJetsonAudioHubEndpointsAreAllUnhealthy pins the shape of the device that
// exposed this defect: of the twenty-one capture sources a Jetson Orin Nano
// advertises, exactly one is a real microphone. Before this check all
// twenty-one reported healthy, so a campaign could resolve to an endpoint that
// records silence forever without erring at any layer.
func TestJetsonAudioHubEndpointsAreAllUnhealthy(t *testing.T) {
	sources := discoverWithAlsaFixtures(t, jetsonAplayFixture, jetsonArecordFixture)
	if len(sources) != 21 {
		t.Fatalf("got %d audio sources, want 21 (one C920 plus twenty ADMAIF); got %+v", len(sources), sources)
	}

	var healthy, unhealthy []data.Source
	for _, source := range sources {
		if source.Kind != "audio" {
			t.Fatalf("unexpected source kind %q in audio discovery: %+v", source.Kind, source)
		}
		if source.Healthy {
			healthy = append(healthy, source)
		} else {
			unhealthy = append(unhealthy, source)
		}
	}
	if len(healthy) != 1 {
		t.Fatalf("want exactly 1 healthy audio source, got %d: %+v", len(healthy), healthy)
	}
	if len(unhealthy) != 20 {
		t.Fatalf("want all 20 ADMAIF endpoints unhealthy, got %d", len(unhealthy))
	}

	// The one healthy source is the real microphone, and its description
	// survives intact so the source is still identifiable.
	if !strings.Contains(healthy[0].Detail, "HD Pro Webcam C920") || !strings.Contains(healthy[0].Detail, "plughw:0,0") {
		t.Errorf("healthy source is not the C920 with its description intact: %q", healthy[0].Detail)
	}
	if strings.Contains(healthy[0].Detail, audioHubDMAEndpointReason) {
		t.Errorf("a real microphone was annotated with the audio hub reason: %q", healthy[0].Detail)
	}

	// Every unhealthy source keeps its description and gains the reason.
	for _, source := range unhealthy {
		if !strings.Contains(source.Detail, "fe.admaif@290f000.ADMAIF") {
			t.Errorf("unhealthy source lost its description: %q", source.Detail)
		}
		if !strings.Contains(source.Detail, audioHubDMAEndpointReason) {
			t.Errorf("unhealthy source does not say why: %q", source.Detail)
		}
		if !strings.Contains(source.Detail, "plughw:2,") {
			t.Errorf("unhealthy source lost its device address: %q", source.Detail)
		}
	}
}

// TestUsbMicrophoneStaysHealthy proves the check does not cost a normal device
// its health or its description when no audio hub endpoint is present at all.
func TestUsbMicrophoneStaysHealthy(t *testing.T) {
	const arecordOnlyUSB = `**** List of CAPTURE Hardware Devices ****
card 0: C920 [HD Pro Webcam C920], device 0: USB Audio [USB Audio]
  Subdevices: 1/1
  Subdevice #0: subdevice #0
`
	sources := discoverWithAlsaFixtures(t, "**** List of PLAYBACK Hardware Devices ****\n", arecordOnlyUSB)
	if len(sources) != 1 {
		t.Fatalf("got %d sources, want 1: %+v", len(sources), sources)
	}
	if !sources[0].Healthy {
		t.Errorf("USB microphone reported unhealthy: %+v", sources[0])
	}
	want := "C920 [HD Pro Webcam C920], device 0: USB Audio [USB Audio] plughw:0,0"
	if sources[0].Detail != want {
		t.Errorf("detail = %q, want %q", sources[0].Detail, want)
	}
}

// TestAudioSegmentStampNeverPrecedesTheEpisodeOrigin pins the one direction the
// backdating correction is not allowed to go.
//
// The correction walks the stamp back past the chunk in hand and past the
// pipeline delay behind it, and nothing stopped it walking back past the start
// of the episode. Audio has no pre-roll ring: capture opens when the episode
// does, so there are no samples from before the origin. A negative ActualOffset
// is how this tree says "pre-roll was achieved" (see the camera adapter and
// data_service_test), so a negative audio offset advertises pre-trigger audio
// that does not exist, on the strength of an estimated constant.
//
// The gap here is the realistic one the existing stamping tests dodge: an
// episode origin at ten seconds and the first 8192-byte read a hundred
// milliseconds later, which is what a Jetson Orin Nano with a Logitech C920
// actually delivers. Unclamped that produced canonical_episode_nanos of
// -5333333.
func TestAudioSegmentStampNeverPrecedesTheEpisodeOrigin(t *testing.T) {
	const (
		// 8192 bytes is what a real pipe read returns: 85.333 ms of PCM.
		chunkBytes    = 8192
		origin        = int64(10 * time.Second)
		receiptDelay  = int64(100 * time.Millisecond)
		clockHalfWide = 3 * time.Microsecond
	)
	chunkNanos := pcmDurationNanos(chunkBytes)
	stamp := bootReceipt(time.Duration(origin+receiptDelay), clockHalfWide)

	// The premise, computed rather than asserted from memory: the uncorrected
	// arithmetic really does reach back past the origin here, so this test
	// exercises the clamp instead of quietly agreeing with an unclamped stamp.
	wantShortfall := chunkNanos + int64(audioPipelineDelayEstimate) - receiptDelay
	if wantShortfall <= 0 {
		t.Fatalf("this gap does not reach past the origin (shortfall %d); the test would prove nothing", wantShortfall)
	}

	result, dir := runAudioCaptureWith(t, audioRun{
		chunks:           [][]byte{pcmChunk(chunkBytes)},
		receipts:         scriptedReceipts(stamp),
		requestBootNanos: origin,
	})

	records := readAudioIndex(t, dir)
	if len(records) != 1 {
		t.Fatalf("got %d index records, want 1: %+v", len(records), records)
	}
	record := records[0]

	if record.CanonicalEpisodeNanos != 0 {
		t.Errorf("canonical_episode_nanos = %d, want 0: audio has no pre-roll ring, so a segment stamp may not precede the episode origin",
			record.CanonicalEpisodeNanos)
	}
	// Clamped, and said so. A silent max would leave a reader unable to tell a
	// bound from an estimate that happened to land on the origin.
	if !record.ClampedToOrigin {
		t.Error("clamped_to_origin is absent; the clamp must be visible in the record, not silent")
	}
	if record.ClampShortfallNanos != wantShortfall {
		t.Errorf("clamp_shortfall_nanos = %d, want %d", record.ClampShortfallNanos, wantShortfall)
	}
	// And the uncertainty widened by the shortfall, so the instant the
	// correction pointed at is still inside the published bracket.
	wantUncertainty := stamp.bracket() + int64(audioPipelineDelayBound) + wantShortfall
	if record.CanonicalUncertaintyNanos != wantUncertainty {
		t.Errorf("canonical_uncertainty_nanos = %d, want %d (clock bracket %d, the %v delay bound, and the %d ns shortfall)",
			record.CanonicalUncertaintyNanos, wantUncertainty, stamp.bracket(), audioPipelineDelayBound, wantShortfall)
	}
	if record.CanonicalEpisodeNanos-record.CanonicalUncertaintyNanos > -wantShortfall {
		t.Error("the widened bracket does not reach back to where the correction pointed")
	}

	if result.ActualOffset == nil || *result.ActualOffset != 0 {
		t.Errorf("result.ActualOffset = %v, want 0: a negative offset would claim pre-trigger audio that was never captured",
			result.ActualOffset)
	}
	if result.MappingError == nil || *result.MappingError != wantUncertainty {
		t.Errorf("result.MappingError = %v, want %d so the episode summary carries the widened bound",
			result.MappingError, wantUncertainty)
	}
	if !strings.Contains(result.SourceDetail, "clamped to the episode origin") {
		t.Errorf("source detail = %q, want it to name the clamp so an operator need not open the audio index",
			result.SourceDetail)
	}
}

// TestAudioSegmentStampIsNotClampedWhenItNeedNotBe is the other half: the clamp
// must fire only when the correction really would reach past the origin. A
// clamp that fired early would flatten genuine sub-origin arithmetic into a
// pile of stamps all reading zero, which is a different lie.
func TestAudioSegmentStampIsNotClampedWhenItNeedNotBe(t *testing.T) {
	const (
		chunkBytes    = 8192
		origin        = int64(10 * time.Second)
		receiptDelay  = int64(200 * time.Millisecond)
		clockHalfWide = 3 * time.Microsecond
	)
	chunkNanos := pcmDurationNanos(chunkBytes)
	stamp := bootReceipt(time.Duration(origin+receiptDelay), clockHalfWide)

	result, dir := runAudioCaptureWith(t, audioRun{
		chunks:           [][]byte{pcmChunk(chunkBytes)},
		receipts:         scriptedReceipts(stamp),
		requestBootNanos: origin,
	})

	records := readAudioIndex(t, dir)
	if len(records) != 1 {
		t.Fatalf("got %d index records, want 1: %+v", len(records), records)
	}
	record := records[0]
	wantCanonical := receiptDelay - chunkNanos - int64(audioPipelineDelayEstimate)
	if wantCanonical <= 0 {
		t.Fatalf("this gap does reach past the origin; the test would prove nothing")
	}
	if record.CanonicalEpisodeNanos != wantCanonical {
		t.Errorf("canonical_episode_nanos = %d, want the uncorrected %d", record.CanonicalEpisodeNanos, wantCanonical)
	}
	if record.ClampedToOrigin || record.ClampShortfallNanos != 0 {
		t.Errorf("a stamp that lands after the origin was reported as clamped: %+v", record)
	}
	if record.CanonicalUncertaintyNanos != stamp.bracket()+int64(audioPipelineDelayBound) {
		t.Errorf("canonical_uncertainty_nanos = %d, want the unwidened %d",
			record.CanonicalUncertaintyNanos, stamp.bracket()+int64(audioPipelineDelayBound))
	}
	if strings.Contains(result.SourceDetail, "clamped") {
		t.Errorf("source detail = %q mentions a clamp that did not happen", result.SourceDetail)
	}
}
