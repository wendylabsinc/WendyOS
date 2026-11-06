#ifndef WENDY_ACCOUNT_PICKER_TYPES_H
#define WENDY_ACCOUNT_PICKER_TYPES_H
#include "BridgeTypes.h"
typedef struct {
 char *context, *email, *cloud, *organization_id, *organization_name;
 WDAccountState state;
 int32_t active;
} WDAccountChoice;
typedef struct { WDStatus status; WDAccountChoice *items; size_t count; } WDAccountChoicesResult;
WDAccountChoicesResult *WDAccountChoices(void);
void WDAccountChoicesFree(WDAccountChoicesResult *value);
WDStatus *WDUseAccount(char *context);
#endif
