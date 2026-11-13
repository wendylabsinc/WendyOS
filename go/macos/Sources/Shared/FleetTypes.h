#ifndef WENDY_FLEET_TYPES_H
#define WENDY_FLEET_TYPES_H
typedef struct { char *id, *name, *selector, *site_id, *site, *hardware, *address; int32_t online; int64_t last_seen; } WDFleetDevice;
typedef struct { WDStatus status; WDFleetDevice *items; size_t count; char *presence_error; } WDFleetResult;
WDFleetResult *WDFleet(char *account, char *org);
void WDFleetFree(WDFleetResult *value);
#endif
