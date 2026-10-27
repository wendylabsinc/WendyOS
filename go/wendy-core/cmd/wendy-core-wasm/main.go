//go:build js && wasm

// Command wendy-core-wasm exposes the same engine to a browser backend.
package main

import (
	"os"
	"syscall/js"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/core"
)

func main() {
	// The loader supplies a unique, short-lived initialization callback name.
	// Separate package instances/versions therefore do not share export globals.
	ready := js.Global().Get(os.Args[1])
	hello := js.FuncOf(func(this js.Value, arguments []js.Value) any {
		return core.WendyHelloWorld()
	})
	version := js.FuncOf(func(this js.Value, arguments []js.Value) any {
		return core.Version()
	})
	ready.Invoke(map[string]any{"WendyHelloWorld": hello, "WendyCoreVersion": version})
	// Keep the Go runtime and callback alive for this module's lifetime.
	select {}
}
