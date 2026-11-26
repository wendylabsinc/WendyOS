package backend

import (
	"errors"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func (b *Backend) VisualRecordingStatus(session uint64) (*pb.DataEpisode, error) {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return nil, errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return nil, err
	}
	status, err := conn.DataService.Status(ctx, &pb.DataStatusRequest{})
	if err != nil {
		return nil, err
	}
	return status.GetActive(), nil
}
func (b *Backend) StartVisualRecording(session uint64, name string, sources []string) (*pb.DataEpisode, error) {
	ctx, done := b.begin()
	defer done()
	if session == 0 || len(sources) == 0 || len(sources) > 256 {
		return nil, errors.New("choose recording sources on the connected device")
	}
	conn, err := b.connection(session)
	if err != nil {
		return nil, err
	}
	return conn.DataService.Start(ctx, &pb.DataStartRequest{Name: name, Sources: sources})
}
func (b *Backend) StopVisualRecording(session uint64) (*pb.DataEpisode, error) {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return nil, errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return nil, err
	}
	return conn.DataService.Stop(ctx, &pb.DataStopRequest{})
}

// HomeCampaign reads only this campaign's in-progress clips or manually triggers
// it. DataService.Stop is intentionally not exposed: it stops the unrelated
// global manual recording, not an individual campaign clip.
func (b *Backend) HomeCampaign(session uint64, name string, trigger bool) ([]*pb.DataEpisode, error) {
	ctx, done := b.begin()
	defer done()
	if session == 0 || name == "" {
		return nil, errors.New("choose a campaign on the connected device")
	}
	conn, err := b.connection(session)
	if err != nil {
		return nil, err
	}
	if trigger {
		row, err := conn.DataService.CampaignTrigger(ctx, &pb.DataCampaignTriggerRequest{Name: name, Reason: "manual"})
		if err != nil {
			return nil, err
		}
		if row == nil {
			return nil, errors.New("the device did not return a recording")
		}
		return []*pb.DataEpisode{row}, nil
	}
	status, err := conn.DataService.Status(ctx, &pb.DataStatusRequest{})
	if err != nil {
		return nil, err
	}
	var rows []*pb.DataEpisode
	for _, row := range status.GetInProgress() {
		if row.GetCampaignName() == name {
			rows = append(rows, row)
		}
	}
	return rows, nil
}
