#include "CUVCControl.h"
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/IOCFPlugIn.h>
#include <IOKit/usb/IOUSBLib.h>
#include <stdlib.h>
#include <string.h>

// The public legacy USB device user client permits control requests without
// taking ownership of the video interface. IOUSBHost capture/seize is NOT used.
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
struct wendy_uvc_device {
    IOUSBDeviceInterface500 **interface;
};

static int property_matches(io_service_t service, CFStringRef name, uint32_t expected) {
    CFTypeRef property = IORegistryEntryCreateCFProperty(service, name, kCFAllocatorDefault, 0);
    int64_t value = -1;
    if (property && CFGetTypeID(property) == CFNumberGetTypeID()) {
        CFNumberGetValue((CFNumberRef)property, kCFNumberSInt64Type, &value);
    }
    if (property) CFRelease(property);
    return value >= 0 && (uint64_t)value == expected;
}

int32_t wendy_uvc_open(uint32_t location, uint16_t vendor, uint16_t product, wendy_uvc_device **result) {
    if (!result) return kIOReturnBadArgument;
    *result = NULL;
    io_iterator_t iterator = IO_OBJECT_NULL;
    kern_return_t status = IOServiceGetMatchingServices(kIOMainPortDefault,
                                                       IOServiceMatching(kIOUSBDeviceClassName), &iterator);
    if (status != KERN_SUCCESS) return status;
    io_service_t chosen = IO_OBJECT_NULL;
    io_service_t service;
    while ((service = IOIteratorNext(iterator)) != IO_OBJECT_NULL) {
        if (property_matches(service, CFSTR(kUSBDevicePropertyLocationID), location) &&
            property_matches(service, CFSTR(kUSBVendorID), vendor) &&
            property_matches(service, CFSTR(kUSBProductID), product)) {
            if (chosen != IO_OBJECT_NULL) {
                IOObjectRelease(service); IOObjectRelease(chosen); IOObjectRelease(iterator);
                return kIOReturnBadArgument; // ambiguous identity; no first-match fallback
            }
            chosen = service;
        } else { IOObjectRelease(service); }
    }
    IOObjectRelease(iterator);
    if (chosen == IO_OBJECT_NULL) return kIOReturnNotFound;
    IOCFPlugInInterface **plugin = NULL;
    SInt32 score = 0;
    status = IOCreatePlugInInterfaceForService(chosen, kIOUSBDeviceUserClientTypeID,
                                             kIOCFPlugInInterfaceID, &plugin, &score);
    IOObjectRelease(chosen);
    if (status != KERN_SUCCESS || !plugin) return status != KERN_SUCCESS ? status : kIOReturnNotFound;
    IOUSBDeviceInterface500 **interface = NULL;
    HRESULT query = (*plugin)->QueryInterface(plugin, CFUUIDGetUUIDBytes(kIOUSBDeviceInterfaceID500), (LPVOID *)&interface);
    IODestroyPlugInInterface(plugin);
    if (query != S_OK || !interface) return kIOReturnUnsupported;
    status = (*interface)->USBDeviceOpen(interface); // non-seizing; preserve other clients
    if (status != KERN_SUCCESS) { (*interface)->Release(interface); return status; }
    wendy_uvc_device *device = calloc(1, sizeof(*device));
    if (!device) { (*interface)->USBDeviceClose(interface); (*interface)->Release(interface); return kIOReturnNoMemory; }
    device->interface = interface;
    *result = device;
    return kIOReturnSuccess;
}

void wendy_uvc_close(wendy_uvc_device *device) {
    if (!device) return;
    (*device->interface)->USBDeviceClose(device->interface);
    (*device->interface)->Release(device->interface);
    free(device);
}

int32_t wendy_uvc_configuration(wendy_uvc_device *device, uint8_t *bytes, size_t capacity, size_t *count) {
    if (!device || !bytes || !count) return kIOReturnBadArgument;
    *count = 0;
    UInt8 current = 0, configurations = 0;
    IOReturn status = (*device->interface)->GetConfiguration(device->interface, &current);
    if (status != kIOReturnSuccess) return status;
    status = (*device->interface)->GetNumberOfConfigurations(device->interface, &configurations);
    if (status != kIOReturnSuccess) return status;
    for (UInt8 i = 0; i < configurations; i++) {
        IOUSBConfigurationDescriptorPtr descriptor = NULL;
        status = (*device->interface)->GetConfigurationDescriptorPtr(device->interface, i, &descriptor);
        if (status != kIOReturnSuccess) return status;
        if (!descriptor || descriptor->bConfigurationValue != current) continue;
        uint16_t length = USBToHostWord(descriptor->wTotalLength);
        if (length < sizeof(IOUSBConfigurationDescriptor) || length > capacity) return kIOReturnNoSpace;
        memcpy(bytes, descriptor, length); *count = length;
        return kIOReturnSuccess;
    }
    return kIOReturnNotFound;
}

int32_t wendy_uvc_request(wendy_uvc_device *device, uint8_t interface_number, uint8_t entity,
                        uint8_t selector, uint8_t request, uint8_t *bytes, uint16_t length) {
    if (!device || !bytes || length == 0 || length > 64) return kIOReturnBadArgument;
    // Only UVC GET requests and SET_CUR. No arbitrary USB/vendor requests.
    if (request != 0x01 && (request < 0x81 || request > 0x87)) return kIOReturnBadArgument;
    IOUSBDevRequestTO transfer = {0};
    transfer.bmRequestType = request == 0x01 ? 0x21 : 0xa1;
    transfer.bRequest = request;
    transfer.wValue = (uint16_t)selector << 8;
    transfer.wIndex = ((uint16_t)entity << 8) | interface_number;
    transfer.wLength = length;
    transfer.pData = bytes;
    transfer.noDataTimeout = 1000;
    transfer.completionTimeout = 1000;
    IOReturn status = (*device->interface)->DeviceRequestTO(device->interface, &transfer);
    if (status == kIOReturnSuccess && transfer.wLenDone != length) return kIOReturnUnderrun;
    return status;
}
#pragma clang diagnostic pop
