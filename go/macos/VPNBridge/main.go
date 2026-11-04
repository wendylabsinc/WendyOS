package main

/*
#include <stdlib.h>
#include "../Sources/Shared/VPNBridge.h"
*/
import "C"
import (
	"context"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsharing"
	"github.com/wendylabsinc/wendy/go/macos/vpn"
	"sync"
	"time"
	"unsafe"
)

var operationMu sync.Mutex
var controlConfig vpn.Config
var reachability vpn.ReachabilityState
var mu sync.RWMutex
var engine *vpn.Engine
var engineConfig vpn.Config
var refreshCancel context.CancelFunc

//export WVPNStart
func WVPNStart(input *C.WDVPNConfig) *C.char {
	operationMu.Lock()
	defer operationMu.Unlock()
	if input == nil || input.route_count > 1024 || (input.route_count > 0 && input.routes == nil) {
		return C.CString("select between 1 and 1024 routes")
	}
	cfg := vpn.Config{Endpoint: C.GoString(input.endpoint), Token: C.GoString(input.token), BootstrapDirectory: C.GoString(input.bootstrap_directory), Gateway: C.GoString(input.gateway)}
	if C.GoString(input.internet_address4) != "" {
		cfg.Internet = &meshsharing.InternetConfig{Address4: C.GoString(input.internet_address4), Address6: C.GoString(input.internet_address6), MTU: int(input.internet_mtu)}
	}
	for _, r := range unsafe.Slice(input.routes, int(input.route_count)) {
		cfg.Routes = append(cfg.Routes, vpn.Route{Selector: C.GoString(r.selector), Name: C.GoString(r.name), Address: C.GoString(r.address), Host: C.GoString(r.host)})
	}

	cfg, err := platformConfig(cfg)
	if err != nil {
		stopPlatform()
		return C.CString(err.Error())
	}
	mu.Lock()
	controlConfig = cfg
	mu.Unlock()
	reachability = vpn.ReachabilityState{}
	if cfg.Gateway == "auto" {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		cfg, err = vpn.Refresh(ctx, controlConfig, true)
		cancel()
		if err != nil {
			stopPlatform()
			return C.CString(err.Error())
		}
	}
	next, err := vpn.NewEngine(cfg)
	if err != nil {
		stopPlatform()
		return C.CString(err.Error())
	}
	mu.Lock()
	old := engine
	engine = next
	engineConfig = cfg
	mu.Unlock()
	if old != nil {
		old.Close()
	}
	return nil
}

//export WVPNStop
func WVPNStop() {
	mu.RLock()
	cancel := refreshCancel
	mu.RUnlock()
	if cancel != nil {
		cancel()
	}
	operationMu.Lock()
	defer operationMu.Unlock()
	mu.Lock()
	controlConfig = vpn.Config{}
	old := engine
	engine = nil
	engineConfig = vpn.Config{}
	mu.Unlock()
	if old != nil {
		old.Close()
	}
	stopPlatform()
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
	controller := controlConfig
	mu.RUnlock()
	if controller.Gateway == "auto" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if vpn.Healthy(ctx, controller) {
			return 1
		}
		return 0
	}
	mu.RLock()
	e := engine
	mu.RUnlock()
	if e != nil && e.Healthy() {
		return 1
	}
	return 0
}

//export WVPNConfiguration
func WVPNConfiguration() *C.char {
	mu.RLock()
	defer mu.RUnlock()
	data, _ := json.Marshal(engineConfig)
	return C.CString(string(data))
}

//export WVPNRefresh
func WVPNRefresh() *C.char {
	operationMu.Lock()
	defer operationMu.Unlock()
	if controlConfig.Gateway != "auto" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), vpn.RefreshTimeout+10*time.Second)
	defer cancel()
	mu.Lock()
	refreshCancel = cancel
	mu.Unlock()
	defer func() { mu.Lock(); refreshCancel = nil; mu.Unlock() }()
	online := reachability.Observe(vpn.InternetReachable(ctx))
	cfg, err := vpn.Refresh(ctx, controlConfig, online)
	if err != nil {
		return nil
	} // Keep the current device routes on a transient failure.
	mu.RLock()
	current := engineConfig.Endpoint
	mu.RUnlock()
	if cfg.Endpoint == current {
		return nil
	}
	next, err := vpn.NewEngine(cfg)
	if err != nil {
		return nil
	}
	mu.Lock()
	old := engine
	engine, engineConfig = next, cfg
	mu.Unlock()
	if old != nil {
		old.Close()
	}
	data, _ := json.Marshal(cfg)
	return C.CString(string(data))
}
