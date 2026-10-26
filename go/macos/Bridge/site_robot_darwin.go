//go:build darwin

package main

/*
#include <stdlib.h>
#include "../Sources/Shared/SiteRobotTypes.h"
*/
import "C"
import (
	"errors"
	"github.com/wendylabsinc/wendy/go/macos/backend"
	"github.com/wendylabsinc/wendy/go/macos/backend/sitenav"
	"sync"
	"unsafe"
)

// Serialize replacement so a previous window cannot continue commanding the same robot.
var siteRobotOpenMu sync.Mutex
var siteRobotOwners = make(map[string]uint64) // guarded by siteRobots

var siteRobots = struct {
	sync.Mutex
	next  uint64
	items map[uint64]*backend.SiteRobot
}{items: make(map[uint64]*backend.SiteRobot)}

func siteRobot(handle C.uint64_t) (*backend.SiteRobot, error) {
	siteRobots.Lock()
	defer siteRobots.Unlock()
	r := siteRobots.items[uint64(handle)]
	if r == nil {
		return nil, errors.New("Go2 connection is no longer active.")
	}
	return r, nil
}

//export WDSiteRobotVideoNext
func WDSiteRobotVideoNext(handle C.uint64_t) *C.WDUnitreeVideoFrame {
	p := alloc[C.WDUnitreeVideoFrame](1)
	r, err := siteRobot(handle)
	var data []byte
	if err == nil {
		data, err = r.NextVideo()
	}
	p.status = status(err)
	if len(data) != 0 {
		p.bytes = (*C.uint8_t)(C.CBytes(data))
		p.count = C.size_t(len(data))
	}
	return p
}

//export WDSiteRobotOpen
func WDSiteRobotOpen(session C.uint64_t, serial *C.char) *C.WDUnitreeConnection {
	p := alloc[C.WDUnitreeConnection](1)
	siteRobotOpenMu.Lock()
	defer siteRobotOpenMu.Unlock()
	key := C.GoString(serial)
	siteRobots.Lock()
	previous := siteRobots.items[siteRobotOwners[key]]
	siteRobots.Unlock()
	if previous != nil {
		previous.Close()
	}
	r, err := desktop.OpenSiteRobot(uint64(session), key)
	p.status = status(err)
	if err != nil {
		return p
	}
	siteRobots.Lock()
	siteRobots.next++
	p.handle = C.uint64_t(siteRobots.next)
	siteRobots.items[siteRobots.next] = r
	siteRobotOwners[key] = siteRobots.next
	siteRobots.Unlock()
	return p
}

//export WDSiteRobotPoll
func WDSiteRobotPoll(handle C.uint64_t) *C.WDSiteRobotState {
	p := alloc[C.WDSiteRobotState](1)
	r, err := siteRobot(handle)
	p.status = status(err)
	if err != nil {
		return p
	}
	s := r.Snapshot()
	p.look_state = C.int32_t(s.LookState)
	p.look_direction = C.int32_t(s.LookDirection)
	if s.UnderBodyAllowance {
		p.under_body_allowance = 1
	}
	if s.ScanReady {
		p.scan_ready = 1
	}
	p.message = C.CString(s.Message)
	if s.Pose {
		p.pose = 1
	}
	if s.Ready {
		p.ready = 1
	}
	if s.Aligned {
		p.aligned = 1
	}
	if s.Moving {
		p.moving = 1
	}
	if s.ArrivalTiming {
		p.arrival_timing = 1
	}
	if s.UsingArrivalTiming {
		p.using_arrival_timing = 1
	}
	if s.TimingUnavailable {
		p.timing_unavailable = 1
	}
	p.x = C.double(s.X)
	p.z = C.double(s.Z)
	p.yaw = C.double(s.Yaw)
	p.floor_y = C.double(s.FloorY)
	p.observation_revision = C.uint64_t(s.ObservationRevision)
	p.route_count = C.size_t(len(s.Route))
	p.route = alloc[C.WDSiteRoutePoint](len(s.Route))
	for i, v := range s.Route {
		out := &unsafe.Slice(p.route, len(s.Route))[i]
		out.x = C.double(v.X)
		out.z = C.double(-v.Y)
	}
	p.joint_count = C.size_t(len(s.Joints))
	p.joints = alloc[C.double](len(s.Joints))
	for i, v := range s.Joints {
		unsafe.Slice(p.joints, len(s.Joints))[i] = C.double(v)
	}
	return p
}

//export WDSiteRobotStateFree
func WDSiteRobotStateFree(p *C.WDSiteRobotState) {
	if p == nil {
		return
	}
	free(p.status.message)
	free(p.message)
	free(p.route)
	free(p.joints)
	free(p)
}

//export WDSiteRobotAlign
func WDSiteRobotAlign(handle, sequence C.uint64_t, frame *C.char, x, z, floor, yaw C.double) *C.WDStatus {
	r, err := siteRobot(handle)
	if err == nil {
		err = r.Align(uint64(sequence), C.GoString(frame), float64(x), float64(z), float64(floor), float64(yaw))
	}
	return statusResult(err)
}

//export WDSiteRobotDestination
func WDSiteRobotDestination(handle, sequence C.uint64_t, frame *C.char, x, z C.double, move C.int32_t) *C.WDStatus {
	r, err := siteRobot(handle)
	if err == nil {
		err = r.Destination(uint64(sequence), C.GoString(frame), float64(x), float64(z), move != 0)
	}
	return statusResult(err)
}

//export WDSiteRobotStop
func WDSiteRobotStop(handle, sequence C.uint64_t) {
	if r, err := siteRobot(handle); err == nil {
		r.Stop(uint64(sequence))
	}
}

//export WDSiteRobotArrivalTiming
func WDSiteRobotArrivalTiming(handle, sequence C.uint64_t, enabled C.int32_t) *C.WDStatus {
	r, err := siteRobot(handle)
	if err == nil {
		err = r.SetArrivalTiming(uint64(sequence), enabled != 0)
	}
	return statusResult(err)
}

//export WDSiteRobotUnderBodyAllowance
func WDSiteRobotUnderBodyAllowance(handle, sequence C.uint64_t, enabled C.int32_t) *C.WDStatus {
	r, err := siteRobot(handle)
	if err == nil {
		err = r.SetUnderBodyAllowance(uint64(sequence), enabled != 0)
	}
	return statusResult(err)
}

//export WDSiteRobotLocalize
func WDSiteRobotLocalize(handle, sequence C.uint64_t, frame *C.char, vertices *C.double, count C.size_t) *C.WDSiteRobotCandidates {
	p := alloc[C.WDSiteRobotCandidates](1)
	r, err := siteRobot(handle)
	if err == nil && (vertices == nil || count < 12 || count > 300000 || count%3 != 0) {
		err = errors.New("Unsupported room geometry.")
	}
	if err != nil {
		p.status = status(err)
		return p
	}
	values := unsafe.Slice(vertices, int(count)*3)
	mesh := make([]sitenav.Triangle, int(count)/3)
	for i := range mesh {
		for j := range mesh[i] {
			k := (i*3 + j) * 3
			mesh[i][j] = sitenav.Vertex{X: float64(values[k]), Y: float64(values[k+1]), Z: float64(values[k+2])}
		}
	}
	results, err := r.Localize(uint64(sequence), C.GoString(frame), mesh)
	p.status = status(err)
	if err != nil {
		return p
	}
	p.count = C.size_t(len(results))
	p.items = alloc[C.WDSiteRobotCandidate](len(results))
	for i, v := range results {
		out := &unsafe.Slice(p.items, len(results))[i]
		out.x = C.double(v.X)
		out.z = C.double(v.Z)
		out.yaw = C.double(v.Yaw)
		out.floor_y = C.double(v.FloorY)
		out.rms = C.double(v.RMS)
		out.coverage = C.double(v.Coverage)
	}
	return p
}

//export WDSiteRobotCandidatesFree
func WDSiteRobotCandidatesFree(p *C.WDSiteRobotCandidates) {
	if p != nil {
		free(p.status.message)
		free(p.items)
		free(p)
	}
}

//export WDSiteRobotConfirmLocalization
func WDSiteRobotConfirmLocalization(handle, sequence, matchSequence C.uint64_t, frame *C.char, index C.int32_t) *C.WDStatus {
	r, err := siteRobot(handle)
	if err == nil {
		err = r.ConfirmLocalization(uint64(sequence), uint64(matchSequence), C.GoString(frame), int(index))
	}
	return statusResult(err)
}

//export WDSiteRobotClose
func WDSiteRobotClose(handle C.uint64_t) {
	siteRobotOpenMu.Lock()
	defer siteRobotOpenMu.Unlock()
	siteRobots.Lock()
	r := siteRobots.items[uint64(handle)]
	delete(siteRobots.items, uint64(handle))
	for serial, owner := range siteRobotOwners {
		if owner == uint64(handle) {
			delete(siteRobotOwners, serial)
		}
	}
	siteRobots.Unlock()
	if r != nil {
		r.Close()
	}
}

//export WDSiteRobotFuse
func WDSiteRobotFuse(handle, sequence C.uint64_t, request *C.char) *C.WDSiteRobotFusionResult {
	p := alloc[C.WDSiteRobotFusionResult](1)
	r, err := siteRobot(handle)
	if request == nil {
		err = errors.New("Missing localization request.")
	}
	if err == nil {
		var data []byte
		data, err = r.FuseLocalizationJSON(uint64(sequence), []byte(C.GoString(request)))
		if err == nil {
			p.json = C.CString(string(data))
		}
	}
	p.status = status(err)
	return p
}

//export WDSiteRobotFusionFree
func WDSiteRobotFusionFree(p *C.WDSiteRobotFusionResult) {
	if p == nil {
		return
	}
	free(p.status.message)
	free(p.json)
	free(p)
}

//export WDSiteRobotLook
func WDSiteRobotLook(handle, sequence C.uint64_t, direction C.int32_t) *C.WDStatus {
	r, err := siteRobot(handle)
	if err == nil {
		err = r.Look(uint64(sequence), int(direction))
	}
	return statusResult(err)
}
