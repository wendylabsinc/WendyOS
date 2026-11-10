package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/encoding/protojson"
)

type mobileOSRun struct {
	mu      sync.Mutex
	session uint64
	state   OSInstallState
}
type OSInstallState struct {
	Active         bool   `json:"active"`
	Phase          string `json:"phase"`
	Percent        int32  `json:"percent"`
	RebootRequired bool   `json:"rebootRequired"`
	Error          string `json:"error"`
	Baseline       int64  `json:"baseline"`
}
type OSUpdateSnapshot struct {
	Status  json.RawMessage `json:"status"`
	Install OSInstallState  `json:"install"`
}

// GetOSUpdateStatus is read-only; progress is scoped to the connection that
// started it. A reconnected client can still read the persisted agent outcome.
func (b *Backend) MobileOSUpdate(session uint64, artifact string, start bool) (*OSUpdateSnapshot, error) {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return nil, errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return nil, err
	}
	client := pb.NewWendyOSUpdateServiceClient(conn.Conn)
	b.osUpdate.mu.Lock()
	state := OSInstallState{}
	if b.osUpdate.session == session {
		state = b.osUpdate.state
	}
	anyActive := b.osUpdate.state.Active
	b.osUpdate.mu.Unlock()
	// Do not serialize a live engine probe against each progress poll.
	if state.Active && !start {
		return &OSUpdateSnapshot{Status: json.RawMessage(`{}`), Install: state}, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := client.GetOSUpdateStatus(probeCtx, &pb.GetOSUpdateStatusRequest{IncludeEngineStatus: true})
	if err != nil {
		return nil, err
	}
	if start {
		if anyActive {
			return nil, errors.New("an OS update is already running; check its status")
		}
		if state.RebootRequired && result.GetCreatedAtUnix() <= state.Baseline {
			return nil, errors.New("the installed update still needs a restart and a verified outcome")
		}
		if err := validateOSUpdateStart(artifact, result); err != nil {
			return nil, err
		}
		// A long-running update must not hold the backend's request lock. Its life
		// ends with this connection, not with navigation away from the screen.
		runCtx, stop := context.WithTimeout(b.life, 45*time.Minute)
		stream, err := client.UpdateOS(runCtx, &pb.UpdateOSRequest{ArtifactUrl: artifact})
		if err != nil {
			stop()
			return nil, err
		}
		state = OSInstallState{Active: true, Phase: "Starting", Percent: -1, Baseline: result.GetCreatedAtUnix()}
		b.osUpdate.mu.Lock()
		b.osUpdate.session = session
		b.osUpdate.state = state
		b.osUpdate.mu.Unlock()
		go func() {
			defer stop()
			final := consumeOSUpdate(stream.Recv, state, func(next OSInstallState) {
				b.osUpdate.mu.Lock()
				defer b.osUpdate.mu.Unlock()
				if b.osUpdate.session == session {
					b.osUpdate.state = next
				}
			})
			b.osUpdate.mu.Lock()
			defer b.osUpdate.mu.Unlock()
			if b.osUpdate.session == session {
				b.osUpdate.state = final
			}
		}()
	}
	encoded, err := protojson.Marshal(result)
	return &OSUpdateSnapshot{Status: encoded, Install: state}, err
}
func validateOSUpdateStart(artifact string, result *pb.GetOSUpdateStatusResponse) error {
	u, err := url.Parse(artifact)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return errors.New("enter an HTTP or HTTPS OS artifact URL")
	}
	if result.GetEngineStatus().GetConnector() == "" {
		return errors.New("the device has not reported a supported OS updater; refresh or check its Agent version")
	}
	if result.GetEngineStatus().GetPending() != nil {
		return errors.New("an OS update is pending; restart the device and verify its outcome before starting another")
	}
	return nil
}
func consumeOSUpdate(recv func() (*pb.UpdateOSResponse, error), state OSInstallState, publish func(OSInstallState)) OSInstallState {
	for {
		response, err := recv()
		if err != nil {
			state.Active = false
			if err == io.EOF {
				state.Error = "The update stream ended without a completion result. Check device status before retrying."
			} else {
				state.Error = fmt.Sprintf("Update connection ended: %v. Check device status before retrying.", err)
			}
			return state
		}
		switch frame := response.ResponseType.(type) {
		case *pb.UpdateOSResponse_Progress_:
			state.Phase = strings.TrimSpace(frame.Progress.GetPhase())
			state.Percent = frame.Progress.GetPercent()
			publish(state)
		case *pb.UpdateOSResponse_Completed_:
			state.Active = false
			state.RebootRequired = frame.Completed.GetRebootRequired()
			state.Phase = "Installed"
			state.Percent = 100
			return state
		case *pb.UpdateOSResponse_Failed_:
			state.Active = false
			state.Error = frame.Failed.GetErrorMessage()
			if state.Error == "" {
				state.Error = "The device reported an OS update failure without details."
			}
			state.Phase = "Failed"
			return state
		}
	}
}
