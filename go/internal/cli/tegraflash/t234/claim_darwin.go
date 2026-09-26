//go:build darwin

package t234

/*
#cgo LDFLAGS: -framework DiskArbitration -framework CoreFoundation
#include <stdlib.h>
#include <string.h>
#include <DiskArbitration/DiskArbitration.h>
#include <dispatch/dispatch.h>

static DASessionRef wendyClaimSession;
static dispatch_queue_t wendyClaimQueue;
static char wendyClaimVendors[2][16];

// wendyIsFlashingLUN reports whether a disk's SCSI vendor is one of the
// flashing initrd's export names (the inquiry vendor field is space-padded).
static Boolean wendyIsFlashingLUN(DADiskRef disk) {
	Boolean match = FALSE;
	CFDictionaryRef desc = DADiskCopyDescription(disk);
	if (desc == NULL) {
		return FALSE;
	}
	CFStringRef vendor = CFDictionaryGetValue(desc, kDADiskDescriptionDeviceVendorKey);
	char buf[32];
	if (vendor != NULL && CFStringGetCString(vendor, buf, sizeof buf, kCFStringEncodingUTF8)) {
		size_t n = strlen(buf);
		while (n > 0 && buf[n - 1] == ' ') {
			buf[--n] = '\0';
		}
		match = strcmp(buf, wendyClaimVendors[0]) == 0 || strcmp(buf, wendyClaimVendors[1]) == 0;
	}
	CFRelease(desc);
	return match;
}

// Peek callbacks run as a disk appears, after it is probed but before it is
// mounted or offered to the user as unreadable; a claimed disk is neither.
static void wendyClaimPeek(DADiskRef disk, void *context) {
	if (wendyIsFlashingLUN(disk)) {
		DADiskClaim(disk, kDADiskClaimOptionDefault, NULL, NULL, NULL, NULL);
	}
}

static int wendyClaimStart(const char *flashpkg, const char *rootfs) {
	if (wendyClaimSession != NULL) {
		return -1;
	}
	strlcpy(wendyClaimVendors[0], flashpkg, sizeof wendyClaimVendors[0]);
	strlcpy(wendyClaimVendors[1], rootfs, sizeof wendyClaimVendors[1]);
	wendyClaimSession = DASessionCreate(kCFAllocatorDefault);
	if (wendyClaimSession == NULL) {
		return -1;
	}
	wendyClaimQueue = dispatch_queue_create("sh.wendy.tegraflash.diskclaim", NULL);
	DARegisterDiskPeekCallback(wendyClaimSession, NULL, 0, wendyClaimPeek, NULL);
	DASessionSetDispatchQueue(wendyClaimSession, wendyClaimQueue);
	return 0;
}

static void wendyClaimNoop(void *context) {}

// Ending the session releases every claim it holds. Draining the queue first
// lets a peek callback already queued finish before the session goes away.
static void wendyClaimStop(void) {
	DASessionSetDispatchQueue(wendyClaimSession, NULL);
	dispatch_sync_f(wendyClaimQueue, NULL, wendyClaimNoop);
	DAUnregisterCallback(wendyClaimSession, (void *)wendyClaimPeek, NULL);
	CFRelease(wendyClaimSession);
	wendyClaimSession = NULL;
	dispatch_release(wendyClaimQueue);
	wendyClaimQueue = NULL;
}
*/
import "C"

import (
	"errors"
	"sync"
	"unsafe"
)

var claimMu sync.Mutex

// SuppressDiskPrompts claims the flashing initrd's disks as macOS attaches
// them, so it neither mounts them nor asks the user about unreadable disks.
// The returned func releases the claims; ejects work either way.
func SuppressDiskPrompts() (release func(), err error) {
	claimMu.Lock()
	defer claimMu.Unlock()
	flashpkg, rootfs := C.CString(FlashpkgVendor), C.CString(RootfsLUNVendor)
	defer C.free(unsafe.Pointer(flashpkg))
	defer C.free(unsafe.Pointer(rootfs))
	if C.wendyClaimStart(flashpkg, rootfs) != 0 {
		return nil, errors.New("could not start a DiskArbitration session")
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			claimMu.Lock()
			defer claimMu.Unlock()
			C.wendyClaimStop()
		})
	}, nil
}
