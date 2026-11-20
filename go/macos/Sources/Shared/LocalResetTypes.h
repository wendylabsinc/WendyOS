#ifndef WENDY_LOCAL_RESET_TYPES_H
#define WENDY_LOCAL_RESET_TYPES_H
#include "BridgeTypes.h"
typedef struct { WDStatus status; char *principal, *fingerprint; int32_t completed; } WDLocalResetResult;
WDLocalResetResult *WDPrepareLocalReset(uint64_t session, char *account, char *organization, char *device);
WDLocalResetResult *WDResetLocalEnrollment(uint64_t session, char *account, char *organization, char *device, char *principal, char *fingerprint);
void WDLocalResetFree(WDLocalResetResult *value);
#endif
