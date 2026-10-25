package main

/*
#include <stdlib.h>
#include "../Sources/Shared/Bridge.h"
*/
import "C"

import (
	"errors"
	"unsafe"

	"github.com/wendylabsinc/wendy/go/internal/agent/data/thermal"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/macos/backend"
	"github.com/wendylabsinc/wendy/go/macos/vpn"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

var desktop = backend.New()

// Only C allocations cross the ABI. No Go pointer or borrowed Go slice escapes.
func alloc[T any](n int) *T {
	if n == 0 {
		return nil
	}
	var value T
	return (*T)(C.calloc(C.size_t(n), C.size_t(unsafe.Sizeof(value))))
}
func free[T any](p *T) { C.free(unsafe.Pointer(p)) }
func status(err error) C.WDStatus {
	if err == nil {
		return C.WDStatus{}
	}
	s := C.WDStatus{message: C.CString(err.Error())}
	var recovery *backend.IdentityRecoveryError
	if errors.As(err, &recovery) {
		s.can_forget_identity = 1
	}
	return s
}
func statusResult(err error) *C.WDStatus { p := alloc[C.WDStatus](1); *p = status(err); return p }

//export WDInitialize
func WDInitialize(cli *C.char) *C.WDStatus { return statusResult(desktop.Initialize(C.GoString(cli))) }

//export WDLogin
func WDLogin(development C.int32_t) *C.WDStatus { return statusResult(desktop.Login(development != 0)) }

//export WDCancelLogin
func WDCancelLogin() *C.WDStatus { return statusResult(desktop.CancelLogin()) }

//export WDDisconnect
func WDDisconnect() *C.WDStatus { return statusResult(desktop.Disconnect()) }

//export WDStopVPN
func WDStopVPN() *C.WDStatus { return statusResult(desktop.StopVPN()) }

//export WDShutdown
func WDShutdown() *C.WDStatus { return statusResult(desktop.Shutdown()) }

//export WDStartLogs
func WDStartLogs(app *C.char, stream C.uint64_t) *C.WDStatus {
	return statusResult(desktop.StartLogs(C.GoString(app), uint64(stream)))
}

//export WDStopLogs
func WDStopLogs() *C.WDStatus { return statusResult(desktop.StopLogs()) }

//export WDStartCamera
func WDStartCamera(camera C.uint32_t, stream, session C.uint64_t, stableID *C.char) *C.WDStatus {
	return statusResult(desktop.StartCamera(uint32(camera), uint64(stream), uint64(session), C.GoString(stableID)))
}

//export WDStopCamera
func WDStopCamera(camera C.uint32_t, stream, session C.uint64_t) *C.WDStatus {
	return statusResult(desktop.StopCamera(uint32(camera), uint64(stream), uint64(session)))
}

//export WDStopCameras
func WDStopCameras(session C.uint64_t) *C.WDStatus {
	return statusResult(desktop.StopCameras(uint64(session)))
}

func connect(selector *C.char, forget bool, request C.uint64_t) *C.WDConnectionResult {
	session, err := desktop.ConnectRequest(C.GoString(selector), forget, uint64(request))
	p := alloc[C.WDConnectionResult](1)
	p.status, p.session = status(err), C.uint64_t(session)
	p.lite = boolInt(desktop.IsLite(session))
	return p
}

//export WDConnect
func WDConnect(selector *C.char, request C.uint64_t) *C.WDConnectionResult {
	return connect(selector, false, request)
}

//export WDForgetIdentityAndReconnect
func WDForgetIdentityAndReconnect(selector *C.char, request C.uint64_t) *C.WDConnectionResult {
	return connect(selector, true, request)
}

func devices(items []commands.DesktopDevice, err error) *C.WDDevicesResult {
	p := alloc[C.WDDevicesResult](1)
	p.status = status(err)
	p.items, p.count = alloc[C.WDDevice](len(items)), C.size_t(len(items))
	for i, d := range items {
		transport := C.WDTransport(C.WD_TRANSPORT_LAN)
		if d.Transport == "Mesh" {
			transport = C.WD_TRANSPORT_MESH
		}
		if d.Transport == "USB" {
			transport = C.WD_TRANSPORT_USB
		}
		if d.Transport == "Cloud" {
			transport = C.WD_TRANSPORT_CLOUD
		}
		unsafe.Slice(p.items, len(items))[i] = C.WDDevice{
			id: C.CString(d.ID), name: C.CString(d.Name), selector: C.CString(d.Selector),
			address: C.CString(d.Address), transport: transport, lite: boolInt(d.Lite),
		}
	}
	return p
}

//export WDNearby
func WDNearby() *C.WDDevicesResult { return devices(desktop.Nearby()) }

//export WDCloud
func WDCloud() *C.WDDevicesResult { return devices(desktop.Cloud()) }

func appState(state pb.AppRunningState) C.WDAppState {
	switch state {
	case pb.AppRunningState_STOPPED:
		return C.WD_APP_STOPPED
	case pb.AppRunningState_RUNNING:
		return C.WD_APP_RUNNING
	case pb.AppRunningState_CRASH_LOOPING:
		return C.WD_APP_CRASH_LOOPING
	default:
		return C.WD_APP_UNKNOWN
	}
}
func apps(items []*pb.AppContainer, err error) *C.WDAppsResult {
	p := alloc[C.WDAppsResult](1)
	p.status = status(err)
	p.items, p.count = alloc[C.WDApp](len(items)), C.size_t(len(items))
	for i, a := range items {
		out := &unsafe.Slice(p.items, len(items))[i]
		out.name, out.version, out.termination_reason = C.CString(a.GetAppName()), C.CString(a.GetAppVersion()), C.CString(a.GetTerminationReason())
		out.state = appState(a.GetRunningState())
		out.http_port = C.uint32_t(a.GetHttpPort())
		services := a.GetServices()
		out.services, out.service_count = alloc[C.WDService](len(services)), C.size_t(len(services))
		for j, s := range services {
			unsafe.Slice(out.services, len(services))[j] = C.WDService{name: C.CString(s.GetName()), state: appState(s.GetRunningState())}
		}
	}
	return p
}

//export WDApps
func WDApps() *C.WDAppsResult { return apps(desktop.Apps()) }
func cameras(items []*pb.VideoDevice, err error) *C.WDCamerasResult {
	p := alloc[C.WDCamerasResult](1)
	p.status = status(err)
	p.items, p.count = alloc[C.WDCamera](len(items)), C.size_t(len(items))
	for i, camera := range items {
		unsafe.Slice(p.items, len(items))[i] = C.WDCamera{id: C.uint32_t(camera.GetId()), name: C.CString(camera.GetName()), path: C.CString(camera.GetPath()), stable_id: C.CString(camera.GetStableId()), thermal: thermalFlag(camera)}
	}
	return p
}

//export WDCameras
func WDCameras() *C.WDCamerasResult { return cameras(desktop.Cameras()) }

//export WDStartVPN
func WDStartVPN(input *C.WDRouteInput, count C.size_t) *C.WDVPNResult {
	p := alloc[C.WDVPNResult](1)
	if count == 0 || count > 1024 || input == nil {
		p.status = status(errors.New("select between 1 and 1024 routes"))
		return p
	}
	routes := make([]vpn.Route, int(count))
	for i, r := range unsafe.Slice(input, int(count)) {
		routes[i] = vpn.Route{Selector: C.GoString(r.selector), Name: C.GoString(r.name)}
	}
	config, err := desktop.StartVPN(routes)
	p.status = status(err)
	if err != nil {
		return p
	}
	p.config.endpoint, p.config.token = C.CString(config.Endpoint), C.CString(config.Token)
	p.config.routes, p.config.route_count = alloc[C.WDRoute](len(config.Routes)), C.size_t(len(config.Routes))
	for i, r := range config.Routes {
		unsafe.Slice(p.config.routes, len(config.Routes))[i] = C.WDRoute{selector: C.CString(r.Selector), name: C.CString(r.Name), address: C.CString(r.Address), host: C.CString(r.Host)}
	}
	return p
}

func events(items []backend.Event) *C.WDEvents {
	p := alloc[C.WDEvents](1)
	p.items, p.count = alloc[C.WDEvent](len(items)), C.size_t(len(items))
	for i, e := range items {
		out := &unsafe.Slice(p.items, len(items))[i]
		out.kind, out.session, out.stream = C.WDEventKind(e.Kind), C.uint64_t(e.Session), C.uint64_t(e.Stream)
		if e.Kind == backend.EventLog && e.Log != nil {
			out.log = C.WDLogRecord{timestamp_ns: C.uint64_t(e.Log.Timestamp), level: C.WDLogLevel(e.Log.Level),
				body: C.CString(e.Log.Body), app: C.CString(e.Log.App), service: C.CString(e.Log.Service)}
		} else if e.Kind == backend.EventVideo || e.Kind == backend.EventJPEG || e.Kind == backend.EventThermal {
			if f := e.Thermal; f != nil {
				out.thermal = C.WDThermalFrame{width: C.uint32_t(f.Width), height: C.uint32_t(f.Height),
					cold_x: C.uint32_t(f.Coldest.X), cold_y: C.uint32_t(f.Coldest.Y), cold_raw: C.uint16_t(f.Coldest.Raw),
					hot_x: C.uint32_t(f.Hottest.X), hot_y: C.uint32_t(f.Hottest.Y), hot_raw: C.uint16_t(f.Hottest.Raw), timestamp_ns: C.uint64_t(f.TimestampNS)}
			}
			if len(e.Bytes) > 0 {
				out.bytes = (*C.uint8_t)(C.CBytes(e.Bytes))
				out.byte_count = C.size_t(len(e.Bytes))
			}
		} else if e.Kind != backend.EventLoginDone {
			out.text = C.CString(e.Text)
		}
	}
	return p
}

//export WDPollEvents
func WDPollEvents() *C.WDEvents { return events(desktop.Poll()) }

//export WDStatusFree
func WDStatusFree(p *C.WDStatus) {
	if p != nil {
		free(p.message)
		free(p)
	}
}

//export WDConnectionFree
func WDConnectionFree(p *C.WDConnectionResult) {
	if p != nil {
		free(p.status.message)
		free(p)
	}
}

//export WDDevicesFree
func WDDevicesFree(p *C.WDDevicesResult) {
	if p == nil {
		return
	}
	for _, d := range unsafe.Slice(p.items, int(p.count)) {
		free(d.id)
		free(d.name)
		free(d.selector)
		free(d.address)
	}
	free(p.items)
	free(p.status.message)
	free(p)
}

//export WDAppsFree
func WDAppsFree(p *C.WDAppsResult) {
	if p == nil {
		return
	}
	for _, a := range unsafe.Slice(p.items, int(p.count)) {
		for _, s := range unsafe.Slice(a.services, int(a.service_count)) {
			free(s.name)
		}
		free(a.services)
		free(a.name)
		free(a.version)
		free(a.termination_reason)
	}
	free(p.items)
	free(p.status.message)
	free(p)
}

//export WDCamerasFree
func WDCamerasFree(p *C.WDCamerasResult) {
	if p == nil {
		return
	}
	for _, c := range unsafe.Slice(p.items, int(p.count)) {
		free(c.name)
		free(c.path)
		free(c.stable_id)
	}
	free(p.items)
	free(p.status.message)
	free(p)
}

//export WDVPNFree
func WDVPNFree(p *C.WDVPNResult) {
	if p == nil {
		return
	}
	for _, r := range unsafe.Slice(p.config.routes, int(p.config.route_count)) {
		free(r.selector)
		free(r.name)
		free(r.address)
		free(r.host)
	}
	free(p.config.routes)
	free(p.config.endpoint)
	free(p.config.token)
	free(p.status.message)
	free(p)
}

//export WDEventsFree
func WDEventsFree(p *C.WDEvents) {
	if p == nil {
		return
	}
	for _, e := range unsafe.Slice(p.items, int(p.count)) {
		free(e.text)
		free(e.log.body)
		free(e.log.app)
		free(e.log.service)
		free(e.bytes)
	}
	free(p.items)
	free(p)
}
func main() {}

//export WDAccount
func WDAccount() *C.WDAccountResult {
	info, err := desktop.Account()
	p := alloc[C.WDAccountResult](1)
	p.status, p.state = status(err), C.WDAccountState(info.State)
	p.context, p.cloud, p.email = C.CString(info.Context), C.CString(info.Cloud), C.CString(info.Email)
	return p
}

//export WDAccountFree
func WDAccountFree(p *C.WDAccountResult) {
	if p != nil {
		free(p.context)
		free(p.email)
		free(p.cloud)
		free(p.status.message)
		free(p)
	}
}

//export WDBeginConnection
func WDBeginConnection() C.uint64_t { return C.uint64_t(desktop.ReserveConnection()) }

//export WDAccounts
func WDAccounts() *C.WDAccountsResult {
	items, err := desktop.Accounts()
	p := alloc[C.WDAccountsResult](1)
	p.status = status(err)
	p.items, p.count = alloc[C.WDAccountEntry](len(items)), C.size_t(len(items))
	for i, item := range items {
		entry := &unsafe.Slice(p.items, len(items))[i]
		entry.context, entry.cloud, entry.email = C.CString(item.Context), C.CString(item.Cloud), C.CString(item.Email)
		entry.state = C.WDAccountState(item.State)
		if item.Active {
			entry.active = 1
		}
	}
	return p
}

//export WDAccountsFree
func WDAccountsFree(p *C.WDAccountsResult) {
	if p == nil {
		return
	}
	for _, item := range unsafe.Slice(p.items, int(p.count)) {
		free(item.context)
		free(item.cloud)
		free(item.email)
	}
	free(p.items)
	free(p.status.message)
	free(p)
}

func thermalFlag(camera *pb.VideoDevice) C.int32_t {
	if thermal.IsPureThermal(camera.GetName(), camera.GetPixelFormats()) {
		return 1
	}
	return 0
}
