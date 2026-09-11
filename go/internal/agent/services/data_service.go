package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// dataStatusError maps a data-manager error onto a precise gRPC status code.
// An earlier revision collapsed every episode-lookup failure to NotFound,
// which hid malformed requests and genuine I/O or seal failures behind the
// same code. The mapping is:
//
//   - InvalidArgument: the request itself is malformed (bad episode id,
//     out-of-range download offset, malformed or escaping file path).
//   - NotFound: the episode, manifest, or requested file is genuinely absent.
//   - FailedPrecondition: the episode exists but is not currently serviceable
//     (no active episode, or a manifest entry that is not a regular file).
//   - Internal: everything else, which is an I/O, decode, or seal failure.
//
// Messages are the underlying error text, which is generated agent-side and
// carries no client-supplied secrets or host paths beyond episode-relative
// names.
func dataStatusError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, data.ErrInvalidEpisodeID),
		errors.Is(err, data.ErrInvalidDownloadOffset),
		errors.Is(err, data.ErrInvalidEpisodePath),
		errors.Is(err, data.ErrEpisodePathEscapes),
		errors.Is(err, data.ErrInvalidCampaignName):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, os.ErrNotExist):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, data.ErrNoActiveEpisode),
		errors.Is(err, data.ErrEpisodeEntryNotRegular):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}

type DataService struct {
	agentpbv2.UnimplementedDataServiceServer
	manager   *data.Manager
	adapterMu sync.RWMutex
	adapters  []dataCaptureAdapter
	captureMu sync.Mutex
	// Concurrency is keyed per campaign name; data.AdHocEpisodeKey holds the
	// campaign-less episode started through the Start RPC.
	activeCaptures map[string][]runningDataCapture
	autoStopCancel map[string]context.CancelFunc
	// armingAdapter is the capture adapter that supports pre-roll (the camera
	// adapter). It is kept as a typed handle so the deploy, reconcile, and
	// post-episode paths can arm and disarm campaigns; nil until a video service
	// is registered.
	armingAdapter dataArmingAdapter
	// adHocDrain is the post-seal drain applied to episodes started through the
	// Start RPC, which carry no campaign to declare one. Campaign episodes take
	// their own value from the plan.
	adHocDrain time.Duration
}

type dataCaptureAdapter interface {
	Discover(context.Context) []data.Source
	Start(context.Context, data.CaptureSession, []data.Source) (runningDataCapture, error)
}

// dataArmingAdapter is the optional pre-roll extension of the capture-adapter
// contract. An adapter that implements it can be Armed for a campaign that
// requests a buffer: it subscribes to its producer as a non-owning consumer and
// keeps a standby ring of the last `buffer` of encoded payload WITHOUT writing
// an episode, so that when the campaign triggers the episode opens BEFORE the
// trigger instant. The armed ring is consumed by the adapter's own Start, which
// finds it by the episode's CaptureSession.CampaignKey. Only the camera adapter
// implements it this round; audio and ROS 2 still begin at the trigger and
// report their achieved offset honestly.
type dataArmingAdapter interface {
	Arm(campaignKey string, buffer time.Duration, selected []data.Source)
	Disarm(campaignKey string)
}

type runningDataCapture interface {
	Stop(context.Context) ([]data.CaptureResult, error)
}

func NewDataService(m *data.Manager) *DataService {
	s := &DataService{manager: m, activeCaptures: make(map[string][]runningDataCapture), autoStopCancel: make(map[string]context.CancelFunc), adHocDrain: data.DefaultSealDrain}
	m.SetSourceProvider(s.discoverAdapterSources)
	m.SetApplicationObserver(s.observeApplicationRecord)
	return s
}

func (s *DataService) addAdapter(adapter dataCaptureAdapter) {
	if adapter == nil {
		return
	}
	s.adapterMu.Lock()
	s.adapters = append(s.adapters, adapter)
	s.adapterMu.Unlock()
}

// SetVideoService enables local V4L2/CSI and registered IP-camera capture,
// including camera pre-roll (the camera adapter can be armed for campaigns that
// request a buffer).
func (s *DataService) SetVideoService(video *VideoService) {
	adapter := newCameraDataAdapter(video)
	s.addAdapter(adapter)
	if arming, ok := adapter.(dataArmingAdapter); ok {
		s.adapterMu.Lock()
		s.armingAdapter = arming
		s.adapterMu.Unlock()
	}
}

// SetAudioService enables microphone capture, including level-threshold
// fragment sealing.
func (s *DataService) SetAudioService(audioSvc *AudioService) {
	s.addAdapter(newAudioDataAdapter(audioSvc))
}

// SetROS2Service enables one managed rosbag2 recorder per live RMW graph.
func (s *DataService) SetROS2Service(ros2 *ROS2Service) {
	if ros2 != nil {
		s.addAdapter(newROS2DataAdapter(ros2))
	}
}

func (s *DataService) adapterSnapshot() []dataCaptureAdapter {
	s.adapterMu.RLock()
	defer s.adapterMu.RUnlock()
	return append([]dataCaptureAdapter(nil), s.adapters...)
}

func (s *DataService) discoverAdapterSources(ctx context.Context) []data.Source {
	var out []data.Source
	for _, adapter := range s.adapterSnapshot() {
		out = append(out, adapter.Discover(ctx)...)
	}
	return out
}

func (s *DataService) arming() dataArmingAdapter {
	s.adapterMu.RLock()
	defer s.adapterMu.RUnlock()
	return s.armingAdapter
}

// ReconcileArming arms every deployed campaign that requests a buffer and names
// at least one camera source. It is called once at startup, after the video
// service is registered, so campaigns deployed in a previous agent lifetime
// have their pre-roll rings running again before the first trigger.
func (s *DataService) ReconcileArming(context.Context) {
	if s.arming() == nil {
		return
	}
	campaigns, err := s.manager.Campaigns()
	if err != nil {
		s.manager.Warnf("reconciling camera pre-roll arming: reading campaigns failed; armed campaigns will not pre-roll until redeployed: %v", err)
		return
	}
	for _, campaign := range campaigns {
		s.armCampaign(campaign)
	}
}

// armCampaign (re)arms one campaign's camera sources for pre-roll. It is safe to
// call repeatedly: the adapter disarms any prior ring for the same campaign key
// before installing a fresh one. Campaigns without a buffer or without a camera
// source are left untouched.
func (s *DataService) armCampaign(campaign data.Campaign) {
	arming := s.arming()
	if arming == nil || campaign.State != "armed" || campaign.BufferDuration() <= 0 {
		return
	}
	cameras := s.cameraSourcesFor(campaign)
	if len(cameras) == 0 {
		return
	}
	arming.Disarm(campaign.Name)
	arming.Arm(campaign.Name, campaign.BufferDuration(), cameras)
}

// rearmByName re-arms the campaign with the given name after its episode has
// finalized, so the next trigger again opens with pre-roll. A missing or
// non-armed campaign simply leaves nothing armed.
func (s *DataService) rearmByName(name string) {
	if name == data.AdHocEpisodeKey || s.arming() == nil {
		return
	}
	campaign, err := s.manager.Campaign(name)
	if err != nil {
		return
	}
	s.armCampaign(campaign)
}

// cameraSourcesFor resolves a campaign's camera sources to the source objects
// the arming adapter needs (id plus capture policy), dropping non-camera and
// unresolvable sources.
func (s *DataService) cameraSourcesFor(campaign data.Campaign) []data.Source {
	sources, _, captures, err := s.manager.ResolveCampaignSources(campaign)
	if err != nil {
		return nil
	}
	var out []data.Source
	for _, id := range sources {
		if _, ok := cameraDeviceID(id); !ok {
			continue
		}
		out = append(out, data.Source{ID: id, Kind: "camera", Capture: captures[id]})
	}
	return out
}

func (s *DataService) Sources(ctx context.Context, _ *agentpbv2.DataSourcesRequest) (*agentpbv2.DataSourcesResponse, error) {
	r := &agentpbv2.DataSourcesResponse{}
	for _, source := range s.manager.Sources(ctx) {
		r.Sources = append(r.Sources, &agentpbv2.DataSource{Id: source.ID, Kind: source.Kind, ClockDomain: source.ClockDomain, Healthy: source.Healthy, Detail: source.Detail})
	}
	return r, nil
}

func (s *DataService) Start(ctx context.Context, req *agentpbv2.DataStartRequest) (*agentpbv2.DataEpisode, error) {
	cal := map[string][]byte{}
	for _, c := range req.GetCalibrations() {
		cal[c.GetSource()] = c.GetContents()
	}
	return s.startCapture(ctx, data.StartOptions{Name: req.GetName(), Sources: req.GetSources(), ExcludeSources: req.GetExcludeSources(), RequireUTCUncertainty: time.Duration(req.GetRequireUtcUncertaintyNanos()), Calibrations: cal, DrainDuration: s.adHocDrain, CollectorVersion: version.Version})
}

func (s *DataService) startCapture(ctx context.Context, opts data.StartOptions) (*agentpbv2.DataEpisode, error) {
	s.captureMu.Lock()
	defer s.captureMu.Unlock()
	key := opts.Trigger.CampaignName
	m, err := s.manager.Start(opts)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	session, ok := s.manager.ActiveSession(key)
	if !ok {
		return nil, status.Error(codes.Internal, "episode session disappeared during capture startup")
	}
	selected := make([]data.Source, 0, len(m.Sources))
	for _, stats := range m.Sources {
		selected = append(selected, stats.Source)
	}
	var captures []runningDataCapture
	for _, adapter := range s.adapterSnapshot() {
		capture, startErr := adapter.Start(ctx, session, selected)
		if startErr == nil {
			if capture != nil {
				captures = append(captures, capture)
			}
			continue
		}
		var results []data.CaptureResult
		for i := len(captures) - 1; i >= 0; i-- {
			r, _ := captures[i].Stop(context.Background())
			results = append(results, r...)
		}
		if applyErr := s.manager.ApplyCaptureResults(key, results); applyErr != nil {
			s.manager.Warnf("recording capture results for episode key %q after a failed adapter start: %v", key, applyErr)
		}
		// Without the drain, deliberately. captureMu has been held since the top
		// of this function and is what serialises every start and stop on the
		// device, so a drain taken here is charged to every other caller: with
		// the default two second drain a Start whose adapter errors took two
		// seconds to return FailedPrecondition, and with capture.drain: 30s a
		// flapping camera stalled the data service for thirty seconds per
		// attempt. Nothing on this path needs the wait either: the adapters
		// never started, so no application ever read a sample from this episode
		// and no record about it can be outstanding.
		_, _ = s.manager.InterruptWithoutDrain(key, "capture_adapter_start_failed")
		return nil, status.Error(codes.FailedPrecondition, fmt.Sprintf("starting capture adapter: %v", startErr))
	}
	s.activeCaptures[key] = captures
	return manifestEpisode(m), nil
}

func (s *DataService) Stop(ctx context.Context, _ *agentpbv2.DataStopRequest) (*agentpbv2.DataEpisode, error) {
	// The Stop RPC names no episode. Prefer the ad-hoc episode; otherwise stop
	// the single active campaign episode, and refuse when that is ambiguous.
	keys := s.manager.ActiveEpisodeKeys()
	key := data.AdHocEpisodeKey
	adHoc := false
	for _, active := range keys {
		adHoc = adHoc || active == data.AdHocEpisodeKey
	}
	if !adHoc {
		if len(keys) > 1 {
			return nil, status.Error(codes.FailedPrecondition, "multiple campaign episodes are active; wait for their after_trigger windows or trigger-specific tooling to finish them")
		}
		if len(keys) == 1 {
			key = keys[0]
		}
	}
	return s.stopCapture(ctx, key)
}

func (s *DataService) stopCapture(ctx context.Context, key string) (*agentpbv2.DataEpisode, error) {
	s.captureMu.Lock()
	defer s.captureMu.Unlock()
	return s.stopCaptureLocked(ctx, key)
}

// stopCaptureIfCurrent finalizes the episode keyed by key only while the
// given episode is still the active one. The check runs under captureMu so a
// stale auto-stop timer that raced a manual stop plus an immediate re-trigger
// cannot finalize the campaign's new episode.
func (s *DataService) stopCaptureIfCurrent(ctx context.Context, key, episodeID string) {
	s.captureMu.Lock()
	defer s.captureMu.Unlock()
	session, ok := s.manager.ActiveSession(key)
	if !ok || session.ID != episodeID {
		return
	}
	_, _ = s.stopCaptureLocked(ctx, key)
}

func (s *DataService) stopCaptureLocked(ctx context.Context, key string) (*agentpbv2.DataEpisode, error) {
	if cancel := s.autoStopCancel[key]; cancel != nil {
		cancel()
		delete(s.autoStopCancel, key)
	}
	var results []data.CaptureResult
	var captureErrs []error
	captures := s.activeCaptures[key]
	for i := len(captures) - 1; i >= 0; i-- {
		r, err := captures[i].Stop(ctx)
		results = append(results, r...)
		if err != nil {
			captureErrs = append(captureErrs, err)
		}
	}
	delete(s.activeCaptures, key)
	if len(results) > 0 {
		// A manifest sealed without its per-source capture results reports an
		// episode as complete while silently omitting what each source
		// actually produced, so the failure is surfaced rather than dropped.
		if applyErr := s.manager.ApplyCaptureResults(key, results); applyErr != nil {
			s.manager.Warnf("recording capture results into episode key %q failed; its manifest will omit per-source capture counters: %v", key, applyErr)
		}
	}
	var m data.Manifest
	var err error
	if len(captureErrs) > 0 {
		m, err = s.manager.Interrupt(key, "capture_adapter_failed")
	} else {
		m, err = s.manager.Stop(key)
	}
	if errors.Is(err, data.ErrNoActiveEpisode) {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	// The episode's camera pre-roll ring (if any) was consumed at trigger. Re-arm
	// the campaign so its next trigger opens with pre-roll again. Arming
	// subscribes to camera hubs, so it runs off the finalize path.
	if key != data.AdHocEpisodeKey {
		go s.rearmByName(key)
	}
	return manifestEpisode(m), nil
}

func campaignMessage(campaign data.Campaign) (*agentpbv2.DataCampaign, error) {
	b, err := json.MarshalIndent(campaign, "", "  ")
	if err != nil {
		return nil, err
	}
	return &agentpbv2.DataCampaign{Name: campaign.Name, Fleet: campaign.Fleet, State: campaign.State, Revision: campaign.Revision, DeployedUnixNanos: campaign.DeployedUnixNanos, PlanJson: append(b, '\n'), Warnings: campaign.Warnings}, nil
}

func (s *DataService) CampaignDeploy(_ context.Context, req *agentpbv2.DataCampaignDeployRequest) (*agentpbv2.DataCampaign, error) {
	campaign, err := s.manager.DeployCampaign(req.GetCampaignYaml())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	message, err := campaignMessage(campaign)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	// Arm the campaign's camera sources for pre-roll so the very first trigger
	// opens BEFORE the trigger instant. Arming subscribes to camera hubs, so it
	// runs off the request path.
	go s.armCampaign(campaign)
	return message, nil
}

func (s *DataService) Campaigns(context.Context, *agentpbv2.DataCampaignsRequest) (*agentpbv2.DataCampaignsResponse, error) {
	campaigns, err := s.manager.Campaigns()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	response := &agentpbv2.DataCampaignsResponse{}
	for _, campaign := range campaigns {
		message, err := campaignMessage(campaign)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		response.Campaigns = append(response.Campaigns, message)
	}
	return response, nil
}

func (s *DataService) CampaignInspect(_ context.Context, req *agentpbv2.DataCampaignInspectRequest) (*agentpbv2.DataCampaign, error) {
	campaign, err := s.manager.Campaign(req.GetName())
	if err != nil {
		return nil, dataStatusError(err)
	}
	message, err := campaignMessage(campaign)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return message, nil
}

func (s *DataService) CampaignTrigger(ctx context.Context, req *agentpbv2.DataCampaignTriggerRequest) (*agentpbv2.DataEpisode, error) {
	campaign, err := s.manager.Campaign(req.GetName())
	if err != nil {
		return nil, dataStatusError(err)
	}
	reason := strings.TrimSpace(req.GetReason())
	if reason == "" {
		reason = "manual"
	}
	return s.triggerCampaign(ctx, campaign, reason, "manual")
}

func (s *DataService) triggerCampaign(ctx context.Context, campaign data.Campaign, reason, expression string) (*agentpbv2.DataEpisode, error) {
	sources, topics, captures, err := s.manager.ResolveCampaignSources(campaign)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	privacy := make([]data.PrivacyTransformation, 0, len(campaign.Privacy))
	for _, transform := range campaign.Privacy {
		privacy = append(privacy, data.PrivacyTransformation{Name: transform.Name, Revision: transform.Revision, State: "planned_not_applied"})
	}
	calibrationRevisions := map[string]string{}
	for _, source := range campaign.Sources {
		if source.Calibration == "" {
			continue
		}
		identity := source.Camera
		if identity == "" {
			identity = source.ROS2
		}
		if identity == "" && source.Telemetry {
			identity = "telemetry"
		}
		calibrationRevisions[identity] = source.Calibration
	}
	episode, err := s.startCapture(ctx, data.StartOptions{
		Name:                 campaign.Name,
		Sources:              sources,
		SourceCaptures:       captures,
		PreRollDuration:      campaign.BufferDuration(),
		DrainDuration:        campaign.DrainDuration(),
		Trigger:              data.EpisodeTrigger{Reason: reason, CampaignName: campaign.Name, CampaignRevision: campaign.Revision, Expression: expression, Notify: campaign.Notify},
		CollectorVersion:     version.Version,
		ModelVersions:        campaign.Models,
		RequestedTopics:      topics,
		CalibrationRevisions: calibrationRevisions,
		Privacy:              privacy,
		Upload:               data.WorkflowState{State: "pending", Destination: campaign.Upload.Destination},
		Labeling:             data.WorkflowState{State: "unlabeled", Destination: campaign.Export.Annotation},
	})
	if err != nil {
		return nil, err
	}
	timerContext, cancel := context.WithCancel(context.Background())
	s.captureMu.Lock()
	if previous := s.autoStopCancel[campaign.Name]; previous != nil {
		previous()
	}
	s.autoStopCancel[campaign.Name] = cancel
	s.captureMu.Unlock()
	go func(episodeID string, after time.Duration) {
		timer := time.NewTimer(after)
		defer timer.Stop()
		select {
		case <-timerContext.Done():
			return
		case <-timer.C:
			s.stopCaptureIfCurrent(context.Background(), campaign.Name, episodeID)
		}
	}(episode.GetId(), campaign.AfterTriggerDuration())
	return episode, nil
}

func (s *DataService) observeApplicationRecord(_ string, record data.ApplicationRecord) {
	// Triggers are dropped only for campaigns that are still CAPTURING; other
	// campaigns (and ad-hoc recordings) capture independently. A campaign whose
	// previous episode is inside its post-seal drain is not capturing, and
	// ActiveEpisodeKeys no longer names it, so a record that matches during the
	// drain starts the next episode instead of being dropped. That episode
	// still has to wait for captureMu, which the stopping episode holds until
	// its drain ends, so it begins up to one capture.drain late rather than not
	// at all -- documented for operators in the data command reference.
	activeKeys := map[string]bool{}
	for _, key := range s.manager.ActiveEpisodeKeys() {
		activeKeys[key] = true
	}
	campaigns, err := s.manager.Campaigns()
	if err != nil {
		// Without the campaign list no armed trigger can fire, which is
		// indistinguishable from "no record matched" unless it is said.
		s.manager.Warnf("reading campaigns to evaluate triggers failed; no campaign can fire until this clears: %v", err)
		return
	}
	for _, campaign := range campaigns {
		if campaign.State != "armed" || activeKeys[campaign.Name] {
			continue
		}
		reason, expression, matched := campaign.Match(record)
		if !matched {
			continue
		}
		campaign := campaign
		go func() {
			if _, triggerErr := s.triggerCampaign(context.Background(), campaign, reason, expression); triggerErr != nil {
				// An armed campaign that matched but could not start an
				// episode must not look the same as one that never matched.
				s.manager.Warnf("campaign %q matched %s but starting its episode failed: %v", campaign.Name, reason, triggerErr)
			}
		}()
	}
}

func (s *DataService) Status(context.Context, *agentpbv2.DataStatusRequest) (*agentpbv2.DataStatusResponse, error) {
	m := s.manager.Status()
	if m == nil {
		return &agentpbv2.DataStatusResponse{}, nil
	}
	return &agentpbv2.DataStatusResponse{Active: manifestEpisode(*m)}, nil
}

func (s *DataService) Episodes(context.Context, *agentpbv2.DataEpisodesRequest) (*agentpbv2.DataEpisodesResponse, error) {
	list, err := s.manager.List()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	r := &agentpbv2.DataEpisodesResponse{}
	for _, e := range list {
		r.Episodes = append(r.Episodes, infoEpisode(e))
	}
	return r, nil
}

func (s *DataService) Inspect(_ context.Context, req *agentpbv2.DataInspectRequest) (*agentpbv2.DataInspectResponse, error) {
	m, failures, err := s.manager.Inspect(req.GetEpisode(), req.GetVerify())
	if err != nil {
		return nil, dataStatusError(err)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &agentpbv2.DataInspectResponse{ManifestJson: append(b, '\n'), VerificationErrors: failures}, nil
}

func (s *DataService) Download(req *agentpbv2.DataDownloadRequest, stream agentpbv2.DataService_DownloadServer) error {
	s.manager.BeginDownload(req.GetEpisode())
	defer s.manager.EndDownload(req.GetEpisode())
	f, meta, err := s.manager.OpenFile(req.GetEpisode(), req.GetPath(), req.GetOffset())
	if err != nil {
		return dataStatusError(err)
	}
	defer f.Close()
	buf := make([]byte, 256*1024)
	offset := req.GetOffset()
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			if err := stream.Send(&agentpbv2.DataDownloadChunk{Path: meta.Path, Offset: offset, Data: buf[:n], Size: meta.Size, Sha256: meta.SHA256}); err != nil {
				return err
			}
			offset += int64(n)
		}
		if readErr == io.EOF {
			return stream.Send(&agentpbv2.DataDownloadChunk{Path: meta.Path, Offset: offset, Size: meta.Size, Sha256: meta.SHA256, Eof: true})
		}
		if readErr != nil {
			return status.Error(codes.Internal, readErr.Error())
		}
	}
}

func manifestEpisode(m data.Manifest) *agentpbv2.DataEpisode {
	return &agentpbv2.DataEpisode{Id: m.ID, Name: m.Name, State: m.State, StartedUnixNanos: m.StartedUnixNanos, BootId: m.BootID}
}
func infoEpisode(e data.EpisodeInfo) *agentpbv2.DataEpisode {
	return &agentpbv2.DataEpisode{Id: e.ID, Name: e.Name, State: e.State, StartedUnixNanos: e.StartedUnixNanos, SizeBytes: e.SizeBytes, BootId: e.BootID}
}
