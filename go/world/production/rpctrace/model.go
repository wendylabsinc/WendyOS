// Package rpctrace records bounded, opt-in local elapsed-time diagnostics.
// Measurements are never authority time, freshness evidence or admission state.
package rpctrace

import "errors"

const (
	MaxCapacity = 256
	MaxEvents   = 4096
	MaxBytes    = 1 << 20
)

var (
	ErrConfig = errors.New("invalid bounded RPC trace configuration")
	ErrWriter = errors.New("RPC trace writer unavailable")
)

type Config struct {
	// Zero uses the hard limit. Positive values may only lower it. Capacity
	// bounds both queued events and simultaneous live collectors separately.
	Capacity, MaxEvents, MaxBytes int
}

type Method uint8

const (
	MethodClock Method = iota + 1
	MethodPhysicalClock
	MethodQuery
	MethodGetEvidence
	MethodPutEvidence
	MethodGetRecordProof
	MethodPublish
	MethodPublishStream
	MethodPeerWatch
	MethodAuthorityWatch
	MethodGetSourceAuthority
)

var methodNames = [...]string{"", "Clock", "PhysicalClock", "Query", "GetEvidence", "PutEvidence", "GetRecordProof", "Publish", "PublishStream", "PeerWatch", "AuthorityWatch", "GetSourceAuthority"}

type Stage uint8

const (
	StageAuthorize Stage = iota
	StagePeer
	StageConfig
	StagePublisher
	StageAdmissionWait
	StageIdentityWait
	StageIdentityHeld
	StageJournalWait
	StageJournalSnapshot
	StageVerify
	StageJournalSchema
	StageSchema
	StageSourceClock
	StageEvidenceWait
	StageEvidenceRead
	StageEvidenceFloor
	StageJournalCommit
	StageReplica
	StageFinalize
	StageArchivePrune
	StageArchiveRemember
	StageArchiveUsage
	StageArchiveVerify
	StageArchiveCommit
	// Zero-duration branch counts from an already-running remote Snapshot.
	// These observations do not perform another clock or authority check.
	StageRemoteCandidate
	StageRemoteEpochOmitted
	StageRemoteVerifyOmitted
	StageRemoteAuthorityOmitted
	StageRemoteMembershipOmitted
	StageRemoteWindowOmitted
	StageRemoteRecordOmitted
	StageRemoteVisible
	// Details from existing remote-authorization refusal branches. These
	// counters contain no source identity, clock value or error text.
	StageRemoteRecordBoundOmitted
	StageRemotePeerGrantOmitted
	StageRemoteSourceAuthorityOmitted
	StageRemoteInterpretationOmitted
	StageRemoteGrantWindowOmitted
	StageRemoteCallbackWindowOmitted
	StageTotal
	stageCount
)

var stageNames = [...]string{"authorize", "peer", "config", "publisher", "admission_wait", "identity_wait", "identity_held", "journal_wait", "journal_snapshot", "verify", "journal_schema", "schema", "source_clock", "evidence_wait", "evidence_read", "evidence_floor", "journal_commit", "replica", "finalize", "archive_prune", "archive_remember", "archive_usage", "archive_verify", "archive_commit", "remote_candidate", "remote_epoch_omitted", "remote_verify_omitted", "remote_authority_omitted", "remote_membership_omitted", "remote_window_omitted", "remote_record_omitted", "remote_visible", "remote_record_bound_omitted", "remote_peer_grant_omitted", "remote_source_authority_omitted", "remote_interpretation_omitted", "remote_grant_window_omitted", "remote_callback_window_omitted", "total"}

// Code uses the standard gRPC status numbers, without accepting error text.
// Unknown numeric codes are recorded as Unknown.
type Code uint32

const (
	OK Code = iota
	Canceled
	Unknown
	InvalidArgument
	DeadlineExceeded
	NotFound
	AlreadyExists
	PermissionDenied
	ResourceExhausted
	FailedPrecondition
	Aborted
	OutOfRange
	Unimplemented
	Internal
	Unavailable
	DataLoss
	Unauthenticated
)

var codeNames = [...]string{"OK", "Canceled", "Unknown", "InvalidArgument", "DeadlineExceeded", "NotFound", "AlreadyExists", "PermissionDenied", "ResourceExhausted", "FailedPrecondition", "Aborted", "OutOfRange", "Unimplemented", "Internal", "Unavailable", "DataLoss", "Unauthenticated"}

// Labels must come from the host's authenticated service binding. Never copy
// names, credentials, paths or arbitrary request fields into these labels.
type Labels struct {
	Method     Method
	App        string
	Service    string
	Generation uint64
}

type Measurement struct {
	Stage string `json:"stage"`
	Count uint64 `json:"count"`
	SumNS int64  `json:"sum_ns"`
	MaxNS int64  `json:"max_ns"`
}

// Event contains no request, proof, certificate, token, timestamp or error text.
// Stage durations include nested work and must not be added as disjoint costs.
type Event struct {
	Version          int               `json:"version"`
	Sequence         uint64            `json:"sequence"`
	Method           string            `json:"method"`
	App              string            `json:"app"`
	Service          string            `json:"service"`
	Generation       uint64            `json:"generation"`
	Code             string            `json:"code"`
	Inclusive        bool              `json:"inclusive"`
	Stages           []Measurement     `json:"stages"`
	Refusal          *Refusal          `json:"refusal,omitempty"`
	PinInterruptions []PinInterruption `json:"pin_interruptions,omitempty"`
}

type Stats struct {
	Started          uint64 `json:"started"`
	Finished         uint64 `json:"finished"`
	Enqueued         uint64 `json:"enqueued"`
	EnqueuedBytes    uint64 `json:"enqueued_bytes"`
	Written          uint64 `json:"written"`
	WrittenBytes     uint64 `json:"written_bytes"`
	DroppedInvalid   uint64 `json:"dropped_invalid"`
	DroppedInFlight  uint64 `json:"dropped_in_flight"`
	DroppedClosed    uint64 `json:"dropped_closed"`
	DroppedBusy      uint64 `json:"dropped_busy"`
	DroppedQueueFull uint64 `json:"dropped_queue_full"`
	DroppedBudget    uint64 `json:"dropped_budget"`
	WriterErrors     uint64 `json:"writer_errors"`
	Active           int64  `json:"active"`
	Queued           int    `json:"queued"`
	Closed           bool   `json:"closed"`
}

func validLabels(l Labels) bool {
	return l.Method > 0 && int(l.Method) < len(methodNames) && l.Generation != 0 && identifier(l.App, 256) && identifier(l.Service, 128)
}

func identifier(s string, limit int) bool {
	if len(s) == 0 || len(s) > limit {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
