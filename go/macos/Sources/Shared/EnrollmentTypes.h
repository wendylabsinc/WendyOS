#ifndef WENDY_ENROLLMENT_TYPES_H
#define WENDY_ENROLLMENT_TYPES_H
#include "BridgeTypes.h"
typedef struct { WDStatus status; int32_t enrolled; char *organization_id, *organization_name, *device_id; } WDEnrollmentResult;
typedef struct { char *account, *id, *name, *email; } WDEnrollmentOrganization;
typedef struct { WDStatus status; WDEnrollmentOrganization *items; size_t count; } WDEnrollmentOrganizationsResult;
WDEnrollmentResult *WDDeviceEnrollment(uint64_t session);
void WDEnrollmentFree(WDEnrollmentResult *value);
WDEnrollmentOrganizationsResult *WDEnrollmentOrganizations(void);
void WDEnrollmentOrganizationsFree(WDEnrollmentOrganizationsResult *value);
WDStatus *WDEnrollDevice(uint64_t session, char *account, char *organization, char *name);
typedef struct { char *address, *name; int32_t rssi; } WDNearbyDevice;
typedef struct { WDStatus status; WDNearbyDevice *items; size_t count; } WDNearbyDevicesResult;
typedef struct {
    WDStatus status;
    uint64_t session;
    int32_t connected, lite, enrolled, attempted, pending, completed, wifi_connected;
    char *ssid, *device_id;
    char **networks;
    size_t network_count;
} WDNearbySetupResult;
WDNearbyDevicesResult *WDNearbyDiscover(void);
void WDNearbyDevicesFree(WDNearbyDevicesResult *value);
WDNearbySetupResult *WDNearbyOpen(char *address);
WDNearbySetupResult *WDNearbyAction(uint64_t session, int32_t action, char *ssid, char *password, char *account, char *organization, char *name);
void WDNearbyClose(uint64_t session);
void WDNearbySetupFree(WDNearbySetupResult *value);
#endif
