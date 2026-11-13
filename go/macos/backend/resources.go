package backend

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"strings"
	"time"
)

func (b *Backend) Resources(session uint64) (*commands.DesktopResourceSnapshot, error) {
	ctx, done := b.begin()
	defer done()
	conn, err := b.connection(session)
	if err != nil {
		return nil, err
	}
	sampleCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return b.top.Sample(sampleCtx, conn)
}
func (b *Backend) Ports(app string, session uint64) ([]*pb.PortEntry, error) {
	ctx, done := b.begin()
	defer done()
	conn, err := b.connection(session)
	if err != nil {
		return nil, err
	}
	response, err := conn.ContainerService.GetContainerPorts(ctx, &pb.GetContainerPortsRequest{AppName: app})
	if err != nil {
		return nil, err
	}
	return response.GetPorts(), nil
}

func (b *Backend) StartApp(app string, session uint64) error {
	ctx, done := b.begin()
	defer done()
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	return commands.DesktopStartApp(ctx, conn, app)
}
func (b *Backend) StopApp(app string, session uint64) error {
	ctx, done := b.begin()
	defer done()
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	_, err = conn.ContainerService.StopContainer(ctx, &pb.StopContainerRequest{AppName: app})
	return err
}

// DeleteApp removes the deployment, retaining images and persistent volumes.
// A confirmation must remain bound to the device session where it was shown.
func (b *Backend) DeleteApp(app string, session uint64) error {
	ctx, done := b.begin()
	defer done()
	if session == 0 || strings.TrimSpace(app) == "" {
		return errors.New("an app and device session are required")
	}
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	_, err = conn.ContainerService.DeleteContainer(ctx, &pb.DeleteContainerRequest{AppName: app, DeleteImage: false, DeleteVolumes: false})
	return err
}
