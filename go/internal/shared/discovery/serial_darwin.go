//go:build (darwin) && !ios

package discovery

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <IOKit/IOKitLib.h>
#include <IOKit/serial/IOSerialKeys.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>

static char* stringProp(io_service_t svc, CFStringRef key) {
	CFTypeRef v = IORegistryEntryCreateCFProperty(svc, key, kCFAllocatorDefault, 0);
	if (!v || CFGetTypeID(v) != CFStringGetTypeID()) {
		if (v) CFRelease(v);
		return NULL;
	}
	CFIndex n = CFStringGetMaximumSizeForEncoding(
		CFStringGetLength((CFStringRef)v), kCFStringEncodingUTF8) + 1;
	char *buf = (char*)malloc(n);
	if (!CFStringGetCString((CFStringRef)v, buf, n, kCFStringEncodingUTF8)) {
		free(buf);
		CFRelease(v);
		return NULL;
	}
	CFRelease(v);
	return buf;
}

static int intProp(io_service_t svc, CFStringRef key, int *out) {
	CFTypeRef v = IORegistryEntryCreateCFProperty(svc, key, kCFAllocatorDefault, 0);
	if (!v || CFGetTypeID(v) != CFNumberGetTypeID()) {
		if (v) CFRelease(v);
		return 0;
	}
	CFNumberGetValue((CFNumberRef)v, kCFNumberIntType, out);
	CFRelease(v);
	return 1;
}

typedef struct { char **paths; int count; } WendySerialList;

static WendySerialList wendy_find_usb_serial(int wantVID, int wantPID, int wantInterface) {
	WendySerialList result = {NULL, 0};
	int cap = 8;
	result.paths = (char**)malloc(cap * sizeof(char*));

	CFMutableDictionaryRef match = IOServiceMatching(kIOSerialBSDServiceValue);
	if (!match) return result;

	io_iterator_t iter = IO_OBJECT_NULL;
	if (IOServiceGetMatchingServices(0, match, &iter) != KERN_SUCCESS)
		return result;

	io_service_t svc;
	while ((svc = IOIteratorNext(iter)) != IO_OBJECT_NULL) {
		char *path = stringProp(svc, CFSTR(kIOCalloutDeviceKey));
		if (!path) { IOObjectRelease(svc); continue; }

		// Walk up the registry tree to find the USB node with VID/PID.
		int matched = 0;
		int interfaceNumber = -1;
		io_service_t cur = svc;
		IOObjectRetain(cur);
		io_service_t parent;
		while (IORegistryEntryGetParentEntry(cur, kIOServicePlane, &parent) == KERN_SUCCESS) {
			IOObjectRelease(cur);
			cur = parent;
			if (interfaceNumber < 0) intProp(cur, CFSTR("bInterfaceNumber"), &interfaceNumber);
			int vid = 0, pid = 0;
			if ((wantInterface < 0 || interfaceNumber >= 0) &&
			    intProp(cur, CFSTR("idVendor"), &vid) && intProp(cur, CFSTR("idProduct"), &pid)) {
				matched = (vid == wantVID && pid == wantPID &&
				    (wantInterface < 0 || interfaceNumber == wantInterface));
				break;
			}
		}
		IOObjectRelease(cur);

		if (matched) {
			if (result.count >= cap) {
				cap *= 2;
				char **tmp = (char**)realloc(result.paths, cap * sizeof(char*));
				if (!tmp) {
					free(path);
					IOObjectRelease(svc);
					goto cleanup;
				}
				result.paths = tmp;
			}
			result.paths[result.count++] = path;
		} else {
			free(path);
		}
		IOObjectRelease(svc);
	}
cleanup:
	IOObjectRelease(iter);
	return result;
}

static void wendy_free_serial_list(WendySerialList list) {
	for (int i = 0; i < list.count; i++) free(list.paths[i]);
	free(list.paths);
}
*/
import "C"

import (
	"os"
	"unsafe"
)

// ResolveESP32SerialPorts returns all connected serial ports whose USB VID/PID
// match a supported native or USB-to-UART interface, along with each device
// node's plug-in time.
func resolveESP32SerialPorts() ([]SerialPortInfo, error) {
	var result []SerialPortInfo
	seen := make(map[string]struct{})
	for _, id := range supportedESP32SerialUSBIDs {
		iface := -1
		if !serialUSBInterfaceAllowed(id, -1) {
			// macOS attaches the serial node to the CDC data interface.
			iface = 1
		}
		list := C.wendy_find_usb_serial(C.int(id.vendorID), C.int(id.productID), C.int(iface))
		count := int(list.count)
		paths := unsafe.Slice(list.paths, count)
		for _, cp := range paths {
			path := C.GoString(cp)
			if _, ok := seen[path]; ok {
				continue
			}
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			seen[path] = struct{}{}
			result = append(result, SerialPortInfo{
				Port:           path,
				ConnectionTime: info.ModTime(),
				Transport:      id.transport,
			})
		}
		C.wendy_free_serial_list(list)
	}
	return result, nil
}
