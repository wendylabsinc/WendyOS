package backend

import (
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

func (b *Backend) DeviceEnrollment(session uint64) (*commands.DesktopEnrollment, error) {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return nil, errors.New("select a device first")
	}
	if err := b.validSession(session); err != nil {
		return nil, err
	}
	if b.lite != nil {
		client, ok := b.lite.(interface {
			EnrollmentChallenge(bool) (*litepb.WendyComEnrollmentChallenge, error)
		})
		if !ok {
			return nil, errors.New("this device cannot report enrollment status")
		}
		state, err := client.EnrollmentChallenge(true)
		if err != nil {
			return nil, err
		}
		if state == nil {
			return nil, errors.New("the device did not report enrollment status")
		}
		if !state.Enrolled && (state.GetDeviceId() != "" || state.GetTenantId() != "") {
			return nil, errors.New("this device has a pending enrollment; allow it to finish before checking again")
		}
		return &commands.DesktopEnrollment{Enrolled: state.Enrolled, OrganizationID: state.GetTenantId(), DeviceID: state.GetDeviceId()}, nil
	}
	conn, err := b.connection(session)
	if err != nil {
		return nil, err
	}
	return commands.DesktopDeviceEnrollment(ctx, conn)
}

func (b *Backend) EnrollmentOrganizations() ([]commands.DesktopEnrollmentOrganization, error) {
	ctx, done := b.begin()
	defer done()
	return commands.DesktopEnrollmentOrganizations(ctx)
}

func (b *Backend) EnrollDevice(session uint64, account, organization, name string) error {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	next, err := commands.DesktopEnrollDevice(ctx, conn, account, organization, name)
	if err != nil {
		return err
	}
	if next != conn {
		b.conn = next
		conn.Close()
	}
	return nil
}
