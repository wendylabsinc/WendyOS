package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"
import "github.com/wendylabsinc/wendy/go/macos/backend"

//export WDCampaignSettings
func WDCampaignSettings(session C.uint64_t, name *C.char) *C.WDCampaignSettingsResult {
	value, err := desktop.CampaignSettings(uint64(session), C.GoString(name))
	p := alloc[C.WDCampaignSettingsResult](1)
	*p = C.WDCampaignSettingsResult{status: status(err), name: C.CString(value.Name), revision: C.CString(value.Revision), before: C.CString(value.Before), after: C.CString(value.After), upload: C.CString(value.Upload), cooldown: C.CString(value.Cooldown), inference: boolInt(value.Inference), threshold: C.double(value.Threshold)}
	return p
}

//export WDCampaignSettingsFree
func WDCampaignSettingsFree(p *C.WDCampaignSettingsResult) {
	if p == nil {
		return
	}
	free(p.status.message)
	free(p.name)
	free(p.revision)
	free(p.before)
	free(p.after)
	free(p.upload)
	free(p.cooldown)
	free(p)
}

//export WDUpdateCampaignSettings
func WDUpdateCampaignSettings(session C.uint64_t, name, revision, before, after, upload, cooldown *C.char, threshold C.double) *C.WDStatus {
	return statusResult(desktop.UpdateCampaignSettings(uint64(session), backend.CampaignSettings{Name: C.GoString(name), Revision: C.GoString(revision), Before: C.GoString(before), After: C.GoString(after), Upload: C.GoString(upload), Cooldown: C.GoString(cooldown), Threshold: float64(threshold)}))
}
