package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"
import (
	"fmt"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"unsafe"
)

//export WDCameraMetadataList
func WDCameraMetadataList(session C.uint64_t) *C.WDCameraMetadataResult {
	items, err := desktop.CameraMetadata(uint64(session))
	p := alloc[C.WDCameraMetadataResult](1)
	p.status = status(err)
	p.items, p.count = alloc[C.WDCameraMetadata](len(items)), C.size_t(len(items))
	for i, m := range items {
		out := &unsafe.Slice(p.items, len(items))[i]
		out.stable_id = C.CString(m.StableId)
		out.capture_mode = C.CString(m.CaptureMode)
		out.distortion_model = C.CString(m.DistortionModel)
		out.revision = C.uint64_t(m.Revision)
		if k := m.Intrinsics; k != nil {
			if k.Provenance == 1 || k.Provenance == 2 {
				out.has_intrinsics = 1
			}
			if k.Provenance == 1 {
				out.measured = 1
			}
			out.width, out.height = C.uint32_t(k.Width), C.uint32_t(k.Height)
			out.fx, out.fy, out.cx, out.cy = C.double(k.Fx), C.double(k.Fy), C.double(k.Cx), C.double(k.Cy)
			out.note = C.CString(k.Note)
		}
		if len(m.Distortion) == 5 {
			out.k1, out.k2, out.p1, out.p2, out.k3 = C.double(m.Distortion[0]), C.double(m.Distortion[1]), C.double(m.Distortion[2]), C.double(m.Distortion[3]), C.double(m.Distortion[4])
		}
		if m.DistortionModel == "brown_conrady" && len(m.Distortion) != 5 || m.DistortionModel == "none" && len(m.Distortion) != 0 {
			out.has_intrinsics = 0
		}
		out.poses, out.pose_count = alloc[C.WDSiteCameraPose](len(m.SitePoses)), C.size_t(len(m.SitePoses))
		for j, pose := range m.SitePoses {
			dest := &unsafe.Slice(out.poses, len(m.SitePoses))[j]
			dest.model_revision, dest.provenance = C.CString(pose.ModelRevision), C.CString(pose.Provenance)
			dest.method, dest.observed_ms, dest.valid_until_ms = C.int32_t(pose.Method), C.int64_t(pose.ObservedAtUnixMs), C.int64_t(pose.ValidUntilUnixMs)
			// Keep malformed remote arrays invalid rather than indexing outside them.
			if len(pose.Position) == 3 && len(pose.Rotation) == 4 {
				dest.x, dest.y, dest.z = C.double(pose.Position[0]), C.double(pose.Position[1]), C.double(pose.Position[2])
				dest.qx, dest.qy, dest.qz, dest.qw = C.double(pose.Rotation[0]), C.double(pose.Rotation[1]), C.double(pose.Rotation[2]), C.double(pose.Rotation[3])
			}
		}
	}
	return p
}

//export WDCameraMetadataFree
func WDCameraMetadataFree(p *C.WDCameraMetadataResult) {
	if p == nil {
		return
	}
	for _, m := range unsafe.Slice(p.items, int(p.count)) {
		free(m.stable_id)
		free(m.capture_mode)
		free(m.note)
		free(m.distortion_model)
		for _, pose := range unsafe.Slice(m.poses, int(m.pose_count)) {
			free(pose.model_revision)
			free(pose.provenance)
		}
		free(m.poses)
	}
	free(p.items)
	free(p.status.message)
	free(p)
}

//export WDCameraAlignmentSave
func WDCameraAlignmentSave(session C.uint64_t, input *C.WDCameraAlignmentInput) *C.WDStatus {
	if input == nil {
		return statusResult(fmt.Errorf("missing camera alignment"))
	}
	rms := float64(input.rms)
	m := &pb.CameraMetadata{StableId: C.GoString(input.stable_id), CaptureMode: C.GoString(input.capture_mode),
		Intrinsics:      &pb.CameraIntrinsics{Width: uint32(input.width), Height: uint32(input.height), Fx: float32(input.fx), Fy: float32(input.fy), Cx: float32(input.cx), Cy: float32(input.cy), Provenance: pb.CameraIntrinsics_PROVENANCE_MEASURED, Note: C.GoString(input.note)},
		DistortionModel: "brown_conrady", Distortion: []float64{float64(input.k1), float64(input.k2), float64(input.p1), float64(input.p2), float64(input.k3)}, CalibratedAtUnixMs: int64(input.observed_ms),
		SitePoses: []*pb.SiteCameraPose{{ModelRevision: C.GoString(input.model_revision), Position: []float64{float64(input.x), float64(input.y), float64(input.z)}, Rotation: []float64{float64(input.qx), float64(input.qy), float64(input.qz), float64(input.qw)}, Method: pb.SiteCameraPose_METHOD_VISUAL_LOCALIZATION, Provenance: C.GoString(input.pose_provenance), ObservedAtUnixMs: int64(input.observed_ms), ValidUntilUnixMs: int64(input.valid_until_ms), ReprojectionRmsPixels: &rms, InlierCount: uint32(input.inliers)}},
	}
	if input.lens_measured == 0 {
		m.Intrinsics.Provenance = pb.CameraIntrinsics_PROVENANCE_ASSUMED
	}
	if input.lens_only != 0 {
		m.SitePoses = nil
		return statusResult(desktop.SaveCameraLens(uint64(session), m, uint64(input.expected_revision)))
	}
	return statusResult(desktop.SaveCameraAlignment(uint64(session), m, uint64(input.expected_revision)))
}
