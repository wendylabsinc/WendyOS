package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"
import "unsafe"
import "fmt"

//export WDCampaignNotifications
func WDCampaignNotifications(session C.uint64_t) *C.WDCampaignNotificationsResult {
	rows, err := desktop.CampaignNotifications(uint64(session))
	p := alloc[C.WDCampaignNotificationsResult](1)
	*p = C.WDCampaignNotificationsResult{status: status(err), count: C.size_t(len(rows)), items: alloc[C.WDCampaignNotification](len(rows))}
	for i, row := range rows {
		unsafe.Slice(p.items, len(rows))[i] = C.WDCampaignNotification{name: C.CString(row.Name), enabled: boolInt(row.Enabled), registered: boolInt(row.Registered), requests_notifications: boolInt(row.RequestsNotifications)}
	}
	return p
}

//export WDCampaignNotificationsFree
func WDCampaignNotificationsFree(p *C.WDCampaignNotificationsResult) {
	if p == nil {
		return
	}
	for _, row := range unsafe.Slice(p.items, int(p.count)) {
		free(row.name)
	}
	free(p.items)
	free(p.status.message)
	free(p)
}

//export WDSetCampaignNotifications
func WDSetCampaignNotifications(session C.uint64_t, name *C.char, enabled C.int32_t) *C.WDStatus {
	return statusResult(desktop.SetCampaignNotifications(uint64(session), C.GoString(name), enabled != 0))
}

//export WDPushRegistration
func WDPushRegistration(account *C.char, token *C.uint8_t, count C.size_t, topic *C.char, environment C.int32_t, enabled C.int32_t) *C.WDStatus {
	if token == nil || count == 0 || count > 4096 {
		return statusResult(fmt.Errorf("invalid Apple push token"))
	}
	return statusResult(desktop.PushRegistration(C.GoString(account), C.GoBytes(unsafe.Pointer(token), C.int(count)), C.GoString(topic), int32(environment), enabled != 0))
}
