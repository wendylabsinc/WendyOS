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

//export WDInbox
func WDInbox(account *C.char) *C.WDInboxResult {
	rows, err := desktop.Inbox(C.GoString(account))
	p := alloc[C.WDInboxResult](1)
	*p = C.WDInboxResult{status: status(err), count: C.size_t(len(rows)), items: alloc[C.WDInboxNotification](len(rows))}
	for i, row := range rows {
		id := row.GetNotificationId()
		if id == "" {
			id = fmt.Sprintf("legacy:%s:%s:%s", row.GetCreatedAt().AsTime().String(), row.GetTitle(), row.GetBody())
		}
		unsafe.Slice(p.items, len(rows))[i] = C.WDInboxNotification{id: C.CString(id), title: C.CString(row.GetTitle()), body: C.CString(row.GetBody()), created_at: C.double(float64(row.GetCreatedAt().AsTime().UnixMilli()) / 1000)}
	}
	return p
}

//export WDInboxFree
func WDInboxFree(p *C.WDInboxResult) {
	if p == nil {
		return
	}
	for _, row := range unsafe.Slice(p.items, int(p.count)) {
		free(row.id)
		free(row.title)
		free(row.body)
	}
	free(p.items)
	free(p.status.message)
	free(p)
}
