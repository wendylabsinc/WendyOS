#ifndef WD_SITE_MODEL_TYPES_H
#define WD_SITE_MODEL_TYPES_H
typedef struct { WDStatus status; char *json; } WDSiteModelsResult;
WDSiteModelsResult *WDSiteModels(uint64_t session, char *action, char *path, char *name, char *id);
void WDSiteModelsFree(WDSiteModelsResult *value);
#endif
