package cbridge

/*
#include "callbacks.h"
*/
import "C"

import (
	"bytes"
	"unsafe"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
)

func readBytes(b C.WendyHostBytes) []byte {
	return bytes.Clone(unsafe.Slice((*byte)(unsafe.Pointer(b.data)), int(b.size)))
}

func readMetadata(m C.WendyHostMetadata) platform.Metadata {
	if m.count == 0 {
		return nil
	}
	out := make(platform.Metadata, int(m.count))
	for i, e := range unsafe.Slice(m.entries, int(m.count)) {
		out[i] = platform.MetadataEntry{Name: string(readBytes(e.name)), Value: readBytes(e.value)}
	}
	return out
}

// All request storage is C-owned for the duration of the callback. In
// particular, metadata arrays contain only C pointers, never Go pointers.
type arena []unsafe.Pointer

func (a *arena) alloc(n uintptr) unsafe.Pointer {
	p := C.malloc(C.size_t(n))
	*a = append(*a, p)
	return p
}

func (a *arena) data(b []byte) C.WendyHostBytes {
	if len(b) == 0 {
		return C.WendyHostBytes{}
	}
	p := a.alloc(uintptr(len(b)))
	copy(unsafe.Slice((*byte)(p), len(b)), b)
	return C.WendyHostBytes{data: (*C.uint8_t)(p), size: C.size_t(len(b))}
}

func (a *arena) metadata(m platform.Metadata) C.WendyHostMetadata {
	if len(m) == 0 {
		return C.WendyHostMetadata{}
	}
	p := (*C.WendyHostMetadataEntry)(a.alloc(uintptr(len(m)) * unsafe.Sizeof(C.WendyHostMetadataEntry{})))
	for i := range m {
		unsafe.Slice(p, len(m))[i] = C.WendyHostMetadataEntry{name: a.data([]byte(m[i].Name)), value: a.data(m[i].Value)}
	}
	return C.WendyHostMetadata{entries: p, count: C.size_t(len(m))}
}

func (a arena) free() {
	for _, p := range a {
		C.free(p)
	}
}
