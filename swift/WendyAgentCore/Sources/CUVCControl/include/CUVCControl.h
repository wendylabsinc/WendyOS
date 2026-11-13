#ifndef WENDY_UVC_CONTROL_H
#define WENDY_UVC_CONTROL_H
#include <stddef.h>
#include <stdint.h>

typedef struct wendy_uvc_device wendy_uvc_device;
// Exact USB identity only. Never seize, reset, detach or reconfigure a device.
int32_t wendy_uvc_open(uint32_t location, uint16_t vendor, uint16_t product, wendy_uvc_device **result);
void wendy_uvc_close(wendy_uvc_device *device);
int32_t wendy_uvc_configuration(wendy_uvc_device *device, uint8_t *bytes, size_t capacity, size_t *count);
int32_t wendy_uvc_request(wendy_uvc_device *device, uint8_t interface_number, uint8_t entity,
                        uint8_t selector, uint8_t request, uint8_t *bytes, uint16_t length);
#endif
