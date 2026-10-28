package main

/*
#include <stdlib.h>
#include "../Sources/Shared/VPNBridge.h"
*/
import "C"
import (
	"context"
	"github.com/wendylabsinc/wendy/go/macos/vpn"
	"sync"
	"time"
	"unsafe"
)

var mu sync.RWMutex
var engine *vpn.Engine

//export WVPNStart
func WVPNStart(input *C.WDVPNConfig) *C.char {
	if input == nil || input.route_count == 0 || input.route_count > 1024 || input.routes == nil {
		return C.CString("select between 1 and 1024 routes")
	}
	cfg := vpn.Config{Endpoint: C.GoString(input.endpoint), Token: C.GoString(input.token)}
	for _, r := range unsafe.Slice(input.routes, int(input.route_count)) {
		cfg.Routes = append(cfg.Routes, vpn.Route{Selector: C.GoString(r.selector), Name: C.GoString(r.name), Address: C.GoString(r.address), Host: C.GoString(r.host)})
	}

	next, err := vpn.NewEngine(cfg)
	if err != nil {
		return C.CString(err.Error())
	}
	mu.Lock()
	old := engine
	engine = next
	mu.Unlock()
	if old != nil {
		old.Close()
	}
	return nil
}

//export WVPNStop
func WVPNStop() {
	mu.Lock()
	old := engine
	engine = nil
	mu.Unlock()
	if old != nil {
		old.Close()
	}
}

//export WVPNWrite
func WVPNWrite(p unsafe.Pointer, n C.int) {
	if n <= 0 || n > 65535 {
		return
	}
	mu.RLock()
	e := engine
	mu.RUnlock()
	if e != nil {
		e.Write(C.GoBytes(p, n))
	}
}

//export WVPNRead
func WVPNRead(out unsafe.Pointer, capacity C.int) C.int {
	mu.RLock()
	e := engine
	mu.RUnlock()
	if e == nil {
		return -1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	p := e.Read(ctx)
	if len(p) > int(capacity) {
		return -2
	}
	copy(unsafe.Slice((*byte)(out), int(capacity)), p)
	return C.int(len(p))
}

//export WVPNFree
func WVPNFree(p *C.char) { C.free(unsafe.Pointer(p)) }
func main()              {}

//export WVPNHealthy
func WVPNHealthy() C.int {
	mu.RLock()
	e := engine
	mu.RUnlock()
	if e != nil && e.Healthy() {
		return 1
	}
	return 0
}
