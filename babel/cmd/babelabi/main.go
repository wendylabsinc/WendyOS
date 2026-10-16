//go:build cgo

// Experimental C ABI smoke target; build -buildmode=c-archive on iOS or
// -buildmode=c-shared on Android. Not a stable ABI. Each returned string is owned
// by C and must be freed with BabelFree. Destroy must not race other calls.
package main

/*
#include <stdint.h>
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"
	"runtime/cgo"
	"unsafe"

	"github.com/wendylabsinc/WendyOS/babel/mobile"
)

//export BabelNew
func BabelNew(id *C.char, seq C.int) C.uintptr_t {
	if id == nil {
		return 0
	}
	e, err := mobile.New(C.GoString(id), int(seq))
	if err != nil {
		return 0
	}
	return C.uintptr_t(cgo.NewHandle(e))
}

//export BabelStep
func BabelStep(handle C.uintptr_t, now C.int64_t, input *C.char) *C.char {
	e := cgo.Handle(handle).Value().(*mobile.Engine)
	if input == nil {
		return result(nil, errInput{})
	}
	b, err := e.Step(int64(now), []byte(C.GoString(input)))
	return result(b, err)
}

//export BabelCommit
func BabelCommit(handle C.uintptr_t, revision C.int64_t, ok C.int) *C.char {
	e := cgo.Handle(handle).Value().(*mobile.Engine)
	b, err := e.Commit(int64(revision), ok != 0)
	return result(b, err)
}

//export BabelNextDeadlineMS
func BabelNextDeadlineMS(handle C.uintptr_t) C.int64_t {
	return C.int64_t(cgo.Handle(handle).Value().(*mobile.Engine).NextDeadlineMS())
}

//export BabelCheckpoint
func BabelCheckpoint(handle C.uintptr_t) *C.char {
	b, err := cgo.Handle(handle).Value().(*mobile.Engine).Checkpoint()
	return result(b, err)
}

//export BabelRestore
func BabelRestore(state *C.char) C.uintptr_t {
	if state == nil {
		return 0
	}
	e, err := mobile.Restore([]byte(C.GoString(state)))
	if err != nil {
		return 0
	}
	return C.uintptr_t(cgo.NewHandle(e))
}

//export BabelDestroy
func BabelDestroy(handle C.uintptr_t) { cgo.Handle(handle).Delete() }

//export BabelFree
func BabelFree(p *C.char) { C.free(unsafe.Pointer(p)) }
func result(b []byte, err error) *C.char {
	if err != nil {
		b, _ = json.Marshal(struct{ Error string }{err.Error()})
	}
	return C.CString(string(b))
}

type errInput struct{}

func (errInput) Error() string { return "nil input" }
func main()                    {}
