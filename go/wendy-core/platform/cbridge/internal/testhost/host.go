// Package testhost supplies a C fake for the cgo conformance tests.
package testhost

/*
#include "fake.h"
#include <stdlib.h>
*/
import "C"
import "unsafe"

type Host struct{ p *C.FakeHost }

func New(mode int) *Host                { return &Host{C.fake_create(C.int(mode))} }
func (h *Host) ID() uint64              { return uint64(C.fake_id(h.p)) }
func (h *Host) Free()                   { C.fake_free(h.p) }
func (h *Host) Stat(field int) uint64   { return uint64(C.fake_stat(h.p, C.int(field))) }
func (h *Host) Release()                { C.WendyHostRelease(C.WendyHost(h.ID())) }
func Complete(call uint64, code uint32) { C.fake_complete(C.WendyHostCall(call), C.uint32_t(code)) }
func Push(call uint64) bool             { return C.fake_push(C.WendyHostCall(call)) != 0 }
func Header(call uint64) bool           { return C.fake_header(C.WendyHostCall(call)) != 0 }
func InvalidCreate() uint64             { return uint64(C.WendyHostCreate(nil)) }

func StaleComplete(call uint64) { C.fake_stale_complete(C.WendyHostCall(call)) }

// Cloud configures responses before activating an operation. All buffers copy.
func (h *Host) Cloud(key, first, second []byte) {
	a, b, c := C.CBytes(key), C.CBytes(first), C.CBytes(second)
	defer C.free(a)
	defer C.free(b)
	defer C.free(c)
	C.fake_cloud_configure(h.p, C.WendyHostBytes{data: (*C.uint8_t)(a), size: C.size_t(len(key))},
		C.WendyHostBytes{data: (*C.uint8_t)(b), size: C.size_t(len(first))},
		C.WendyHostBytes{data: (*C.uint8_t)(c), size: C.size_t(len(second))})
}
func (h *Host) CloudView(field int) []byte {
	v := C.fake_cloud_view(h.p, C.int(field))
	return C.GoBytes(unsafe.Pointer(v.data), C.int(v.size))
}

type Completion struct{ p *C.FakeCompletion }

func CloudCreate(host uint64, request []byte) *Completion {
	p := C.CBytes(request)
	defer C.free(p)
	c := C.fake_cloud_create(C.WendyHost(host), C.WendyDataView{data: (*C.uint8_t)(p), size: C.size_t(len(request))})
	if len(request) != 0 {
		clear(unsafe.Slice((*byte)(p), len(request)))
	} // Create must copy.
	return &Completion{p: c}
}
func (c *Completion) Start()                { C.fake_cloud_start(c.p) }
func (c *Completion) Cancel()               { C.fake_cloud_cancel(c.p) }
func (c *Completion) ReleaseOperation()     { C.fake_cloud_release_operation(c.p) }
func (c *Completion) ReleaseInCallback()    { C.fake_cloud_release_in_callback(c.p) }
func (c *Completion) Free()                 { C.fake_cloud_free(c.p) }
func (c *Completion) Stat(field int) uint64 { return uint64(C.fake_cloud_stat(c.p, C.int(field))) }
func (c *Completion) Result(field int) []byte {
	v := C.fake_cloud_result(c.p, C.int(field))
	return C.GoBytes(unsafe.Pointer(v.data), C.int(v.size))
}
func InvalidCloudCreate() bool { return C.fake_cloud_invalid_create() != 0 }
