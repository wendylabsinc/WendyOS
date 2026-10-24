#ifndef WENDY_AUTOMATION_TYPES_H
#define WENDY_AUTOMATION_TYPES_H

typedef enum { WD_PAIR_SENSOR, WD_PAIR_IP_CAMERA } WDPairingKind;
typedef struct {
    char *id, *name, *address, *detail;
    uint32_t camera_id;
    int32_t paired, connected, can_pair;
} WDPairing;
typedef struct { WDStatus status; WDPairing *items; size_t count; char *warning; } WDPairingsResult;
typedef struct { char *id, *detail; int32_t healthy; } WDCampaignSource;
typedef struct { WDStatus status; WDCampaignSource *items; size_t count; } WDCampaignSourcesResult;
typedef enum { WD_TRIGGER_DETECTION, WD_TRIGGER_APP_EVENT, WD_TRIGGER_CLASSIFICATION } WDCampaignTrigger;
typedef enum { WD_ACTION_NOTIFY_AND_RECORD, WD_ACTION_RECORD } WDCampaignAction;
typedef struct {
    char *name, *camera, *event;
    WDCampaignTrigger trigger;
    WDCampaignAction action;
    double threshold;
    uint32_t cooldown_seconds, before_seconds, after_seconds;
    int32_t upload;
    char *model, *model_revision;
    char **labels;
    size_t label_count;
} WDCampaignInput;
typedef struct { WDStatus status; char *revision, *license; char **labels; size_t label_count; int64_t weights_bytes; } WDVisionModelResult;
WDVisionModelResult *WDVisionModel(char *repository, WDCampaignTrigger trigger);
void WDVisionModelFree(WDVisionModelResult *value);
typedef struct { char *name, *state, *revision, *summary, *inference_state, *problem, *warnings; } WDCampaign;
typedef struct { WDStatus status; WDCampaign *items; size_t count; } WDCampaignsResult;
typedef struct { char *id, *campaign, *state, *reason; int64_t started_ns, size_bytes; double duration_seconds; } WDCampaignClip;
typedef struct { WDStatus status; WDCampaignClip *items; size_t count; char *warning; } WDCampaignClipsResult;
WDCampaignClipsResult *WDCampaignClips(uint64_t session);
void WDCampaignClipsFree(WDCampaignClipsResult *value);

WDPairingsResult *WDPairings(uint64_t session, WDPairingKind kind);
void WDPairingsFree(WDPairingsResult *value);
WDStatus *WDPairSensor(uint64_t session, char *id);
WDStatus *WDForgetSensor(uint64_t session, char *id);
WDStatus *WDPairIPCamera(uint64_t session, uint32_t id, char *username, char *password);
WDStatus *WDForgetIPCamera(uint64_t session, uint32_t id);
WDCampaignSourcesResult *WDCampaignSources(uint64_t session);
void WDCampaignSourcesFree(WDCampaignSourcesResult *value);
WDCampaignsResult *WDCampaigns(uint64_t session);
void WDCampaignsFree(WDCampaignsResult *value);
WDStatus *WDDeployCampaign(uint64_t session, WDCampaignInput *input);
#endif
