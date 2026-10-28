#ifndef WENDY_SETTINGS_TYPES_H
#define WENDY_SETTINGS_TYPES_H
typedef struct {
    char *ssid;
    int32_t security, signal, has_signal, known, connected, priority;
} WDWiFiNetwork;
typedef struct {
    char *principal, *device_id, *address, *transport, *name;
    uint64_t link_id;
    uint32_t cost;
} WDMeshPeer;
typedef struct {
    char *prefix, *origin, *next_hop;
    uint32_t metric;
    int32_t local, unreachable, authorized;
} WDMeshRoute;
typedef struct {
    char *principal, *device_id, *name, *prefix;
} WDMeshAnnouncement;
typedef struct {
    WDStatus status;
    int32_t topology_available, topology_runtime, topology_truncated;
    int64_t topology_captured_ms;
    char *topology_error, *self_principal, *self_address;
    WDMeshPeer *mesh_links;
    size_t mesh_link_count;
    WDMeshRoute *mesh_routes;
    size_t mesh_route_count;
    WDMeshAnnouncement *mesh_announcements;
    size_t mesh_announcement_count;
    int32_t wifi_available, wifi_connected;
    char *ssid, *wifi_error, *scan_error, *known_error, *mesh_error;
    WDWiFiNetwork *networks, *known;
    size_t network_count, known_count;
    int32_t mesh_available, runtime_available;
    uint32_t mesh_config, peers;
    char *mesh_state, *mesh_detail, *gateway;
} WDNetworkSettingsResult;
WDNetworkSettingsResult *WDNetworkSettings(uint64_t session);
void WDNetworkSettingsFree(WDNetworkSettingsResult *value);
WDStatus *WDJoinWiFi(uint64_t session, char *ssid, char *password, int32_t security, int32_t hidden);
WDStatus *WDDisconnectWiFi(uint64_t session);
WDStatus *WDForgetWiFi(uint64_t session, char *ssid);
WDStatus *WDConfigureMesh(uint64_t session, uint32_t changed, uint32_t enabled);
typedef struct {
    char *name, *address, *device_type;
    int32_t rssi, paired, connected, trusted;
} WDBluetoothDevice;
typedef struct {
    WDStatus status;
    WDBluetoothDevice *devices;
    size_t count;
} WDBluetoothResult;
WDBluetoothResult *WDBluetoothScan(uint64_t session);
void WDBluetoothFree(WDBluetoothResult *value);
WDStatus *WDBluetoothAction(uint64_t session, char *address, int32_t action);
typedef struct {
    uint32_t id;
    char *name, *description;
    int32_t direction, is_default;
} WDAudioDevice;
typedef struct {
    WDStatus status;
    WDAudioDevice *devices;
    size_t count;
} WDAudioResult;
WDAudioResult *WDAudioDevices(uint64_t session);
void WDAudioFree(WDAudioResult *value);
WDStatus *WDSetDefaultAudio(uint64_t session, uint32_t id, int32_t direction);
typedef struct {
    WDStatus status;
    char *name, *revision, *before, *after, *upload, *cooldown;
    int32_t inference;
    double threshold;
} WDCampaignSettingsResult;
WDCampaignSettingsResult *WDCampaignSettings(uint64_t session, char *name);
void WDCampaignSettingsFree(WDCampaignSettingsResult *value);
WDStatus *WDUpdateCampaignSettings(uint64_t session, char *name, char *revision, char *before, char *after, char *upload, char *cooldown, double threshold);
typedef struct {
    WDStatus status;
    char *version, *os, *os_version, *architecture, *board, *serial, *kernel, *jetpack, *cuda, *l4t;
    uint64_t uptime;
    int64_t memory;
    uint32_t cores;
} WDDeviceInfoResult;
WDDeviceInfoResult *WDDeviceInfo(uint64_t session);
void WDDeviceInfoFree(WDDeviceInfoResult *value);
typedef struct {
    WDStatus status;
    char *installed, *latest, *phase, *detail;
    int32_t available;
    int64_t downloaded, total;
} WDOnlineUpdateResult;
WDOnlineUpdateResult *WDOnlineUpdate(uint64_t session, int32_t action, char *version);
void WDOnlineUpdateFree(WDOnlineUpdateResult *value);
typedef struct {
    char *name;
    int32_t enabled, registered, requests_notifications;
} WDCampaignNotification;
typedef struct {
    WDStatus status;
    WDCampaignNotification *items;
    size_t count;
} WDCampaignNotificationsResult;
WDCampaignNotificationsResult *WDCampaignNotifications(uint64_t session);
void WDCampaignNotificationsFree(WDCampaignNotificationsResult *value);
WDStatus *WDSetCampaignNotifications(uint64_t session, char *name, int32_t enabled);
WDStatus *WDPushRegistration(char *account, uint8_t *token, size_t count, char *topic, int32_t environment, int32_t enabled);
#endif
