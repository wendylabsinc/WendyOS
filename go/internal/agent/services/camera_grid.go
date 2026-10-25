package services

// Camera grids: the agent's own measurement streams of its cameras.
//
// A thermal camera's temperatures used to reach an app only as raw frames,
// which need `admin`, and a SensorLink camera's pixels only through its node,
// which serves one reader at a time and so locks the agent's own viewers out.
// The agent now decodes both itself and publishes standard wendy.data.v1
// streams that viewers, and apps with `data-read` and `camera`, read like any
// other (cameraScope):
//
//	agent/camera/grid/<source>     wendy.data.v1.Grid          a TOPDON TC001's
//	                                                           temperatures, or a
//	                                                           JPEG camera's
//	                                                           brightness
//	agent/camera/markers/<source>  wendy.data.v1.VideoMarkers  a TC001's hottest
//	                                                           and coldest pixel
//
// <source> is the camera's source id, such as "v4l2:/dev/video0", as `wendy
// data sources` and campaign markers name it. It comes last because it
// contains "/", as in campaign markers' stream ids.
//
// Decoding costs CPU, and a thermal grid needs the camera running with a raw
// subscriber, so a decoder runs only while a viewer asks for one of its
// camera's streams by id, prefix or schema (live.Hub.Wanted). A viewer that
// takes every stream gets grids while someone else's request runs the
// decoder, but does not start one. The streams are in the live catalog, idle,
// for as long as the camera is present, so a viewer finds them without
// starting anything.
//
// Frames come from the agent's own camera pipeline, never from the device
// node. A thermal camera's frames are the camera hub's raw tap
// (video_raw_tap.go), as a raw StreamVideo subscriber gets them; a SensorLink
// camera's are the JPEG frames of the in-process feed its hub encodes
// (sensorlink_camera_feed.go). Joining the hub asserts no stream parameters,
// so it never takes the camera from a viewer, a recording or an app.
//
// The streams are live only (Manager.PublishLive). What an episode holds must
// not depend on whether someone was watching, and a 256x192 grid at 25 Hz is
// 2.4 MB/s, which would push every app's records out of the shared stream
// pre-roll within seconds.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"github.com/wendylabsinc/wendy/go/proto/gen/datapb"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
)

// gridDescriptor describes a camera grid stream.
var gridDescriptor = &recordingpb.StreamDescriptor{MediaType: "application/protobuf", Schema: string(proto.MessageName(&datapb.Grid{}))}

// cameraGridStream and cameraMarkersStream are a camera's streams, below
// "agent/".
func cameraGridStream(sourceID string) string    { return "camera/grid/" + sourceID }
func cameraMarkersStream(sourceID string) string { return "camera/markers/" + sourceID }

// cameraStreamsPrefix is where the agent's camera streams live.
const cameraStreamsPrefix = "agent/camera/"

// cameraScope is which of the agent's camera streams an app may read. They
// are what its cameras see: a TC001's grid is its whole thermal picture, a
// SensorLink camera's a greyscale one. So data-read admits them only with the
// camera entitlement too, and with that entitlement's allowlist only for the
// cameras it names. Without them an app neither reads a camera stream nor
// makes one wanted, so it cannot start a camera. Operators have no scope.
type cameraScope struct {
	granted bool
	// devices are the device paths, such as "/dev/video0", whose streams the
	// app may read, sorted. Empty with granted admits every camera.
	devices []string
}

// admits reports whether the scope lets an app read streamID, a stream id and
// never a catalog id. Every stream below cameraStreamsPrefix needs the camera
// entitlement, so one added there later is not readable by default.
func (c cameraScope) admits(streamID string) bool {
	rest, ok := strings.CutPrefix(streamID, cameraStreamsPrefix)
	if !ok {
		return true
	}
	if !c.granted {
		return false
	}
	if len(c.devices) == 0 {
		return true
	}
	for _, kind := range []string{"grid/", "markers/"} {
		if source, ok := strings.CutPrefix(rest, kind); ok {
			path, ok := strings.CutPrefix(source, "v4l2:")
			return ok && slices.Contains(c.devices, path)
		}
	}
	return false
}

// covers reports whether c admits every camera stream within admits.
func (c cameraScope) covers(within cameraScope) bool {
	switch {
	case !within.granted:
		return true
	case !c.granted:
		return false
	case len(c.devices) == 0:
		return true
	case len(within.devices) == 0:
		return false
	}
	for _, device := range within.devices {
		if !slices.Contains(c.devices, device) {
			return false
		}
	}
	return true
}

// readable is the scope as a live.Selection.Readable, nil when it admits
// every stream.
func (c cameraScope) readable() func(string) bool {
	if c.granted && len(c.devices) == 0 {
		return nil
	}
	return c.admits
}

const (
	// cameraGridScanEvery is how often the cameras are listed again, and how
	// soon a failed decoder may be retried after its back-off.
	cameraGridScanEvery = 5 * time.Second
	// cameraGridRetryAfter is how long a decoder that failed waits before it
	// tries again, for example while a viewer holds the thermal camera in a
	// mode without temperatures.
	cameraGridRetryAfter = 10 * time.Second
)

// cameraGridKind is the decoder a camera gets.
type cameraGridKind int

const (
	gridNone cameraGridKind = iota
	gridThermal
	gridLuma
)

// gridNodeIdentity names the device behind a node, so a camera is probed once
// rather than on every scan, and again when another device takes its number.
// A seam for tests, whose nodes do not exist.
var gridNodeIdentity = func(path string) (string, bool) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFCHR {
		return "", false
	}
	return fmt.Sprintf("%d:%d:%d.%d", st.Rdev, st.Ino, st.Ctim.Sec, st.Ctim.Nsec), true
}

// advertisesTC001StackedMode reports whether a camera offers the TC001's
// measured stacked mode: 512x484 YUYV, with a 512x384 picture mode 100 rows
// shorter. A read-only enumeration, which answers while the camera streams.
func advertisesTC001StackedMode(path string) bool {
	return deviceSupportsRawSize(path, v4l2PixFmtYUYV, tc001FrameWidth, tc001FrameHeight) &&
		stackedMetadataRows(path, tc001FrameWidth, tc001FrameHeight) == tc001PictureTop
}

// gridNode is a device node and the decoder it gets.
type gridNode struct {
	path     string
	identity string
	kind     cameraGridKind
}

// gridCamera is a camera with a decoder, and the decoder's job while it runs.
// Only the publisher's loop goroutine touches it.
type gridCamera struct {
	sourceID string
	gridNode
	job     *gridJob
	retryAt time.Time
	// failure is the last job failure logged, so a decoder that keeps failing
	// the same way is logged once.
	failure string
}

type gridJob struct {
	cancel context.CancelFunc
	done   chan struct{}
	// err and published are written before done closes.
	err       error
	published bool
}

// cameraGrids publishes the camera grid streams.
type cameraGrids struct {
	manager *data.Manager
	video   *VideoService
	// wake is signalled when a job ends.
	wake chan struct{}
	// scanEvery and retryAfter are cameraGridScanEvery and
	// cameraGridRetryAfter; tests shorten them.
	scanEvery  time.Duration
	retryAfter time.Duration
	// Owned by the loop goroutine: cameras by source id, and the probe of
	// every node seen, by path.
	cameras map[string]*gridCamera
	probed  map[string]gridNode
}

// StartCameraGrids publishes the agent's camera grid streams (camera_grid.go)
// until the returned function is called. It reads the video service's
// SensorLink feed, so it starts after the feed is set.
func (s *DataService) StartCameraGrids(ctx context.Context, video *VideoService) func() {
	if video == nil {
		return func() {}
	}
	g := newCameraGrids(s.manager, video)
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.run(ctx)
	}()
	return func() { cancel(); <-done }
}

func newCameraGrids(manager *data.Manager, video *VideoService) *cameraGrids {
	return &cameraGrids{
		manager:    manager,
		video:      video,
		wake:       make(chan struct{}, 1),
		scanEvery:  cameraGridScanEvery,
		retryAfter: cameraGridRetryAfter,
		cameras:    make(map[string]*gridCamera),
		probed:     make(map[string]gridNode),
	}
}

func (g *cameraGrids) run(ctx context.Context) {
	defer g.stopAll()
	ticker := time.NewTicker(g.scanEvery)
	defer ticker.Stop()
	g.scan()
	for {
		// Taken before reconciling, so a viewer that arrives meanwhile is not
		// missed.
		changed := g.manager.Live().SubscribersChanged()
		g.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.scan()
		case <-changed:
		case <-g.wake:
		}
	}
}

// scan lists the cameras that have a decoder, advertises the streams of new
// ones, and stops and unadvertises those that are gone.
func (g *cameraGrids) scan() {
	present, ok := g.discover()
	if !ok {
		return // keep what is listed until the nodes can be listed again
	}
	for id, cam := range g.cameras {
		if node, ok := present[id]; !ok || node != cam.gridNode {
			g.stop(cam)
			for _, stream := range cam.streams() {
				g.manager.UnadvertiseAgentStream(stream.name)
			}
			delete(g.cameras, id)
		}
	}
	for id, node := range present {
		if g.cameras[id] != nil {
			continue
		}
		cam := &gridCamera{sourceID: id, gridNode: node}
		g.cameras[id] = cam
		for _, stream := range cam.streams() {
			g.manager.AdvertiseAgentStream(stream.name, stream.descriptor)
		}
	}
}

// discover returns the cameras a decoder applies to, by source id: a SensorLink
// camera, whose JPEG frames the agent receives in-process, and a local camera
// that offers the TC001's stacked mode. Network and ROS 2 cameras have no
// decoder.
func (g *cameraGrids) discover() (map[string]gridNode, bool) {
	paths, err := g.video.globDevices()
	if err != nil {
		return nil, false
	}
	present := make(map[string]gridNode)
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		n, err := strconv.ParseUint(strings.TrimPrefix(path, "/dev/video"), 10, 32)
		if err != nil || n > maxVideoDeviceID || n >= ros2camera.IDBandStart && n <= ros2camera.IDBandEnd || n >= ipcam.IDBandStart && n <= ipcam.IDBandEnd {
			continue
		}
		identity, ok := gridNodeIdentity(path)
		if !ok {
			continue
		}
		seen[path] = true
		node, ok := g.probed[path]
		if !ok || node.identity != identity {
			node = gridNode{path: path, identity: identity, kind: g.probe(path)}
			g.probed[path] = node
		}
		if node.kind != gridNone {
			present["v4l2:"+path] = node
		}
	}
	for path := range g.probed {
		if !seen[path] {
			delete(g.probed, path)
		}
	}
	return present, true
}

func (g *cameraGrids) probe(path string) cameraGridKind {
	if isSensorLinkCameraPath(path) {
		if g.video.sensorFrames != nil {
			return gridLuma
		}
		return gridNone
	}
	if advertisesTC001StackedMode(path) {
		return gridThermal
	}
	return gridNone
}

type gridStream struct {
	name       string
	descriptor *recordingpb.StreamDescriptor
}

// streams are the camera's streams, below "agent/".
func (c *gridCamera) streams() []gridStream {
	streams := []gridStream{{cameraGridStream(c.sourceID), gridDescriptor}}
	if c.kind == gridThermal {
		streams = append(streams, gridStream{cameraMarkersStream(c.sourceID), markersDescriptor})
	}
	return streams
}

func (g *cameraGrids) wanted(cam *gridCamera) bool {
	for _, stream := range cam.streams() {
		if g.manager.AgentStreamWanted(stream.name, stream.descriptor) {
			return true
		}
	}
	return false
}

// reconcile starts the decoder of every camera a viewer asks for, unless it
// failed a moment ago, and stops the others.
func (g *cameraGrids) reconcile(ctx context.Context) {
	now := time.Now()
	for _, cam := range g.cameras {
		g.reap(cam, now)
		wanted := g.wanted(cam)
		switch {
		case wanted && cam.job == nil && !now.Before(cam.retryAt):
			g.start(ctx, cam)
		case !wanted && cam.job != nil:
			g.stop(cam)
		}
	}
}

func (g *cameraGrids) start(ctx context.Context, cam *gridCamera) {
	ctx, cancel := context.WithCancel(ctx)
	job := &gridJob{cancel: cancel, done: make(chan struct{})}
	cam.job = job
	run := g.runThermal
	if cam.kind == gridLuma {
		run = g.runLuma
	}
	sourceID, path := cam.sourceID, cam.path
	go func() {
		job.err = run(ctx, sourceID, path, job)
		cancel()
		close(job.done)
		// After done, so the loop that wakes finds the job ended.
		select {
		case g.wake <- struct{}{}:
		default:
		}
	}()
}

// reap collects a job that ended by itself. Its failure is logged once and
// holds the decoder back for retryAfter.
func (g *cameraGrids) reap(cam *gridCamera, now time.Time) {
	job := cam.job
	if job == nil {
		return
	}
	select {
	case <-job.done:
	default:
		return
	}
	cam.job = nil
	if job.published {
		cam.failure = ""
	}
	if job.err == nil || errors.Is(job.err, context.Canceled) {
		return
	}
	cam.retryAt = now.Add(g.retryAfter)
	if msg := job.err.Error(); msg != cam.failure {
		cam.failure = msg
		g.manager.Warnf("camera grid for %s stopped, retrying while a viewer asks for it: %v", cam.sourceID, job.err)
	}
}

func (g *cameraGrids) stop(cam *gridCamera) {
	if cam.job == nil {
		return
	}
	cam.job.cancel()
	<-cam.job.done
	cam.job = nil
}

func (g *cameraGrids) stopAll() {
	for id, cam := range g.cameras {
		g.stop(cam)
		for _, stream := range cam.streams() {
			g.manager.UnadvertiseAgentStream(stream.name)
		}
		delete(g.cameras, id)
	}
}

// publish puts one payload of a camera frame on an agent stream, live only.
// The record names the camera as its source and is stamped with the frame's
// time.
func (g *cameraGrids) publish(stream string, descriptor *recordingpb.StreamDescriptor, sourceID string, unixNanos int64, payload proto.Message, job *gridJob) error {
	bytes, err := proto.Marshal(payload)
	if err != nil {
		return err
	}
	record := &recordingpb.Record{
		Payload: bytes,
		Timing:  &recordingpb.SampleTiming{Clock: "CLOCK_REALTIME", Count: 1, TimestampsNanos: []int64{unixNanos}},
		Inputs:  []*recordingpb.SampleRef{{SourceId: sourceID}},
	}
	if err := g.manager.PublishLive(stream, descriptor, record); err != nil {
		return err
	}
	job.published = true
	return nil
}

// runThermal publishes a TC001's grid and hot and cold markers from its raw
// frames while a viewer asks for either. The records carry each raw frame's
// timestamp_ns, the time a raw StreamVideo subscriber sees on it.
func (g *cameraGrids) runThermal(ctx context.Context, sourceID, _ string, job *gridJob) error {
	sub, err := g.video.subscribeRawFrames(ctx, sourceID)
	if err != nil {
		return err
	}
	defer sub.Close()
	grid, markers := cameraGridStream(sourceID), cameraMarkersStream(sourceID)
	camera := markersCamera(sourceID)
	plausible := true
	for {
		frame, err := sub.Next(ctx)
		if err != nil {
			return err
		}
		if !tc001Layout(frame.format, frame.pictureTop) {
			return fmt.Errorf("camera captures %s with its picture from raw row %d; temperatures need the TC001's stacked 512x484 YUYV mode with its picture from row %d",
				describeRawFormat(frame.format), frame.pictureTop, tc001PictureTop)
		}
		wantGrid := g.manager.AgentStreamWanted(grid, gridDescriptor)
		wantMarkers := g.manager.AgentStreamWanted(markers, markersDescriptor)
		if !wantGrid && !wantMarkers {
			continue // the loop stops this job
		}
		st, ok := computeThermalStats(frame.data, frame.format, frame.pictureTop)
		if ok != plausible {
			plausible = ok
			if !ok {
				g.manager.Warnf("thermal camera %s: mean %.1f °C is outside %.0f..%.0f °C, the wrong scale or not a radiometric frame; publishing no temperatures until it is",
					sourceID, st.meanCelsius, float64(thermalMinMeanCelsius), float64(thermalMaxMeanCelsius))
			}
		}
		if ok && wantGrid {
			if err := g.publish(grid, gridDescriptor, sourceID, frame.unixNanos, thermalGrid(frame.data, camera), job); err != nil {
				return err
			}
		}
		if wantMarkers {
			if err := g.publish(markers, markersDescriptor, sourceID, frame.unixNanos, thermalMarkers(st, ok, camera), job); err != nil {
				return err
			}
		}
	}
}

// runLuma publishes a SensorLink camera's brightness grid from its JPEG frames
// while a viewer asks for it. A frame has no capture time, so the record
// carries the time the agent received it.
func (g *cameraGrids) runLuma(ctx context.Context, sourceID, path string, job *gridJob) error {
	frames, unsubscribe, ok := g.video.sensorLinkFrames(path)
	if !ok {
		return errors.New("the agent does not receive this camera's frames")
	}
	defer unsubscribe()
	grid := cameraGridStream(sourceID)
	camera := markersCamera(sourceID)
	var failure string
	for {
		frame, err := nextSensorFrame(ctx, frames)
		if err != nil {
			return err
		}
		frame = newestSensorFrame(frames, frame)
		received := time.Now().UnixNano()
		if frame.Codec != ros2camera.CodecMJPEG {
			return errSensorFeedNotJPEG
		}
		if !g.manager.AgentStreamWanted(grid, gridDescriptor) {
			continue // the loop stops this job
		}
		decoded, err := lumaGrid(frame.Data, camera)
		if err != nil {
			if msg := err.Error(); msg != failure {
				failure = msg
				g.manager.Warnf("camera %s: no brightness grid for a frame: %v", sourceID, err)
			}
			continue
		}
		failure = ""
		if err := g.publish(grid, gridDescriptor, sourceID, received, decoded, job); err != nil {
			return err
		}
	}
}

// newestSensorFrame skips frames that queued while the previous one decoded,
// so a slow decode shows what the camera sees now.
func newestSensorFrame(frames <-chan ros2camera.Frame, frame ros2camera.Frame) ros2camera.Frame {
	for {
		select {
		case next := <-frames:
			frame = next
		default:
			return frame
		}
	}
}

// sensorLinkFrames subscribes to a SensorLink camera's JPEG frames as the
// agent receives them, the frames its hub encodes, without opening the node.
func (s *VideoService) sensorLinkFrames(path string) (<-chan ros2camera.Frame, func(), bool) {
	if s.sensorFrames == nil || !isSensorLinkCameraPath(path) {
		return nil, nil, false
	}
	frames, unsubscribe := s.sensorFrames.subscribe(path)
	return frames, unsubscribe, true
}

// rawCaptureFrame is one uncompressed capture frame from a camera's raw tap.
type rawCaptureFrame struct {
	// data is shared with the hub's other raw subscribers and never changes.
	data []byte
	// unixNanos is the frame's VideoFrame.timestamp_ns for raw subscribers.
	unixNanos int64
	format    *agentpb.RawFormat
	// pictureTop is the first raw row the camera's encoded viewers see.
	pictureTop uint32
}

// rawFrameSubscription is one raw subscriber on a camera's hub, as a raw
// StreamVideo client is. It asserts no stream parameters, so it joins
// whatever the camera is doing and never takes it from anyone.
type rawFrameSubscription struct {
	video  *VideoService
	key    string
	devID  uint32
	hub    *deviceHub
	id     int
	frames chan *videoFrame
	closed bool
}

// subscribeRawFrames joins a local camera's hub for its raw frames, starting
// the camera if nothing else runs it. A camera whose producer has no raw
// frames to give refuses, with the reason, as it refuses a raw viewer.
func (s *VideoService) subscribeRawFrames(ctx context.Context, sourceID string) (*rawFrameSubscription, error) {
	src, err := s.resolveSensorSource(sourceID)
	if err != nil {
		return nil, err
	}
	if src.kind != sourceV4L2 {
		return nil, errRawUnavailable("only a local camera has raw frames")
	}
	devID, _ := cameraDeviceID(sourceID)
	r := &rawFrameSubscription{video: s, key: src.key, devID: devID}
	if err := r.join(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rawFrameSubscription) join(ctx context.Context) error {
	hub, id, frames, err := r.video.joinHub(ctx, r.key, &agentpb.StreamVideoRequest{DeviceId: r.devID, Codec: agentpb.VideoCodec_VIDEO_CODEC_RAW})
	if err != nil {
		return err
	}
	r.hub, r.id, r.frames = hub, id, frames
	return nil
}

// Next returns the next raw frame. After an episode capture restarts the
// camera at its campaign's parameters, it joins the restarted producer, which
// may or may not offer the same frames.
func (r *rawFrameSubscription) Next(ctx context.Context) (rawCaptureFrame, error) {
	for {
		select {
		case <-ctx.Done():
			return rawCaptureFrame{}, ctx.Err()
		case frame, ok := <-r.frames:
			if !ok {
				if err := r.hub.subscriberErr(r.id); err != nil {
					return rawCaptureFrame{}, err
				}
				if err := r.hub.terminalErr(); err != nil {
					return rawCaptureFrame{}, err
				}
				if r.hub.wasRestarted() {
					r.hub.unsubscribe(r.id)
					if err := r.join(ctx); err != nil {
						r.closed = true
						return rawCaptureFrame{}, err
					}
					continue
				}
				return rawCaptureFrame{}, errSensorProducerStopped
			}
			return rawCaptureFrame{data: frame.data, unixNanos: int64(frame.tsNs), format: frame.rawFmt, pictureTop: r.hub.rawPicture()}, nil
		}
	}
}

func (r *rawFrameSubscription) Close() {
	if r.closed {
		return
	}
	r.closed = true
	r.hub.unsubscribe(r.id)
}
