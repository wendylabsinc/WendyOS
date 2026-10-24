package services

import (
	"context"
	"errors"
	"io"
	"maps"
	"math/rand/v2"
	"os"
	"slices"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"

	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// AppReport is one app's state as the device reports it to cloud.
type AppReport struct {
	AppID string
	// ReleaseID is the cloud AppRelease id from the container's release
	// label; empty when the deploy was not recorded as a release.
	ReleaseID      string
	ManifestDigest string
	Running        bool
	// ExitSignal is the signal that ended the app's last run, 0 for none.
	ExitSignal uint32
}

// AppReporter lists every app on the device for the reported-state stream.
type AppReporter interface {
	ReportApps(ctx context.Context) ([]AppReport, error)
}

// ReportedState reports the device's apps to cloud over the v2
// DeploymentService.HandleReportedState stream (apps spec section 2): every
// app, unconditionally, on change and on a heartbeat, while the device is
// enrolled and cloud is reachable. It never blocks local operation and applies
// nothing cloud answers: the postbox nudge only wakes the attached Postbox,
// whose per-app outcomes ride along on the next report.
type ReportedState struct {
	logger    *zap.Logger
	apps      AppReporter
	pub       TelemetryPublisher // nil: no metric
	poll      time.Duration      // how often a change is looked for
	heartbeat time.Duration      // an unchanged report is resent this often
	retry     time.Duration
	dial      func(cloudURL, certPEM, keyPEM, chainPEM string) (grpc.ClientConnInterface, io.Closer, error)
	postbox   *Postbox // nil: no postbox client

	mu                                  sync.Mutex
	cloudURL, certPEM, keyPEM, chainPEM string
	identityChanged                     chan struct{}
}

// NewReportedState builds a reporter for the given device identity; an empty
// identity (not enrolled) reports nothing until UpdateIdentity supplies one.
func NewReportedState(logger *zap.Logger, apps AppReporter, pub TelemetryPublisher, cloudURL, certPEM, keyPEM, chainPEM string) *ReportedState {
	return &ReportedState{
		logger:    logger,
		apps:      apps,
		pub:       pub,
		poll:      10 * time.Second,
		heartbeat: 5 * time.Minute,
		retry:     30 * time.Second,
		dial:      dialDeviceCloud,
		cloudURL:  cloudURL, certPEM: certPEM, keyPEM: keyPEM, chainPEM: chainPEM,
		identityChanged: make(chan struct{}, 1),
	}
}

func dialDeviceCloud(cloudURL, certPEM, keyPEM, chainPEM string) (grpc.ClientConnInterface, io.Closer, error) {
	endpoint, err := cloudrelay.DeviceEndpoint(cloudURL, os.Getenv("WENDY_DEVICE_CLOUD_URL"))
	if err != nil {
		return nil, nil, err
	}
	conn, err := cloudrelay.DialCloud(endpoint, certPEM, chainPEM, []byte(keyPEM))
	if err != nil {
		return nil, nil, err
	}
	return conn, conn, nil
}

// AttachPostbox routes nudges to p and reports its outcomes. Call before Run.
func (r *ReportedState) AttachPostbox(p *Postbox) { r.postbox = p }

// UpdateIdentity swaps the device identity (enrollment or unenrollment while
// running); an open stream is dropped and reopened under the new identity.
func (r *ReportedState) UpdateIdentity(cloudURL, certPEM, keyPEM, chainPEM string) {
	r.mu.Lock()
	r.cloudURL, r.certPEM, r.keyPEM, r.chainPEM = cloudURL, certPEM, keyPEM, chainPEM
	r.mu.Unlock()
	select {
	case r.identityChanged <- struct{}{}:
	default:
	}
}

// Run reports until ctx is done, reconnecting after any stream failure.
func (r *ReportedState) Run(ctx context.Context) {
	for {
		err := r.session(ctx)
		if errors.Is(err, errIdentityChanged) {
			continue
		}
		if err != nil && ctx.Err() == nil {
			r.logger.Warn("reported state stream ended", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-r.identityChanged:
		case <-time.After(r.retry + rand.N(r.retry)): // jitter: a fleet does not reconnect in one wave
		}
	}
}

var errIdentityChanged = errors.New("device identity changed")

// session runs one stream; it returns nil when there is no identity to report
// under.
func (r *ReportedState) session(ctx context.Context) error {
	r.mu.Lock()
	cloudURL, certPEM, keyPEM, chainPEM := r.cloudURL, r.certPEM, r.keyPEM, r.chainPEM
	r.mu.Unlock()
	if certPEM == "" || keyPEM == "" {
		return nil
	}
	conn, closer, err := r.dial(cloudURL, certPEM, keyPEM, chainPEM)
	if err != nil {
		return err
	}
	defer func() { _ = closer.Close() }()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := cloudpbv2.NewDeploymentServiceClient(conn).HandleReportedState(ctx)
	if err != nil {
		return err
	}
	recvErr := make(chan error, 1)
	go func() {
		for {
			resp, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			if head := resp.PostboxHeadEntryId; head != nil {
				r.nudged(*head)
				if r.postbox != nil {
					r.postbox.Nudge(*head)
				}
			}
		}
	}()

	var last *cloudpbv2.UpdateReportedStateRequest
	var lastSent time.Time
	ticker := time.NewTicker(r.poll)
	defer ticker.Stop()
	for {
		// ponytail: change detection polls the runtime every r.poll; subscribe
		// to containerd task events if a 10 s lag ever matters.
		if req, err := r.snapshot(ctx); err != nil {
			r.logger.Warn("reported state: listing apps failed", zap.Error(err))
		} else if !proto.Equal(req, last) || time.Since(lastSent) >= r.heartbeat {
			if err := stream.Send(req); err != nil {
				return err
			}
			last, lastSent = req, time.Now()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-r.identityChanged:
			return errIdentityChanged
		case err := <-recvErr:
			return err
		case <-ticker.C:
		}
	}
}

func (r *ReportedState) snapshot(ctx context.Context) (*cloudpbv2.UpdateReportedStateRequest, error) {
	apps, err := r.apps.ReportApps(ctx)
	if err != nil {
		return nil, err
	}
	req := &cloudpbv2.UpdateReportedStateRequest{CurrentStates: make([]*cloudpbv2.CurrentState, 0, len(apps))}
	for _, app := range apps {
		state := &cloudpbv2.CurrentState{
			AppId:                  app.AppID,
			ReportedState:          cloudpbv2.Running_STOPPED,
			ReportedLastExitSignal: app.ExitSignal,
			ManifestDigest:         app.ManifestDigest,
		}
		if app.Running {
			state.ReportedState = cloudpbv2.Running_RUNNING
		}
		if app.ReleaseID != "" {
			state.AppReleaseId = proto.String(app.ReleaseID)
		}
		req.CurrentStates = append(req.CurrentStates, state)
	}
	if r.postbox == nil {
		return req, nil
	}
	outcomes := r.postbox.Outcomes()
	for _, state := range req.CurrentStates {
		if o, ok := outcomes[state.AppId]; ok {
			state.PostboxEntryId, state.CorrelationId, state.Outcome = o.EntryID, o.CorrelationID, o.Outcome
			delete(outcomes, state.AppId)
		}
	}
	// A refused or pending app is not on the device: report it absent.
	for _, app := range slices.Sorted(maps.Keys(outcomes)) {
		o := outcomes[app]
		req.CurrentStates = append(req.CurrentStates, &cloudpbv2.CurrentState{
			AppId: app, ReportedState: cloudpbv2.Running_ABSENT,
			PostboxEntryId: o.EntryID, CorrelationId: o.CorrelationID, Outcome: o.Outcome,
		})
	}
	return req, nil
}

// nudged surfaces the postbox head cloud announced. It carries no authority.
func (r *ReportedState) nudged(head int64) {
	r.logger.Info("postbox nudge", zap.Int64("postbox_head_entry_id", head))
	if r.pub == nil {
		return
	}
	r.pub.PublishMetrics(&colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: newAgentResource(),
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Scope: &commonpb.InstrumentationScope{Name: "wendy.postbox"},
				Metrics: []*metricspb.Metric{{
					Name: "postbox.head_entry_id",
					Unit: "1",
					Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
						DataPoints: []*metricspb.NumberDataPoint{{
							TimeUnixNano: uint64(time.Now().UnixNano()),
							Value:        &metricspb.NumberDataPoint_AsInt{AsInt: head},
						}},
					}},
				}},
			}},
		}},
	})
}
