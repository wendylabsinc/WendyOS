package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"

import (
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"unsafe"
)

func optional(value *float64) C.WDOptionalDouble {
	if value == nil {
		return C.WDOptionalDouble{}
	}
	return C.WDOptionalDouble{available: 1, value: C.double(*value)}
}
func resourceResult(snapshot *commands.DesktopResourceSnapshot, err error) *C.WDResourcesResult {
	p := alloc[C.WDResourcesResult](1)
	p.status = status(err)
	if err != nil || snapshot == nil {
		return p
	}
	if h := snapshot.Host; h != nil {
		p.has_host = 1
		if snapshot.HasCPU {
			p.cpu = optional(&snapshot.CPUPercent)
		}
		p.cpu_count = C.uint32_t(h.GetCpuCount())
		p.memory_total = C.int64_t(h.GetMemTotalBytes())
		p.memory_used = C.int64_t(h.GetMemTotalBytes() - h.GetMemAvailableBytes())
		p.gpus, p.gpu_count = alloc[C.WDGPU](len(h.Gpus)), C.size_t(len(h.Gpus))
		for i, g := range h.Gpus {
			unsafe.Slice(p.gpus, len(h.Gpus))[i] = C.WDGPU{index: C.uint32_t(g.GetIndex()), name: C.CString(g.GetName()),
				utilization: C.double(g.GetUtilPercent()), memory_used: C.int64_t(g.GetMemUsedBytes()),
				memory_total: C.int64_t(g.GetMemTotalBytes()), temperature: optional(g.TempC), power: optional(g.PowerW)}
		}
		p.thermals, p.thermal_count = alloc[C.WDThermal](len(h.ThermalZones)), C.size_t(len(h.ThermalZones))
		for i, z := range h.ThermalZones {
			unsafe.Slice(p.thermals, len(h.ThermalZones))[i] = C.WDThermal{name: C.CString(z.GetName()), celsius: C.double(z.GetTempC())}
		}
		if battery := h.GetBattery(); battery != nil {
			p.battery = C.WDBattery{available: 1, percent: C.double(battery.GetPercent()), state: C.WDBatteryState(battery.GetState())}
			if battery.SecondsRemaining != nil {
				p.battery.has_estimate = 1
				p.battery.seconds_remaining = C.int64_t(*battery.SecondsRemaining)
			}
		}
	}
	if d := snapshot.Storage; d != nil {
		p.has_disk = 1
		p.disk_mount = C.CString(d.GetMountpoint())
		p.disk_used, p.disk_total = C.int64_t(d.GetUsedBytes()), C.int64_t(d.GetTotalBytes())
	}
	p.disk_error = C.CString(snapshot.StorageError)
	p.rows, p.row_count = alloc[C.WDResourceRow](len(snapshot.Rows)), C.size_t(len(snapshot.Rows))
	for i, r := range snapshot.Rows {
		row := &unsafe.Slice(p.rows, len(snapshot.Rows))[i]
		row.id, row.app, row.name = C.CString(r.ID), C.CString(r.App), C.CString(r.Name)
		row.state, row.memory = appState(r.State), C.int64_t(r.MemoryBytes)
		if r.HasCPU {
			row.cpu = optional(&r.CPUPercent)
		}
		if r.IsService {
			row.is_service = 1
		}
	}
	return p
}

//export WDResources
func WDResources(session C.uint64_t) *C.WDResourcesResult {
	return resourceResult(desktop.Resources(uint64(session)))
}

//export WDPorts
func WDPorts(app *C.char, session C.uint64_t) *C.WDPortsResult {
	items, err := desktop.Ports(C.GoString(app), uint64(session))
	p := alloc[C.WDPortsResult](1)
	p.status = status(err)
	p.items, p.count = alloc[C.WDPort](len(items)), C.size_t(len(items))
	for i, port := range items {
		unsafe.Slice(p.items, len(items))[i] = C.WDPort{protocol: C.CString(port.GetProtocol()), address: C.CString(port.GetAddress()), port: C.uint32_t(port.GetPort())}
	}
	return p
}

//export WDResourcesFree
func WDResourcesFree(p *C.WDResourcesResult) {
	if p == nil {
		return
	}
	for _, row := range unsafe.Slice(p.rows, int(p.row_count)) {
		free(row.id)
		free(row.app)
		free(row.name)
	}
	for _, gpu := range unsafe.Slice(p.gpus, int(p.gpu_count)) {
		free(gpu.name)
	}
	for _, zone := range unsafe.Slice(p.thermals, int(p.thermal_count)) {
		free(zone.name)
	}
	free(p.rows)
	free(p.gpus)
	free(p.thermals)
	free(p.disk_mount)
	free(p.disk_error)
	free(p.status.message)
	free(p)
}

//export WDPortsFree
func WDPortsFree(p *C.WDPortsResult) {
	if p == nil {
		return
	}
	for _, port := range unsafe.Slice(p.items, int(p.count)) {
		free(port.protocol)
		free(port.address)
	}
	free(p.items)
	free(p.status.message)
	free(p)
}

//export WDStartApp
func WDStartApp(app *C.char, session C.uint64_t) *C.WDStatus {
	return statusResult(desktop.StartApp(C.GoString(app), uint64(session)))
}

//export WDStopApp
func WDStopApp(app *C.char, session C.uint64_t) *C.WDStatus {
	return statusResult(desktop.StopApp(C.GoString(app), uint64(session)))
}

//export WDDeleteApp
func WDDeleteApp(app *C.char, session C.uint64_t) *C.WDStatus {
	return statusResult(desktop.DeleteApp(C.GoString(app), uint64(session)))
}
