package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	v1 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// RestartAfterOSUpdate uses the existing authenticated host-shell service. A
// short systemd timer gives the shell time to acknowledge scheduling the reboot.
// The subsequent boot and health-check result must still be verified separately.
func (b *Backend) RestartAfterOSUpdate(session uint64) error {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return err
	}
	if !conn.IsMTLS {
		return errors.New("restart requires an authenticated device connection; enroll the device and reconnect")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	status, err := pb.NewWendyOSUpdateServiceClient(conn.Conn).GetOSUpdateStatus(ctx, &pb.GetOSUpdateStatusRequest{IncludeEngineStatus: true})
	if err != nil {
		return err
	}
	b.osUpdate.mu.Lock()
	state := OSInstallState{}
	if b.osUpdate.session == session {
		state = b.osUpdate.state
	}
	b.osUpdate.mu.Unlock()
	if err := validateOSRestart(state, status); err != nil {
		return err
	}
	stream, err := v1.NewWendyShellServiceClient(conn.Conn).HostShell(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&v1.HostShellRequest{RequestType: &v1.HostShellRequest_Start_{Start: &v1.HostShellRequest_Start{
		Command:  []string{"systemd-run", "--quiet", "--on-active=3s", "--collect", "systemctl", "reboot"},
		TermSize: &v1.WindowSize{Rows: 24, Cols: 80},
	}}}); err != nil {
		return err
	}
	return receiveOSRestart(stream.Recv)
}

func validateOSRestart(state OSInstallState, status *pb.GetOSUpdateStatusResponse) error {
	if state.Active {
		return errors.New("wait for OS installation to finish before restarting")
	}
	if pending := status.GetEngineStatus().GetPending(); pending != nil && pending.GetPhase() != "installed" {
		return errors.New("the OS updater has not reported installation complete; refresh device status before restarting")
	}
	if status.GetEngineStatus().GetPending() == nil && !(state.RebootRequired && status.GetCreatedAtUnix() <= state.Baseline) {
		return errors.New("no installed OS update is waiting for a restart; refresh device status")
	}
	return nil
}

func receiveOSRestart(recv func() (*v1.HostShellResponse, error)) error {
	for {
		response, err := recv()
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("restart was not acknowledged: %w; reconnect and check device status before retrying", err)
		}
		if exit, ok := response.ResponseType.(*v1.HostShellResponse_ExitCode); ok {
			if exit.ExitCode != 0 {
				return fmt.Errorf("device could not schedule restart (exit %d)", exit.ExitCode)
			}
			return nil
		}
	}
}
