package backend

import (
	"context"
	"errors"
	"sync"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// Preview sessions own their connection and event queue. They never replace the
// interactive device, and all handles are retired before an account switch.
type cameraPreviews struct {
	mu      sync.Mutex
	next    uint64
	blocked bool
	entries map[uint64]*cameraPreview
}
type cameraPreview struct {
	mu      sync.Mutex
	backend *Backend
	request uint64
	secure  bool
	ctx     context.Context
	cancel  context.CancelFunc
}

func (b *Backend) PreviewCreate() uint64 {
	p := &b.previews
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.blocked {
		return 0
	}
	if p.entries == nil {
		p.entries = make(map[uint64]*cameraPreview)
	}
	p.next++
	child := New()
	// Use the parent's scoped transport, including its mobile tunnel lifetime.
	child.connectTarget = b.connectTarget
	ctx, cancel := context.WithCancel(context.Background())
	p.entries[p.next] = &cameraPreview{backend: child, request: child.ReserveConnection(), ctx: ctx, cancel: cancel}
	return p.next
}
func (b *Backend) preview(id uint64) *cameraPreview {
	b.previews.mu.Lock()
	defer b.previews.mu.Unlock()
	return b.previews.entries[id]
}
func (b *Backend) PreviewCameras(id uint64, selector string, secure bool) ([]*pb.VideoDevice, error) {
	p := b.preview(id)
	if p == nil {
		return nil, errors.New("camera preview is closed")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ctx.Err(); err != nil {
		return nil, err
	}
	// Redialing ends the playing stream without an error event. Refresh over
	// the current connection; redial for another target or a lost connection.
	var rows []*pb.VideoDevice
	var err error
	connected := p.backend.deviceSelector == selector && p.secure == secure
	if connected {
		rows, err = p.backend.Cameras()
	}
	if !connected || (err != nil && p.ctx.Err() == nil) {
		if _, err := p.backend.ConnectTargetRequest(selector, false, p.request, secure); err != nil {
			return nil, err
		}
		p.secure = secure
		if err := p.ctx.Err(); err != nil {
			return nil, err
		}
		rows, err = p.backend.Cameras()
	}
	if err == nil {
		p.backend.cameraDevices = make(map[uint32]*pb.VideoDevice, len(rows))
		for _, row := range rows {
			p.backend.cameraDevices[row.GetId()] = row
		}
	}
	if p.ctx.Err() != nil {
		return nil, p.ctx.Err()
	}
	return rows, err
}
func (b *Backend) PreviewPlay(id uint64, stableID string) error {
	p := b.preview(id)
	if p == nil {
		return errors.New("camera preview is closed")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ctx.Err(); err != nil {
		return err
	}
	var camera *pb.VideoDevice
	for _, row := range p.backend.cameraDevices {
		if stableID != "" && row.GetStableId() == stableID {
			if camera != nil {
				return errors.New("camera identity is ambiguous; refresh sensors")
			}
			camera = row
		}
	}
	if camera == nil {
		return errors.New("this sensor is no longer connected to the device")
	}
	// Every stream of this preview uses the handle id, so it plays one sensor
	// at a time. Stop the current one before starting another.
	p.backend.mu.Lock()
	p.backend.stopCameras()
	p.backend.mu.Unlock()
	return p.backend.StartCamera(camera.GetId(), id, p.backend.session, stableID)
}
func (b *Backend) PreviewPoll(id uint64) []Event {
	p := b.preview(id)
	if p == nil || p.ctx.Err() != nil {
		return nil
	}
	return p.backend.Poll()
}
func (p *cameraPreview) stop() {
	p.cancel()
	p.backend.ReserveConnection()
	p.backend.cancelOperation()
}

// Cancellation is nonblocking, so Swift can interrupt a dial from its task's
// cancellation handler. Close joins cleanup off the main actor afterwards.
func (b *Backend) PreviewCancel(id uint64) {
	if p := b.preview(id); p != nil {
		p.stop()
	}
}
func (b *Backend) PreviewClose(id uint64) {
	b.previews.mu.Lock()
	p := b.previews.entries[id]
	delete(b.previews.entries, id)
	b.previews.mu.Unlock()
	if p == nil {
		return
	}
	p.stop()
	p.mu.Lock()
	defer p.mu.Unlock()
	// Do not call Shutdown: the embedded mesh belongs to the root backend.
	_ = p.backend.Disconnect()
}
func (b *Backend) previewsReset(blocked bool) {
	b.previews.mu.Lock()
	b.previews.blocked = blocked
	entries := b.previews.entries
	b.previews.entries = nil
	b.previews.mu.Unlock()
	for _, p := range entries {
		p.stop()
	}
	for _, p := range entries {
		p.mu.Lock()
		_ = p.backend.Disconnect()
		p.mu.Unlock()
	}
}

func (b *Backend) PreviewApps(id uint64) ([]*pb.AppContainer, error) {
	p := b.preview(id)
	if p == nil {
		return nil, errors.New("camera preview is closed")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ctx.Err(); err != nil {
		return nil, err
	}
	return p.backend.Apps()
}
func (b *Backend) PreviewOpenApp(id uint64, app string) (string, error) {
	p := b.preview(id)
	if p == nil {
		return "", errors.New("camera preview is closed")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ctx.Err(); err != nil {
		return "", err
	}
	return p.backend.OpenApp(app, p.backend.session)
}
