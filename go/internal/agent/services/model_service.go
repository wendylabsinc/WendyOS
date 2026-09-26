package services

import (
	"context"
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ModelService serves WendyModelService from the model supervisor.
type ModelService struct {
	agentpbv2.UnimplementedWendyModelServiceServer
	logger     *zap.Logger
	supervisor *models.Supervisor
}

func NewModelService(logger *zap.Logger, supervisor *models.Supervisor) *ModelService {
	return &ModelService{logger: logger, supervisor: supervisor}
}

func modelStatusError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, models.ErrUnknownModel), errors.Is(err, models.ErrUnknownCamera),
		errors.Is(err, models.ErrUnknownInstance), errors.Is(err, models.ErrUnknownWatch):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, models.ErrNoVariant), errors.Is(err, models.ErrCameraNotStreamable):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, models.ErrCapacity):
		return status.Error(codes.ResourceExhausted, err.Error())
	case errors.Is(err, models.ErrInvalidFilter):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, models.ErrShuttingDown):
		return status.Error(codes.Unavailable, err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

func (s *ModelService) ListCatalog(ctx context.Context, _ *agentpbv2.ListModelCatalogRequest) (*agentpbv2.ListModelCatalogResponse, error) {
	view := s.supervisor.Catalog(ctx)
	resp := &agentpbv2.ListModelCatalogResponse{
		CatalogVersion: view.Version, MaxRunning: uint32(view.MaxRunning), Running: uint32(view.Running),
	}
	for _, e := range view.Models {
		m := &agentpbv2.CatalogModel{Id: e.Model.ID, Description: e.Model.Description, Kind: e.Model.Kind,
			Labels: e.Model.Labels, UnavailableReason: e.UnavailableReason}
		if e.Variant != nil {
			m.Variant = &agentpbv2.CatalogVariant{Id: e.Variant.ID, Engine: e.Variant.Engine,
				DownloadBytes: uint64(e.DownloadBytes), NeedsEngineBuild: e.NeedsEngineBuild, ImageCached: e.ImageCached}
		}
		resp.Models = append(resp.Models, m)
	}
	for _, c := range view.Cameras {
		resp.Cameras = append(resp.Cameras, &agentpbv2.ModelCamera{SourceId: c.SourceID, Name: c.Name})
	}
	return resp, nil
}

func (s *ModelService) StartModel(ctx context.Context, req *agentpbv2.StartModelRequest) (*agentpbv2.StartModelResponse, error) {
	info, reused, err := s.supervisor.Start(ctx, req.GetModelId(), req.GetCameraSourceId())
	if err != nil {
		return nil, modelStatusError(err)
	}
	return &agentpbv2.StartModelResponse{Instance: modelInstanceProto(info), Reused: reused}, nil
}

// WatchModel holds the instance for as long as the client stays connected.
// A client that goes away is detached, which starts the lease grace period.
func (s *ModelService) WatchModel(req *agentpbv2.WatchModelRequest, stream grpc.ServerStreamingServer[agentpbv2.ModelWatchMessage]) error {
	w, info, last, err := s.supervisor.Watch(models.WatchRequest{
		InstanceID: req.GetInstanceId(), Label: req.GetLabel(), AfterSequence: req.GetAfterSequence(),
		Filter: models.Filter{Classes: req.GetClasses(), MinConfidence: req.GetMinConfidence(), Types: req.GetEventTypes()},
	})
	if err != nil {
		return modelStatusError(err)
	}
	started := &agentpbv2.ModelWatchMessage{Message: &agentpbv2.ModelWatchMessage_Started{Started: &agentpbv2.WatchStarted{
		WatchId: w.ID, Instance: modelInstanceProto(info), LastSequence: last}}}
	if err := stream.Send(started); err != nil {
		s.supervisor.Detach(w.InstanceID, w.ID)
		return err
	}
	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			s.supervisor.Detach(w.InstanceID, w.ID)
			return status.FromContextError(ctx.Err()).Err()
		case msg, ok := <-w.C:
			if !ok {
				if final := w.Final(); final != nil {
					return stream.Send(modelStatusMessage(*final))
				}
				return nil // the watch was stopped explicitly
			}
			if err := stream.Send(modelWatchMessage(msg)); err != nil {
				s.supervisor.Detach(w.InstanceID, w.ID)
				return err
			}
		}
	}
}

func (s *ModelService) ListModels(context.Context, *agentpbv2.ListModelsRequest) (*agentpbv2.ListModelsResponse, error) {
	resp := &agentpbv2.ListModelsResponse{}
	for _, info := range s.supervisor.List() {
		resp.Instances = append(resp.Instances, modelInstanceProto(info))
	}
	return resp, nil
}

func (s *ModelService) StopModel(ctx context.Context, req *agentpbv2.StopModelRequest) (*agentpbv2.StopModelResponse, error) {
	info, err := s.supervisor.Stop(ctx, req.GetInstanceId(), req.GetWatchId())
	if err != nil {
		return nil, modelStatusError(err)
	}
	return &agentpbv2.StopModelResponse{Instance: modelInstanceProto(info)}, nil
}

func modelInstanceProto(i models.InstanceInfo) *agentpbv2.ModelInstance {
	return &agentpbv2.ModelInstance{
		InstanceId: i.ID, ModelId: i.ModelID, VariantId: i.VariantID, Engine: i.Engine, CameraSourceId: i.CameraSourceID,
		State: modelStateProto(i.State), StateDetail: i.StateDetail, Watchers: uint32(i.Watchers), WatchLabels: i.WatchLabels,
		StartedUnixNanos: i.StartedAt.UnixNano(), FileSha256: i.FileSHA256,
		Stats: &agentpbv2.ModelStats{ProcessedFps: i.Stats.ProcessedFPS, LatencyP50Ms: i.Stats.LatencyP50Ms, FramesSkipped: i.Stats.FramesSkipped},
	}
}

func modelStateProto(s models.State) agentpbv2.ModelState {
	switch s {
	case models.StatePreparing:
		return agentpbv2.ModelState_MODEL_STATE_PREPARING
	case models.StateStarting:
		return agentpbv2.ModelState_MODEL_STATE_STARTING
	case models.StateReady:
		return agentpbv2.ModelState_MODEL_STATE_READY
	case models.StateRestarting:
		return agentpbv2.ModelState_MODEL_STATE_RESTARTING
	case models.StateFailed:
		return agentpbv2.ModelState_MODEL_STATE_FAILED
	case models.StateStopped:
		return agentpbv2.ModelState_MODEL_STATE_STOPPED
	}
	return agentpbv2.ModelState_MODEL_STATE_UNSPECIFIED
}

func modelStatusMessage(i models.InstanceInfo) *agentpbv2.ModelWatchMessage {
	return &agentpbv2.ModelWatchMessage{Message: &agentpbv2.ModelWatchMessage_Status{Status: modelInstanceProto(i)}}
}

func modelWatchMessage(m models.WatchMessage) *agentpbv2.ModelWatchMessage {
	switch {
	case m.Status != nil:
		return modelStatusMessage(*m.Status)
	case m.Event != nil:
		e := m.Event
		return &agentpbv2.ModelWatchMessage{Message: &agentpbv2.ModelWatchMessage_Event{Event: &agentpbv2.ModelEvent{
			Sequence: e.Sequence, Type: e.Type, ClassName: e.Class, Confidence: e.Confidence, TrackId: e.TrackID,
			Box:      &agentpbv2.BoundingBox{X: e.Box.X, Y: e.Box.Y, Width: e.Box.Width, Height: e.Box.Height},
			SourceId: e.SourceID, SampleId: e.SampleID, TimeUnixNanos: e.Time.UnixNano(),
		}}}
	default:
		return &agentpbv2.ModelWatchMessage{Message: &agentpbv2.ModelWatchMessage_Gap{Gap: &agentpbv2.ModelGap{
			FirstMissing: m.Gap.FirstMissing, LastMissing: m.Gap.LastMissing}}}
	}
}
