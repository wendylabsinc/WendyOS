// Command wendy-core-native builds the C shared library incarnation.
package main

/*
#include "../../include/WendyCore/WendyCore.h"
*/
import "C"

import "github.com/wendylabsinc/WendyOS/go/wendy-core/core"
import _ "github.com/wendylabsinc/WendyOS/go/wendy-core/platform/cbridge"

//export WendyHelloWorld
func WendyHelloWorld() C.int32_t {
	return C.int32_t(core.WendyHelloWorld())
}

// Keep one C allocation for the process lifetime. No Go pointer crosses the ABI.
var version = C.CString(core.Version())

//export WendyCoreVersion
func WendyCoreVersion() *C.char {
	return version
}

func main() {}
