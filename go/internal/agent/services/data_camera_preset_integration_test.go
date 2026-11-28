package services

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// h264ParameterSetsIn are the distinct sequence and picture parameter sets of
// an Annex B stream.
func h264ParameterSetsIn(stream []byte) (sps, pps map[string]bool) {
	sps, pps = map[string]bool{}, map[string]bool{}
	var starts, headers []int
	for start, header, ok := h264NextNAL(stream, 0); ok; start, header, ok = h264NextNAL(stream, header+1) {
		starts, headers = append(starts, start), append(headers, header)
	}
	for i, header := range headers {
		end := len(stream)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		switch stream[header] & 0x1f {
		case h264NALSPS:
			sps[string(stream[header:end])] = true
		case h264NALPPS:
			pps[string(stream[header:end])] = true
		}
	}
	return sps, pps
}

// checkVideoSegmentsDecode fails unless every video segment of a recording
// holds one encoder run, opens on a key frame, is tiled by its index records
// and decodes, record by record, to the picture of the sample each names. It
// returns the samples that have a video record.
func checkVideoSegmentsDecode(t *testing.T, r storeRecording, gstPath string, available map[string]bool, skip func(string)) (stored map[uint64]bool, segments []string) {
	t.Helper()
	stored = map[uint64]bool{}
	bySegment := map[string][]cameraIndexRecord{}
	for _, record := range r.recordsOf(t, indexedAsH264) {
		if stored[record.SampleID] {
			t.Errorf("sample %d has two H.264 records", record.SampleID)
		}
		stored[record.SampleID] = true
		segment := filepath.Base(record.Segment)
		if _, seen := bySegment[segment]; !seen {
			segments = append(segments, segment)
		}
		bySegment[segment] = append(bySegment[segment], record)
	}
	sort.Strings(segments)
	files := r.files(t)
	var decoded decodedGray
	for _, segment := range segments {
		records, raw := bySegment[segment], files[segment]
		sps, pps := h264ParameterSetsIn(raw)
		if len(sps) != 1 || len(pps) != 1 {
			t.Errorf("%s holds %d distinct SPS and %d distinct PPS, want one encoder run's", segment, len(sps), len(pps))
		}
		units := make([][]byte, len(records))
		var tiled []byte
		for i, record := range records {
			units[i] = r.cut(t, record)
			tiled = append(tiled, units[i]...)
		}
		if _, randomAccess := h264RandomAccessOffset(units[0]); !randomAccess || records[0].ByteOffset != 0 {
			t.Errorf("%s does not open on a key frame at offset 0", segment)
		}
		if !bytes.Equal(tiled, raw) {
			t.Errorf("%s: its index records do not tile the file (%d bytes indexed, %d in the file)", segment, len(tiled), len(raw))
		}
		pictures := decodeTestH264(t, gstPath, available, skip, units)
		if len(pictures) != len(units) {
			t.Errorf("%s: %d indexed access units decode to %d pictures", segment, len(units), len(pictures))
		}
		for i := 0; i < len(pictures) && i < len(records); i++ {
			if luma, shade := yuyvMeanLuma(pictures[i]), derivedTestShade(records[i].SampleID); !decoded.shows(luma, shade) {
				t.Errorf("%s record %d names sample %d but decodes to luma %d; that sample is gray %d (%s)",
					segment, i, records[i].SampleID, luma, shade, decoded.lumasOf(shade))
			}
		}
	}
	return stored, segments
}

// A store: both recording that states medium, through the real derived layer
// and real x264enc, while the camera keeps delivering: the encoder is replaced
// for the stated preset after the grace, for a faster one when a consumer
// stating it joins, and back when that one has left. Every camera frame that
// became no stored access unit meanwhile is counted as a drop, and every
// segment holds one encoder run and decodes to its own samples' pictures.
func TestRecordingAcrossPresetChangesOnRealGStreamerCountsEveryFrameItDidNotStore(t *testing.T) {
	gstPath, available, skip := derivedTestGStreamer(t)
	cam := newStoreTestCamera(t)
	readings := int64(0)
	cam.adapter.monotonicMapping = func() (data.MonotonicMappingSample, error) {
		readings++
		offset := int64(scriptedBootMinusMonotonic) + readings*int64(time.Microsecond)
		return data.MonotonicMappingSample{BootAfterNanos: -int64(time.Hour),
			OffsetLowerNanos: offset - scriptedMappingHalfWidth, OffsetUpperNanos: offset + scriptedMappingHalfWidth}, nil
	}
	const grace = 600 * time.Millisecond
	layer, err := newDerivedLayer(derivedLayerConfig{
		logger: zap.NewNop(), device: storeTestCameraPath, format: videoCodecMJPEG,
		gstPath: gstPath, available: available, width: derivedTestWidth, height: derivedTestHeight, framerate: 30,
		grace: grace, demand: cam.hub, publish: cam.hub.broadcastDerivedFrom,
	})
	if err != nil {
		skip("this GStreamer cannot run the derived layer: " + err.Error())
	}
	detach := cam.hub.feedDerivedLayer(layer)
	defer detach()
	layer.offerTo(cam.hub)
	layerCtx, stopLayer := context.WithCancel(context.Background())
	layerDone := make(chan error, 1)
	go func() { layerDone <- layer.run(layerCtx) }()
	defer func() {
		stopLayer()
		<-layerDone
	}()

	jpegs := derivedTestJPEGs(t)
	var fed []uint64
	feed := func() uint64 {
		sampleID := cam.hub.sampleSeq.Load() + 1
		_, receipt, _, err := data.CaptureReceipt()
		if err != nil {
			t.Fatal(err)
		}
		fed = append(fed, sampleID)
		cam.hub.produceCaptured(jpegs[sampleID%derivedTestShades], frameTimestamp{wallNs: uint64(time.Now().UnixNano()),
			nativeNs: receipt - int64(scriptedCaptureToReceipt) - int64(scriptedBootMinusMonotonic), nativeClock: kernelClock,
			sequence: uint32(sampleID), sequenceValid: true, auAligned: true, native: true}, videoCodecMJPEG)
		return sampleID
	}
	paced := func(n int, what string) {
		t.Helper()
		for i := 0; i < n; i++ {
			before := layer.counts().published
			sampleID := feed()
			deadline := time.Now().Add(20 * time.Second)
			for layer.counts().published == before {
				if time.Now().After(deadline) {
					t.Fatalf("%s: sample %d came back as no access unit (counts %+v)", what, sampleID, layer.counts())
				}
				time.Sleep(time.Millisecond)
			}
			cam.waitUntilTaken(t)
		}
	}
	// The camera keeps capturing at 30 frames per second while the encoder is
	// replaced.
	liveUntil := func(what string, happened func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !happened() {
			if time.Now().After(deadline) {
				t.Fatalf("%s never happened (counts %+v, the hub's encoder at %q)", what, layer.counts(), cam.hub.encoderPreset())
			}
			feed()
			time.Sleep(33 * time.Millisecond)
		}
		cam.waitUntilTaken(t)
	}
	runsAt := func(preset appconfig.EncodePreset, after uint64) func() bool {
		return func() bool {
			return layer.counts().childRuns > after && cam.hub.encoderPreset() == preset && layer.feeding.Load()
		}
	}

	r := cam.record(t, &data.SourceCapture{Store: data.CaptureStoreBoth, Preset: "medium"}, 3, func() {
		waitUntil(t, "the child takes frames", layer.feeding.Load)
		paced(10, "as the recording begins")
		liveUntil("the relaunch at medium after the grace", runsAt(appconfig.EncodePresetMedium, 0))
		paced(20, "at medium")
		runs := layer.counts().childRuns
		viewer, viewerFrames, err := cam.hub.subscribeAsKind("", encodedFrames, appconfig.EncodePresetSuperfast)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for range viewerFrames {
			}
		}()
		liveUntil("the relaunch at superfast", runsAt(appconfig.EncodePresetSuperfast, runs))
		paced(20, "at superfast")
		runs = layer.counts().childRuns
		cam.hub.unsubscribe(viewer)
		liveUntil("the relaunch back at medium", runsAt(appconfig.EncodePresetMedium, runs))
		paced(20, "back at medium")
	}, 1)

	stored, segments := checkVideoSegmentsDecode(t, r, gstPath, available, skip)
	var missing []uint64
	for _, sampleID := range fed {
		if !stored[sampleID] {
			missing = append(missing, sampleID)
		}
	}
	own := r.recordsOf(t, indexedAsJPEG)
	t.Logf("fed %d frames: %d JPEG records, %d H.264 records in %v, %d samples with no H.264 record %v, drops %d; detail: %s",
		len(fed), len(own), len(stored), segments, len(missing), missing, r.dropCount(), r.result.SourceDetail)
	if len(own) != len(fed) {
		t.Errorf("%d of the camera's %d own frames are stored", len(own), len(fed))
	}
	if len(segments) < 3 {
		t.Errorf("the video is in %v, want a segment for each of the encoder runs the recording saw", segments)
	}
	if got := r.dropCount(); got != int64(len(missing)) {
		t.Errorf("%d camera frames have no stored access unit %v, and the recording counts %d drop(s)", len(missing), missing, got)
	}
}

// The recording misses the first access unit of the encoder that replaced the
// one before it, as when the hub drops it for a slow subscriber. The units
// that follow decode from neither segment until the new run's next key frame:
// they are left out and counted, and what is stored decodes.
func TestRecordingThatMissesTheNewEncodersFirstUnitStoresOnlyWhatDecodesOnRealGStreamer(t *testing.T) {
	gstPath, available, skip := derivedTestGStreamer(t)
	h, cancel := newTestHub(t)
	defer cancel()
	h.sampleSeq = NewVideoService(context.Background(), zap.NewNop(), nil).sampleSeqLocked(storeTestCameraPath)
	layer, err := newDerivedLayer(derivedLayerConfig{
		logger: zap.NewNop(), device: storeTestCameraPath, format: videoCodecMJPEG,
		gstPath: gstPath, available: available, width: derivedTestWidth, height: derivedTestHeight, framerate: 30,
		grace: time.Minute, demand: h, publish: h.broadcastDerivedFrom,
	})
	if err != nil {
		skip("this GStreamer cannot run the derived layer: " + err.Error())
	}
	detach := h.feedDerivedLayer(layer)
	defer detach()
	layer.offerTo(h)
	_, units, err := h.subscribeAsKind("", encodedFrames, appconfig.EncodePresetMedium)
	if err != nil {
		t.Fatal(err)
	}
	var got []*videoFrame
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		for unit := range units {
			got = append(got, unit)
		}
	}()
	ctx, stop := context.WithCancel(context.Background())
	layerDone := make(chan error, 1)
	go func() { layerDone <- layer.run(ctx) }()
	jpegs := derivedTestJPEGs(t)
	paced := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			before := layer.counts().published
			sampleID := h.sampleSeq.Load() + 1
			h.produceCaptured(jpegs[sampleID%derivedTestShades], frameTimestamp{wallNs: uint64(time.Now().UnixNano()), nativeNs: int64(sampleID),
				nativeClock: kernelClock, sequence: uint32(sampleID), sequenceValid: true, auAligned: true, native: true}, videoCodecMJPEG)
			deadline := time.Now().Add(20 * time.Second)
			for layer.counts().published == before {
				if time.Now().After(deadline) {
					t.Fatalf("sample %d came back as no access unit", sampleID)
				}
				time.Sleep(time.Millisecond)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	const beforeChange, afterChange = 6, 24
	waitUntil(t, "the child takes frames", layer.feeding.Load)
	paced(beforeChange)
	runs := layer.counts().childRuns
	faster, fasterFrames, err := h.subscribeAsKind("", encodedFrames, appconfig.EncodePresetUltrafast)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for range fasterFrames {
		}
	}()
	waitUntil(t, "the relaunch at ultrafast", func() bool { return layer.counts().childRuns > runs && layer.feeding.Load() })
	paced(afterChange)
	h.unsubscribe(faster)
	stop()
	<-layerDone
	h.mu.Lock()
	for _, sub := range h.subs {
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
	}
	h.mu.Unlock()
	<-collected
	if len(got) != beforeChange+afterChange {
		t.Fatalf("%d access units came back, want %d", len(got), beforeChange+afterChange)
	}
	if _, startsHere := h264RandomAccessOffset(got[beforeChange].data); !startsHere || got[beforeChange].encodedAt != appconfig.EncodePresetUltrafast {
		t.Fatalf("unit %d is not the key frame that opens the run at ultrafast (encoded at %q)", beforeChange, got[beforeChange].encodedAt)
	}

	c := newStoreTestCapture(t, &fakeReceipt{}, &data.SourceCapture{Store: data.CaptureStoreVideo, Preset: "medium"})
	for i, unit := range got {
		if i == beforeChange {
			continue
		}
		if err := c.handleFrame(encodedAt(unit.encodedAt, unit.sampleID, time.Duration(i+1)*33*time.Millisecond, unit.data)); err != nil && !errors.Is(err, errAwaitCameraRandomAccess) {
			t.Fatal(err)
		}
	}
	stored, segments := checkVideoSegmentsDecode(t, storeRecording{dir: c.dir, result: c.result}, gstPath, available, skip)
	t.Logf("%d of %d access units stored in %v, drops %d", len(stored), len(got), segments, storedDrops(c))
	if len(segments) != 2 {
		t.Errorf("the video is in %v, want the run at medium and, from its next key frame, the run at ultrafast", segments)
	}
	if left := uint64(len(got) - len(stored)); storedDrops(c) != left || left < 2 {
		t.Errorf("%d access units are not stored, and the recording counts %d drop(s); want the missed unit and those left out before the next key frame", left, storedDrops(c))
	}
}
