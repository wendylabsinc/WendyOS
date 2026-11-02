package usbthermal

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Device ids for userspace USB cameras. Every camera the agent lists needs a
// number in 0..255 (StreamVideoRequest.device_id), and the numbers above 95
// are taken by the v4l2loopback bands (ipcam/registry.go: MCU 96-127, ROS 2
// 128-199, network cameras 200-255). These cameras have no /dev/videoN, so they
// take the top of the physical range, which kernel enumeration does not reach
// in practice. A number in the band that a real /dev/videoN already holds is
// never handed out (Options.IDTaken).
const (
	IDBandStart = 88
	IDBandEnd   = 95
)

// Options configures a Manager. The zero value works on a real device.
type Options struct {
	SysfsRoot string   // default "/sys"
	DevRoot   string   // default "/dev"
	Open      OpenFunc // default usbfs (Linux); tests substitute a fake
	// IDTaken reports a device id the manager must not hand out, normally one
	// a real /dev/videoN node already has. Nil means none is taken.
	IDTaken func(id uint32) bool
	Logger  *zap.Logger

	// Test seams.
	sleep sleepFunc
	now   func() time.Time
}

// Listing is one attached camera as ListVideoDevices reports it.
type Listing struct {
	ID       uint32 // device id, in IDBandStart..IDBandEnd
	StableID string // "by-id:usb-<vendor>_<model>_<serial>-thermal", or "by-path:..." for a twin
	Name     string // e.g. "TC001 Max (thermal)"
	Model    string // the camera's USB product string, e.g. "TC001 Max"
	Serial   string
	Port     string // sysfs bus-port name, e.g. "1-2.3"
	Layout   Layout // geometry of the camera's thermal frames
}

// Manager finds the supported cameras on the bus and owns one Camera per
// stable id. Listing reads sysfs only, so it is cheap and never touches a
// camera; a camera is opened only while someone is subscribed to it.
type Manager struct {
	opts   Options
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	cameras map[string]*Camera // by stable id; kept across an unplug so a replug resumes
	ids     map[string]uint32  // stable id -> device id, for the agent's lifetime
	warned  map[string]bool    // stable ids already logged as unlistable
}

// NewManager returns a manager whose cameras stop when ctx ends or Close is
// called.
func NewManager(ctx context.Context, opts Options) *Manager {
	if opts.SysfsRoot == "" {
		opts.SysfsRoot = "/sys"
	}
	if opts.DevRoot == "" {
		opts.DevRoot = "/dev"
	}
	if opts.Open == nil {
		opts.Open = defaultOpen()
	}
	if opts.Logger == nil {
		opts.Logger = zap.NewNop()
	}
	if opts.sleep == nil {
		opts.sleep = sleepCtx
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	mctx, cancel := context.WithCancel(ctx)
	return &Manager{
		opts:    opts,
		ctx:     mctx,
		cancel:  cancel,
		cameras: map[string]*Camera{},
		ids:     map[string]uint32{},
		warned:  map[string]bool{},
	}
}

// List returns the supported cameras attached now, each with its device id.
//
// A device id is stable for the agent's lifetime: a camera keeps its number
// across an unplug and replug. Across an agent restart the first camera is
// IDBandStart again, which is the common case of one camera; StableID is the
// name that survives everything.
func (m *Manager) List() []Listing {
	cams := scanSysfs(m.opts.SysfsRoot)
	present := make(map[string]bool, len(cams))
	for _, a := range cams {
		present[a.stableID] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Listing, 0, len(cams))
	for _, a := range cams {
		id, ok := m.idLocked(a.stableID, present)
		if !ok {
			if !m.warned[a.stableID] {
				m.warned[a.stableID] = true
				m.opts.Logger.Warn("no free device id for a USB thermal camera; it is not listed",
					zap.String("camera", a.stableID), zap.Int("band_start", IDBandStart), zap.Int("band_end", IDBandEnd))
			}
			continue
		}
		model := a.product
		if model == "" {
			model = a.model.Name
		}
		out = append(out, Listing{
			ID:       id,
			StableID: a.stableID,
			Name:     a.model.DisplayName,
			Model:    model,
			Serial:   a.serial,
			Port:     a.port,
			Layout:   a.model.Layout,
		})
	}
	return out
}

// idLocked returns the device id for a stable id, allocating the lowest free
// one in the band. A number belonging to a camera that is not attached is
// reused only when the band has no other free number.
func (m *Manager) idLocked(stableID string, present map[string]bool) (uint32, bool) {
	if id, ok := m.ids[stableID]; ok {
		return id, true
	}
	owner := make(map[uint32]string, len(m.ids))
	for sid, id := range m.ids {
		owner[id] = sid
	}
	taken := func(id uint32) bool { return m.opts.IDTaken != nil && m.opts.IDTaken(id) }
	for id := uint32(IDBandStart); id <= IDBandEnd; id++ {
		if _, used := owner[id]; !used && !taken(id) {
			m.ids[stableID] = id
			return id, true
		}
	}
	for id := uint32(IDBandStart); id <= IDBandEnd; id++ {
		if sid := owner[id]; !present[sid] && !taken(id) {
			delete(m.ids, sid)
			m.ids[stableID] = id
			return id, true
		}
	}
	return 0, false
}

// Lookup returns the attached camera with this device id.
func (m *Manager) Lookup(id uint32) (Listing, bool) {
	if id < IDBandStart || id > IDBandEnd {
		return Listing{}, false
	}
	for _, l := range m.List() {
		if l.ID == id {
			return l, true
		}
	}
	return Listing{}, false
}

// ResolveStableID returns the device id of the attached camera with this
// stable id. The match is exact, like the V4L2 one.
func (m *Manager) ResolveStableID(stableID string) (uint32, bool) {
	for _, l := range m.List() {
		if l.StableID == stableID {
			return l.ID, true
		}
	}
	return 0, false
}

// Camera returns the camera with this stable id. It must be attached the first
// time it is asked for; after that the same Camera is returned, attached or
// not, so a subscriber survives an unplug.
func (m *Manager) Camera(stableID string) (*Camera, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return nil, ErrClosed
	}
	if c, ok := m.cameras[stableID]; ok {
		return c, nil
	}
	var found *attached
	for _, a := range scanSysfs(m.opts.SysfsRoot) {
		if a.stableID == stableID {
			found = &a
			break
		}
	}
	if found == nil {
		return nil, fmt.Errorf("no USB thermal camera %q is connected", stableID)
	}
	c := &Camera{
		stableID: stableID,
		model:    found.model,
		locate:   func() (attached, bool) { return m.locate(stableID) },
		devRoot:  m.opts.DevRoot,
		open:     m.opts.Open,
		sleep:    m.opts.sleep,
		now:      m.opts.now,
		logger:   m.opts.Logger,
		base:     m.ctx,
		subs:     map[int]*subscriber{},
	}
	m.cameras[stableID] = c
	return c, nil
}

// locate finds where a camera is on the bus right now.
func (m *Manager) locate(stableID string) (attached, bool) {
	for _, a := range scanSysfs(m.opts.SysfsRoot) {
		if a.stableID == stableID {
			return a, true
		}
	}
	return attached{}, false
}

// SubscribeRaw is Camera(stableID).SubscribeRaw(ctx).
func (m *Manager) SubscribeRaw(ctx context.Context, stableID string) (<-chan RawFrame, error) {
	c, err := m.Camera(stableID)
	if err != nil {
		return nil, err
	}
	return c.SubscribeRaw(ctx)
}

// SubscribePictures is Camera(stableID).SubscribePictures(ctx): the thermal
// picture of every frame, for the encoded live view.
func (m *Manager) SubscribePictures(ctx context.Context, stableID string) (<-chan Frame, error) {
	c, err := m.Camera(stableID)
	if err != nil {
		return nil, err
	}
	return c.SubscribePictures(ctx)
}

// LastError is why the camera's most recent capture attempt ended, or nil.
func (m *Manager) LastError(stableID string) error {
	m.mu.Lock()
	c, ok := m.cameras[stableID]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	return c.LastError()
}

// Close stops every camera, waits until each is idle and released, and makes
// further subscriptions fail with ErrClosed.
func (m *Manager) Close() {
	m.cancel()
	m.mu.Lock()
	cams := make([]*Camera, 0, len(m.cameras))
	for _, c := range m.cameras {
		cams = append(cams, c)
	}
	m.mu.Unlock()
	for _, c := range cams {
		c.wait()
	}
}
