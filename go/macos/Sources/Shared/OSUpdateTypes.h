#ifndef WENDY_OS_UPDATE_TYPES_H
#define WENDY_OS_UPDATE_TYPES_H
#include "BridgeTypes.h"
typedef struct { WDStatus status; char *json; } WDOSUpdateResult;
WDOSUpdateResult *WDOSRelease(uint64_t session);
WDOSUpdateResult *WDOSUpdate(uint64_t session, char *artifact, int32_t start);
WDStatus *WDOSRestart(uint64_t session);
void WDOSUpdatePrepare(uint64_t session);
void WDOSUpdateCancel(uint64_t session);
void WDOSUpdateFree(WDOSUpdateResult *value);
#endif
