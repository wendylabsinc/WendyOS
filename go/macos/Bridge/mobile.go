package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

//export WDInitializeMobile
func WDInitializeMobile(directory *C.char) *C.WDStatus {
	if runtime.GOOS != "ios" {
		return statusResult(errors.New("mobile initialization requires iOS"))
	}
	path := C.GoString(directory)
	if !filepath.IsAbs(path) {
		return statusResult(errors.New("app configuration directory must be absolute"))
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return statusResult(err)
	}
	if err := os.Setenv("WENDY_CONFIG_DIR", path); err != nil {
		return statusResult(err)
	}
	return statusResult(desktop.Initialize(""))
}
