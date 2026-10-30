package backend

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func (b *Backend) Pairings(session uint64, cameras bool) (commands.DesktopPairings, error) {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return commands.DesktopPairings{}, errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return commands.DesktopPairings{}, err
	}
	if cameras {
		return commands.DesktopIPCameraPairings(ctx, conn)
	}
	return commands.DesktopSensorPairings(ctx, conn)
}
func (b *Backend) PairSensor(session uint64, id string, remove bool) error {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	return commands.DesktopPairSensor(ctx, conn, id, remove)
}
func (b *Backend) PairIPCamera(session uint64, id uint32, username, password string, remove bool) error {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	return commands.DesktopPairIPCamera(ctx, conn, id, username, password, remove)
}
func (b *Backend) Campaigns(session uint64) ([]commands.DesktopCampaign, error) {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return nil, errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return nil, err
	}
	return commands.DesktopCampaigns(ctx, conn)
}
func (b *Backend) CampaignSources(session uint64) ([]*pb.DataSource, error) {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return nil, errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return nil, err
	}
	r, err := conn.DataService.Sources(ctx, &pb.DataSourcesRequest{})
	if err != nil {
		return nil, err
	}
	return r.GetSources(), nil
}
func (b *Backend) DeployCampaign(session uint64, input commands.DesktopCampaignInput) error {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	return commands.DesktopDeployCampaign(ctx, conn, input)
}
