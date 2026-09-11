// Package data implements trustworthy device-local episode capture. It keeps
// UTC correlation separate from the immutable CLOCK_BOOTTIME episode timeline.
package data

import (
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/timesync"
)

const (
	ManifestVersion = 2
	ClockAlgorithm  = "wendy-sandwich-v1"
)

type ClockSample struct {
	BootBeforeNanos int64 `json:"boot_before_nanos"`
	TargetNanos     int64 `json:"target_nanos"`
	BootAfterNanos  int64 `json:"boot_after_nanos"`
}

type UTCObservation struct {
	EpisodeNanos     int64       `json:"episode_nanos"`
	OffsetLowerNanos int64       `json:"offset_lower_nanos"`
	OffsetUpperNanos int64       `json:"offset_upper_nanos"`
	OffsetMidNanos   int64       `json:"offset_midpoint_nanos"`
	UncertaintyNanos int64       `json:"uncertainty_nanos"`
	Confidence       string      `json:"confidence"`
	EvidenceSource   string      `json:"evidence_source"`
	Algorithm        string      `json:"algorithm"`
	ObservedUnixNano int64       `json:"observed_unix_nanos"`
	AgeNanos         int64       `json:"age_nanos"`
	Sample           ClockSample `json:"sample"`
}

type Source struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	ClockDomain string `json:"clock_domain"`
	Healthy     bool   `json:"healthy"`
	Detail      string `json:"detail,omitempty"`
	// Capture is the per-episode capture policy a campaign attached to this
	// source, nil when none applies. It is deliberately excluded from the
	// manifest: the policy lives in the campaign plan, while the manifest
	// records the behavior the adapter actually achieved.
	Capture *SourceCapture `json:"-"`
}

type SourceStats struct {
	Source          Source         `json:"source"`
	RequestedOffset int64          `json:"requested_offset_nanos"`
	ActualOffset    int64          `json:"actual_offset_nanos"`
	Count           uint64         `json:"count"`
	Drops           *uint64        `json:"drops,omitempty"`
	DropAccounting  string         `json:"drop_accounting"`
	MappingError    *int64         `json:"mapping_error_nanos,omitempty"`
	Discontinuities uint64         `json:"discontinuities"`
	Mappings        []ClockMapping `json:"clock_mappings,omitempty"`
	// ModelInputs is present only when this source fed a model subscriber
	// during the episode. It is absent, not zeroed, for a source no model
	// consumed.
	ModelInputs *SourceModelInputs `json:"model_inputs,omitempty"`
}

// Payload retention classes for samples a source handed to a model. They say
// what the episode holds for those samples; they never claim more than the
// capture policy can deliver.
const (
	// RetentionCapturePolicy means this episode captures the source with no
	// rate cap or snapshot interval, so every sample the capture kept is in the
	// episode. What the capture itself lost is in Drops/DropAccounting: this
	// class is not a promise that no frame was dropped.
	RetentionCapturePolicy = "captured_subject_to_drop_accounting"
	// RetentionPolicySubset means the campaign's capture policy deliberately
	// keeps less than the model consumed (a snapshot interval or a rate cap),
	// so the episode holds payloads for only some of the referenced samples.
	RetentionPolicySubset = "capture_policy_keeps_a_subset"
	// RetentionNotCaptured means the source is not among this episode's
	// sources, so the episode holds the ledger entries but no payload bytes.
	RetentionNotCaptured = "not_captured_by_this_episode"
)

// SourceModelInputs accounts for one source's samples that were handed to a
// model during the episode. Requested-versus-kept is explicit: Delivered counts
// what the model consumed, PayloadRetention says what the episode retains of
// it, and SubscriberDrops counts samples the harness produced but the model
// never saw because it was not reading fast enough.
type SourceModelInputs struct {
	SourceID         string `json:"source_id"`
	Delivered        uint64 `json:"delivered_to_models"`
	SubscriberDrops  uint64 `json:"subscriber_drops"`
	FirstSampleID    uint64 `json:"first_sample_id"`
	LastSampleID     uint64 `json:"last_sample_id"`
	PayloadRetention string `json:"payload_retention"`
	Note             string `json:"note,omitempty"`
}

// ModelIO describes how to reconstruct (model input, model outcome) pairs from
// this episode offline. It is the point of the whole record: an outcome without
// its input is not training data.
type ModelIO struct {
	// InputLedger is the episode-relative path of the model-input ledger: one
	// JSON object per sample handed to a model subscriber.
	InputLedger string `json:"input_ledger"`
	// OutcomeLog is the episode-relative path of the application records,
	// including predictions and the samples they reference.
	OutcomeLog string `json:"outcome_log"`
	// JoinKeys are the ledger fields a prediction's "inputs" entries match on.
	JoinKeys []string `json:"join_keys"`
	// PayloadLocator explains how a ledger entry reaches its payload bytes.
	PayloadLocator string `json:"payload_locator"`
	// SamplesDelivered counts ledger entries across all sources.
	SamplesDelivered uint64 `json:"samples_delivered"`
	// Predictions counts prediction records recorded into this episode, and
	// PredictionsWithInputs how many of those named the samples they came from.
	// Their difference is the honest measure of unusable outcomes.
	Predictions           uint64 `json:"predictions"`
	PredictionsWithInputs uint64 `json:"predictions_with_inputs"`
	// ReferencesOutsideDelivered counts sample references that name a source
	// this episode delivered nothing for, or a sample_id outside the range it
	// delivered. Such a reference cannot be resolved inside this episode.
	//
	// It is deliberately a range check over the whole episode, and it is NOT
	// proof that a reference names a sample the referring app was actually
	// handed: an in-range identifier that fell in a gap, or one delivered to a
	// different app subscribed to the same source, is not counted here. A
	// consumer that needs exact attribution must join the input ledger on
	// app_id as well (see ModelIO.JoinKeys), because the ledger records which
	// app each sample went to and this counter cannot.
	ReferencesOutsideDelivered uint64 `json:"input_references_outside_delivered_range"`
	// Uncaptured accounts for sources that fed a model during this episode but
	// are not among the episode's own sources. The ledger records what the
	// model consumed; the episode holds no payload bytes for them.
	Uncaptured []SourceModelInputs `json:"uncaptured_sources,omitempty"`
}

// ModelInput is one sample handed to a model subscriber, as recorded in the
// episode's model-input ledger. The payload bytes are NOT duplicated here: the
// ledger references the sample by (SourceID, SampleID), which is the same
// identifier the episode's own capture index records for the payload it kept.
type ModelInput struct {
	AppID            string `json:"app_id"`
	Model            string `json:"model,omitempty"`
	SourceID         string `json:"source_id"`
	SampleID         uint64 `json:"sample_id"`
	BootNanos        int64  `json:"-"`
	EpisodeNanos     int64  `json:"episode_nanos"`
	UncertaintyNanos int64  `json:"timestamp_uncertainty_nanos"`
	PayloadBytes     int    `json:"payload_bytes"`
	Encoding         string `json:"encoding,omitempty"`
	SelfContained    bool   `json:"payload_self_contained"`
	DroppedBefore    uint64 `json:"dropped_before"`
}

// SampleRef names one harness sample a model consumed. It is the reference a
// prediction record carries so an outcome can be traced back to its input.
type SampleRef struct {
	SourceID string `json:"source_id"`
	SampleID uint64 `json:"sample_id"`
}

// ClockMapping describes one immutable source-clock to CLOCK_BOOTTIME mapping
// segment. Raw samples live beside the captured stream; this summary makes the
// achieved bound and any discontinuity visible without parsing the index.
type ClockMapping struct {
	ID                string `json:"id"`
	SourceClockDomain string `json:"source_clock_domain"`
	CanonicalClock    string `json:"canonical_clock"`
	StartedEpisodeNS  int64  `json:"started_episode_nanos"`
	EndedEpisodeNS    int64  `json:"ended_episode_nanos,omitempty"`
	MaxErrorNanos     int64  `json:"max_error_nanos"`
	Samples           uint64 `json:"samples"`
	Algorithm         string `json:"algorithm"`
	Discontinuity     string `json:"discontinuity,omitempty"`
}

// CaptureSession is the immutable episode context handed to source adapters.
// Directory names the journaled .partial directory and is never user supplied.
type CaptureSession struct {
	ID               string
	Directory        string
	RequestBootNanos int64
	BootID           string
	// CampaignKey is the concurrency key of the episode's campaign
	// (AdHocEpisodeKey for campaign-less episodes). Capture adapters use it to
	// find the standby pre-roll ring a campaign armed for its camera sources, so
	// the trigger flushes that ring into the opening frames on the same
	// subscription that then continues live (see the camera adapter's Arm).
	CampaignKey string
}

// CaptureResult is reported by an adapter before the episode is sealed.
type CaptureResult struct {
	SourceID        string
	ClockDomain     string
	SourceDetail    string
	ActualOffset    *int64
	Count           uint64
	Drops           *uint64
	DropAccounting  string
	MappingError    *int64
	Discontinuities uint64
	Mappings        []ClockMapping
}

// MonotonicMappingSample is a sandwich sample mapping Linux CLOCK_MONOTONIC
// into canonical CLOCK_BOOTTIME. The interval captures syscall/read latency.
type MonotonicMappingSample struct {
	BootBeforeNanos  int64 `json:"boot_before_nanos"`
	MonotonicNanos   int64 `json:"monotonic_nanos"`
	BootAfterNanos   int64 `json:"boot_after_nanos"`
	OffsetLowerNanos int64 `json:"offset_lower_nanos"`
	OffsetUpperNanos int64 `json:"offset_upper_nanos"`
}

// FileRoleDerived marks a manifest file computed from capture payload at seal
// time rather than recorded during capture. Today that is the per-source
// cameras/<source>/playable.mp4 remux. A derived file is checksummed, uploaded
// and verified exactly like every other listed file, but it is not capture
// payload: model-input and payload-retention accounting resolve payload bytes
// through cameras/<source>/index.jsonl into the raw segments and must never
// count a derived file, and deleting one loses no recorded data.
const FileRoleDerived = "derived"

type File struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	SourceID  string `json:"source_id,omitempty"`
	Format    string `json:"format"`
	MediaType string `json:"media_type"`
	// Role distinguishes what a file is to the episode. Empty means capture
	// payload or capture metadata written while recording; FileRoleDerived
	// marks an artifact computed from that payload at seal time.
	Role string `json:"role,omitempty"`
}

type Calibration struct {
	Source   string `json:"source"`
	Revision string `json:"revision,omitempty"`
	Path     string `json:"path,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
}

type DeviceIdentity struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname,omitempty"`
	BootID   string `json:"boot_id"`
}

type EpisodeTrigger struct {
	Reason           string `json:"reason"`
	CampaignName     string `json:"campaign_name,omitempty"`
	CampaignRevision string `json:"campaign_revision,omitempty"`
	Expression       string `json:"expression,omitempty"`
	// Notify carries the triggering campaign's notify block verbatim into the
	// committed manifest, and from there into the manifest JSON the cloud
	// ingest service receives. The device attaches no behavior to it; the
	// cloud reads it to decide whether to notify.
	Notify *CampaignNotify `json:"notify,omitempty"`
}

type PrivacyTransformation struct {
	Name     string `json:"name"`
	Revision string `json:"revision,omitempty"`
	State    string `json:"state"`
}

type WorkflowState struct {
	State       string `json:"state"`
	Destination string `json:"destination,omitempty"`
	UpdatedAt   int64  `json:"updated_unix_nanos,omitempty"`
	// Attempts counts the transfer attempts made so far for this workflow. It
	// drives the bounded retry ceiling; a successful transfer leaves it at the
	// value reached, a permanent failure records the final count.
	Attempts int `json:"attempts,omitempty"`
	// LastError records the human-readable reason for the most recent retryable
	// failure or the permanent failure that moved the workflow to "failed".
	LastError string `json:"last_error,omitempty"`
	// NextAttemptUnixNanos is the earliest wall-clock time the transfer worker
	// should retry a "pending" workflow after a retryable failure (backoff).
	NextAttemptUnixNanos int64 `json:"next_attempt_unix_nanos,omitempty"`
}

type Manifest struct {
	Version           int                     `json:"version"`
	ID                string                  `json:"id"`
	Name              string                  `json:"name,omitempty"`
	State             string                  `json:"state"`
	Interruption      string                  `json:"interruption,omitempty"`
	Device            DeviceIdentity          `json:"device"`
	CanonicalClock    string                  `json:"canonical_clock"`
	BootID            string                  `json:"boot_id"`
	RequestBootNanos  int64                   `json:"request_boottime_nanos"`
	StartedEpisodeNS  int64                   `json:"started_episode_nanos"`
	StartedUnixNanos  int64                   `json:"started_unix_nanos"`
	StoppedEpisodeNS  int64                   `json:"stopped_episode_nanos,omitempty"`
	Trigger           EpisodeTrigger          `json:"trigger"`
	CollectorVersion  string                  `json:"collector_version"`
	ModelVersions     map[string]string       `json:"model_versions"`
	RequestedTopics   []string                `json:"requested_ros2_topics"`
	UTCObservations   []UTCObservation        `json:"utc_observations"`
	Roughtime         []timesync.Consensus    `json:"roughtime_observations,omitempty"`
	SystemClockStatus string                  `json:"system_clock_status"`
	Sources           []SourceStats           `json:"sources"`
	Calibrations      []Calibration           `json:"calibrations"`
	Files             []File                  `json:"files"`
	Privacy           []PrivacyTransformation `json:"privacy_transformations"`
	Upload            WorkflowState           `json:"upload"`
	Labeling          WorkflowState           `json:"labeling"`
	RecoveryActions   []string                `json:"recovery_actions,omitempty"`
	// PlayableNotes records, per camera source, why the seal wrote no
	// cameras/<source>/playable.mp4 or why the one it wrote omits frames.
	// Absence of a note plus absence of the file means the episode captured
	// no camera; a note is the seal's honest account of a mux it refused.
	PlayableNotes []string `json:"playable_notes,omitempty"`
	// PreRollLost counts the application records that would have reached THIS
	// episode's pre-roll window and did not, because the ring buffer hit its
	// byte budget and dropped them while they were still inside their window.
	// Records that merely aged out of the ring are not counted: they were no
	// longer pre-roll for anything, so nothing was lost.
	PreRollLost       uint64  `json:"pre_roll_lost"`
	PreRollAccounting string  `json:"pre_roll_accounting"`
	ModelIO           ModelIO `json:"model_io"`
}

type StartOptions struct {
	Name           string
	Sources        []string
	ExcludeSources []string
	// SourceCaptures carries per-source capture policies keyed by resolved
	// source ID (see Manager.ResolveCampaignSources).
	SourceCaptures        map[string]*SourceCapture
	RequireUTCUncertainty time.Duration
	Calibrations          map[string][]byte
	CalibrationRevisions  map[string]string
	PreRollDuration       time.Duration
	// DrainDuration holds an episode that captures applications open for this
	// long after its capture adapters stop, so an application that scores
	// asynchronously files its verdict into the episode it read rather than
	// into the next one. Zero or less means no drain. The manager applies no
	// default of its own; callers choose the policy, and the service call sites
	// use data.DefaultSealDrain.
	DrainDuration    time.Duration
	Trigger          EpisodeTrigger
	CollectorVersion string
	ModelVersions    map[string]string
	RequestedTopics  []string
	Privacy          []PrivacyTransformation
	Upload           WorkflowState
	Labeling         WorkflowState
}

type EpisodeInfo struct {
	ID               string `json:"id"`
	Name             string `json:"name,omitempty"`
	State            string `json:"state"`
	StartedUnixNanos int64  `json:"started_unix_nanos"`
	SizeBytes        int64  `json:"size_bytes"`
	BootID           string `json:"boot_id"`
}
