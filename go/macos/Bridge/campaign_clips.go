package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"
import "unsafe"

//export WDCampaignClips
func WDCampaignClips(session C.uint64_t) *C.WDCampaignClipsResult {
	p := alloc[C.WDCampaignClipsResult](1)
	r, err := desktop.CampaignClips(uint64(session))
	p.status = status(err)
	p.warning = C.CString(r.Warning)
	p.items, p.count = alloc[C.WDCampaignClip](len(r.Items)), C.size_t(len(r.Items))
	for i, row := range r.Items {
		unsafe.Slice(p.items, len(r.Items))[i] = C.WDCampaignClip{id: C.CString(row.ID), campaign: C.CString(row.Campaign), state: C.CString(row.State), reason: C.CString(row.Reason), started_ns: C.int64_t(row.Started), size_bytes: C.int64_t(row.Size), duration_seconds: C.double(row.Duration)}
	}
	return p
}

//export WDCampaignClipsFree
func WDCampaignClipsFree(p *C.WDCampaignClipsResult) {
	if p == nil {
		return
	}
	for _, r := range unsafe.Slice(p.items, int(p.count)) {
		free(r.id)
		free(r.campaign)
		free(r.state)
		free(r.reason)
	}
	free(p.items)
	free(p.warning)
	free(p.status.message)
	free(p)
}
