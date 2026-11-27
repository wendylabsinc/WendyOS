// Package appservice hosts private, per-service World publication sockets.
// The socket mount and a commissioned Linux peer check identify the caller;
// requests never supply a device/app identity or a signing key.
package appservice

import (
	"context"
	"errors"
	"math"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/cameraframe"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/protobuf/proto"
)

const SocketFilename = "world.sock"

var (
	ErrClosed    = errors.New("world service socket closed")
	ErrPeer      = errors.New("world service peer denied")
	ErrAdmission = errors.New("publication admission unavailable; outcome may be unknown")
)

// Publisher owns agent-held signing authority. Done must close when the host
// invalidates that authority. Validate must check authenticated time and current
// signing state; the broker never substitutes its wall clock for that check.
type Publisher interface {
	Publish(context.Context, serviceorigin.PublishRequest) (serviceorigin.SignedRecord, error)
	Validate(context.Context) error
	Done() <-chan struct{}
	Err() error
	Close() error
}

type PublisherFactory func(context.Context, serviceorigin.Scope) (Publisher, error)

// Peer is obtained from the kernel, never from an RPC header or request. UID
// alone does not identify a container. The host must attribute PID to the exact
// app, service and deployment generation in Scope and fail closed on ambiguity.
type Peer struct {
	PID int32
	UID uint32
	GID uint32
}

type PeerCheck func(context.Context, Peer, serviceorigin.Scope) error

// Acceptor must perform its atomic mutation inside fence.Commit. It must not
// acknowledge a record before committing it to its intended acceptance store.
// It must honor cancellation, avoid broker lifecycle calls from Commit, and
// bound all work. A cancelled/failed RPC does not prove the record was rolled
// back: a commit that already started may have completed.
type Acceptor func(context.Context, serviceorigin.Scope, serviceorigin.SignedRecord, *Fence) error

// Access callbacks use the same single-use lifecycle fence as admission.
// Reads must happen within Commit too, so authority replacement cannot detach
// the operation from the exact authenticated service generation.
type Querier func(context.Context, serviceorigin.Scope, serviceaccess.Query, *Fence) (serviceaccess.Page, error)
type EvidenceWriter func(context.Context, serviceorigin.Scope, serviceorigin.PublishScope, serviceevidence.PutRequest, *Fence) (serviceevidence.PutResponse, error)
type EvidenceReader func(context.Context, serviceorigin.Scope, serviceaccess.Source, serviceevidence.GetRequest, *Fence) (serviceevidence.GetResponse, error)
type ClockReader func(context.Context, serviceorigin.Scope, []byte, *Fence) (*appv1.ClockReply, error)
type PhysicalClockReader func(context.Context, serviceorigin.Scope, []byte, *Fence) (*appv1.PhysicalClockReply, error)
type RecordProofReader func(context.Context, serviceorigin.Scope, proofref.Reference, *Fence) (serviceaccess.RecordProof, error)
type SourceAuthorityReader func(context.Context, serviceorigin.Scope, []byte, []serviceaccess.Source, *Fence) (*appv1.SourceAuthorityReply, error)
type SensorFrameReader func(context.Context, serviceorigin.Scope, serviceorigin.PublishScope, *Fence) (cameraframe.Frame, error)

// ResponseFinalizer runs after the last potentially blocking peer/authority
// check. It may preserve or filter an already budgeted response, never enlarge
// it. Hosts use a common final clock/binding snapshot for expiring read data.
type ResponseFinalizer func(context.Context, serviceorigin.Scope, proto.Message) (proto.Message, error)

type Limits struct {
	MaxConnections         int
	MaxConcurrentRPCs      int
	MaxRequestBytes        int
	MaxResponseBytes       int
	MessagesPerSecond      float64
	MessageBurst           int
	BytesPerSecond         float64
	ByteBurst              int
	RequestTimeout         time.Duration
	AuthorityCheckInterval time.Duration
}

type Config struct {
	// Directory is an agent-owned root, never an app-controlled path. Each
	// Ensure creates a random child directory; replacements never reuse it.
	Directory        string
	UID, GID         int
	Factory          PublisherFactory
	PeerCheck        PeerCheck
	Accept           Acceptor
	Query            Querier
	PutEvidence      EvidenceWriter
	GetEvidence      EvidenceReader
	Clock            ClockReader
	PhysicalClock    PhysicalClockReader
	GetRecordProof   RecordProofReader
	SourceAuthority  SourceAuthorityReader
	GetSensorFrame   SensorFrameReader
	FinalizeResponse ResponseFinalizer
	Limits           Limits
	// Trace optionally records bounded local profiling data. The host owns its
	// off-RPC writer; a nil sink installs no tracing interceptors.
	Trace *rpctrace.Sink
}

type Mount struct {
	Directory  string
	SocketPath string
}

// ValidateLimits lets commissioning reject invalid resource budgets before
// initializing durable state. Zero fields retain the broker's bounded defaults.
func ValidateLimits(l Limits) error {
	_, err := defaults(l)
	return err
}

func defaults(l Limits) (Limits, error) {
	if math.IsNaN(l.MessagesPerSecond) || math.IsInf(l.MessagesPerSecond, 0) || math.IsNaN(l.BytesPerSecond) || math.IsInf(l.BytesPerSecond, 0) {
		return l, errors.New("finite service rates required")
	}
	if l.MaxConnections == 0 {
		l.MaxConnections = 4
	}
	if l.MaxConcurrentRPCs == 0 {
		l.MaxConcurrentRPCs = 4
	}
	if l.MaxRequestBytes == 0 {
		l.MaxRequestBytes = 64 << 10
	}
	if l.MaxResponseBytes == 0 {
		l.MaxResponseBytes = 256 << 10
	}
	if l.MessagesPerSecond == 0 {
		l.MessagesPerSecond = 20
	}
	if l.MessageBurst == 0 {
		l.MessageBurst = 40
	}
	if l.BytesPerSecond == 0 {
		l.BytesPerSecond = 1 << 20
	}
	if l.ByteBurst == 0 {
		l.ByteBurst = 2 << 20
	}
	if l.RequestTimeout == 0 {
		l.RequestTimeout = 5 * time.Second
	}
	if l.AuthorityCheckInterval == 0 {
		l.AuthorityCheckInterval = 100 * time.Millisecond
	}
	if l.MaxConnections < 1 || l.MaxConnections > 64 || l.MaxConcurrentRPCs < 1 || l.MaxConcurrentRPCs > 64 || l.MaxRequestBytes < 1024 || l.MaxRequestBytes > 128<<10 || l.MaxResponseBytes < 1024 || l.MaxResponseBytes > 1<<20 || l.MessagesPerSecond <= 0 || l.MessagesPerSecond > 10000 || l.MessageBurst < 1 || l.MessageBurst > 10000 || l.BytesPerSecond <= 0 || l.BytesPerSecond > 64<<20 || l.ByteBurst < l.MaxRequestBytes || l.ByteBurst > 64<<20 || l.RequestTimeout < time.Millisecond || l.RequestTimeout > 30*time.Second || l.AuthorityCheckInterval < time.Millisecond || l.AuthorityCheckInterval > time.Second {
		return l, errors.New("bounded world service limits required")
	}
	return l, nil
}
