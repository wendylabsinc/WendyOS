package services

// The encoded live view of a USB thermal camera (the TOPDON TC001 Max).
//
// The camera has no /dev/videoN, so GStreamer cannot open it. Its driver
// (go/internal/agent/usbthermal) hands out the picture half of every thermal
// frame instead, and the hub can encode frames that come from inside the agent
// (video_inprocess.go). This file connects the two:
//
//   - every attached camera is registered as an in-process camera under the
//     device id the driver gives it, so StreamVideo's encoded (default) path
//     serves that id from a rawvideo.Feed. It is registered when it is plugged
//     in and unregistered when it is unplugged; a viewer still watching then
//     gets "camera was disconnected".
//   - the camera streams only while someone watches. A bridge goroutine per
//     camera subscribes to the driver's pictures while the feed has a
//     subscriber and drops the subscription when it has none, so the driver's
//     own rule holds: the last subscriber leaving puts the camera back to idle.
//   - raw requests are not this path's. The camera is registered with its raw
//     frames marked as served elsewhere, so a VIDEO_CODEC_RAW request for its id
//     passes the in-process check and reaches the raw tap
//     (video_usbthermal.go), exactly as before the live view existed.

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/rawvideo"
	"github.com/wendylabsinc/wendy/go/internal/agent/usbthermal"
)

// Vars rather than constants only so tests can shorten them.
var (
	// usbThermalPlugPoll is how often the attached cameras are re-listed to
	// register a camera that was plugged in and unregister one that was
	// unplugged. Listing reads sysfs only and never touches a camera. A
	// StreamVideo call for an id in the band also re-lists first, so a viewer
	// never waits for this.
	usbThermalPlugPoll = 2 * time.Second
	// usbThermalWatchPoll is how often a camera's bridge checks whether its
	// feed has a viewer: the delay before an idle camera starts (its start
	// sequence alone takes about four seconds), and before a watched one is
	// put back to idle after its last viewer leaves.
	usbThermalWatchPoll = 250 * time.Millisecond
	// usbThermalSubscribeRetry spaces attempts to subscribe to a camera whose
	// subscription was refused while a viewer waits.
	usbThermalSubscribeRetry = time.Second
)

// usbThermalPictureSource is what the live view needs from the driver: the
// attached cameras, and a camera's pictures. *usbthermal.Manager provides
// both, through usbThermalDriver.
type usbThermalPictureSource interface {
	List() []usbthermal.Listing
	SubscribePictures(ctx context.Context, stableID string) (<-chan usbthermal.Frame, error)
}

// inProcessRegistrar registers an in-process camera, as
// VideoService.registerInProcessCamera does with rawElsewhere set.
type inProcessRegistrar func(id uint32, feed *rawvideo.Feed) (unregister func(), err error)

// usbThermalLiveView keeps one in-process camera registration per attached USB
// thermal camera.
type usbThermalLiveView struct {
	ctx      context.Context
	cancel   context.CancelFunc
	logger   *zap.Logger
	src      usbThermalPictureSource
	register inProcessRegistrar

	mu      sync.Mutex
	cams    map[string]*usbThermalLiveCamera // by stable id
	refused map[string]string                // stable id -> registration error already logged
	stopped bool
	wg      sync.WaitGroup // the poll loop and every bridge
}

// usbThermalLiveCamera is one registered camera.
type usbThermalLiveCamera struct {
	id         uint32
	feed       *rawvideo.Feed
	unregister func()
	stop       context.CancelFunc // ends the bridge
}

func newUSBThermalLiveView(ctx context.Context, logger *zap.Logger, src usbThermalPictureSource, register inProcessRegistrar) *usbThermalLiveView {
	vctx, cancel := context.WithCancel(ctx)
	if logger == nil {
		logger = zap.NewNop()
	}
	return &usbThermalLiveView{
		ctx:      vctx,
		cancel:   cancel,
		logger:   logger,
		src:      src,
		register: register,
		cams:     map[string]*usbThermalLiveCamera{},
		refused:  map[string]string{},
	}
}

// startUSBThermalLiveView wires the service's USB thermal driver into its
// in-process cameras and keeps the registrations in step with the bus. Nil
// when the service has no driver.
func (s *VideoService) startUSBThermalLiveView() *usbThermalLiveView {
	if s.usbThermal == nil {
		return nil
	}
	v := newUSBThermalLiveView(s.ctx, s.logger, s.usbThermal, func(id uint32, feed *rawvideo.Feed) (func(), error) {
		return s.registerInProcessCamera(id, feed, true)
	})
	v.run(usbThermalPlugPoll)
	return v
}

// syncUSBThermalLiveView brings the registrations up to date before a stream
// for devID is resolved, when devID is in the USB thermal band: a camera
// plugged in a moment ago is served now rather than at the next plug poll, and
// one unplugged a moment ago is not.
func (s *VideoService) syncUSBThermalLiveView(devID uint32) {
	if s.usbThermalLive == nil || devID < usbthermal.IDBandStart || devID > usbthermal.IDBandEnd {
		return
	}
	s.usbThermalLive.sync()
}

// run syncs now and then every interval until the live view stops.
func (v *usbThermalLiveView) run(interval time.Duration) {
	v.sync()
	v.wg.Add(1)
	go func() {
		defer v.wg.Done()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-v.ctx.Done():
				return
			case <-t.C:
				v.sync()
			}
		}
	}()
}

// sync registers every attached camera that is not registered yet and
// unregisters every registered camera that is no longer attached.
func (v *usbThermalLiveView) sync() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stopped {
		return
	}
	listed := v.src.List()
	attached := make(map[string]bool, len(listed))
	for _, l := range listed {
		attached[l.StableID] = true
		if cam, ok := v.cams[l.StableID]; ok {
			if cam.id == l.ID {
				continue
			}
			// The driver keeps a camera's id for the agent's lifetime, so this
			// is defensive: follow the id rather than serve a stale one.
			v.dropLocked(l.StableID, "its device id changed")
		}
		v.addLocked(l)
	}
	for sid := range v.cams {
		if !attached[sid] {
			v.dropLocked(sid, "unplugged")
		}
	}
}

// addLocked registers one camera and starts its bridge.
func (v *usbThermalLiveView) addLocked(l usbthermal.Listing) {
	feed := rawvideo.NewFeed()
	unregister, err := v.register(l.ID, feed)
	if err != nil {
		// Logged once per cause, not every poll. Raw frames still work; an
		// encoded request is refused by streamUSBThermal with a reason.
		if v.refused[l.StableID] != err.Error() {
			v.refused[l.StableID] = err.Error()
			v.logger.Warn("USB thermal camera has no encoded live view",
				zap.String("camera", l.StableID), zap.Uint32("id", l.ID), zap.Error(err))
		}
		return
	}
	delete(v.refused, l.StableID)
	ctx, stop := context.WithCancel(v.ctx)
	v.cams[l.StableID] = &usbThermalLiveCamera{id: l.ID, feed: feed, unregister: unregister, stop: stop}
	v.wg.Add(1)
	go v.bridge(ctx, l.StableID, feed)
	v.logger.Info("USB thermal camera live view registered",
		zap.String("camera", l.StableID), zap.Uint32("id", l.ID))
}

// dropLocked unregisters one camera and stops its bridge. A viewer watching it
// gets "camera was disconnected"; the bridge's picture subscription ends, so
// the driver idles the camera once nothing else holds it.
func (v *usbThermalLiveView) dropLocked(stableID, why string) {
	cam, ok := v.cams[stableID]
	if !ok {
		return
	}
	delete(v.cams, stableID)
	cam.unregister()
	cam.stop()
	v.logger.Info("USB thermal camera live view unregistered",
		zap.String("camera", stableID), zap.Uint32("id", cam.id), zap.String("why", why))
}

// stop unregisters every camera and waits for the poll loop and the bridges,
// so no picture subscription outlives it.
func (v *usbThermalLiveView) stop() {
	v.mu.Lock()
	v.stopped = true
	for sid := range v.cams {
		v.dropLocked(sid, "the video service is shutting down")
	}
	v.mu.Unlock()
	v.cancel()
	v.wg.Wait()
}

// bridge publishes a camera's pictures to its feed while the feed has a
// viewer, until ctx ends.
func (v *usbThermalLiveView) bridge(ctx context.Context, stableID string, feed *rawvideo.Feed) {
	defer v.wg.Done()
	tick := time.NewTicker(usbThermalWatchPoll)
	defer tick.Stop()
	var lastErr string
	for {
		for !feed.Watched() {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
		err := v.pump(ctx, stableID, feed, tick.C)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			lastErr = ""
			continue
		}
		if err.Error() != lastErr {
			lastErr = err.Error()
			v.logger.Warn("USB thermal camera live view could not subscribe to the camera",
				zap.String("camera", stableID), zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(usbThermalSubscribeRetry):
		}
	}
}

// pump holds one picture subscription and publishes every picture until the
// feed has no viewer, the subscription ends, or ctx ends. Leaving cancels the
// subscription, which is what lets the driver idle the camera.
func (v *usbThermalLiveView) pump(ctx context.Context, stableID string, feed *rawvideo.Feed, tick <-chan time.Time) error {
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pictures, err := v.src.SubscribePictures(subCtx, stableID)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
			if !feed.Watched() {
				return nil
			}
		case p, ok := <-pictures:
			if !ok {
				return nil
			}
			// Publish copies the bytes, so the driver's shared, read-only
			// block is never handed to the encoder.
			feed.Publish(usbThermalPicture(p))
		}
	}
}

// usbThermalPicture converts a driver picture to a feed frame. The two
// packages spell formats the same way, as GStreamer names them.
func usbThermalPicture(p usbthermal.Frame) rawvideo.Frame {
	return rawvideo.Frame{
		Data:      p.Data,
		Width:     p.Width,
		Height:    p.Height,
		Format:    p.Format,
		Timestamp: p.Timestamp,
	}
}
