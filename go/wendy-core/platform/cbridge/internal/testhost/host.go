// Package testhost supplies a C fake for the cgo conformance tests.
package testhost

/*
#include "fake.h"
*/
import "C"

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
