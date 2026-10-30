package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/cli/modelhub"
	"strings"
	"time"
	"unsafe"
)

func boolInt(v bool) C.int32_t {
	if v {
		return 1
	}
	return 0
}

//export WDPairings
func WDPairings(session C.uint64_t, kind C.WDPairingKind) *C.WDPairingsResult {
	p := alloc[C.WDPairingsResult](1)
	if kind != C.WD_PAIR_SENSOR && kind != C.WD_PAIR_IP_CAMERA {
		p.status = status(errors.New("unknown pairing kind"))
		return p
	}
	r, err := desktop.Pairings(uint64(session), kind == C.WD_PAIR_IP_CAMERA)
	p.status = status(err)
	p.warning = C.CString(r.Warning)
	p.items, p.count = alloc[C.WDPairing](len(r.Items)), C.size_t(len(r.Items))
	for i, row := range r.Items {
		unsafe.Slice(p.items, len(r.Items))[i] = C.WDPairing{
			id: C.CString(row.ID), name: C.CString(row.Name), address: C.CString(row.Address), detail: C.CString(row.Detail),
			camera_id: C.uint32_t(row.CameraID), paired: boolInt(row.Paired), connected: boolInt(row.Connected), can_pair: boolInt(row.CanPair)}
	}
	return p
}

//export WDPairingsFree
func WDPairingsFree(p *C.WDPairingsResult) {
	if p == nil {
		return
	}
	for _, r := range unsafe.Slice(p.items, int(p.count)) {
		free(r.id)
		free(r.name)
		free(r.address)
		free(r.detail)
	}
	free(p.items)
	free(p.warning)
	free(p.status.message)
	free(p)
}

//export WDPairSensor
func WDPairSensor(session C.uint64_t, id *C.char) *C.WDStatus {
	return statusResult(desktop.PairSensor(uint64(session), C.GoString(id), false))
}

//export WDForgetSensor
func WDForgetSensor(session C.uint64_t, id *C.char) *C.WDStatus {
	return statusResult(desktop.PairSensor(uint64(session), C.GoString(id), true))
}

//export WDPairIPCamera
func WDPairIPCamera(session C.uint64_t, id C.uint32_t, username, password *C.char) *C.WDStatus {
	return statusResult(desktop.PairIPCamera(uint64(session), uint32(id), C.GoString(username), C.GoString(password), false))
}

//export WDForgetIPCamera
func WDForgetIPCamera(session C.uint64_t, id C.uint32_t) *C.WDStatus {
	return statusResult(desktop.PairIPCamera(uint64(session), uint32(id), "", "", true))
}

//export WDCampaignSources
func WDCampaignSources(session C.uint64_t) *C.WDCampaignSourcesResult {
	p := alloc[C.WDCampaignSourcesResult](1)
	rows, err := desktop.CampaignSources(uint64(session))
	p.status = status(err)
	var sources []C.WDCampaignSource
	for _, row := range rows {
		if row.GetKind() == "camera" || row.GetKind() == "audio" || row.GetKind() == "ros2" || row.GetKind() == "telemetry" {
			sources = append(sources, C.WDCampaignSource{kind: C.CString(row.GetKind()), id: C.CString(row.GetId()), detail: C.CString(row.GetDetail()), healthy: boolInt(row.GetHealthy())})
		}
	}
	p.items, p.count = alloc[C.WDCampaignSource](len(sources)), C.size_t(len(sources))
	copy(unsafe.Slice(p.items, len(sources)), sources)
	return p
}

//export WDCampaignSourcesFree
func WDCampaignSourcesFree(p *C.WDCampaignSourcesResult) {
	if p == nil {
		return
	}
	for _, r := range unsafe.Slice(p.items, int(p.count)) {
		free(r.id)
		free(r.detail)
		free(r.kind)
	}
	free(p.items)
	free(p.status.message)
	free(p)
}

//export WDCampaigns
func WDCampaigns(session C.uint64_t) *C.WDCampaignsResult {
	p := alloc[C.WDCampaignsResult](1)
	rows, err := desktop.Campaigns(uint64(session))
	p.status = status(err)
	p.items, p.count = alloc[C.WDCampaign](len(rows)), C.size_t(len(rows))
	for i, r := range rows {
		unsafe.Slice(p.items, len(rows))[i] = C.WDCampaign{name: C.CString(r.Name), state: C.CString(r.State), revision: C.CString(r.Revision), summary: C.CString(r.Summary), inference_state: C.CString(r.InferenceState), problem: C.CString(r.Problem), warnings: C.CString(strings.Join(r.Warnings, "\n"))}
	}
	return p
}

//export WDCampaignsFree
func WDCampaignsFree(p *C.WDCampaignsResult) {
	if p == nil {
		return
	}
	for _, r := range unsafe.Slice(p.items, int(p.count)) {
		free(r.name)
		free(r.state)
		free(r.revision)
		free(r.summary)
		free(r.inference_state)
		free(r.problem)
		free(r.warnings)
	}
	free(p.items)
	free(p.status.message)
	free(p)
}

//export WDDeployCampaign
func WDDeployCampaign(session C.uint64_t, in *C.WDCampaignInput) *C.WDStatus {
	if in == nil || (in.trigger != C.WD_TRIGGER_DETECTION && in.trigger != C.WD_TRIGGER_APP_EVENT && in.trigger != C.WD_TRIGGER_CLASSIFICATION) || (in.action != C.WD_ACTION_NOTIFY_AND_RECORD && in.action != C.WD_ACTION_RECORD) {
		return statusResult(errors.New("invalid campaign input"))
	}
	if in.label_count > 32 || (in.label_count > 0 && in.labels == nil) {
		return statusResult(errors.New("choose at most 32 model labels"))
	}
	if in.audio_count > 256 || in.ros2_count > 256 || (in.audio_count > 0 && in.audio == nil) || (in.ros2_count > 0 && in.ros2 == nil) {
		return statusResult(errors.New("invalid recording source selection"))
	}
	var audio, ros2 []string
	for _, id := range unsafe.Slice(in.audio, int(in.audio_count)) {
		audio = append(audio, C.GoString(id))
	}
	for _, id := range unsafe.Slice(in.ros2, int(in.ros2_count)) {
		ros2 = append(ros2, C.GoString(id))
	}
	var labels []string
	for _, l := range unsafe.Slice(in.labels, int(in.label_count)) {
		labels = append(labels, C.GoString(l))
	}
	return statusResult(desktop.DeployCampaign(uint64(session), commands.DesktopCampaignInput{
		Audio: audio, ROS2: ros2, Telemetry: in.telemetry != 0, Name: C.GoString(in.name), Camera: C.GoString(in.camera), Event: C.GoString(in.event), RunInference: in.trigger != C.WD_TRIGGER_APP_EVENT, Classification: in.trigger == C.WD_TRIGGER_CLASSIFICATION, Model: C.GoString(in.model), ModelRevision: C.GoString(in.model_revision), Labels: labels,
		Notify: in.action == C.WD_ACTION_NOTIFY_AND_RECORD, Upload: in.upload != 0, Threshold: float64(in.threshold),
		Cooldown: uint32(in.cooldown_seconds), Before: uint32(in.before_seconds), After: uint32(in.after_seconds)}))
}

//export WDVisionModel
func WDVisionModel(repo *C.char, trigger C.WDCampaignTrigger) *C.WDVisionModelResult {
	p := alloc[C.WDVisionModelResult](1)
	task := "object-detection"
	if trigger == C.WD_TRIGGER_CLASSIFICATION {
		task = "image-classification"
	} else if trigger != C.WD_TRIGGER_DETECTION {
		p.status = status(errors.New("choose a vision task"))
		return p
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	v, err := modelhub.New().VisionDetails(ctx, C.GoString(repo), task)
	p.status = status(err)
	p.revision, p.license = C.CString(v.Revision), C.CString(v.License)
	p.weights_bytes = C.int64_t(v.WeightsBytes)
	p.labels, p.label_count = alloc[*C.char](len(v.Labels)), C.size_t(len(v.Labels))
	for i, l := range v.Labels {
		unsafe.Slice(p.labels, len(v.Labels))[i] = C.CString(l)
	}
	return p
}

//export WDVisionModelFree
func WDVisionModelFree(p *C.WDVisionModelResult) {
	if p == nil {
		return
	}
	for _, l := range unsafe.Slice(p.labels, int(p.label_count)) {
		free(l)
	}
	free(p.labels)
	free(p.revision)
	free(p.license)
	free(p.status.message)
	free(p)
}
