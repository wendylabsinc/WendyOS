#ifndef WENDY_SITE_ROBOT_TYPES_H
#define WENDY_SITE_ROBOT_TYPES_H
#include "UnitreeTypes.h"
typedef struct { double x, z; } WDSiteRoutePoint;
typedef struct { double x, z, yaw, floor_y, rms, coverage; } WDSiteRobotCandidate;
typedef struct { WDStatus status; WDSiteRobotCandidate *items; size_t count; } WDSiteRobotCandidates;
typedef struct {
    WDStatus status;
    char *message;
    int32_t pose, ready, aligned, moving;
    int32_t arrival_timing, using_arrival_timing, timing_unavailable, scan_ready;
    int32_t under_body_allowance, look_state, look_direction;
    double x, z, yaw, floor_y;
    uint64_t observation_revision;
    WDSiteRoutePoint *route;
    size_t route_count;
    double *joints;
    size_t joint_count;
} WDSiteRobotState;
WDUnitreeConnection *WDSiteRobotOpen(uint64_t session, char *serial);
WDSiteRobotState *WDSiteRobotPoll(uint64_t handle);
WDUnitreeVideoFrame *WDSiteRobotVideoNext(uint64_t handle);
void WDSiteRobotStateFree(WDSiteRobotState *state);
WDStatus *WDSiteRobotAlign(uint64_t handle, uint64_t sequence, char *frame, double x, double z, double floor_y, double yaw);
WDStatus *WDSiteRobotDestination(uint64_t handle, uint64_t sequence, char *frame, double x, double z, int32_t move);
WDStatus *WDSiteRobotLook(uint64_t handle, uint64_t sequence, int32_t direction);
void WDSiteRobotStop(uint64_t handle, uint64_t sequence);
WDStatus *WDSiteRobotArrivalTiming(uint64_t handle, uint64_t sequence, int32_t enabled);
WDStatus *WDSiteRobotUnderBodyAllowance(uint64_t handle, uint64_t sequence, int32_t enabled);
WDSiteRobotCandidates *WDSiteRobotLocalize(uint64_t handle, uint64_t sequence, char *frame, double *vertices, size_t vertex_count);
void WDSiteRobotCandidatesFree(WDSiteRobotCandidates *value);
WDStatus *WDSiteRobotConfirmLocalization(uint64_t handle, uint64_t sequence, uint64_t match_sequence, char *frame, int32_t candidate);
typedef struct { WDStatus status; char *json; } WDSiteRobotFusionResult;
WDSiteRobotFusionResult *WDSiteRobotFuse(uint64_t handle, uint64_t sequence, char *request);
void WDSiteRobotFusionFree(WDSiteRobotFusionResult *value);
void WDSiteRobotClose(uint64_t handle);
#endif
