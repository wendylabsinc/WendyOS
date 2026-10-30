#ifndef WENDY_DESKTOP_BRIDGE_H
#define WENDY_DESKTOP_BRIDGE_H
#include "BridgeTypes.h"
#include "VisualRecognition.h"
#include "SiteLocalization.h"
WDCameraMetadataResult *WDCameraMetadataList(uint64_t session);
WDStatus *WDCameraAlignmentSave(uint64_t session, WDCameraAlignmentInput *input);
void WDCameraMetadataFree(WDCameraMetadataResult *value);
#include <TargetConditionals.h>
#include "UnitreeTypes.h"
#include "SiteRobotTypes.h"

WDAccountResult *WDAccount(void);
WDAccountsResult *WDAccounts(void);
void WDAccountsFree(WDAccountsResult *value);
void WDAccountFree(WDAccountResult *value);
WDStatus *WDInitializeMobile(char *directory);
WDStatus *WDInitialize(char *cli_path);
WDDevicesResult *WDNearby(void);
WDDevicesResult *WDCloud(void);
WDStatus *WDLogin(int32_t development);
WDStatus *WDCancelLogin(void);
uint64_t WDBeginConnection(void);
WDConnectionResult *WDConnect(char *selector, uint64_t request);
WDConnectionResult *WDForgetIdentityAndReconnect(char *selector, uint64_t request);
WDStatus *WDDisconnect(void);
WDAppsResult *WDApps(void);
WDOpenAppResult *WDOpenApp(char *app, uint64_t session);
void WDOpenAppFree(WDOpenAppResult *value);
WDStatus *WDStartApp(char *app, uint64_t session);
WDStatus *WDStopApp(char *app, uint64_t session);
WDStatus *WDDeleteApp(char *app, uint64_t session);
WDCamerasResult *WDCameras(void);
WDStatus *WDStartLogs(char *app, uint64_t stream);
WDStatus *WDStopLogs(void);
WDStatus *WDStartCamera(uint32_t camera, uint64_t stream, uint64_t session, char *stable_id);
WDStatus *WDStopCamera(uint32_t camera, uint64_t stream, uint64_t session);
WDStatus *WDStopCameras(uint64_t session);
WDVPNResult *WDStartVPN(WDRouteInput *routes, size_t count, char *gateway);
WDStatus *WDStopVPN(void);
WDStatus *WDShutdown(void);
WDEvents *WDPollEvents(void);
WDResourcesResult *WDResources(uint64_t session);
WDPortsResult *WDPorts(char *app, uint64_t session);
void WDResourcesFree(WDResourcesResult *value);
void WDPortsFree(WDPortsResult *value);

void WDStatusFree(WDStatus *value);
void WDDevicesFree(WDDevicesResult *value);
void WDAppsFree(WDAppsResult *value);
void WDCamerasFree(WDCamerasResult *value);
void WDConnectionFree(WDConnectionResult *value);
void WDVPNFree(WDVPNResult *value);
void WDEventsFree(WDEvents *value);

WDOrganizationsResult *WDOrganizations(char *account);
void WDOrganizationsFree(WDOrganizationsResult *value);
WDOrganizationResult *WDOrganizationOverview(char *account, char *org);
WDOrganizationResult *WDDeviceAccess(char *account, char *org, char *device);
void WDOrganizationFree(WDOrganizationResult *value);
WDStatus *WDAddMember(char *account, char *org, char *email, char *role);
WDStatus *WDSetMemberRole(char *account, char *org, char *user, char *role);
WDStatus *WDRemoveMember(char *account, char *org, char *user);
WDInviteLinkResult *WDCreateInvite(char *account, char *org, char *role, int64_t expires);
void WDInviteLinkFree(WDInviteLinkResult *value);
WDStatus *WDRevokeInvite(char *account, char *org, char *id);
WDStatus *WDCreateAccess(char *account, char *org, char *user, char *device, WDAccessEffect effect);
WDStatus *WDDeleteAccess(char *account, char *org, char *id);
#include "AutomationTypes.h"
#include "VisualizerTypes.h"
#include "ChatTypes.h"
#include "ModelLibraryTypes.h"
#include "FleetTypes.h"
#include "SettingsTypes.h"
#include "EnrollmentTypes.h"
#ifdef __APPLE__
#include <TargetConditionals.h>
#if TARGET_OS_OSX
#include "SimulatorTypes.h"
#include "AccountPickerTypes.h"
#endif
#endif
#endif
