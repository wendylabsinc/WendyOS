package services

import (
	"context"
	"errors"
	"testing"
	"time"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
)

// fakeLoopbackWriter stands in for the v4l2loopback node.
//
// It hands back sequences starting from a NON-ZERO base on purpose. The kernel's
// write_position is reset only when the device is created, not per open, so a
// pump attaching to a node that has been written before starts partway up the
// counter. A pump that quietly numbered frames itself would pass a test whose
// fake started at zero and then misattribute every frame in production, so the
// fake refuses to make that bug invisible.
type fakeLoopbackWriter struct {
	nextSeq uint32
	writes  [][]byte
	// stamps records the boottime the pump handed to each write, in the same
	// order as writes, so a test can assert the in-band identity the real writer
	// would put on the v4l2 buffer.
	stamps []int64
	// failOn holds write indices (0-based) that should return an error.
	failOn map[int]bool
	closed bool
}

func newFakeLoopbackWriter(base uint32) *fakeLoopbackWriter {
	return &fakeLoopbackWriter{nextSeq: base, failOn: map[int]bool{}}
}

func (f *fakeLoopbackWriter) WriteFrame(data []byte, boottimeNanos int64) (uint32, error) {
	if f.failOn[len(f.writes)] {
		f.writes = append(f.writes, nil)
		f.stamps = append(f.stamps, boottimeNanos)
		return 0, errors.New("simulated write failure")
	}
	f.writes = append(f.writes, append([]byte(nil), data...))
	f.stamps = append(f.stamps, boottimeNanos)
	seq := f.nextSeq
	// The kernel advances its counter only for a frame it actually accepted.
	f.nextSeq++
	return seq, nil
}

func (f *fakeLoopbackWriter) Close() error { f.closed = true; return nil }

// newPumpTestHub builds a deviceHub with one subscriber registered through the
// hub's own subscribe(), so the pump's drop accounting reads exactly the
// bookkeeping the real hub keeps rather than a hand-built approximation of it.
func newPumpTestHub(t *testing.T) (*deviceHub, int, chan *videoFrame) {
	t.Helper()
	hub, cancel := newTestHub(t)
	t.Cleanup(cancel)
	subID, frames, err := hub.subscribe()
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	return hub, subID, frames
}

// waitForBindings blocks until the pump has recorded n bindings.
//
// The drop tests need this because the hub's drop counter is read when the pump
// CONSUMES a frame, not when the hub queued it (this is the same accounting
// cameraSensorSubscription.Next does, deliberately). Staging all the frames and
// all the drops up front would therefore attribute drops to whichever frame the
// pump happened to reach first, which says nothing about the real behaviour.
// Interleaving the way the hub actually would is the only way these tests mean
// anything.
func waitForBindings(t *testing.T, pump *hubLoopbackPump, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pump.Bindings().Len() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d bindings; have %d", n, pump.Bindings().Len())
}

func bindableFrame(sampleID uint64, boot, unc int64) *videoFrame {
	return &videoFrame{
		data:                    []byte{0, 0, 0, 1, 0x65, byte(sampleID)},
		codec:                   agentpb.VideoCodec_VIDEO_CODEC_H264,
		auAligned:               true,
		sampleID:                sampleID,
		receiptBootNanos:        boot,
		receiptUncertaintyNanos: unc,
	}
}

// TestPumpBindsKernelSequenceNotArrivalOrder is the central assertion of the
// whole feature: the recorded sequence is the one the WRITER (standing in for
// the kernel) returned, not a number the pump derived from arrival order.
func TestPumpBindsKernelSequenceNotArrivalOrder(t *testing.T) {
	hub, subID, frames := newPumpTestHub(t)
	writer := newFakeLoopbackWriter(9000)
	pump := newHubLoopbackPump(zap.NewNop(), "v4l2:/dev/video0", "/dev/video200")

	frames <- bindableFrame(1, 111, 5)
	frames <- bindableFrame(2, 222, 5)
	close(frames)

	if err := pump.pump(context.Background(), hub, subID, frames, writer); err != nil {
		t.Fatalf("pump returned %v, want nil on producer stop", err)
	}

	for seq, wantSample := range map[uint32]uint64{9000: 1, 9001: 2} {
		got, ok := pump.Bindings().Lookup(seq)
		if !ok {
			t.Fatalf("Lookup(%d) not found; the pump did not use the kernel-assigned sequence", seq)
		}
		if got.SampleID != wantSample {
			t.Errorf("Lookup(%d).SampleID = %d, want %d", seq, got.SampleID, wantSample)
		}
	}
	// A pump that numbered frames itself would have recorded 0 and 1.
	if _, ok := pump.Bindings().Lookup(0); ok {
		t.Error("pump recorded a sequence of its own rather than the kernel's")
	}
}

// TestPumpCarriesCanonicalIdentity pins that the binding reproduces the hub's
// canonical time and uncertainty verbatim, so every plane reports the same
// numbers for the same sample.
func TestPumpCarriesCanonicalIdentity(t *testing.T) {
	hub, subID, frames := newPumpTestHub(t)
	writer := newFakeLoopbackWriter(1)
	pump := newHubLoopbackPump(zap.NewNop(), "v4l2:/dev/video0", "/dev/video200")

	frames <- bindableFrame(42, 1234567890123, 750)
	close(frames)
	if err := pump.pump(context.Background(), hub, subID, frames, writer); err != nil {
		t.Fatalf("pump returned %v", err)
	}

	got, ok := pump.Bindings().Lookup(1)
	if !ok {
		t.Fatal("binding not recorded")
	}
	if got.BootNanos != 1234567890123 || got.UncertaintyNanos != 750 {
		t.Errorf("binding carried boot=%d unc=%d, want 1234567890123/750",
			got.BootNanos, got.UncertaintyNanos)
	}
}

// TestPumpStampsWriterWithCanonicalBoottime pins the in-band half of the
// identity: the value the pump hands the writer, which the real writer puts in
// the v4l2 buffer's timestamp, is the frame's canonical CLOCK_BOOTTIME receipt
// and nothing else.
//
// It must be the SAME number the binding publishes. A pump that stamped a
// locally read clock instead would still produce a plausible-looking timestamp
// on the node, and a consumer cross-checking a sequence join against it would
// then reject frames that were correctly identified. The whole value of the
// in-band stamp is that it agrees with the control plane by construction.
func TestPumpStampsWriterWithCanonicalBoottime(t *testing.T) {
	hub, subID, frames := newPumpTestHub(t)
	writer := newFakeLoopbackWriter(7)
	pump := newHubLoopbackPump(zap.NewNop(), "v4l2:/dev/video0", "/dev/video200")

	const wantBoot int64 = 1234567890123456789
	frames <- bindableFrame(11, wantBoot, 250)
	close(frames)
	if err := pump.pump(context.Background(), hub, subID, frames, writer); err != nil {
		t.Fatalf("pump returned %v", err)
	}

	if len(writer.stamps) != 1 {
		t.Fatalf("writer saw %d stamps, want 1", len(writer.stamps))
	}
	if writer.stamps[0] != wantBoot {
		t.Fatalf("writer was stamped %d, want the frame's canonical receipt %d",
			writer.stamps[0], wantBoot)
	}

	// And the published identity carries the same number, so a consumer that
	// derives the expected buffer timestamp as boottime_nanos/1000 will match
	// what the node actually carries.
	binding, ok := pump.Bindings().Lookup(7)
	if !ok {
		t.Fatal("binding not recorded")
	}
	if binding.BootNanos != writer.stamps[0] {
		t.Fatalf("binding boot=%d but writer stamped %d; the two planes disagree",
			binding.BootNanos, writer.stamps[0])
	}
}

// TestPumpReportsHubDroppedFrames is drop case 1: the hub dropped frames for the
// pump, so those samples never reached the node.
//
// The loopback sequence stays DENSE across the loss, which is exactly why the
// data plane alone cannot report it and why the binding must carry the count.
// This is the dropped-frame case the design has to survive.
func TestPumpReportsHubDroppedFrames(t *testing.T) {
	hub, subID, frames := newPumpTestHub(t)
	writer := newFakeLoopbackWriter(500)
	pump := newHubLoopbackPump(zap.NewNop(), "v4l2:/dev/video0", "/dev/video200")

	// The hub delivered sample 1, then dropped three, then delivered sample 5.
	// The drops are recorded only once the pump has consumed sample 1, which is
	// how they would really accumulate: the hub fails to send while the pump is
	// busy with the previous frame.
	done := make(chan error, 1)
	go func() { done <- pump.pump(context.Background(), hub, subID, frames, writer) }()

	frames <- bindableFrame(1, 100, 1)
	waitForBindings(t, pump, 1)
	hub.mu.Lock()
	hub.subDrops[subID] = 3
	hub.mu.Unlock()
	frames <- bindableFrame(5, 200, 1)
	waitForBindings(t, pump, 2)
	close(frames)

	if err := <-done; err != nil {
		t.Fatalf("pump returned %v", err)
	}

	first, ok := pump.Bindings().Lookup(500)
	if !ok {
		t.Fatal("first binding missing")
	}
	second, ok := pump.Bindings().Lookup(501)
	if !ok {
		t.Fatal("second binding missing")
	}

	// The sequence is dense: the kernel counted only what we wrote.
	if second.LoopbackSequence-first.LoopbackSequence != 1 {
		t.Errorf("sequence gapped across a hub-side drop (%d -> %d); it must stay dense, "+
			"which is why the control plane has to report the loss",
			first.LoopbackSequence, second.LoopbackSequence)
	}
	// The loss is recoverable only from the recorded count.
	if second.HubDropsBefore != 3 {
		t.Errorf("HubDropsBefore = %d, want 3", second.HubDropsBefore)
	}
	// And it agrees with the sample_id jump, the corroborating signal.
	if gap := second.SampleID - first.SampleID - 1; gap != second.HubDropsBefore {
		t.Errorf("sample_id gap %d disagrees with HubDropsBefore %d", gap, second.HubDropsBefore)
	}
}

// TestPumpWriteFailureIsNotADroppedFrame pins that a failed write leaves the
// sample absent from both planes rather than appearing as a loss, and that the
// hub drops accumulated before it are still attributed to the next frame that
// actually reached the node.
func TestPumpWriteFailureIsNotADroppedFrame(t *testing.T) {
	hub, subID, frames := newPumpTestHub(t)
	writer := newFakeLoopbackWriter(10)
	writer.failOn[1] = true // the second write fails
	pump := newHubLoopbackPump(zap.NewNop(), "v4l2:/dev/video0", "/dev/video200")

	done := make(chan error, 1)
	go func() { done <- pump.pump(context.Background(), hub, subID, frames, writer) }()

	frames <- bindableFrame(1, 100, 1)
	waitForBindings(t, pump, 1)
	hub.mu.Lock()
	hub.subDrops[subID] = 2 // two samples lost after frame 1 was consumed
	hub.mu.Unlock()
	frames <- bindableFrame(4, 200, 1) // this write fails, so it records nothing
	frames <- bindableFrame(5, 300, 1)
	waitForBindings(t, pump, 2)
	close(frames)

	if err := <-done; err != nil {
		t.Fatalf("pump returned %v; a write failure must not stop the pump", err)
	}

	// The failed frame consumed no sequence, so the next frame got the next one.
	if _, ok := pump.Bindings().Lookup(11); !ok {
		t.Fatal("frame after a failed write did not get the next kernel sequence")
	}
	got, _ := pump.Bindings().Lookup(11)
	if got.SampleID != 5 {
		t.Errorf("sequence 11 resolved to sample %d, want 5", got.SampleID)
	}
	// Sample 4 was never written, so nothing may name it.
	if pump.Bindings().Len() != 2 {
		t.Errorf("recorded %d bindings, want 2: a failed write must record none", pump.Bindings().Len())
	}
	// The drops that accumulated before the failed write are still reported,
	// rather than being lost along with it.
	if got.HubDropsBefore != 2 {
		t.Errorf("HubDropsBefore = %d, want 2 (drops before a failed write must carry forward)", got.HubDropsBefore)
	}
}

// TestPumpRefusesUnbindableSource pins the fail-closed behaviour: a source whose
// frames cannot be identified stops the pump rather than publishing a node whose
// sequences name nothing.
func TestPumpRefusesUnbindableSource(t *testing.T) {
	hub, subID, frames := newPumpTestHub(t)
	writer := newFakeLoopbackWriter(1)
	pump := newHubLoopbackPump(zap.NewNop(), "ipcamera:200", "/dev/video200")

	// An IP camera producer's frame: an arbitrary chunk of a byte stream.
	frames <- &videoFrame{
		data:      []byte{1, 2, 3},
		codec:     agentpb.VideoCodec_VIDEO_CODEC_H264,
		auAligned: false,
		sampleID:  1,
	}

	err := pump.pump(context.Background(), hub, subID, frames, writer)
	if err == nil {
		t.Fatal("pump accepted an unbindable source; it must refuse rather than publish meaningless sequences")
	}
	if len(writer.writes) != 0 {
		t.Errorf("pump wrote %d frames for an unbindable source, want 0", len(writer.writes))
	}
	if pump.Bindings().Len() != 0 {
		t.Errorf("pump recorded %d bindings for an unbindable source, want 0", pump.Bindings().Len())
	}
}

// TestPumpStopsOnContextCancel pins clean shutdown.
func TestPumpStopsOnContextCancel(t *testing.T) {
	hub, subID, frames := newPumpTestHub(t)
	writer := newFakeLoopbackWriter(1)
	pump := newHubLoopbackPump(zap.NewNop(), "v4l2:/dev/video0", "/dev/video200")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pump.pump(ctx, hub, subID, frames, writer); !errors.Is(err, context.Canceled) {
		t.Errorf("pump returned %v, want context.Canceled", err)
	}
}

// TestPumpPropagatesProducerError pins that a producer failure surfaces rather
// than looking like a graceful stop, so a supervisor can back off and retry.
func TestPumpPropagatesProducerError(t *testing.T) {
	hub, subID, frames := newPumpTestHub(t)
	writer := newFakeLoopbackWriter(1)
	pump := newHubLoopbackPump(zap.NewNop(), "v4l2:/dev/video0", "/dev/video200")

	wantErr := errors.New("camera went away")
	hub.mu.Lock()
	hub.err = wantErr
	hub.mu.Unlock()
	close(frames)

	if err := pump.pump(context.Background(), hub, subID, frames, writer); !errors.Is(err, wantErr) {
		t.Errorf("pump returned %v, want %v", err, wantErr)
	}
}

// TestPumpWritesFramePayloadVerbatim pins that the bytes on the node are the
// bytes the hub broadcast. If the pump altered them, an app scoring the node
// would not be scoring the frame the episode recorded, which is the entire
// property the two-plane path exists to provide.
func TestPumpWritesFramePayloadVerbatim(t *testing.T) {
	hub, subID, frames := newPumpTestHub(t)
	writer := newFakeLoopbackWriter(1)
	pump := newHubLoopbackPump(zap.NewNop(), "v4l2:/dev/video0", "/dev/video200")

	frame := bindableFrame(7, 1, 1)
	want := append([]byte(nil), frame.data...)
	frames <- frame
	close(frames)

	if err := pump.pump(context.Background(), hub, subID, frames, writer); err != nil {
		t.Fatalf("pump returned %v", err)
	}
	if len(writer.writes) != 1 {
		t.Fatalf("wrote %d frames, want 1", len(writer.writes))
	}
	if string(writer.writes[0]) != string(want) {
		t.Errorf("wrote %v, want %v (payload must reach the node unaltered)", writer.writes[0], want)
	}
}

// TestPumpSignalsRejoinAfterCaptureTakeover: a hub torn down by an episode
// capture takeover must not end the data plane. The pump reports the restart
// so Run rejoins the replacement hub instead of stopping.
func TestPumpSignalsRejoinAfterCaptureTakeover(t *testing.T) {
	hub, subID, frames := newPumpTestHub(t)
	writer := newFakeLoopbackWriter(0)
	pump := newHubLoopbackPump(zap.NewNop(), "v4l2:/dev/video0", "/dev/video200")

	// Simulate takeOverDefaultedHub plus the old producer's teardown: mark the
	// hub restarted, then close the subscriber channel as runProducer does.
	hub.mu.Lock()
	hub.restarted = true
	hub.mu.Unlock()
	close(frames)

	err := pump.pump(context.Background(), hub, subID, frames, writer)
	if !errors.Is(err, errLoopbackHubRestarted) {
		t.Fatalf("pump returned %v, want errLoopbackHubRestarted", err)
	}
}

// TestPumpGatesRejoinedStreamToRandomAccess: after a rejoin the node's new
// stream must begin on a random-access unit, and the frames the gate skipped
// are reported on the next binding like hub-side drops, never lost silently.
func TestPumpGatesRejoinedStreamToRandomAccess(t *testing.T) {
	hub, subID, frames := newPumpTestHub(t)
	writer := newFakeLoopbackWriter(100)
	pump := newHubLoopbackPump(zap.NewNop(), "v4l2:/dev/video0", "/dev/video200")

	// A bare IDR without SPS/PPS is a whole access unit but not a
	// random-access one; the gate must skip it.
	frames <- bindableFrame(7, 700, 5)
	rau := &videoFrame{
		data:             append([]byte(nil), testRAUFrame...),
		codec:            agentpb.VideoCodec_VIDEO_CODEC_H264,
		auAligned:        true,
		sampleID:         8,
		receiptBootNanos: 800,
	}
	frames <- rau
	close(frames)

	if _, err := pump.pumpFrom(context.Background(), hub, subID, frames, writer, true, 0); err != nil {
		t.Fatalf("pumpFrom: %v", err)
	}
	if len(writer.writes) != 1 {
		t.Fatalf("wrote %d frames, want only the random-access unit", len(writer.writes))
	}
	binding, ok := pump.Bindings().Lookup(100)
	if !ok {
		t.Fatal("no binding recorded for the written frame")
	}
	if binding.SampleID != 8 {
		t.Fatalf("binding sample id = %d, want 8", binding.SampleID)
	}
	if binding.HubDropsBefore != 1 {
		t.Fatalf("HubDropsBefore = %d, want 1: the gated frame never reached the node and must be reported", binding.HubDropsBefore)
	}
}

// TestPumpRunRejoinsReplacementHub drives Run across a capture takeover: the
// pump must survive the restart as one more parameter-less subscriber, keep
// the same writer, and continue recording bindings from the replacement hub.
func TestPumpRunRejoinsReplacementHub(t *testing.T) {
	svc := newTestVideoService(nil, nil)
	fp := installFakeProducers(svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	writer := newFakeLoopbackWriter(400)
	restore := openLoopbackFrameWriter
	openLoopbackFrameWriter = func(string) (loopbackFrameWriter, error) { return writer, nil }
	defer func() { openLoopbackFrameWriter = restore }()

	pump := newHubLoopbackPump(zap.NewNop(), "v4l2:/dev/video0", "/dev/video200")
	runDone := make(chan error, 1)
	go func() {
		runDone <- pump.Run(ctx, svc, videoSource{kind: sourceV4L2, key: "/dev/video0", path: "/dev/video0"}, 0)
	}()

	// Wait for the pump's parameter-less join to create the defaulted hub.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && fp.count() == 0 {
		time.Sleep(time.Millisecond)
	}
	if fp.count() == 0 {
		t.Fatal("pump never created a hub")
	}
	fp.hub(0).broadcast(mustRAUVideoFrame(1))
	waitForBindings(t, pump, 1)

	// A campaign takes the camera over at explicit parameters.
	req := &agentpb.StreamVideoRequest{DeviceId: 0, Width: 640, Height: 480, Framerate: 15}
	newHub, captureID, _, _, _, _, restarted, err := svc.joinHubForCapture(ctx, "/dev/video0", req)
	if err != nil || !restarted {
		t.Fatalf("takeover failed: restarted=%v err=%v", restarted, err)
	}
	defer newHub.unsubscribe(captureID)

	// The pump rejoins the replacement hub and keeps binding frames. Keep
	// broadcasting until its binding lands: the rejoin is asynchronous.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && pump.Bindings().Len() < 2 {
		newHub.broadcast(mustRAUVideoFrame(0))
		time.Sleep(2 * time.Millisecond)
	}
	if pump.Bindings().Len() < 2 {
		t.Fatal("pump recorded no binding from the replacement hub")
	}

	cancel()
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
	if !writer.closed {
		t.Fatal("writer not closed on Run exit")
	}
}

// mustRAUVideoFrame builds an access-unit-aligned SPS/PPS/IDR frame the hub
// stamps with its own sample identity at broadcast.
func mustRAUVideoFrame(_ uint64) *videoFrame {
	return &videoFrame{
		data:      append([]byte(nil), testRAUFrame...),
		codec:     agentpb.VideoCodec_VIDEO_CODEC_H264,
		auAligned: true,
	}
}

// TestPumpReturnsUnreportedLossesOnRestart: when the hub is torn down by a
// capture takeover, this subscription will never write another binding, so
// everything still unreported (the carried count plus hub drops since the last
// written frame) must be handed back for the replacement subscription's first
// binding.
func TestPumpReturnsUnreportedLossesOnRestart(t *testing.T) {
	hub, subID, frames := newPumpTestHub(t)
	writer := newFakeLoopbackWriter(0)
	pump := newHubLoopbackPump(zap.NewNop(), "v4l2:/dev/video0", "/dev/video200")

	hub.mu.Lock()
	hub.subDrops[subID] = 3
	hub.restarted = true
	hub.mu.Unlock()
	close(frames)

	carried, err := pump.pumpFrom(context.Background(), hub, subID, frames, writer, false, 2)
	if !errors.Is(err, errLoopbackHubRestarted) {
		t.Fatalf("pumpFrom returned %v, want errLoopbackHubRestarted", err)
	}
	if carried != 5 {
		t.Fatalf("carried = %d, want 5 (2 carried in + 3 hub drops never reported)", carried)
	}
}

// TestPumpReportsCarriedLossesOnFirstBinding: losses inherited from a previous
// subscription ride on the first binding the new subscription writes.
func TestPumpReportsCarriedLossesOnFirstBinding(t *testing.T) {
	hub, subID, frames := newPumpTestHub(t)
	writer := newFakeLoopbackWriter(50)
	pump := newHubLoopbackPump(zap.NewNop(), "v4l2:/dev/video0", "/dev/video200")

	frames <- bindableFrame(9, 900, 5)
	close(frames)

	if _, err := pump.pumpFrom(context.Background(), hub, subID, frames, writer, false, 4); err != nil {
		t.Fatalf("pumpFrom: %v", err)
	}
	binding, ok := pump.Bindings().Lookup(50)
	if !ok {
		t.Fatal("no binding recorded")
	}
	if binding.HubDropsBefore != 4 {
		t.Fatalf("HubDropsBefore = %d, want the 4 carried losses", binding.HubDropsBefore)
	}
}
