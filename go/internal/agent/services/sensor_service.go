package services

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	appspbv1 "github.com/wendylabsinc/wendy/go/proto/gen/appspb/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// maxSubscribeSources bounds how many sources one Subscribe stream may join.
// Each source costs a producer subscription and a goroutine; a model consuming
// more than this many sensors at once is better served by several streams whose
// backpressure is independent.
const maxSubscribeSources = 8

// sensorFanInBuffer is the depth of the per-source queue between a producer
// subscription and the single stream sender. It exists only to absorb the
// jitter of serializing one sample while the next arrives; a model that cannot
// keep up must see honest drops rather than unbounded memory growth.
const sensorFanInBuffer = 2

// SensorSample is one sample handed from a producer to a model subscriber. It
// is the harness-internal form of appspbv1.SensorSample.
type SensorSample struct {
	SourceID string
	SampleID uint64
	// BootNanos is the agent's bracketed CLOCK_BOOTTIME receipt of the sample;
	// UncertaintyNanos is the bracket half-width.
	BootNanos        int64
	UncertaintyNanos int64
	Payload          []byte
	Encoding         string
	// SelfContained reports that Payload holds exactly one whole encoded unit.
	SelfContained bool
	// DroppedBefore counts samples lost between the producer and this
	// subscriber since the previous delivered sample.
	DroppedBefore uint64
}

// sensorSubscription is one live subscription to a source's producer.
type sensorSubscription interface {
	// Next blocks until the next sample is available. It returns a non-nil
	// error when the producer stopped or ctx was cancelled.
	Next(ctx context.Context) (SensorSample, error)
	// Close releases the subscription.
	Close()
}

// sensorProvider is implemented by the services that own a multiplexing sensor
// producer. Implementations must join the SAME producer the episode capture
// adapter uses; a provider that opens the device again would recreate the
// single-holder conflict this whole path exists to remove.
type sensorProvider interface {
	// SupportsSensorSource reports whether sourceID names a source this
	// provider can hand to a model subscriber.
	SupportsSensorSource(sourceID string) bool
	// SubscribeSensor joins the producer for sourceID.
	SubscribeSensor(ctx context.Context, sourceID string) (sensorSubscription, error)
}

// SensorService serves the read-only model-input surface: an app subscribes to
// sensor sources and receives identified samples, and every sample it receives
// is recorded into whatever episodes are active.
//
// One instance is bound to one app identity. The identity comes from the socket
// the request arrived on (the private mount is the credential, exactly as for
// the System API socket), never from the request body.
type SensorService struct {
	appspbv1.UnimplementedSensorServiceServer
	appID   string
	manager *data.Manager
	// permits reports whether this app is allowed to reach a source id. It
	// carries the entitlement's allowlist, which sensor-read requires, so an
	// app neither sees nor can subscribe to any sensor it did not name. Nil
	// means unrestricted and is a test-only default: the socket manager always
	// installs a check. It is consulted live because a multi-service app's
	// owner set changes while the socket keeps serving.
	permits func(sourceID string) bool

	mu        sync.RWMutex
	providers []sensorProvider
}

// NewSensorService builds the service for one app identity. A nil manager
// disables the tee, which is only correct in tests: production always passes
// the capture manager, because a sample handed to a model that no episode can
// see is the defect this service exists to fix.
func NewSensorService(appID string, manager *data.Manager) *SensorService {
	return &SensorService{appID: appID, manager: manager}
}

// SetSourcePermission installs the entitlement's allowlist check. A nil permits
// function (the default) allows every source; production never leaves it nil,
// because the sensor-read entitlement requires an allowlist and the socket
// manager installs the check derived from it.
func (s *SensorService) SetSourcePermission(permits func(sourceID string) bool) {
	s.mu.Lock()
	s.permits = permits
	s.mu.Unlock()
}

// permitted reports whether this app's entitlement covers a source id.
func (s *SensorService) permitted(sourceID string) bool {
	s.mu.RLock()
	permits := s.permits
	s.mu.RUnlock()
	return permits == nil || permits(sourceID)
}

// AddProvider registers a producer-owning service.
func (s *SensorService) AddProvider(provider sensorProvider) {
	if provider == nil {
		return
	}
	s.mu.Lock()
	s.providers = append(s.providers, provider)
	s.mu.Unlock()
}

func (s *SensorService) providerSnapshot() []sensorProvider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]sensorProvider(nil), s.providers...)
}

// providerFor resolves the provider that can stream a source.
func (s *SensorService) providerFor(sourceID string) sensorProvider {
	for _, provider := range s.providerSnapshot() {
		if provider.SupportsSensorSource(sourceID) {
			return provider
		}
	}
	return nil
}

func (s *SensorService) Sources(ctx context.Context, _ *appspbv1.SensorSourcesRequest) (*appspbv1.SensorSourcesResponse, error) {
	if s.manager == nil {
		return nil, status.Error(codes.Unavailable, "sensor sources are unavailable: no capture manager is configured")
	}
	response := &appspbv1.SensorSourcesResponse{}
	for _, source := range s.manager.Sources(ctx) {
		if !s.permitted(source.ID) {
			// An allowlisted app must not even learn what else the device has.
			continue
		}
		subscribable := s.providerFor(source.ID) != nil
		detail := source.Detail
		if !subscribable {
			// Say why, rather than hiding the source: a caller must be able to
			// tell "not available to models" from "does not exist".
			detail = appendDetail(detail, "not subscribable: this source has no producer that can multiplex to a model subscriber yet")
		}
		response.Sources = append(response.Sources, &appspbv1.SensorSource{
			Id: source.ID, Kind: source.Kind, ClockDomain: source.ClockDomain,
			Healthy: source.Healthy, Detail: detail, Subscribable: subscribable,
		})
	}
	return response, nil
}

func appendDetail(detail, note string) string {
	if detail == "" {
		return note
	}
	return detail + " (" + note + ")"
}

// Subscribe streams samples for the requested sources and tees every delivered
// sample into the active episodes.
//
// Ordering is deliberate: a sample is recorded into the episode only AFTER it
// has been handed to the model. The ledger therefore never claims a delivery
// that did not happen. The reverse order would be a stronger guarantee against
// losing the record of a delivered sample, but it would let a stream that dies
// mid-send leave the episode asserting the model saw a frame it never received,
// and a false record is worse than a missing one.
func (s *SensorService) Subscribe(req *appspbv1.SensorSubscribeRequest, stream appspbv1.SensorService_SubscribeServer) error {
	sourceIDs, err := normalizeSubscribeSources(req.GetSourceIds())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	samples := make(chan SensorSample, sensorFanInBuffer*len(sourceIDs))
	var wg sync.WaitGroup
	fatal := make(chan error, len(sourceIDs))
	for _, sourceID := range sourceIDs {
		if !s.permitted(sourceID) {
			cancel()
			wg.Wait()
			return status.Errorf(codes.PermissionDenied, "this app's sensor-read entitlement allowlist does not list source %q", sourceID)
		}
		provider := s.providerFor(sourceID)
		if provider == nil {
			cancel()
			wg.Wait()
			return s.unsubscribableError(ctx, sourceID)
		}
		subscription, err := provider.SubscribeSensor(ctx, sourceID)
		if err != nil {
			cancel()
			wg.Wait()
			return err
		}
		wg.Add(1)
		go func(sourceID string, subscription sensorSubscription) {
			defer wg.Done()
			defer subscription.Close()
			pending := uint64(0)
			for {
				sample, err := subscription.Next(ctx)
				if err != nil {
					if ctx.Err() == nil {
						fatal <- fmt.Errorf("source %s: %w", sourceID, err)
					}
					return
				}
				sample.SourceID = sourceID
				sample.DroppedBefore += pending
				select {
				case samples <- sample:
					pending = 0
				case <-ctx.Done():
					return
				default:
					// The model is not keeping up. Dropping here and reporting
					// it on the next delivered sample keeps memory bounded and
					// keeps the gap in sample_id explained.
					pending = sample.DroppedBefore + 1
				}
			}
		}(sourceID, subscription)
	}
	// Closing samples once every producer goroutine has returned lets the send
	// loop below exit on producer termination without a second signal.
	done := make(chan struct{})
	go func() { wg.Wait(); close(samples); close(done) }()
	defer func() { cancel(); <-done }()

	for {
		select {
		case err := <-fatal:
			return status.Error(codes.Unavailable, err.Error())
		case <-ctx.Done():
			return nil
		case sample, ok := <-samples:
			if !ok {
				select {
				case err := <-fatal:
					return status.Error(codes.Unavailable, err.Error())
				default:
					return nil
				}
			}
			if err := stream.Send(sensorSampleMessage(sample)); err != nil {
				return err
			}
			s.recordDelivered(req.GetModel(), sample)
		}
	}
}

// unsubscribableError explains why a source has no producer a model can join.
// "Does not exist on this device" and "exists, but nothing can multiplex a
// source of that kind to an app yet" are different problems with different
// fixes, and one shared "not available" message left the app author guessing
// which one they had hit. Only camera sources have a producer hub today; audio
// and ROS 2 sources are captured into episodes but cannot be streamed to an
// app, so an app subscribing to one must be told that in as many words.
func (s *SensorService) unsubscribableError(ctx context.Context, sourceID string) error {
	kind, found := s.sourceKind(ctx, sourceID)
	if !found {
		return status.Errorf(
			codes.NotFound,
			"no sensor source %q exists on this device; run `wendy data sources` to list the source ids it offers",
			sourceID,
		)
	}
	if kind == "" {
		kind = "unknown"
	}
	return status.Errorf(
		codes.Unimplemented,
		"sensor source %q exists but %s sources are not subscribable in this release: no producer can multiplex a %s source to a model subscriber yet, so it can be captured into an episode but not streamed to an app. SensorService.Sources (`wendy data sources`) marks the sources an app may subscribe to with subscribable=true",
		sourceID, kind, kind,
	)
}

// sourceKind looks up the declared kind of a source id, reporting whether the
// device knows the source at all.
func (s *SensorService) sourceKind(ctx context.Context, sourceID string) (string, bool) {
	if s.manager == nil {
		return "", false
	}
	for _, source := range s.manager.Sources(ctx) {
		if source.ID == sourceID {
			return source.Kind, true
		}
	}
	return "", false
}

// recordDelivered tees one delivered sample into every active episode. A
// failure to record is not reported to the app — the model has already had the
// sample and cannot un-see it — but it must not be silent either, so it is
// surfaced through the manager's warning logger.
func (s *SensorService) recordDelivered(model string, sample SensorSample) {
	if s.manager == nil {
		return
	}
	err := s.manager.RecordModelInput(data.ModelInput{
		AppID: s.appID, Model: model, SourceID: sample.SourceID, SampleID: sample.SampleID,
		BootNanos: sample.BootNanos, UncertaintyNanos: sample.UncertaintyNanos,
		PayloadBytes: len(sample.Payload), Encoding: sample.Encoding,
		SelfContained: sample.SelfContained, DroppedBefore: sample.DroppedBefore,
	})
	if err != nil {
		s.manager.Warnf("recording model input %s#%d into the active episode failed: %v", sample.SourceID, sample.SampleID, err)
	}
}

func sensorSampleMessage(sample SensorSample) *appspbv1.SensorSample {
	return &appspbv1.SensorSample{
		SourceId: sample.SourceID, SampleId: sample.SampleID,
		BoottimeNanos: sample.BootNanos, TimestampUncertaintyNanos: sample.UncertaintyNanos,
		Payload: sample.Payload, Encoding: sample.Encoding,
		PayloadSelfContained: sample.SelfContained, DroppedBefore: sample.DroppedBefore,
		BootId: data.BootID(),
	}
}

// normalizeSubscribeSources validates and deduplicates the requested sources.
func normalizeSubscribeSources(requested []string) ([]string, error) {
	if len(requested) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one source id is required")
	}
	seen := map[string]bool{}
	var out []string
	for _, id := range requested {
		if id == "" {
			return nil, status.Error(codes.InvalidArgument, "source id must not be empty")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) > maxSubscribeSources {
		return nil, status.Errorf(codes.InvalidArgument, "at most %d sources may be subscribed on one stream", maxSubscribeSources)
	}
	sort.Strings(out)
	return out, nil
}

// errSensorProducerStopped is returned by a subscription whose producer ended.
var errSensorProducerStopped = errors.New("sensor producer stopped")
