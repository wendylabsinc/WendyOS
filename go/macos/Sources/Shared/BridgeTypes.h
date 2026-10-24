#ifndef WENDY_BRIDGE_TYPES_H
#define WENDY_BRIDGE_TYPES_H
#include <stdint.h>
#include <stddef.h>

typedef enum { WD_TRANSPORT_LAN, WD_TRANSPORT_MESH, WD_TRANSPORT_CLOUD, WD_TRANSPORT_USB } WDTransport;
typedef enum { WD_APP_STOPPED, WD_APP_RUNNING, WD_APP_CRASH_LOOPING, WD_APP_UNKNOWN } WDAppState;
typedef enum {
    WD_EVENT_LOGIN_URL = 1, WD_EVENT_LOGIN_DONE, WD_EVENT_LOGIN_ERROR,
    WD_EVENT_LOG, WD_EVENT_LOGS_ERROR, WD_EVENT_VIDEO, WD_EVENT_CAMERA_ERROR, WD_EVENT_JPEG, WD_EVENT_THERMAL
} WDEventKind;

// All returned memory is C-owned. Free the root with its matching Free function.
// Nested pointers live until that call; clients must copy anything they retain.
// Input strings/arrays are borrowed for the duration of the call, never mutated.
typedef struct { char *message; int32_t can_forget_identity; } WDStatus;
typedef struct { char *id, *name, *selector, *address; WDTransport transport; int32_t lite; } WDDevice;
typedef struct { char *name; WDAppState state; } WDService;
typedef struct {
    char *name, *version, *termination_reason;
    WDAppState state;
    WDService *services; size_t service_count;
    uint32_t http_port;
} WDApp;
typedef struct { uint32_t id; char *name, *path; int32_t thermal; } WDCamera;
typedef struct { char *selector, *name; } WDRouteInput;
typedef struct { char *selector, *name, *address, *host; } WDRoute;
typedef struct { char *endpoint, *token; WDRoute *routes; size_t route_count; } WDVPNConfig;
typedef enum { WD_LOG_UNKNOWN, WD_LOG_TRACE, WD_LOG_DEBUG, WD_LOG_INFO, WD_LOG_WARN, WD_LOG_ERROR, WD_LOG_FATAL } WDLogLevel;
typedef struct { uint64_t timestamp_ns; WDLogLevel level; char *body, *app, *service; } WDLogRecord;
// Thermal image bytes are packed RGBA8 from the same frame as these extrema.
// Raw values have no implied temperature unit. Coordinates are sensor pixels.
typedef struct {
 uint32_t width, height, cold_x, cold_y, hot_x, hot_y;
 uint16_t cold_raw, hot_raw;
 uint64_t timestamp_ns;
} WDThermalFrame;
typedef struct {
    WDThermalFrame thermal;
    WDLogRecord log;
    WDEventKind kind;
    uint64_t session, stream;
    char *text;                 // Text events only; NULL for video/login-done.
    uint8_t *bytes; size_t byte_count; // H.264 for WD_EVENT_VIDEO; complete JPEG for WD_EVENT_JPEG.
} WDEvent;

typedef enum { WD_ACCOUNT_SIGNED_OUT, WD_ACCOUNT_SIGNED_IN, WD_ACCOUNT_NEEDS_LOGIN } WDAccountState;
typedef struct { WDStatus status; WDAccountState state; char *context, *cloud, *email; } WDAccountResult;
typedef struct { WDAccountState state; char *context, *cloud, *email; int32_t active; } WDAccountEntry;
typedef struct { WDStatus status; WDAccountEntry *items; size_t count; } WDAccountsResult;
typedef struct { WDStatus status; WDDevice *items; size_t count; } WDDevicesResult;
typedef struct { WDStatus status; WDApp *items; size_t count; } WDAppsResult;
typedef struct { WDStatus status; char *url; } WDOpenAppResult;
typedef struct { WDStatus status; WDCamera *items; size_t count; } WDCamerasResult;
typedef struct { WDStatus status; uint64_t session; int32_t lite; } WDConnectionResult;
typedef struct { WDStatus status; WDVPNConfig config; } WDVPNResult;
typedef struct { WDEvent *items; size_t count; } WDEvents;
typedef struct { int32_t available; double value; } WDOptionalDouble;
typedef struct { uint32_t index; char *name; double utilization; int64_t memory_used, memory_total; WDOptionalDouble temperature, power; } WDGPU;
typedef struct { char *name; double celsius; } WDThermal;
typedef enum { WD_BATTERY_UNKNOWN, WD_BATTERY_CHARGING, WD_BATTERY_DISCHARGING, WD_BATTERY_FULL, WD_BATTERY_NOT_CHARGING } WDBatteryState;
typedef struct { int32_t available; double percent; WDBatteryState state; int32_t has_estimate; int64_t seconds_remaining; } WDBattery;
typedef struct {
 char *id, *app, *name; WDAppState state; WDOptionalDouble cpu; int64_t memory; int32_t is_service;
} WDResourceRow;
typedef struct {
 WDStatus status; int32_t has_host; WDOptionalDouble cpu; uint32_t cpu_count;
 int64_t memory_used, memory_total;
 int32_t has_disk; char *disk_mount, *disk_error; int64_t disk_used, disk_total;
 WDBattery battery;
 WDGPU *gpus; size_t gpu_count;
 WDThermal *thermals; size_t thermal_count;
 WDResourceRow *rows; size_t row_count;
} WDResourcesResult;
typedef struct { char *protocol, *address; uint32_t port; } WDPort;
typedef struct { WDStatus status; WDPort *items; size_t count; } WDPortsResult;

typedef struct { char *id, *name; } WDOrganization;
typedef struct { WDStatus status; WDOrganization *items; size_t count; } WDOrganizationsResult;
typedef struct { char *id, *name, *email, *role_id, *profile_error; } WDCloudMember;
typedef struct { char *id, *name, *description; char **permissions; size_t permission_count; } WDCloudRole;
typedef enum { WD_PRINCIPAL_UNKNOWN, WD_PRINCIPAL_USER, WD_PRINCIPAL_GROUP, WD_PRINCIPAL_DEVICE } WDPrincipalType;
typedef enum { WD_TARGET_UNKNOWN, WD_TARGET_DEVICE, WD_TARGET_SITE, WD_TARGET_TAG, WD_TARGET_ALL } WDAccessTarget;
typedef enum { WD_ACCESS_UNKNOWN, WD_ACCESS_ALLOW, WD_ACCESS_DENY } WDAccessEffect;
typedef struct {
 char *id, *principal_id, *principal_name, *target_id, *target_name;
 WDPrincipalType principal_type; WDAccessTarget target_type; WDAccessEffect effect;
} WDAccessEntry;
typedef struct { char *id, *role; int64_t expires; int32_t revoked; } WDCloudInvite;
typedef struct {
 WDStatus status; char *name;
 WDCloudMember *members; size_t member_count;
 WDCloudRole *roles; size_t role_count;
 WDOrganization *devices; size_t device_count;
 WDAccessEntry *access; size_t access_count;
 WDCloudInvite *invites; size_t invite_count;
 char **permissions; size_t permission_count;
 char *team_error, *roles_error, *devices_error, *access_error, *invites_error, *permissions_error;
} WDOrganizationResult;
typedef struct { WDStatus status; char *link; } WDInviteLinkResult;
#endif
