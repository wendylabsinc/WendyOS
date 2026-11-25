#ifndef WENDY_UNITREE_TYPES_H
#define WENDY_UNITREE_TYPES_H
#include "BridgeTypes.h"
typedef struct { char *serial, *name, *address; int32_t paired, connected; } WDUnitreeRobot;
typedef struct { WDStatus status; WDUnitreeRobot *items; size_t count; char *warning; } WDUnitreeResult;
typedef struct { WDStatus status; uint64_t handle; } WDUnitreeConnection;
WDUnitreeResult *WDUnitreeRobots(uint64_t session);
void WDUnitreeRobotsFree(WDUnitreeResult *value);
uint64_t WDUnitreeAttemptCreate(void);
void WDUnitreeAttemptCancel(uint64_t attempt);
void WDUnitreeAttemptFree(uint64_t attempt);
WDStatus *WDUnitreePair(uint64_t attempt, uint64_t session, char *serial, char *name, char *address, char *region, char *email, char *password);
WDStatus *WDUnitreeForget(uint64_t session, char *serial);
WDUnitreeConnection *WDUnitreeConnect(uint64_t attempt, char *serial, char *address, char *region, char *email, char *password);
void WDUnitreeConnectionFree(WDUnitreeConnection *value);
WDStatus *WDUnitreeMove(uint64_t handle, double forward, double lateral, double yaw);
WDStatus *WDUnitreeClose(uint64_t handle);
typedef struct { WDStatus status; uint8_t *bytes; size_t count; } WDUnitreeVideoFrame;
WDStatus *WDUnitreeVideoStart(uint64_t handle);
WDUnitreeVideoFrame *WDUnitreeVideoNext(uint64_t handle);
void WDUnitreeVideoFrameFree(WDUnitreeVideoFrame *frame);
void WDUnitreeVideoStop(uint64_t handle);
#endif
