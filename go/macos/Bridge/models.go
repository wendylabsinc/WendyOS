package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"
import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/cli/modelhub"
	"time"
	"unsafe"
)

//export WDSearchModels
func WDSearchModels(query *C.char) *C.WDModelSearchResult {
	p := alloc[C.WDModelSearchResult](1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rows, err := modelhub.New().Search(ctx, C.GoString(query))
	p.status = status(err)
	p.items, p.count = alloc[C.WDModelRepository](len(rows)), C.size_t(len(rows))
	for i, r := range rows {
		unsafe.Slice(p.items, len(rows))[i] = C.WDModelRepository{id: C.CString(r.ID), name: C.CString(r.Name), description: C.CString(r.Description), license: C.CString(r.License), downloads: C.int64_t(r.Downloads)}
	}
	return p
}

//export WDModelSearchFree
func WDModelSearchFree(p *C.WDModelSearchResult) {
	if p == nil {
		return
	}
	for _, r := range unsafe.Slice(p.items, int(p.count)) {
		free(r.id)
		free(r.name)
		free(r.description)
		free(r.license)
	}
	free(p.items)
	free(p.status.message)
	free(p)
}

//export WDModelFiles
func WDModelFiles(repo *C.char) *C.WDModelFilesResult {
	p := alloc[C.WDModelFilesResult](1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	d, err := modelhub.New().Details(ctx, C.GoString(repo))
	p.status = status(err)
	p.revision, p.license = C.CString(d.Revision), C.CString(d.License)
	p.items, p.count = alloc[C.WDModelFile](len(d.Files)), C.size_t(len(d.Files))
	for i, f := range d.Files {
		unsafe.Slice(p.items, len(d.Files))[i] = C.WDModelFile{name: C.CString(f.Name), quantization: C.CString(f.Quantization), unavailable_reason: C.CString(f.UnavailableReason), bytes: C.int64_t(f.Bytes)}
	}
	return p
}

//export WDModelFilesFree
func WDModelFilesFree(p *C.WDModelFilesResult) {
	if p == nil {
		return
	}
	for _, f := range unsafe.Slice(p.items, int(p.count)) {
		free(f.name)
		free(f.quantization)
		free(f.unavailable_reason)
	}
	free(p.items)
	free(p.revision)
	free(p.license)
	free(p.status.message)
	free(p)
}

//export WDRunModel
func WDRunModel(session, job C.uint64_t, port C.uint32_t, accelerator C.WDModelAccelerator, repo, file, revision *C.char, install, acceptWarning C.int32_t) *C.WDStatus {
	return statusResult(desktop.StartModelRun(uint64(session), uint64(job), uint32(port), uint32(accelerator), C.GoString(repo), C.GoString(file), C.GoString(revision), install != 0, acceptWarning != 0))
}

//export WDModelHardware
func WDModelHardware(session C.uint64_t) *C.WDModelHardwareResult {
	p := alloc[C.WDModelHardwareResult](1)
	h, err := desktop.ModelHardware(uint64(session))
	p.status = status(err)
	p.os, p.architecture, p.gpu, p.jetpack, p.warning = C.CString(h.OS), C.CString(h.Architecture), C.CString(h.GPU), C.CString(h.JetPack), C.CString(h.Warning)
	p.memory_total, p.memory_available, p.disk_free, p.gpu_total, p.gpu_available = C.int64_t(h.MemoryTotal), C.int64_t(h.MemoryAvailable), C.int64_t(h.DiskFree), C.int64_t(h.GPUTotal), C.int64_t(h.GPUAvailable)
	p.disk_known, p.cuda, p.shared_memory = boolInt(h.DiskKnown), boolInt(h.CUDA), boolInt(h.SharedMemory)
	return p
}

//export WDModelHardwareFree
func WDModelHardwareFree(p *C.WDModelHardwareResult) {
	if p == nil {
		return
	}
	free(p.os)
	free(p.architecture)
	free(p.gpu)
	free(p.jetpack)
	free(p.warning)
	free(p.status.message)
	free(p)
}

//export WDModelRunStatus
func WDModelRunStatus(session, job C.uint64_t) *C.WDModelRunResult {
	p := alloc[C.WDModelRunResult](1)
	r, err := desktop.ModelRunStatus(uint64(session), uint64(job))
	p.status = status(err)
	p.port = C.uint32_t(r.Port)
	p.completed, p.total = C.int64_t(r.Completed), C.int64_t(r.Total)
	p.model, p.progress, p.error, p.done = C.CString(r.Model), C.CString(r.Progress), C.CString(r.Error), boolInt(r.Done)
	return p
}

//export WDModelRunFree
func WDModelRunFree(p *C.WDModelRunResult) {
	if p == nil {
		return
	}
	free(p.model)
	free(p.progress)
	free(p.error)
	free(p.status.message)
	free(p)
}

//export WDCancelModelRun
func WDCancelModelRun(session, job C.uint64_t) *C.WDStatus {
	return statusResult(desktop.CancelModelRun(uint64(session), uint64(job)))
}
