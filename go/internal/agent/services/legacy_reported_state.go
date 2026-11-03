package services

import (
	"context"
	"fmt"
	"io"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
)

// LegacyReleaseInstaller pulls a Cloud-assigned, digest-pinned container into
// the existing container service. Registry credentials never enter an RPC or
// persist in a container's labels.
type LegacyReleaseInstaller interface {
	InstallLegacyRelease(context.Context, string, int32, string, *cloudpb.GetPullCredentialsResponse) error
}

type legacyAppControl interface {
	StartApp(context.Context, string) error
	StopApp(context.Context, string) error
	RemoveApp(context.Context, string) error
}

// LegacyReportedState implements the numeric v1 deployment contract. It is
// deliberately separate from v2's signed postbox, whose trust and IDs differ.
type LegacyReportedState struct {
	logger                 *zap.Logger
	provisioning           *ProvisioningService
	apps                   AppReporter
	control                legacyAppControl
	install                LegacyReleaseInstaller
	poll, heartbeat, retry time.Duration
	changed                chan struct{}
	mu                     sync.Mutex
	cancel                 context.CancelFunc
	dial                   func(CloudEnrollment, string, string, string) (grpc.ClientConnInterface, io.Closer, brokerRequestMetadata, error)
}

func NewLegacyReportedState(logger *zap.Logger, provisioning *ProvisioningService, apps AppReporter, containers *ContainerService, install LegacyReleaseInstaller) *LegacyReportedState {
	if containers != nil {
		apps = legacyMonitoredApps{AppReporter: apps, containers: containers}
	}
	return &LegacyReportedState{
		logger: logger, provisioning: provisioning, apps: apps, control: containerControl{svc: containers}, install: install,
		poll: 10 * time.Second, heartbeat: time.Minute, retry: 30 * time.Second, changed: make(chan struct{}, 1),
		dial: func(e CloudEnrollment, cert, key, chain string) (grpc.ClientConnInterface, io.Closer, brokerRequestMetadata, error) {
			conn, proof, err := dialLegacyCloud(NewTunnelBrokerClient(logger, e.Host, cert, key, chain, 0))
			return conn, conn, proof, err
		},
	}
}

func (r *LegacyReportedState) UpdateIdentity() {
	r.mu.Lock()
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

func (r *LegacyReportedState) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := r.session(ctx); err != nil && ctx.Err() == nil {
			r.logger.Warn("legacy reported state stream ended", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-r.changed:
		case <-time.After(r.retry):
		}
	}
}

func (r *LegacyReportedState) session(parent context.Context) error {
	// Install the cancellation before reading the identity: an enrollment
	// transition must not apply commands received for a previous enrollment.
	ctx, cancel := context.WithCancel(parent)
	r.mu.Lock()
	r.cancel = cancel
	r.mu.Unlock()
	defer func() { cancel(); r.mu.Lock(); r.cancel = nil; r.mu.Unlock() }()
	enrollment, cert, chain, key, err := r.provisioning.LegacyCloudCredentials()
	if err != nil || enrollment.Mode != CloudLegacy {
		return err
	}
	conn, closer, proof, err := r.dial(enrollment, cert, key, chain)
	if err != nil {
		return err
	}
	defer closer.Close()
	md, err := proof(cloudpb.DeploymentService_HandleReportedState_FullMethodName)
	if err != nil {
		return err
	}
	stream, err := cloudpb.NewDeploymentServiceClient(conn).HandleReportedState(metadata.NewOutgoingContext(ctx, md))
	if err != nil {
		return err
	}
	responses := make(chan *cloudpb.DesiredStateResponse)
	recvErr := make(chan error, 1)
	go func() {
		for {
			response, err := stream.Recv()
			if err != nil {
				recvErr <- err
				cancel() // cancel an in-flight image pull or lifecycle operation too
				return
			}
			select {
			case responses <- response:
			case <-ctx.Done():
				return
			}
		}
	}()
	ticker := time.NewTicker(r.poll)
	defer ticker.Stop()
	var last *cloudpb.UpdateReportedStateRequest
	desiredReleases := map[string]int32{}
	var sent time.Time
	for {
		apps, err := r.apps.ReportApps(ctx)
		if err != nil {
			return err
		}
		req := legacyAppSnapshot(apps)
		protectLegacyDesiredReleases(req, desiredReleases)
		if !proto.Equal(req, last) || time.Since(sent) >= r.heartbeat {
			if err := stream.Send(req); err != nil {
				return err
			}
			last, sent = req, time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-recvErr:
			return err
		case response := <-responses:
			desiredReleases = map[string]int32{}
			for _, app := range response.GetDesiredAppStates() {
				desiredReleases[app.GetAppId()] = app.GetAppReleaseId()
			}
			if err := r.apply(ctx, conn, proof, response); err != nil {
				r.logger.Warn("legacy desired state application failed", zap.Error(err))
			}
		case <-ticker.C:
		}
	}
}

func legacyAppSnapshot(apps []AppReport) *cloudpb.UpdateReportedStateRequest {
	req := &cloudpb.UpdateReportedStateRequest{}
	for _, app := range apps {
		state := &cloudpb.CurrentState{AppId: app.AppID, ReportedState: cloudpb.Running_STOPPED, ReportedLastExitSignal: app.ExitSignal, ReportedRestartCount: app.RestartCount}
		if app.Running {
			state.ReportedState = cloudpb.Running_RUNNING
		}
		if id, err := strconv.ParseInt(app.ReleaseID, 10, 32); err == nil && id > 0 && strconv.FormatInt(id, 10) == app.ReleaseID {
			state.AppReleaseId = proto.Int32(int32(id))
		}
		req.CurrentStates = append(req.CurrentStates, state)
	}
	return req
}

func (r *LegacyReportedState) apply(ctx context.Context, conn grpc.ClientConnInterface, proof brokerRequestMetadata, response *cloudpb.DesiredStateResponse) error {
	apps, err := r.apps.ReportApps(ctx)
	if err != nil {
		return err
	}
	current := map[string]AppReport{}
	for _, app := range apps {
		current[app.AppID] = app
	}
	// Only explicit desired rows carry authority. An empty response must not
	// delete local/offline apps (v1 returns only Cloud-registered assignments).
	for _, desired := range response.GetDesiredAppStates() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		id := desired.GetAppId()
		if desired.AppReleaseId != nil && desired.GetAppReleaseId() <= 0 {
			return fmt.Errorf("invalid legacy desired release ID")
		}
		if err := appconfig.ValidateAppID(id); err != nil {
			return err
		}
		app, exists := current[id]
		switch desired.GetDesiredState() {
		case cloudpb.Running_ABSENT:
			if exists {
				if err := r.control.RemoveApp(ctx, id); err != nil {
					return err
				}
			}
		case cloudpb.Running_RUNNING, cloudpb.Running_STOPPED:
			release := desired.GetAppReleaseId()
			if release > 0 && (!exists || app.ReleaseID != strconv.FormatInt(int64(release), 10)) {
				if desired.GetReleaseType() != cloudpb.ReleaseType_CONTAINER {
					return fmt.Errorf("legacy release %d is not a container", release)
				}
				if r.install == nil {
					return fmt.Errorf("legacy release installer unavailable")
				}
				md, err := proof(cloudpb.DeploymentService_GetPullCredentials_FullMethodName)
				if err != nil {
					return err
				}
				callCtx, cancel := context.WithTimeout(metadata.NewOutgoingContext(ctx, md), 30*time.Second)
				credentials, err := cloudpb.NewDeploymentServiceClient(conn).GetPullCredentials(callCtx, &cloudpb.GetPullCredentialsRequest{AppReleaseId: release})
				cancel()
				if err != nil {
					return fmt.Errorf("getting legacy release %d pull credentials: %w", release, err)
				}
				if err := r.install.InstallLegacyRelease(ctx, id, release, desired.GetImageDigest(), credentials); err != nil {
					return err
				}
				app.Running = false
				exists = true
			}
			if !exists {
				continue
			} // no assigned release: do not guess an image
			if desired.GetDesiredState() == cloudpb.Running_RUNNING && !app.Running {
				if err := r.control.StartApp(ctx, id); err != nil {
					return err
				}
			}
			if desired.GetDesiredState() == cloudpb.Running_STOPPED && app.Running {
				if err := r.control.StopApp(ctx, id); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unknown legacy desired state %d", desired.GetDesiredState())
		}
	}
	return nil
}

// The deployed v1 schema shares its release column between reported and
// desired state. Never let an older local release overwrite a newly assigned
// release (including the first report after boot, before reading the target).
func protectLegacyDesiredReleases(report *cloudpb.UpdateReportedStateRequest, desired map[string]int32) {
	for _, app := range report.CurrentStates {
		if app.AppReleaseId != nil && app.GetAppReleaseId() != desired[app.AppId] {
			app.AppReleaseId = nil
		}
	}
}

type legacyMonitoredApps struct {
	AppReporter
	containers *ContainerService
}

func (r legacyMonitoredApps) ReportApps(ctx context.Context) ([]AppReport, error) {
	apps, err := r.AppReporter.ReportApps(ctx)
	if err != nil {
		return nil, err
	}
	if _, ok := r.containers.monitor.(RestartStatusProvider); !ok {
		return apps, nil
	}
	containers, err := r.containers.containerd.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	r.containers.applyRestartStatus(containers)
	counts := map[string]int32{}
	for _, container := range containers {
		counts[container.GetAppName()] = int32(min(uint32(math.MaxInt32), container.GetFailureCount()))
	}
	for i := range apps {
		apps[i].RestartCount = counts[apps[i].AppID]
	}
	return apps, nil
}
