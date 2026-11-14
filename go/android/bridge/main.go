//go:build android

package main

/*
#include <stdlib.h>
void *WendyPlatform(void *input, int length, int *outputLength);
*/
import "C"
import (
	"errors"
	"github.com/wendylabsinc/wendy/go/android/mobile"
	"github.com/wendylabsinc/wendy/go/internal/shared/androidhost"
	"unsafe"
)

var client = mobile.New()

//export WendyInvoke
func WendyInvoke(input *C.char, length C.int, outputLength *C.int) unsafe.Pointer {
	output := client.Invoke(C.GoBytes(unsafe.Pointer(input), length))
	*outputLength = C.int(len(output))
	return C.CBytes(output)
}
func main() {}

func init() {
	androidhost.Invoke = func(input []byte) ([]byte, error) {
		buffer := C.CBytes(input)
		defer C.free(buffer)
		var length C.int
		output := C.WendyPlatform(buffer, C.int(len(input)), &length)
		if output == nil {
			return nil, errors.New("Android platform request failed")
		}
		defer C.free(output)
		return C.GoBytes(output, length), nil
	}
}
