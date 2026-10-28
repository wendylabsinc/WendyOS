#ifndef WENDY_VPN_BRIDGE_H
#define WENDY_VPN_BRIDGE_H
#include "BridgeTypes.h"
char *WVPNStart(WDVPNConfig *config);
void WVPNStop(void);
int WVPNHealthy(void);
void WVPNWrite(void *bytes, int length);
int WVPNRead(void *bytes, int capacity);
void WVPNFree(char *pointer);
#endif
