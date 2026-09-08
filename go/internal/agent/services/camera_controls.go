package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"github.com/wendylabsinc/wendy/go/internal/agent/ipcam"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// This file adds tunable V4L2 controls (exposure, gain, white balance, ...) to
// the video service. It exists because the agent owns the /dev/videoN node --
// StreamVideo multiplexes one capture to every subscriber (see video_service.go)
// -- so the agent is the only process that can set these to stick. A control set
// by anyone else is lost the moment the capture pipeline reopens the device, and
// on a device the app reads through the agent (CAMERA_SOURCE=wendy-agent://...)
// there is no other writer at all. The concrete need: a scene the camera's
// auto-exposure gets wrong, e.g. a flame that blows out to white, where forcing
// manual exposure keeps the highlight from clipping so a fire model can see it.

const (
	// VIDIOC_G_EXT_CTRLS, _IOWR('V', 71, struct v4l2_ext_controls) -- reads the
	// current value; the S_ variant (vidiocSExtCtrls, video_service.go) writes it.
	vidiocGExtCtrls = 0xC0205647
	// VIDIOC_QUERYCTRL, _IOWR('V', 36, struct v4l2_queryctrl) -- range + flags.
	vidiocQueryCtrl = 0xC0445624
	// V4L2_CTRL_WHICH_CUR_VAL: address a control by CID regardless of its class,
	// which the classic per-class `which` cannot do for a mix of USER and CAMERA
	// controls. This is what v4l2-ctl uses.
	v4l2CtrlWhichCurVal = 0x0

	// V4L2_CTRL_FLAG_NEXT_CTRL: OR this into the id and QUERYCTRL returns the
	// next control the DRIVER has, rather than the one asked for. Walking from
	// 0 therefore enumerates a camera's whole control set without any list
	// being written down here -- which is the point: a hardcoded table only
	// ever covers the cameras somebody owned when they wrote it, and silently
	// omits the rest (zoom, pan, tilt and focus were all missing that way).
	v4l2CtrlFlagNextCtrl = 0x80000000
	// V4L2_CTRL_TYPE_CTRL_CLASS: a heading in the enumeration, not a control.
	v4l2CtrlTypeCtrlClass = 6

	// v4l2_queryctrl flags we act on.
	v4l2CtrlFlagDisabled = 0x0001
	v4l2CtrlFlagReadOnly = 0x0004
	v4l2CtrlFlagInactive = 0x0010

	cameraControlsPath = "/var/lib/wendy/camera-controls.json"
)

// tunableControls is the fixed set of controls the agent will read and set, by
// stable slug. The slugs match v4l2-ctl's control names so an operator can move
// between the CLI and this API without a translation table. CIDs are the
// standard V4L2_CID_* values from linux/videodev2.h: the USER class base is
// 0x00980900, the CAMERA class base 0x009a0900.
// tunableControls is no longer what this service supports --
// enumerateV4L2Controls asks the camera. It survives only as the fallback probe
// list for a driver that does not implement NEXT_CTRL, where the set has to be
// guessed at.
var tunableControls = []struct {
	name string
	cid  uint32
}{
	{"brightness", 0x00980900},
	{"contrast", 0x00980901},
	{"saturation", 0x00980902},
	{"hue", 0x00980903},
	{"white_balance_automatic", 0x0098090c}, // V4L2_CID_AUTO_WHITE_BALANCE
	{"gamma", 0x00980910},
	{"gain", 0x00980913},
	{"power_line_frequency", 0x00980918},
	{"white_balance_temperature", 0x0098091a},
	{"sharpness", 0x0098091b},
	{"backlight_compensation", 0x0098091c},
	{"auto_exposure", 0x009a0901},          // V4L2_CID_EXPOSURE_AUTO
	{"exposure_time_absolute", 0x009a0902}, // V4L2_CID_EXPOSURE_ABSOLUTE
	{"exposure_dynamic_framerate", 0x009a0903},
}

// controlValue is a resolved control ready to write.
type controlValue struct {
	name  string
	cid   uint32
	value int32
}

// orderControls sorts so that a mode control is applied before the control it
// gates: auto_exposure=Manual must precede exposure_time_absolute (the driver
// marks the absolute control inactive until then), and likewise auto white
// balance before the temperature. Stable, so unrelated controls keep their order.
func orderControls(in []controlValue) []controlValue {
	out := append([]controlValue(nil), in...)
	rank := func(name string) int {
		switch name {
		case "auto_exposure", "white_balance_automatic":
			return 0
		default:
			return 1
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i].name) < rank(out[j].name) })
	return out
}

// --- V4L2 ioctls -----------------------------------------------------------

// getV4L2ExtControl issues VIDIOC_G_EXT_CTRLS for a single control and returns
// its current value. Mirrors setV4L2ExtControl (video_service.go); the inner
// control is pinned because it is reached only through a uintptr the GC cannot see.
func getV4L2ExtControl(fd int, controlID uint32) (int32, unix.Errno) {
	var ctrl v4l2ExtControl
	ctrl.setID(controlID)

	var pinner runtime.Pinner
	pinner.Pin(&ctrl)
	defer pinner.Unpin()

	var ctrls v4l2ExtControls
	ctrls.setWhich(v4l2CtrlWhichCurVal)
	ctrls.setCount(1)
	ctrls.setControlsPtr(uintptr(unsafe.Pointer(&ctrl)))

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocGExtCtrls, uintptr(unsafe.Pointer(&ctrls)))
	return ctrl.value(), errno
}

// v4l2QueryCtrl matches struct v4l2_queryctrl (68 bytes): id@0, type@4,
// name[32]@8, minimum@40, maximum@44, step@48, default@52, flags@56, reserved@60.
type v4l2QueryCtrl [68]byte

func (q *v4l2QueryCtrl) setID(id uint32)  { *(*uint32)(unsafe.Pointer(&q[0])) = id }
func (q *v4l2QueryCtrl) id() uint32       { return *(*uint32)(unsafe.Pointer(&q[0])) }
func (q *v4l2QueryCtrl) ctrlType() uint32 { return *(*uint32)(unsafe.Pointer(&q[4])) }

// name is the driver's label, normalised to the spelling v4l2-ctl prints and
// this API accepts: lower case, each run of non-alphanumerics becoming one
// underscore. "White Balance, Automatic" -> "white_balance_automatic".
func (q *v4l2QueryCtrl) name() string {
	raw := q[8:40]
	if i := bytes.IndexByte(raw, 0); i >= 0 {
		raw = raw[:i]
	}
	var b strings.Builder
	lastUnderscore := true // leading separators are dropped
	for _, r := range strings.ToLower(string(raw)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}
func (q *v4l2QueryCtrl) minimum() int32    { return *(*int32)(unsafe.Pointer(&q[40])) }
func (q *v4l2QueryCtrl) maximum() int32    { return *(*int32)(unsafe.Pointer(&q[44])) }
func (q *v4l2QueryCtrl) step() int32       { return *(*int32)(unsafe.Pointer(&q[48])) }
func (q *v4l2QueryCtrl) defaultVal() int32 { return *(*int32)(unsafe.Pointer(&q[52])) }
func (q *v4l2QueryCtrl) flags() uint32     { return *(*uint32)(unsafe.Pointer(&q[56])) }

// queryV4L2Control issues VIDIOC_QUERYCTRL. ok is false when the driver does not
// expose the control (EINVAL) -- a normal answer, not an error, since the tunable
// set is a superset of any one camera's controls.
func queryV4L2Control(fd int, controlID uint32) (q v4l2QueryCtrl, ok bool) {
	q.setID(controlID)
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), vidiocQueryCtrl, uintptr(unsafe.Pointer(&q)))
	return q, errno == 0
}

// enumerateV4L2Controls walks every control the driver exposes, in driver
// order, skipping class headings and controls the driver reports as disabled.
//
// Falls back to the static tunableControls table when the driver does not
// support NEXT_CTRL (pre-2.6.18 semantics, and a few odd drivers): the first
// probe returning nothing is indistinguishable from "no controls", so a camera
// that really has some would otherwise look empty.
func enumerateV4L2Controls(fd int) []v4l2QueryCtrl {
	var out []v4l2QueryCtrl
	id := uint32(0)
	for len(out) < 256 { // a driver looping would otherwise hang the agent
		q, ok := queryV4L2Control(fd, id|v4l2CtrlFlagNextCtrl)
		if !ok {
			break
		}
		next := q.id()
		if next == id { // driver ignored NEXT_CTRL; stop rather than spin
			break
		}
		id = next
		if q.ctrlType() == v4l2CtrlTypeCtrlClass || q.flags()&v4l2CtrlFlagDisabled != 0 {
			continue
		}
		if q.name() == "" {
			continue
		}
		out = append(out, q)
	}
	if len(out) == 0 {
		for _, tc := range tunableControls {
			if q, ok := queryV4L2Control(fd, tc.cid); ok {
				out = append(out, q)
			}
		}
	}
	return out
}

// cameraControlDefaults reads the driver's own default for each named control.
// The default is the camera's answer, not something a caller has to know, which
// is what makes "reset" meaningful for a control nobody documented.
func cameraControlDefaults(path string, names []string) (map[string]int32, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd) //nolint:errcheck

	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	out := map[string]int32{}
	for _, q := range enumerateV4L2Controls(fd) {
		if want[q.name()] {
			out[q.name()] = q.defaultVal()
		}
	}
	return out, nil
}

// cameraControlIndex maps a camera's control names to their CIDs by asking the
// camera, so a name is resolved against the device it is destined for rather
// than against a table. Read-only open, same EBUSY reasoning as elsewhere.
func cameraControlIndex(path string) (map[string]uint32, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd) //nolint:errcheck

	index := map[string]uint32{}
	for _, q := range enumerateV4L2Controls(fd) {
		index[q.name()] = q.id()
	}
	return index, nil
}

// applyCameraControlsV4L2 opens the node read-write and sets each control,
// reporting per-control success. Controls are reordered (orderControls) so a
// mode change lands before the control it enables. A control the camera does
// not have comes back applied=false with the errno, not as a hard failure: one
// unknown knob must not sink the rest of the request.
func applyCameraControlsV4L2(path string, ctrls []controlValue) ([]*agentpb.CameraControlResult, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd) //nolint:errcheck

	results := make([]*agentpb.CameraControlResult, 0, len(ctrls))
	for _, c := range orderControls(ctrls) {
		errno := setV4L2ExtControl(fd, c.cid, c.value, v4l2CtrlWhichCurVal)
		r := &agentpb.CameraControlResult{Name: c.name, Applied: errno == 0}
		if errno != 0 {
			r.Detail = errno.Error()
		}
		results = append(results, r)
	}
	return results, nil
}

// queryCameraControlsV4L2 reports every tunable control the camera actually
// exposes, with its current value and range. O_RDONLY: QUERYCTRL and
// G_EXT_CTRLS are read-only, and a read-only open avoids the EBUSY an exclusive
// camera can return on a second writable open (same reasoning as hasVideoCapture).
func queryCameraControlsV4L2(path string) ([]*agentpb.CameraControl, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd) //nolint:errcheck

	discovered := enumerateV4L2Controls(fd)
	out := make([]*agentpb.CameraControl, 0, len(discovered))
	for _, q := range discovered {
		val, errno := getV4L2ExtControl(fd, q.id())
		if errno != 0 {
			val = q.defaultVal() // report something rather than dropping the row
		}
		flags := q.flags()
		out = append(out, &agentpb.CameraControl{
			Name:         q.name(),
			Value:        val,
			Minimum:      q.minimum(),
			Maximum:      q.maximum(),
			Step:         q.step(),
			DefaultValue: q.defaultVal(),
			Mutable:      flags&(v4l2CtrlFlagDisabled|v4l2CtrlFlagReadOnly) == 0 && flags&v4l2CtrlFlagInactive == 0,
		})
	}
	return out, nil
}

// --- persistence -----------------------------------------------------------

type storedControl struct {
	Name  string `json:"name"`
	Value int32  `json:"value"`
}

// cameraControlStore persists desired controls per device path so they can be
// re-asserted whenever the capture pipeline reopens the node and across agent
// restarts -- the difference between a fix that holds and one that reverts to
// the firmware default on the next stream reconnect or reboot.
type cameraControlStore struct {
	path   string
	mu     sync.Mutex
	byPath map[string][]storedControl
}

func newCameraControlStore(path string) *cameraControlStore {
	return &cameraControlStore{path: path, byPath: map[string][]storedControl{}}
}

func (c *cameraControlStore) Load() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing persisted yet is not an error
		}
		return err
	}
	var m map[string][]storedControl
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	if m != nil {
		c.byPath = m
	}
	return nil
}

// merge updates or inserts each control for path and persists. Merge, not
// replace: setting exposure after having set auto-mode must keep both.
func (c *cameraControlStore) merge(path string, ctrls []storedControl) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.byPath[path]
	for _, nc := range ctrls {
		found := false
		for i := range cur {
			if cur[i].Name == nc.Name {
				cur[i].Value = nc.Value
				found = true
				break
			}
		}
		if !found {
			cur = append(cur, nc)
		}
	}
	c.byPath[path] = cur
	return c.save()
}

// save writes the store atomically. Caller holds c.mu.
func (c *cameraControlStore) save() error {
	b, err := json.MarshalIndent(c.byPath, "", "  ")
	if err != nil {
		return err
	}
	// 0o700/0o600, matching ipcam's credential store rather than the 0o755/0o644
	// this used to use. The contents are not secrets, but they are device
	// configuration that decides how a camera captures -- and on a box where
	// the picture feeds a detector, a local user who can rewrite the exposure
	// can blind it without touching the detector at all. Only the agent needs
	// to read or write this.
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// remove drops controls from a camera's stored set, so a reset is not
// re-applied on the next reopen. Without this the store only ever grows and a
// persisted value cannot be taken back.
func (c *cameraControlStore) remove(path string, names []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	drop := make(map[string]bool, len(names))
	for _, n := range names {
		drop[n] = true
	}
	kept := c.byPath[path][:0:0]
	for _, sc := range c.byPath[path] {
		if !drop[sc.Name] {
			kept = append(kept, sc)
		}
	}
	if len(kept) == 0 {
		delete(c.byPath, path)
	} else {
		c.byPath[path] = kept
	}
	return c.save()
}

func (c *cameraControlStore) get(path string) []storedControl {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]storedControl(nil), c.byPath[path]...)
}

// --- RPCs ------------------------------------------------------------------

// localCameraPath validates that devID names a local V4L2 camera and returns
// its node path. Network cameras are rejected: they expose no V4L2 controls here.
func localCameraPath(devID uint32) (string, error) {
	if devID >= ipcam.IDBandStart && devID <= ipcam.IDBandEnd {
		return "", status.Errorf(codes.InvalidArgument,
			"camera %d is a network camera; V4L2 controls apply only to local (USB/CSI) cameras", devID)
	}
	return fmt.Sprintf("/dev/video%d", devID), nil
}

// GetCameraControls reports the tunable controls a local camera exposes.
func (s *VideoService) GetCameraControls(_ context.Context, req *agentpb.GetCameraControlsRequest) (*agentpb.GetCameraControlsResponse, error) {
	path, err := localCameraPath(req.GetDeviceId())
	if err != nil {
		return nil, err
	}
	controls, err := s.queryLocalControls(path)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading controls from %s: %v", path, err)
	}
	return &agentpb.GetCameraControlsResponse{Controls: controls}, nil
}

// SetCameraControls sets controls on a local camera and, when persist is set,
// remembers them so they survive a pipeline reopen and an agent restart.
//
// Setting only. Putting a control BACK is ResetCameraControls: it is a
// different verb, changing the persisted state as well as the value, and
// folding it in here as a flag meant every request carried a field that made
// `value` meaningless whenever it was true.
func (s *VideoService) SetCameraControls(_ context.Context, req *agentpb.SetCameraControlsRequest) (*agentpb.SetCameraControlsResponse, error) {
	path, err := localCameraPath(req.GetDeviceId())
	if err != nil {
		return nil, err
	}
	if len(req.GetControls()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no controls given")
	}

	// Resolve names against THIS camera rather than a table, so any control the
	// camera exposes is settable without a code change -- zoom, pan, tilt and
	// focus were all unreachable while the table was the source of truth.
	index, err := s.controlIndexFor(path)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading controls from %s: %v", path, err)
	}

	var resolved []controlValue
	results := make([]*agentpb.CameraControlResult, 0, len(req.GetControls()))
	for _, c := range req.GetControls() {
		cid, ok := index[c.GetName()]
		if !ok {
			results = append(results, unknownControlResult(c.GetName()))
			continue
		}
		resolved = append(resolved, controlValue{name: c.GetName(), cid: cid, value: c.GetValue()})
	}

	if len(resolved) > 0 {
		applied, err := s.applyLocalControls(path, resolved)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "opening %s: %v", path, err)
		}
		results = append(results, applied...)

		if req.GetPersist() && s.controls != nil {
			var toStore []storedControl
			for _, r := range applied {
				if !r.GetApplied() {
					continue
				}
				for _, rv := range resolved {
					if rv.name == r.GetName() {
						toStore = append(toStore, storedControl{Name: rv.name, Value: rv.value})
					}
				}
			}
			if len(toStore) > 0 {
				if err := s.controls.merge(path, toStore); err != nil {
					s.logger.Warn("persisting camera controls failed",
						zap.String("device", path), zap.Error(err))
				}
			}
		}
	}
	return &agentpb.SetCameraControlsResponse{Results: results}, nil
}

// ResetCameraControls puts controls back to the driver's own defaults and stops
// persisting them.
//
// Its own RPC rather than a flag on Set because it is a different operation:
// Set says "make it this", Reset says "forget I ever said anything". It changes
// the STORE as well as the value, and a caller that modelled it as "set to the
// default value" would leave the control pinned to that default forever --
// which is precisely what reset undoes.
//
// The default comes from the driver (QUERYCTRL), so the camera answers rather
// than the caller having to know.
func (s *VideoService) ResetCameraControls(_ context.Context, req *agentpb.ResetCameraControlsRequest) (*agentpb.ResetCameraControlsResponse, error) {
	path, err := localCameraPath(req.GetDeviceId())
	if err != nil {
		return nil, err
	}

	// No names means every control currently persisted for this camera. That is
	// the case that matters after a tuning session went wrong: the caller wants
	// the camera back as it shipped and should not have to remember what it
	// changed to get there.
	names := req.GetNames()
	if len(names) == 0 {
		if s.controls == nil {
			return &agentpb.ResetCameraControlsResponse{}, nil
		}
		for _, sc := range s.controls.get(path) {
			names = append(names, sc.Name)
		}
		if len(names) == 0 {
			return &agentpb.ResetCameraControlsResponse{}, nil
		}
	}

	// Forget FIRST, and unconditionally -- not only where the write below
	// succeeds. A control the driver reports inactive right now
	// (exposure_time_absolute while auto_exposure is on) cannot be written, and
	// if that also blocked forgetting it the stored value would be re-asserted
	// on every reopen with no way to clear it. Reset is two promises: stop
	// persisting this, and put it back. The first must hold even when the
	// second cannot.
	if s.controls != nil {
		if err := s.controls.remove(path, names); err != nil {
			s.logger.Warn("forgetting reset camera controls failed",
				zap.String("device", path), zap.Error(err))
		}
	}

	index, err := s.controlIndexFor(path)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading controls from %s: %v", path, err)
	}
	defaults, err := s.controlDefaultsFor(path, names)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading defaults from %s: %v", path, err)
	}

	var resolved []controlValue
	results := make([]*agentpb.CameraControlResult, 0, len(names))
	for _, name := range names {
		cid, ok := index[name]
		if !ok {
			results = append(results, unknownControlResult(name))
			continue
		}
		def, known := defaults[name]
		if !known {
			results = append(results, &agentpb.CameraControlResult{
				Name: name, Applied: false,
				Detail: "this camera reports no default for that control",
			})
			continue
		}
		resolved = append(resolved, controlValue{name: name, cid: cid, value: def})
	}

	if len(resolved) > 0 {
		applied, err := s.applyLocalControls(path, resolved)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "opening %s: %v", path, err)
		}
		results = append(results, applied...)
	}
	return &agentpb.ResetCameraControlsResponse{Results: results}, nil
}

// unknownControlResult is the answer for a name this camera does not expose.
// Shared so Set and Reset word it identically -- an operator comparing the two
// should not have to wonder whether a different phrasing means something else.
func unknownControlResult(name string) *agentpb.CameraControlResult {
	return &agentpb.CameraControlResult{
		Name: name, Applied: false,
		Detail: "this camera has no control by that name",
	}
}

// applyStoredCameraControls re-asserts any persisted controls for a local
// camera. Called from runProducer each time a producer (re)starts, so a stream
// reconnect does not silently revert exposure to the firmware default. Best
// effort: a camera that has lost a control since it was stored just logs.
func (s *VideoService) applyStoredCameraControls(path string) {
	if s.controls == nil {
		return
	}
	stored := s.controls.get(path)
	if len(stored) == 0 {
		return
	}
	index, err := s.controlIndexFor(path)
	if err != nil {
		s.logger.Debug("re-applying stored camera controls: cannot read control list",
			zap.String("device", path), zap.Error(err))
		return
	}
	resolved := make([]controlValue, 0, len(stored))
	for _, sc := range stored {
		if cid, ok := index[sc.Name]; ok {
			resolved = append(resolved, controlValue{name: sc.Name, cid: cid, value: sc.Value})
		}
	}
	if len(resolved) == 0 {
		return
	}
	if _, err := s.applyLocalControls(path, resolved); err != nil {
		s.logger.Debug("re-applying stored camera controls failed",
			zap.String("device", path), zap.Error(err))
		return
	}
	s.logger.Info("re-applied stored camera controls",
		zap.String("device", path), zap.Int("count", len(resolved)))
}
