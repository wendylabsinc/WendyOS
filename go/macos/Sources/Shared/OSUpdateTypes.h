#ifndef WENDY_OS_UPDATE_TYPES_H
#define WENDY_OS_UPDATE_TYPES_H
#include "BridgeTypes.h"
typedef struct { WDStatus status; char *json; } WDOSUpdateResult;
WDOSUpdateResult *WDOSUpdate(uint64_t session, char *artifact, int32_t start);
void WDOSUpdateFree(WDOSUpdateResult *value);
#endif
