#ifndef WENDY_ENROLLMENT_TYPES_H
#define WENDY_ENROLLMENT_TYPES_H
#include "BridgeTypes.h"
typedef struct { WDStatus status; int32_t enrolled; char *organization_id, *organization_name; } WDEnrollmentResult;
typedef struct { char *account, *id, *name, *email; } WDEnrollmentOrganization;
typedef struct { WDStatus status; WDEnrollmentOrganization *items; size_t count; } WDEnrollmentOrganizationsResult;
WDEnrollmentResult *WDDeviceEnrollment(uint64_t session);
void WDEnrollmentFree(WDEnrollmentResult *value);
WDEnrollmentOrganizationsResult *WDEnrollmentOrganizations(void);
void WDEnrollmentOrganizationsFree(WDEnrollmentOrganizationsResult *value);
WDStatus *WDEnrollDevice(uint64_t session, char *account, char *organization, char *name);
#endif
