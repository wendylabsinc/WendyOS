package mcusource

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/audioloop"
	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

// Loopback is the subset of ipcam.Loopback the supervisor needs.
type Loopback interface {
	EnsureNode(ctx context.Context, id uint32, label string) error
	NodePath(id uint32) (string, bool)
	RemoveCamera(id uint32)
}

// AudioLoop is the subset of audioloop.Manager the supervisor needs to fan
// microphone channels out to snd-aloop PCM sinks.
type AudioLoop interface {
	ReleaseSource(sourceAssetID int32)
	Allocate(sourceAssetID int32, channelID uint32, sensorName string) (int, error)
	OpenWriter(ctx context.Context, sub int, f audioloop.PCMFormat) (audioloop.AudioWriter, error)
}

// Supervisor runs one reconcile goroutine per pairing: dial the source, mount
// its camera and microphone channels, and pump frames with backoff.
type Supervisor struct {
	logger        *zap.Logger
	lb            Loopback
	transportFor  TransportFactory
	newWriter     func(path string) ros2camera.CameraWriter
	audioLoop     AudioLoop
	resolveSource SourceResolver

	statusMu  sync.Mutex
	connected map[string]bool

	nodeIDsMu sync.Mutex
	nodeIDs   map[string]uint32 // "pairing key:channelID" -> MCU-band node id
}

// SourceResolver locates the current endpoints for a saved source identity.
// Resolution is repeated on reconnect; addresses are never persisted.
type SourceResolver func(context.Context, SensorPairing) ([]string, bool)

// SetSourceResolver installs network-specific discovery before pairings start.
// Nil keeps the normal physical-LAN discovery path.
func (s *Supervisor) SetSourceResolver(resolve SourceResolver) {
	s.resolveSource = resolve
}

// transportFor is called once per streamOnce attempt (not cached) so each
// pairing's mTLS handshake pins that specific source's identity,
// mirroring mesh_dialer.go's per-target pinning — a single shared transport
// cannot do that across pairings with different source identities.
func NewSupervisor(logger *zap.Logger, lb Loopback, transportFor TransportFactory, newWriter func(path string) ros2camera.CameraWriter, audioLoop AudioLoop) *Supervisor {
	return &Supervisor{logger: logger, lb: lb, transportFor: transportFor, newWriter: newWriter, audioLoop: audioLoop, nodeIDs: make(map[string]uint32), connected: make(map[string]bool)}
}

// IsConnected reports whether the current stream has delivered a frame to a
// mounted sensor. A retrying supervisor alone is not a live connection.
func (s *Supervisor) IsConnected(sourceAssetID int32) bool {
	return s.IsPairingConnected(SensorPairing{SourceAssetID: sourceAssetID})
}

func (s *Supervisor) IsPairingConnected(p SensorPairing) bool {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return s.connected[p.Key()]
}

// CameraIdentities maps mounted nodes to their source principal and channel.
// Node numbers are allocated in connection order and can change after restart;
// consumers such as site models must persist this identity instead.
func (s *Supervisor) CameraIdentities() map[uint32]string {
	s.nodeIDsMu.Lock()
	defer s.nodeIDsMu.Unlock()
	out := make(map[uint32]string, len(s.nodeIDs))
	for key, id := range s.nodeIDs {
		if _, exists := s.lb.NodePath(id); exists {
			out[id] = "sensorlink:" + key
		}
	}
	return out
}

func (s *Supervisor) setConnected(p SensorPairing, connected bool) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if connected {
		s.connected[p.Key()] = true
	} else {
		delete(s.connected, p.Key())
	}
}

// nodeID returns a stable MCU-band node id for (sourceAssetID, channelID),
// allocating the lowest free id in the band on first use. Reusing the same
// key across reconnects (even after the source reorders its manifest) always
// returns the same id, and different sources never collide on one id.
func (s *Supervisor) nodeID(sourceAssetID int32, channelID uint32) (uint32, error) {
	return s.nodeIDForKey((SensorPairing{SourceAssetID: sourceAssetID}).Key(), channelID)
}

func (s *Supervisor) nodeIDForKey(sourceKey string, channelID uint32) (uint32, error) {
	key := fmt.Sprintf("%s:%d", sourceKey, channelID)
	s.nodeIDsMu.Lock()
	defer s.nodeIDsMu.Unlock()
	if id, ok := s.nodeIDs[key]; ok {
		return id, nil
	}
	used := make(map[uint32]bool, len(s.nodeIDs))
	for _, id := range s.nodeIDs {
		used[id] = true
	}
	for id := uint32(ipcam.MCUBandStart); id <= ipcam.MCUBandEnd; id++ {
		if !used[id] {
			s.nodeIDs[key] = id
			return id, nil
		}
	}
	return 0, fmt.Errorf("mcusource: MCU node band [%d,%d] exhausted", ipcam.MCUBandStart, ipcam.MCUBandEnd)
}

// releaseSource runs only after the pairing's supervisor has exited. Camera
// nodes still held open by consumers remain reserved if removal fails, so a
// different source cannot reuse a node that still belongs to the old one.
func (s *Supervisor) releaseSource(sourceAssetID int32) {
	s.releasePairing(SensorPairing{SourceAssetID: sourceAssetID})
}

func (s *Supervisor) releasePairing(p SensorPairing) {
	if s.audioLoop != nil {
		if p.SourcePrincipalURI == "" {
			s.audioLoop.ReleaseSource(p.SourceAssetID)
		} else if loop, ok := s.audioLoop.(interface{ ReleasePrincipal(string) }); ok {
			loop.ReleasePrincipal(p.SourcePrincipalURI)
		}
	}
	s.nodeIDsMu.Lock()
	defer s.nodeIDsMu.Unlock()
	prefix := p.Key() + ":"
	for key, id := range s.nodeIDs {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		s.lb.RemoveCamera(id)
		if _, exists := s.lb.NodePath(id); !exists {
			delete(s.nodeIDs, key)
		}
	}
}

const (
	backoffBase = 1 * time.Second
	backoffCap  = 30 * time.Second
)

func backoffDelay(level int) time.Duration {
	d := backoffBase << level
	if d > backoffCap || d <= 0 {
		return backoffCap
	}
	return d
}

// RunPairing reconciles a single pairing until ctx is cancelled.
//
// addr decides the dial target for every (re)connect. A non-empty addr is an
// explicitly-pinned target (a caller that wants this exact address) and is
// reused unchanged for the life of the pairing. An empty addr is the dynamic,
// identity-resolved case — the common `device pair` path, which sends no
// address — and is RE-resolved by asset id (via the resolveLANAddrs seam)
// before every attempt: when the source's address changes (e.g. a Mac
// rejoins WiFi with a new IP), the next reconnect re-browses mDNS and dials
// the source's CURRENT address instead of forever redialing the stale one.
func (s *Supervisor) RunPairing(ctx context.Context, p SensorPairing, addr string) error {
	return s.runPairing(ctx, p, addr, addr != "")
}

func (s *Supervisor) runPairing(ctx context.Context, p SensorPairing, addr string, pinned bool) error {
	level := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		dialAddrs := []string{addr}
		if !pinned && addr == "" {
			rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
			resolve := s.resolveSource
			if resolve == nil {
				resolve = resolvePairingLANAddrs
			}
			resolved, ok := resolve(rctx, p)
			rcancel()
			if !ok {
				// Source not (yet) reachable — boot-resume before it's up, or
				// mid-move between addresses. Back off on the same schedule as
				// a failed stream and retry the browse.
				s.logger.Warn("sensor source not currently reachable, retrying", zap.String("source", p.Key()))
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(backoffDelay(level)):
				}
				if level < 5 {
					level++
				}
				continue
			}
			dialAddrs = resolved
		}
		if !pinned {
			// A picker address is a first-attempt hint. Subsequent attempts
			// resolve the saved identity again, including its current port.
			addr = ""
		}
		var delivered bool
		var err error
		for _, dialAddr := range dialAddrs {
			delivered, err = s.streamOnce(ctx, p, dialAddr)
			if delivered || ctx.Err() != nil {
				break
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			s.logger.Warn("sensor source stream ended", zap.String("source", p.Key()), zap.Error(err))
		}
		if delivered {
			// A healthy stream delivered at least one frame before ending;
			// don't let a brief drop after hours of good streaming pay the
			// full climbed-up backoff (up to backoffCap) to reconnect.
			level = 0
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoffDelay(level)):
		}
		if level < 5 {
			level++
		}
	}
}

// streamOnce connects, mounts camera channels, and copies frames until error.
// delivered reports whether at least one frame was written, so the caller
// can reset its reconnect backoff after a stream that was actually healthy.
func (s *Supervisor) streamOnce(ctx context.Context, p SensorPairing, addr string) (delivered bool, err error) {
	s.setConnected(p, false)
	defer s.setConnected(p, false)
	tr, err := s.transportFor(p, addr)
	if err != nil {
		return false, fmt.Errorf("mcusource: resolving transport for source %s: %w", p.Key(), err)
	}
	defer tr.Close()
	// WendyCom keeps one connection for the manifest and stream. Its TLS and
	// protocol handshakes have their own timeout, separate from the manifest
	// round trip, which starts once the connection is ready.
	if connector, ok := tr.(interface{ Connect(context.Context) error }); ok {
		if err := connector.Connect(ctx); err != nil {
			return false, err
		}
	}
	// Peek the manifest first (subscribe to nothing) to learn the channels.
	manifestCtx, cancelManifest := context.WithTimeout(ctx, 3*time.Second)
	manifest, err := tr.FetchManifest(manifestCtx)
	cancelManifest()
	if err != nil {
		return false, err
	}
	cams := cameraChannels(manifest, p.SensorAllowlist)
	mics := microphoneChannels(manifest, p.SensorAllowlist)
	// Lite channels sharing an input are mutually exclusive, including camera
	// and microphone channels. Prefer cameras, then the lowest channel ID so
	// manifest reordering does not change the selected mode. Older agent-hosted
	// sources leave every input ID at zero despite supporting concurrent streams.
	exclusiveInputs := p.Transport == "wendycom"
	if exclusiveInputs {
		sort.Slice(cams, func(i, j int) bool { return cams[i].ChannelId < cams[j].ChannelId })
		sort.Slice(mics, func(i, j int) bool { return mics[i].ChannelId < mics[j].ChannelId })
	}
	if len(cams) == 0 && len(mics) == 0 {
		return false, nil // nothing to mount; caller backs off and retries
	}

	// Assign a stable per-(source,channel) MCU-band node id to each camera
	// channel, and a snd-aloop subdevice to each microphone channel.
	writers := make(map[uint32]ros2camera.CameraWriter, len(cams))
	defer func() {
		for _, w := range writers {
			w.Close()
		}
	}()
	audioWriters := make(map[uint32]audioloop.AudioWriter, len(mics))
	defer func() {
		for _, aw := range audioWriters {
			aw.Close()
		}
	}()
	subs := make([]uint32, 0, len(cams)+len(mics))
	usedInputs := make(map[uint32]bool)
	for _, ch := range cams {
		if exclusiveInputs && usedInputs[ch.InputId] {
			continue
		}
		id, err := s.nodeIDForKey(p.Key(), ch.ChannelId)
		if err != nil {
			s.logger.Warn("skipping sensor channel", zap.String("source", p.Key()), zap.Uint32("channel", ch.ChannelId), zap.Error(err))
			continue
		}
		if err := s.lb.EnsureNode(ctx, id, nodeLabel(p, ch)); err != nil {
			// Per-channel isolation: a camera that can't be mounted (e.g. the
			// consumer kernel lacks v4l2loopback) must not tear down the whole
			// pairing — skip it so other sensors (mic) still mount.
			s.logger.Warn("skipping camera channel (loopback node unavailable)", zap.String("source", p.Key()), zap.Uint32("channel", ch.ChannelId), zap.Error(err))
			continue
		}
		path, _ := s.lb.NodePath(id)
		writers[ch.ChannelId] = s.newWriter(path)
		subs = append(subs, ch.ChannelId)
		usedInputs[ch.InputId] = true
	}
	for _, ch := range mics {
		if exclusiveInputs && usedInputs[ch.InputId] {
			continue
		}
		sub, err := s.allocateAudio(p, ch.ChannelId, ch.GetName())
		if err != nil {
			s.logger.Warn("skipping sensor channel", zap.String("source", p.Key()), zap.Uint32("channel", ch.ChannelId), zap.Error(err))
			continue
		}
		aw, err := s.audioLoop.OpenWriter(ctx, sub, audioloop.PCMFormat{SampleRate: ch.GetAudio().GetSampleRate(), Channels: ch.GetAudio().GetChannels()})
		if err != nil {
			// Per-channel isolation: a mic that can't be mounted (e.g. no
			// snd-aloop) must not tear down the pairing — skip it.
			s.logger.Warn("skipping microphone channel (audio loopback unavailable)", zap.String("source", p.Key()), zap.Uint32("channel", ch.ChannelId), zap.Error(err))
			continue
		}
		audioWriters[ch.ChannelId] = aw
		subs = append(subs, ch.ChannelId)
		usedInputs[ch.InputId] = true
	}
	if len(subs) == 0 {
		return false, nil
	}

	frames, closeStream, err := tr.Stream(ctx, subs)
	if err != nil {
		return false, err
	}
	closeStream = sync.OnceValue(closeStream)
	defer closeStream()
	// The reader goroutine inside Connect only unblocks when the conn is
	// closed or a read fails; without this, an idle source (no frames, no
	// error) leaves `range frames` blocked forever even after ctx is
	// cancelled. Closing the stream on cancellation forces the read to fail.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			closeStream()
		case <-stop:
		}
	}()
	for f := range frames {
		if w := writers[f.ChannelID]; w != nil {
			if err := w.WriteFrame(frameToCamera(f, cams)); err != nil {
				return delivered, err
			}
			if !delivered {
				s.setConnected(p, true)
			}
			delivered = true
		} else if aw := audioWriters[f.ChannelID]; aw != nil {
			if err := aw.WritePCM(f.Payload); err != nil {
				return delivered, err
			}
			if !delivered {
				s.setConnected(p, true)
			}
			delivered = true
		}
	}
	return delivered, nil
}

// cameraChannels returns the manifest's video channels. The descriptor's
// format says what kind of sensor it is.
func cameraChannels(m *sensorlinkpb.SensorManifest, allow []string) []*sensorlinkpb.SensorDescriptor {
	var out []*sensorlinkpb.SensorDescriptor
	for _, d := range m.GetSensors() {
		if d.GetVideo() == nil {
			continue
		}
		if len(allow) > 0 && !contains(allow, d.Name) {
			continue
		}
		out = append(out, d)
	}
	return out
}

// microphoneChannels returns the manifest's audio channels.
func microphoneChannels(m *sensorlinkpb.SensorManifest, allow []string) []*sensorlinkpb.SensorDescriptor {
	var out []*sensorlinkpb.SensorDescriptor
	for _, d := range m.GetSensors() {
		if d.GetAudio() == nil {
			continue
		}
		if len(allow) > 0 && !contains(allow, d.Name) {
			continue
		}
		out = append(out, d)
	}
	return out
}

func frameToCamera(f *sensorlink.SensorFrame, cams []*sensorlinkpb.SensorDescriptor) ros2camera.Frame {
	codec := ros2camera.CodecMJPEG
	w, h := 0, 0
	for _, c := range cams {
		if c.ChannelId == f.ChannelID {
			if v := c.GetVideo(); v != nil {
				if v.Codec == sensorlinkpb.VideoFormat_H264 {
					codec = ros2camera.CodecH264
				}
				w, h = int(v.Width), int(v.Height)
			}
		}
	}
	return ros2camera.Frame{Data: f.Payload, Width: w, Height: h, Codec: codec}
}

func nodeLabel(p SensorPairing, d *sensorlinkpb.SensorDescriptor) string {
	if p.Name != "" {
		return p.Name + ":" + d.Name
	}
	return d.Name
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func (s *Supervisor) allocateAudio(p SensorPairing, channel uint32, name string) (int, error) {
	if s.audioLoop == nil {
		return 0, fmt.Errorf("audio loopback unavailable")
	}
	if p.SourcePrincipalURI == "" {
		return s.audioLoop.Allocate(p.SourceAssetID, channel, name)
	}
	loop, ok := s.audioLoop.(interface {
		AllocatePrincipal(string, uint32, string) (int, error)
	})
	if !ok {
		return 0, fmt.Errorf("audio loopback does not support PKI source identities")
	}
	return loop.AllocatePrincipal(p.SourcePrincipalURI, channel, name)
}
