package backend

import (
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"slices"
	"strings"
	"time"
)

func (b *Backend) CameraMetadata(session uint64) ([]*pb.CameraMetadata, error) {
	ctx, done := b.begin()
	defer done()
	if err := b.validSession(session); err != nil {
		return nil, err
	}
	if b.lite != nil {
		return nil, nil
	}
	conn, err := b.connection(session)
	if err != nil {
		return nil, err
	}
	if conn.CalibratedFrameService == nil {
		return nil, nil
	}
	response, err := conn.CalibratedFrameService.ListCameraMetadata(ctx, &pb.ListCameraMetadataRequest{})
	// Older gateways still provide video. They have no metadata, not a default FOV.
	if status.Code(err) == codes.Unimplemented {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return response.GetCameras(), nil
}

// SaveCameraAlignment preserves other model poses and uses the revision read
// before solving. A concurrent calibration must never be silently overwritten.
func (b *Backend) SaveCameraAlignment(session uint64, input *pb.CameraMetadata, expected uint64) error {
	return b.saveCameraCalibration(session, input, expected, true)
}
func (b *Backend) SaveCameraLens(session uint64, input *pb.CameraMetadata, expected uint64) error {
	return b.saveCameraCalibration(session, input, expected, false)
}
func (b *Backend) saveCameraCalibration(session uint64, input *pb.CameraMetadata, expected uint64, poseRequired bool) error {
	ctx, done := b.begin()
	defer done()
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	if conn.CalibratedFrameService == nil {
		return status.Error(codes.Unimplemented, "Update the gateway to save camera calibration")
	}
	if input == nil || input.Intrinsics == nil || input.StableId == "" || (poseRequired && (len(input.SitePoses) != 1 || input.SitePoses[0] == nil)) || (!poseRequired && len(input.SitePoses) != 0) {
		return status.Error(codes.InvalidArgument, "one measured camera alignment is required")
	}
	input = proto.Clone(input).(*pb.CameraMetadata)
	response, err := conn.CalibratedFrameService.ListCameraMetadata(ctx, &pb.ListCameraMetadataRequest{StableIds: []string{input.StableId}})
	if err != nil {
		return err
	}
	var current *pb.CameraMetadata
	for _, record := range response.Cameras {
		if record.StableId == input.StableId {
			if current != nil {
				return status.Error(codes.FailedPrecondition, "Ambiguous camera metadata")
			}
			current = record
		}
	}
	if current.GetRevision() != expected {
		return status.Error(codes.Aborted, "Camera calibration changed. Run alignment again before saving.")
	}
	if current != nil {
		if current.GetIntrinsics().GetProvenance() == pb.CameraIntrinsics_PROVENANCE_MEASURED && input.Intrinsics.Provenance == pb.CameraIntrinsics_PROVENANCE_ASSUMED {
			return status.Error(codes.FailedPrecondition, "A measured lens is available. Use calibrated alignment instead of replacing it with an estimate.")
		}
		sameLens := proto.Equal(current.Intrinsics, input.Intrinsics) && current.DistortionModel == input.DistortionModel && slices.Equal(current.Distortion, input.Distortion) && current.CaptureMode == input.CaptureMode
		if sameLens {
			input.CalibratedAtUnixMs = current.CalibratedAtUnixMs
		}
		for _, pose := range current.SitePoses {
			if sameLens && pose != nil && pose.ValidUntilUnixMs > time.Now().UnixMilli() && (!poseRequired || !strings.EqualFold(pose.ModelRevision, input.SitePoses[0].ModelRevision)) {
				input.SitePoses = append(input.SitePoses, pose)
			}
		}
	}
	_, err = conn.CalibratedFrameService.SetCameraMetadata(ctx, &pb.SetCameraMetadataRequest{StableId: input.StableId, ExpectedRevision: expected, Metadata: input})
	return err
}
