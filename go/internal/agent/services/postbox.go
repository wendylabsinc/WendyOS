package services

import (
	"bytes"
	"context"
	"crypto/mldsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"

	"github.com/wendylabsinc/wendy/go/internal/agent/devicesign"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// Postbox is the device half of the AAA contract section 5.3 postbox: it pulls
// this device's operator-signed change requests from cloud over the device's
// own mTLS, verifies every entry itself (hash chain, notarized checkpoint,
// high-water mark, operator signature anchored at this device's tenant CA,
// target, instruction validity, nonce), applies a verified request through the agent's
// existing paths, reports the outcome on the reported-state stream and signs
// an AckHead for the newest verified entry. Cloud is only a queue: nothing is
// applied on its word, and every refusal is named, logged, counted and
// reported, never skipped. Every failure keeps what is running.
type Postbox struct {
	logger     *zap.Logger
	pub        TelemetryPublisher // nil: no metric
	configPath string
	control    postboxControl
	poll       time.Duration
	retry      time.Duration
	now        func() time.Time
	dial       func(cloudURL, certPEM, keyPEM, chainPEM string) (cloudpbv2.DevicePostboxServiceClient, io.Closer, error)
	// checkpoint verifies PullChangesResponse.checkpoint (verifyCheckpoint).
	checkpoint func(raw []byte, roots *x509.CertPool, tenant, target string, mark int64, now time.Time) (postboxCheckpoint, error)
	write      func(configPath, name string, data []byte) error

	wake            chan struct{}
	identityChanged chan struct{}

	mu                                  sync.Mutex
	cloudURL, certPEM, keyPEM, chainPEM string
	state                               *postboxState
	refusals                            map[string]int64
	// answered counts, per app, the answered reports that carried its
	// current ABSENT outcome (see Reported). In memory: a restart counts again.
	answered map[string]postboxAnswered
}

type postboxAnswered struct {
	entryID int64
	reports int
}

// postboxControl is the agent's existing container control path, the one
// the CLI's start/stop calls take.
type postboxControl interface {
	StartApp(ctx context.Context, name string) error
	StopApp(ctx context.Context, name string) error
	// AppOf is the app a container name addresses: the app whose service
	// container it names ("{appID}_{service}"), else the name itself, whole.
	AppOf(ctx context.Context, name string) string
}

// postboxState is everything that must survive a restart. It belongs to one
// chain: a new device identity starts a new one.
type postboxState struct {
	Principal string `json:"principal"`
	// HighWater is the newest entry verified, HeadHash its entry_hash.
	HighWater int64  `json:"highWater"`
	HeadHash  []byte `json:"headHash,omitempty"`
	// Acked is the newest entry cloud accepted an AckHead for; HighWater above
	// it is a pending ack.
	Acked int64 `json:"acked"`
	// Nonces maps each claimed request nonce to its expiry (unix seconds); an
	// entry is dropped once expired, since an expired request is refused anyway.
	Nonces      map[string]int64              `json:"nonces,omitempty"`
	Assignments map[string]*postboxAssignment `json:"assignments,omitempty"` // by deployment id
	Outcomes    map[string]*postboxOutcome    `json:"outcomes,omitempty"`    // by app id
	// Pending is the entry claimed past the mark and not yet decided. Found on
	// load, it is the entry a crash interrupted: never applied, never retried.
	Pending *postboxPending `json:"pending,omitempty"`
	// CheckpointMark is the newest max_id a verified checkpoint bound; it never
	// goes back (checkpoint step 9). TimeFloor (unix seconds) is the newest
	// verified checkpoint genTime: trusted now never falls below it (AAA D12).
	CheckpointMark int64 `json:"checkpointMark,omitempty"`
	TimeFloor      int64 `json:"timeFloor,omitempty"`
	// OrgCAs caches, DER, the CAs GetOrgCAs served that this device verified as
	// issued directly by its tenant CA (the Operator Authority CA).
	OrgCAs [][]byte `json:"orgCAs,omitempty"`
}

type postboxPending struct {
	EntryID       int64  `json:"entryId"`
	CorrelationID string `json:"correlationId"`
	AppID         string `json:"appId,omitempty"`
}

// postboxAssignment is a verified DeviceAssignment held as desired state; the
// registry pull that acts on it is T14.
type postboxAssignment struct {
	AppID           string `json:"appId"`
	AppReleaseID    string `json:"appReleaseId"`
	ManifestDigest  string `json:"manifestDigest"`
	DeploymentKeyID string `json:"deploymentKeyId"`
	DesiredState    string `json:"desiredState"`
}

// postboxOutcome is the latest postbox decision about an app, carried on the
// reported-state stream.
type postboxOutcome struct {
	EntryID       int64                  `json:"entryId"`
	CorrelationID string                 `json:"correlationId"`
	Outcome       cloudpbv2.ApplyOutcome `json:"outcome"`
}

const (
	postboxStateFile = "postbox.json"
	postboxPageSize  = 100
	postboxMaxJWS    = 64 << 10
	// postboxPlatformCap caps valid_until - signing time (the entry token's
	// genTime, RULINGS-LOG 2026-10-24 03:25): 365 days (sem 2026-10-23
	// 22:27, orchestrator 22:45). The device enforces it even though cloud
	// does at accept. A request's own 30 s expiry and its iat window bind the
	// call to cloud only and are not checked here (reqsig README, WDY-4015).
	postboxPlatformCap = 365 * 24 * time.Hour
	postboxAudience    = "https://cloud.wendy.sh/broker"
	// materialPending is the one T14 seam's answer until the registry pull exists.
	materialPending = "material pull pending (T14)"
)

// postboxOperations maps each postbox-relayed method (the descriptor's
// operation) to the payload_type it must carry.
var postboxOperations = map[string]string{
	strings.TrimPrefix(cloudpbv2.DeploymentService_CreateDeployment_FullMethodName, "/"):     "wendycloud.v2.CreateDeploymentRequest",
	strings.TrimPrefix(cloudpbv2.DeploymentService_ControlContainer_FullMethodName, "/"):     "wendycloud.v2.ControlContainerRequest",
	strings.TrimPrefix(cloudpbv2.ArtifactRegistryService_AssignDevice_FullMethodName, "/"):   "wendycloud.v2.DeviceAssignment",
	strings.TrimPrefix(cloudpbv2.ArtifactRegistryService_UnassignDevice_FullMethodName, "/"): "wendycloud.v2.UnassignDeviceRequest",
}

// NewPostbox builds the client; it pulls nothing until UpdateIdentity supplies
// an enrolled identity (the agent passes its boot identity straight away).
func NewPostbox(logger *zap.Logger, pub TelemetryPublisher, configPath string, control postboxControl) *Postbox {
	return &Postbox{
		logger: logger, pub: pub, configPath: configPath, control: control,
		poll:            5 * time.Minute, // the nudge is the fast path; this is the fallback
		retry:           30 * time.Second,
		now:             time.Now,
		dial:            dialPostbox,
		checkpoint:      verifyCheckpoint,
		write:           writeProvisioningFile,
		wake:            make(chan struct{}, 1),
		identityChanged: make(chan struct{}, 1),
		refusals:        map[string]int64{},
	}
}

func dialPostbox(cloudURL, certPEM, keyPEM, chainPEM string) (cloudpbv2.DevicePostboxServiceClient, io.Closer, error) {
	conn, closer, err := dialDeviceCloud(cloudURL, certPEM, keyPEM, chainPEM)
	if err != nil {
		return nil, nil, err
	}
	return cloudpbv2.NewDevicePostboxServiceClient(conn), closer, nil
}

// UpdateIdentity swaps the device identity; empty values stop pulling.
func (p *Postbox) UpdateIdentity(cloudURL, certPEM, keyPEM, chainPEM string) {
	p.mu.Lock()
	p.cloudURL, p.certPEM, p.keyPEM, p.chainPEM = cloudURL, certPEM, keyPEM, chainPEM
	p.mu.Unlock()
	select {
	case p.identityChanged <- struct{}{}:
	default:
	}
}

// Nudge is the head cloud announced on the reported-state stream. It carries
// no authority: it only wakes a pull, which verifies everything it gets.
func (p *Postbox) Nudge(head int64) {
	p.mu.Lock()
	behind := p.state == nil || head > p.state.HighWater
	p.mu.Unlock()
	if !behind {
		return
	}
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Run pulls until ctx is done: on a nudge, on an identity change and on the
// poll timer, backing off with jitter after a failure.
func (p *Postbox) Run(ctx context.Context) {
	failures := 0
	for {
		wait := p.poll
		if err := p.cycle(ctx); err != nil && ctx.Err() == nil {
			failures++
			p.logger.Warn("postbox pull failed", zap.Error(err))
			wait = min(p.retry<<min(failures-1, 5), 30*time.Minute)
			wait = wait/2 + rand.N(wait/2+1) // a fleet does not retry in one wave
		} else {
			failures = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-p.identityChanged:
		case <-time.After(wait):
		}
	}
}

// postboxIdentity is this device as its own leaf proves it.
type postboxIdentity struct {
	tenant, uuid, uri string
	// anchors are the tenant CAs of the device's pinned chain; roots the
	// platform roots there (the checkpoint and TSA chain to those).
	anchors []*x509.Certificate
	roots   *x509.CertPool
	// orgCAs are the cached GetOrgCAs answers, intermediates under anchors.
	orgCAs []*x509.Certificate
	signer *devicesign.Signer // nil: cannot ack (P-256 device)
}

func (p *Postbox) identity() (*postboxIdentity, string, string, string, string, error) {
	p.mu.Lock()
	cloudURL, certPEM, keyPEM, chainPEM := p.cloudURL, p.certPEM, p.keyPEM, p.chainPEM
	p.mu.Unlock()
	if certPEM == "" || keyPEM == "" {
		return nil, "", "", "", "", nil
	}
	leaves, err := certs.ParseCertsFromPEM([]byte(certPEM))
	if err != nil || len(leaves) == 0 {
		return nil, "", "", "", "", fmt.Errorf("parsing device certificate: %v", err)
	}
	principal, _ := certs.TenantPrincipalFromCert(leaves[0])
	id, err := certs.ParsePrincipal(principal)
	if err != nil || principal != certs.DeviceSPIFFEURI(id.TenantUUID, id.EntityID) {
		return nil, "", "", "", "", fmt.Errorf("device certificate names %q, not a device principal", principal)
	}
	// The postbox target is the device UUID, never a name (WDY-3919).
	if u, err := uuid.Parse(id.EntityID); err != nil || u.String() != id.EntityID {
		return nil, "", "", "", "", fmt.Errorf("device principal %q is not UUID-keyed; the postbox needs the UUID identity", principal)
	}
	chain, err := certs.ParseCertsFromPEM([]byte(chainPEM))
	if err != nil {
		return nil, "", "", "", "", fmt.Errorf("parsing device CA chain: %w", err)
	}
	ident := &postboxIdentity{tenant: id.TenantUUID, uuid: id.EntityID, uri: principal, roots: x509.NewCertPool()}
	// The tenant CA of the device's own pinned chain: a CA not issued by
	// another intermediate there. Never the platform root (another tenant's
	// operator authority would vouch for a signer) and never the Device CA
	// (the Operator Authority CA is its sibling, issued by the tenant CA).
	var cas []*x509.Certificate
	for _, ca := range chain {
		switch {
		case !ca.BasicConstraintsValid || !ca.IsCA:
		case bytes.Equal(ca.RawSubject, ca.RawIssuer):
			ident.roots.AddCert(ca)
		default:
			cas = append(cas, ca)
		}
	}
	for _, ca := range cas {
		if !slices.ContainsFunc(cas, func(o *x509.Certificate) bool { return bytes.Equal(o.RawSubject, ca.RawIssuer) }) {
			ident.anchors = append(ident.anchors, ca)
		}
	}
	if len(ident.anchors) == 0 {
		return nil, "", "", "", "", errors.New("device CA chain has no tenant certificate authority to anchor operator signatures")
	}
	if ident.signer, err = devicesign.New(certPEM, keyPEM); err != nil {
		p.logger.Warn("postbox: this device cannot sign AckHead", zap.Error(err))
	}
	return ident, cloudURL, certPEM, keyPEM, chainPEM, nil
}

// cycle pulls every page above the high-water mark, processes what the
// notarized checkpoint covers, then acks the newest verified entry.
func (p *Postbox) cycle(ctx context.Context) error {
	id, cloudURL, certPEM, keyPEM, chainPEM, err := p.identity()
	if id == nil {
		return err
	}
	if err := p.load(id.uri); err != nil {
		return err
	}
	for _, der := range p.snapshot().OrgCAs {
		if ca, err := x509.ParseCertificate(der); err == nil {
			id.orgCAs = append(id.orgCAs, ca)
		}
	}
	if err := p.settleInterrupted(ctx); err != nil {
		return err
	}
	client, closer, err := p.dial(cloudURL, certPEM, keyPEM, chainPEM)
	if err != nil {
		return err
	}
	defer func() { _ = closer.Close() }()

	for {
		hwm := p.snapshot().HighWater
		resp, err := client.PullChanges(ctx, &cloudpbv2.PullChangesRequest{AfterEntryId: hwm, Limit: postboxPageSize})
		if err != nil {
			return fmt.Errorf("pulling postbox: %w", err)
		}
		if len(resp.GetEntries()) == 0 {
			break
		}
		cp, err := p.checkpoint(resp.GetCheckpoint(), id.roots, id.tenant, id.uri, p.snapshot().CheckpointMark, p.trustedNow())
		if cp.tenantWide {
			// Its max_id and head describe the tenant's chain, not this
			// device's: nothing here or in process compares with them
			// (WDY-4015 item 9).
			cp.maxID, cp.head = math.MaxInt64, nil
		}
		cpMax, cpHead := cp.maxID, cp.head
		if err == nil && cpMax < hwm {
			err = fmt.Errorf("checkpoint max_id %d is below the high-water mark %d", cpMax, hwm)
		}
		if err == nil && cpMax == hwm && hwm > 0 && !bytes.Equal(cpHead, p.snapshot().HeadHash) {
			err = fmt.Errorf("checkpoint head at %d differs from the verified head", hwm)
		}
		if err == nil {
			err = p.commit(func(st *postboxState) {
				// A tenant-wide checkpoint (empty target) is valid but binds the
				// tenant's chain, not this device's: only a device-scoped one moves
				// this device's mark (WDY-4015 item 9).
				if !cp.tenantWide {
					st.CheckpointMark = max(st.CheckpointMark, cpMax)
				}
				st.TimeFloor = max(st.TimeFloor, cp.genTime.Unix())
			})
		}
		if err != nil {
			// Hold: nothing is applied, the mark stays, and it is retried.
			p.count("checkpoint")
			p.logger.Warn("postbox held: checkpoint not verified", zap.Error(err),
				zap.Int64("high_water", hwm), zap.Int("entries_waiting", len(resp.GetEntries())))
			break
		}
		stop := false
		for _, entry := range resp.GetEntries() {
			if entry.GetEntryId() > cpMax {
				stop = true // not notarized yet; the next checkpoint covers it
				break
			}
			if err := p.process(ctx, client, id, entry, cp); err != nil {
				p.logger.Error("postbox entry not processed; holding", zap.Error(err), zap.Int64("entry_id", entry.GetEntryId()))
				stop = true
				break
			}
		}
		if stop || !resp.GetHasMore() {
			break
		}
	}
	return p.ack(ctx, client, id)
}

// process verifies one entry's chain link and then its request. A chain
// failure is returned: nothing after a broken link can be trusted, so the
// mark stays and the pull holds. A request refusal advances the mark: the
// entry is part of the verified chain, it is just not applied.
func (p *Postbox) process(ctx context.Context, client cloudpbv2.DevicePostboxServiceClient, id *postboxIdentity, e *cloudpbv2.PostboxEntry, cp postboxCheckpoint) error {
	st := p.snapshot()
	prev := st.HeadHash
	if st.HighWater == 0 {
		prev = make([]byte, sha256.Size) // genesis
	}
	var chainErr error
	switch want := postboxEntryHash(e.GetPrevHash(), id.tenant, id.uri, e.GetEntryId(), e.GetRequestJws(), e.GetTsrResponse()); {
	case e.GetEntryId() != st.HighWater+1:
		chainErr = fmt.Errorf("entry %d does not follow the high-water mark %d", e.GetEntryId(), st.HighWater)
	case !bytes.Equal(e.GetPrevHash(), prev):
		chainErr = errors.New("prev_hash does not link to the verified head")
	case !bytes.Equal(e.GetEntryHash(), want):
		chainErr = errors.New("entry_hash does not match the entry")
	case e.GetEntryId() == cp.maxID && !bytes.Equal(want, cp.head):
		chainErr = errors.New("entry is not the head the checkpoint notarized")
	}
	if chainErr != nil {
		return errors.Join(chainErr, p.decide(ctx, e, "", "", cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_SIGNATURE, "chain", chainErr))
	}

	// The entry's RFC 3161 token, taken by cloud at accept over
	// SHA-256(request_jws), is its trusted signing time (AAA D9), not the
	// signer's iat. A token that does not verify holds the entry, like an
	// unverified checkpoint.
	signedAt, err := verifyTimestampToken(e.GetTsrResponse(), e.GetRequestJws(), cp.inter, id.roots)
	if err != nil {
		p.count("entry_timestamp")
		p.logger.Warn("postbox entry held: timestamp token not verified", zap.String("audit", "postbox"),
			zap.Int64("entry_id", e.GetEntryId()), zap.String("correlation_id", peekCorrelation(e.GetRequestJws())), zap.Error(err))
		return fmt.Errorf("entry timestamp token not verified: %w", err)
	}
	now := p.trustedNow()
	// An operator leaf whose issuer this device does not hold holds the entry
	// (not consumed, not refused) until cloud serves a CA that verifies at the
	// entry's signing time, as the operator chain is checked.
	if err := p.resolveIssuer(ctx, client, id, e, signedAt); err != nil {
		p.count("operator_ca")
		p.logger.Warn("postbox entry held: operator CA not resolved", zap.String("audit", "postbox"),
			zap.Int64("entry_id", e.GetEntryId()), zap.String("correlation_id", peekCorrelation(e.GetRequestJws())), zap.Error(err))
		return fmt.Errorf("operator CA not resolved: %w", err)
	}
	req, desc, outcome, reason, verr := verifyPostboxRequest(e, id, signedAt, now)
	nonceExpiry := int64(0)
	if outcome == cloudpbv2.ApplyOutcome_APPLY_OUTCOME_UNSPECIFIED && verr == nil {
		// Kept until the instruction itself expires: replay is refused while
		// the request could still be valid.
		nonceExpiry = postboxValidUntil(req).GetSeconds() + 1
	}
	// req is set only for a request verified as signed for this device: only
	// that may set an app's reported outcome; any other refusal is audit only.
	app := p.postboxApp(ctx, req)
	// The entry is verified as part of the chain: claim its nonce and move the
	// mark BEFORE acting, so a crash never applies one signed request twice
	// (at most once: an interrupted entry is reported, not retried).
	pending := &postboxPending{EntryID: e.GetEntryId(), CorrelationID: desc.Correlation, AppID: app}
	switch err := p.advance(e, desc.Nonce, nonceExpiry, pending); {
	case errors.Is(err, errReplayed):
		outcome, reason, verr = cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE, "replay", err
	case err != nil:
		return err
	}
	if verr != nil {
		return p.decide(ctx, e, desc.Correlation, app, outcome, reason, verr)
	}
	app, outcome, reason, aerr := p.apply(ctx, req)
	return p.decide(ctx, e, desc.Correlation, app, outcome, reason, aerr)
}

// postboxEntryHash is cloud's chain construction (cloud migration 000062
// postbox_entry_hash): SHA-256 over 4-byte big-endian length-prefixed fields.
func postboxEntryHash(prev []byte, tenant, target string, entryID int64, jws, tsr []byte) []byte {
	h := sha256.New()
	for _, f := range [][]byte{prev, []byte(tenant), []byte(target), binary.BigEndian.AppendUint64(nil, uint64(entryID)), jws, tsr} {
		h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(f))))
		h.Write(f)
	}
	return h.Sum(nil)
}

// postboxDescriptor is the signed request's JCS claims (reqsig v1).
type postboxDescriptor struct {
	Audience    string `json:"aud"`
	BodySHA256  string `json:"body_sha256"`
	Correlation string `json:"correlation_id"`
	Expiry      int64  `json:"expiry"`
	Iat         int64  `json:"iat"`
	Nonce       string `json:"nonce"`
	Operation   string `json:"operation"`
	Target      struct {
		Tenant   string `json:"tenant"`
		CA       string `json:"ca,omitempty"`
		Resource string `json:"resource,omitempty"`
	} `json:"target"`
}

// verifyPostboxRequest checks, in order: the operator signature and its chain
// to this device's tenant CA at signedAt, the entry token's genTime (an
// unverified request's claims mean nothing), then the target, then iat
// against signedAt and the instruction's validity against trusted now. The
// nonce is checked by the caller against the persisted set. It returns the
// decoded payload once signature and target verified, so also with a stale
// refusal.
func verifyPostboxRequest(e *cloudpbv2.PostboxEntry, id *postboxIdentity, signedAt, now time.Time) (proto.Message, postboxDescriptor, cloudpbv2.ApplyOutcome, string, error) {
	desc, outcome, reason, err := verifyPostboxSignature(e, id, signedAt)
	if err != nil {
		return nil, desc, outcome, reason, err
	}
	req, err := decodePostboxPayload(e)
	if err != nil {
		return nil, desc, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_SIGNATURE, "signature", err
	}
	if err := bindsThisDevice(req, desc.Target.Resource, id); err != nil {
		return nil, desc, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_TARGET, "target", err
	}
	// The signer's iat may not claim a time after cloud's stamp.
	if time.Unix(desc.Iat, 0).After(signedAt.Add(postboxCheckpointSkew)) {
		return req, desc, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE, "iat_after_timestamp",
			fmt.Errorf("iat %d is after the entry's timestamp %s", desc.Iat, signedAt)
	}
	if reason, err := postboxValidity(req, signedAt, now); err != nil {
		return req, desc, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE, reason, err
	}
	if desc.Nonce == "" || len(desc.Nonce) > 344 { // 256 bytes, base64url
		return req, desc, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE, "stale", errors.New("request nonce missing or oversized")
	}
	return req, desc, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_UNSPECIFIED, "", nil
}

// postboxValidUntil is the instruction validity the operator signed into the
// payload (WDY-4015 item 3); nil when absent.
func postboxValidUntil(req proto.Message) *timestamppb.Timestamp {
	switch r := req.(type) {
	case *cloudpbv2.ControlContainerRequest:
		return r.GetValidUntil()
	case *cloudpbv2.CreateDeploymentRequest:
		return r.GetValidUntil()
	case *cloudpbv2.UnassignDeviceRequest:
		return r.GetValidUntil()
	case *cloudpbv2.DeviceAssignment:
		return r.GetExpiresAt()
	}
	return nil
}

// postboxValidity is the device's validity check (conformance/postbox-entry):
// valid_until present, within the platform cap of the signing time (the entry
// token's genTime), and trusted now strictly before it. The reason is the
// vectors' refusal code.
func postboxValidity(req proto.Message, signedAt time.Time, now time.Time) (string, error) {
	vu := postboxValidUntil(req)
	switch {
	case vu == nil:
		return "validity_missing", errors.New("instruction carries no valid_until")
	case vu.AsTime().Sub(signedAt) > postboxPlatformCap:
		return "above_platform_cap", fmt.Errorf("valid_until %s is more than the platform cap after the signing time %s", vu.AsTime(), signedAt)
	case !now.Before(vu.AsTime()):
		return "stale", fmt.Errorf("instruction expired at %s (trusted now %s)", vu.AsTime(), now)
	}
	return "", nil
}

// verifyPostboxSignature checks the operator's JWS: ML-DSA-65 over x5c, the
// claims, the operation and payload binding, an operator principal of this
// tenant, and the leaf's chain through the cached org CAs to the tenant CA at
// signedAt.
func verifyPostboxSignature(e *cloudpbv2.PostboxEntry, id *postboxIdentity, signedAt time.Time) (postboxDescriptor, cloudpbv2.ApplyOutcome, string, error) {
	var desc postboxDescriptor
	sig := func(err error) (postboxDescriptor, cloudpbv2.ApplyOutcome, string, error) {
		return desc, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_SIGNATURE, "signature", err
	}
	compact := string(e.GetRequestJws())
	parts := strings.Split(compact, ".")
	if len(parts) != 3 || len(compact) > postboxMaxJWS {
		return sig(errors.New("request is not a compact JWS"))
	}
	header, err := b64url(parts[0])
	if err != nil {
		return sig(err)
	}
	var h struct {
		Alg string   `json:"alg"`
		X5C []string `json:"x5c"`
	}
	if err := strictJSON(header, &h); err != nil {
		return sig(fmt.Errorf("JWS header: %w", err))
	}
	// Tier A/B: operators sign ML-DSA-65 (HARD rules 1-2); x5c only, never kid.
	if h.Alg != "ML-DSA-65" || len(h.X5C) == 0 {
		return sig(fmt.Errorf("JWS header alg %q with %d x5c entries is not an ML-DSA-65 x5c signature", h.Alg, len(h.X5C)))
	}
	chain := make([]*x509.Certificate, 0, len(h.X5C))
	for _, enc := range h.X5C {
		der, err := b64std(enc)
		if err != nil {
			return sig(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return sig(fmt.Errorf("x5c certificate: %w", err))
		}
		chain = append(chain, cert)
	}
	leaf := chain[0]
	if err := verifyMLDSA65(leaf, []byte(parts[0]+"."+parts[1]), parts[2]); err != nil {
		return sig(err)
	}
	claims, err := b64url(parts[1])
	if err != nil {
		return sig(err)
	}
	if err := strictJSON(claims, &desc); err != nil {
		return sig(fmt.Errorf("JWS claims: %w", err))
	}
	wantType, known := postboxOperations[desc.Operation]
	sum := sha256.Sum256(e.GetRequestPayload())
	switch {
	case desc.Audience != postboxAudience:
		return sig(fmt.Errorf("audience %q", desc.Audience))
	case !known || e.GetPayloadType() != wantType:
		return desc, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_TARGET, "operation",
			fmt.Errorf("operation %q with payload_type %q is not a device action", desc.Operation, e.GetPayloadType())
	case desc.BodySHA256 != b64urlEncode(sum[:]):
		return sig(errors.New("body_sha256 does not match the payload"))
	}
	principal, _ := certs.TenantPrincipalFromCert(leaf)
	signer, err := certs.ParsePrincipal(principal)
	if err != nil || signer.EntityType != certs.EntityUser {
		return sig(fmt.Errorf("signer %q is not an operator or service account", principal))
	}
	// Validity at the entry's timestamp: a request queued while the device was
	// offline stays verifiable after its short-lived operator cert expires,
	// and a back-dated iat cannot revive an expired one. The x5c is the leaf
	// (anything after it is only a candidate); the Operator Authority CA
	// comes from the GetOrgCAs cache.
	if _, err := certs.VerifyPeerCertificateChainExpiry(leaf, append(chain[1:], id.orgCAs...), id.anchors, x509.ExtKeyUsageClientAuth, signedAt, signedAt); err != nil {
		return sig(fmt.Errorf("operator certificate does not chain to this device's tenant CA at the entry's timestamp: %w", err))
	}
	if signer.TenantUUID != id.tenant || desc.Target.Tenant != id.tenant {
		return desc, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_TARGET, "target",
			fmt.Errorf("signer tenant %q / target tenant %q is not this device's tenant", signer.TenantUUID, desc.Target.Tenant)
	}
	return desc, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_UNSPECIFIED, "", nil
}

// resolveIssuer makes sure the CA that issued the entry's operator leaf is
// one this device can chain through. When it is not, it asks GetOrgCAs for
// that issuer and caches the served CA that issued the leaf once it verifies
// as issued directly by its tenant CA at signedAt, the entry's signing time
// (conformance/postbox-entry): a CA that expired since still vouches for
// what it signed while valid, as the operator chain is checked. A CA
// rotated under the same subject is a different issuer: the old one stays
// cached for entries it signed. An error means hold: NOT_FOUND, a transport
// failure, or no served CA verified. An unparseable leaf is left for
// verification to refuse.
func (p *Postbox) resolveIssuer(ctx context.Context, client cloudpbv2.DevicePostboxServiceClient, id *postboxIdentity, e *cloudpbv2.PostboxEntry, signedAt time.Time) error {
	leaf := peekLeaf(e.GetRequestJws())
	if leaf == nil {
		return nil
	}
	if slices.ContainsFunc(slices.Concat(id.anchors, id.orgCAs, peekIntermediates(e.GetRequestJws())), func(ca *x509.Certificate) bool { return issued(ca, leaf) }) {
		return nil
	}
	return p.fetchIssuer(ctx, client, id, leaf, signedAt)
}

// issued reports whether ca issued leaf: its subject is the leaf's issuer
// Name and its key signed the leaf.
func issued(ca, leaf *x509.Certificate) bool {
	return bytes.Equal(ca.RawSubject, leaf.RawIssuer) && leaf.CheckSignatureFrom(ca) == nil
}

// fetchIssuer asks GetOrgCAs for the CA named by leaf's issuer and caches a
// served CA only if it issued leaf and verifies at signedAt; the rest of the
// response is ignored. It fails unless a cached CA now issued leaf.
// ponytail: the cache only grows, by one CA per rotation; prune expired CAs
// if rotations ever become frequent.
func (p *Postbox) fetchIssuer(ctx context.Context, client cloudpbv2.DevicePostboxServiceClient, id *postboxIdentity, leaf *x509.Certificate, signedAt time.Time) error {
	resp, err := client.GetOrgCAs(ctx, &cloudpbv2.GetOrgCAsRequest{Issuer: leaf.RawIssuer})
	if err != nil {
		return fmt.Errorf("GetOrgCAs: %w", err)
	}
	var added [][]byte
	for _, der := range resp.GetCaDer() {
		ca, code, err := verifyOrgCA(der, id.anchors, signedAt)
		if err != nil {
			p.count(code)
			p.logger.Warn("postbox: refused a CA cloud served", zap.String("audit", "postbox"), zap.String("reason", code), zap.Error(err))
			continue
		}
		if !issued(ca, leaf) {
			continue // a CA the tenant CA issued, but not this leaf's (e.g. the Device CA)
		}
		if !slices.ContainsFunc(id.orgCAs, ca.Equal) {
			id.orgCAs = append(id.orgCAs, ca)
			added = append(added, ca.Raw)
		}
	}
	if len(added) > 0 {
		if err := p.commit(func(st *postboxState) { st.OrgCAs = append(slices.Clone(st.OrgCAs), added...) }); err != nil {
			return err
		}
	}
	if !slices.ContainsFunc(id.orgCAs, func(ca *x509.Certificate) bool { return issued(ca, leaf) }) {
		return errors.New("no CA cloud served for this issuer is issued by this device's tenant CA")
	}
	return nil
}

// verifyOrgCA accepts a GetOrgCAs certificate only if it is a CA issued
// directly by one of this device's tenant CAs and valid at the given time;
// cloud's word alone is never enough. The code is the vectors' refusal label.
func verifyOrgCA(der []byte, tenantCAs []*x509.Certificate, now time.Time) (*x509.Certificate, string, error) {
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, "not_a_ca", err
	}
	if !ca.BasicConstraintsValid || !ca.IsCA {
		return nil, "not_a_ca", fmt.Errorf("%q is not a CA", ca.Subject.CommonName)
	}
	roots := x509.NewCertPool()
	for _, t := range tenantCAs {
		roots.AddCert(t)
	}
	// Roots only, no intermediates: the one chain possible is [ca, tenant CA].
	chains, err := ca.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	if err != nil || len(chains) == 0 || len(chains[0]) != 2 {
		return nil, "not_under_org_ca", fmt.Errorf("%q is not issued by this device's tenant CA: %v", ca.Subject.CommonName, err)
	}
	return ca, "", nil
}

// trustedNow is the device clock, floored by the newest verified checkpoint
// genTime. With no checkpoint seen yet it is the clock alone, assumed right
// (the agent does not gate on timesync); a clock running behind is bounded by
// the floor once a checkpoint verifies.
func (p *Postbox) trustedNow() time.Time {
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != nil {
		if floor := time.Unix(p.state.TimeFloor, 0); floor.After(now) {
			return floor
		}
	}
	return now
}

// commit applies change to a copy of the state, saves it, and swaps it in
// only once it is on disk.
func (p *Postbox) commit(change func(*postboxState)) error {
	p.mu.Lock()
	st := *p.state
	p.mu.Unlock()
	change(&st)
	data, err := json.Marshal(&st)
	if err == nil {
		err = p.write(p.configPath, postboxStateFile, data)
	}
	if err != nil {
		return fmt.Errorf("persisting postbox state: %w", err)
	}
	p.mu.Lock()
	p.state = &st
	p.mu.Unlock()
	return nil
}

func decodePostboxPayload(e *cloudpbv2.PostboxEntry) (proto.Message, error) {
	var req proto.Message
	switch e.GetPayloadType() {
	case "wendycloud.v2.CreateDeploymentRequest":
		req = &cloudpbv2.CreateDeploymentRequest{}
	case "wendycloud.v2.ControlContainerRequest":
		req = &cloudpbv2.ControlContainerRequest{}
	case "wendycloud.v2.DeviceAssignment":
		req = &cloudpbv2.DeviceAssignment{}
	case "wendycloud.v2.UnassignDeviceRequest":
		req = &cloudpbv2.UnassignDeviceRequest{}
	default:
		return nil, fmt.Errorf("payload_type %q", e.GetPayloadType())
	}
	if err := proto.Unmarshal(e.GetRequestPayload(), req); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", e.GetPayloadType(), err)
	}
	return req, nil
}

// bindsThisDevice checks that the operator signed this request for this
// device, by the target.resource forms cloud's handlers enforce (cloud
// RequestSigning.md) and the device the signed payload names.
func bindsThisDevice(req proto.Message, resource string, id *postboxIdentity) error {
	org := "org/" + id.tenant + "/"
	switch r := req.(type) {
	case *cloudpbv2.ControlContainerRequest:
		if r.GetAssetId() != id.uuid || r.GetOrganizationId() != id.tenant || resource != "asset/"+id.uuid+"/container/"+r.GetContainerName() {
			return fmt.Errorf("ControlContainer for asset %q (resource %q) is not this device", r.GetAssetId(), resource)
		}
	case *cloudpbv2.DeviceAssignment:
		if r.GetDevice() != id.uri || r.GetOrganizationId() != id.tenant || resource != org+"deployment/"+r.GetDeploymentId()+"/device/"+id.uuid {
			return fmt.Errorf("assignment for %q (resource %q) is not this device", r.GetDevice(), resource)
		}
	case *cloudpbv2.UnassignDeviceRequest:
		if r.GetDevice() != id.uri || r.GetOrganizationId() != id.tenant || resource != org+"deployment/"+r.GetDeploymentId()+"/device/"+id.uuid {
			return fmt.Errorf("unassignment for %q (resource %q) is not this device", r.GetDevice(), resource)
		}
	case *cloudpbv2.CreateDeploymentRequest:
		// The descriptor names the release, not a device. The signed payload
		// binds this device only by its UUID in target_device_ids, or by naming
		// no target at all (every device of the tenant). Tags are resolved by
		// cloud and prove nothing here.
		if r.GetOrganizationId() != id.tenant || resource != org+"release/"+r.GetAppReleaseId() {
			return fmt.Errorf("deployment for organization %q (resource %q) is not this tenant", r.GetOrganizationId(), resource)
		}
		whole := len(r.GetTargetDeviceIds()) == 0 && len(r.GetTargetTags()) == 0
		if !whole && !slices.Contains(r.GetTargetDeviceIds(), id.uuid) {
			return errors.New("deployment does not name this device in its signed targets (tag targeting is cloud-resolved)")
		}
	}
	return nil
}

// apply acts on a verified request through the agent's existing paths. Every
// failure leaves what is running alone and runs nothing new.
func (p *Postbox) apply(ctx context.Context, req proto.Message) (string, cloudpbv2.ApplyOutcome, string, error) {
	switch r := req.(type) {
	case *cloudpbv2.ControlContainerRequest:
		app := p.postboxApp(ctx, r)
		var err error
		switch r.GetAction() {
		case cloudpbv2.ContainerAction_CONTAINER_ACTION_START:
			err = p.control.StartApp(ctx, r.GetContainerName())
		case cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP:
			err = p.control.StopApp(ctx, r.GetContainerName())
		default:
			err = fmt.Errorf("unknown container action %v", r.GetAction())
		}
		if err != nil {
			return app, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "apply", err
		}
		return app, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED, "", nil
	case *cloudpbv2.DeviceAssignment:
		p.mu.Lock()
		p.state.Assignments[r.GetDeploymentId()] = &postboxAssignment{
			AppID: r.GetAppId(), AppReleaseID: r.GetAppReleaseId(), ManifestDigest: r.GetManifestDigest(),
			DeploymentKeyID: r.GetDeploymentKeyId(), DesiredState: r.GetDesiredState().String(),
		}
		p.mu.Unlock()
		outcome, reason, err := p.pullMaterial(ctx, r.GetAppReleaseId(), r)
		return r.GetAppId(), outcome, reason, err
	case *cloudpbv2.UnassignDeviceRequest:
		p.mu.Lock()
		app := ""
		if a := p.state.Assignments[r.GetDeploymentId()]; a != nil {
			app = a.AppID
		}
		delete(p.state.Assignments, r.GetDeploymentId())
		p.mu.Unlock()
		return app, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED, "", nil
	case *cloudpbv2.CreateDeploymentRequest:
		outcome, reason, err := p.pullMaterial(ctx, r.GetAppReleaseId(), nil)
		return "", outcome, reason, err // names a release, not an app
	}
	return "", cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "apply", fmt.Errorf("no apply path for %T", req)
}

// pullMaterial is the single seam for the registry pull, unwrap and decrypt
// (T14). Until then a verified deployment or assignment is held as desired
// state and reported, and nothing new runs.
func (p *Postbox) pullMaterial(_ context.Context, releaseID string, _ *cloudpbv2.DeviceAssignment) (cloudpbv2.ApplyOutcome, string, error) {
	return cloudpbv2.ApplyOutcome_APPLY_OUTCOME_PULL_REFUSED, "material", fmt.Errorf("%s for release %q", materialPending, releaseID)
}

// decide audits, counts and records one postbox decision. The record is
// swapped in only once it is on disk, so nothing reports a decision a restart
// could lose; a failed save holds the cycle, retried with backoff until it
// lands, since the Pending claim left on disk would be settled as
// interrupted. It fails only when ctx ends first.
func (p *Postbox) decide(ctx context.Context, e *cloudpbv2.PostboxEntry, correlation, app string, outcome cloudpbv2.ApplyOutcome, reason string, err error) error {
	if correlation == "" {
		correlation = peekCorrelation(e.GetRequestJws()) // unverified, for tracing only
	}
	fields := []zap.Field{
		zap.String("audit", "postbox"), zap.Int64("entry_id", e.GetEntryId()), zap.String("correlation_id", correlation),
		zap.String("payload_type", e.GetPayloadType()), zap.String("app_id", app), zap.String("outcome", outcome.String()),
	}
	if err != nil {
		p.count(reason)
		p.logger.Warn("postbox entry refused", append(fields, zap.String("reason", reason), zap.Error(err))...)
	} else {
		p.logger.Info("postbox entry applied", fields...)
	}
	// Cloud refuses a whole report over one invalid app id: an id the signed
	// payload names that the device would not deploy is audit only.
	if app != "" && appconfig.ValidateAppID(app) != nil {
		p.count("app_id")
		p.logger.Warn("postbox outcome not reported: invalid app id", append(fields, zap.String("reason", "app_id"))...)
		app = ""
	}
	// An outcome not attributable to an app has the audit line as its record.
	record := func(st *postboxState) {
		st.Outcomes = maps.Clone(st.Outcomes)
		if app != "" {
			st.Outcomes[app] = &postboxOutcome{EntryID: e.GetEntryId(), CorrelationID: correlation, Outcome: outcome}
		}
		st.Pending = nil
	}
	for wait := min(time.Second, p.retry); ; wait = min(2*wait, p.retry) {
		serr := p.commit(record)
		if serr == nil {
			return nil
		}
		p.logger.Warn("postbox: saving decision failed; holding until it lands", zap.Int64("entry_id", e.GetEntryId()), zap.Error(serr))
		select {
		case <-ctx.Done():
			return fmt.Errorf("decision for entry %d not saved: %w", e.GetEntryId(), serr)
		case <-time.After(wait):
		}
	}
}

// Reported is called with the newest report sent before a cloud answer,
// once per answer that followed a new report, and drops the outcomes cloud
// has taken.
//
// Cloud stores reports in arrival order, but one writer answers both reports
// and postbox appends, coalescing a burst into one message, so an answer can
// precede the storing of the report before it. An answer therefore proves
// nothing about one report. The rule: an app's outcome is dropped once it was
// carried, ABSENT (not on the device) with the same entry id, in two reports
// each followed by its own cloud answer, while no held assignment targets the
// app. Each of the two answers was written after a wake-up; a report wakes the
// writer only after it is stored, so a premature drop needs two appends to be
// answered while both reports were still being stored. A present or targeted
// app keeps its outcome on its row, and a newer decision (another entry id)
// starts the count again.
func (p *Postbox) Reported(req *cloudpbv2.UpdateReportedStateRequest) {
	p.mu.Lock()
	if p.state == nil {
		p.mu.Unlock()
		return
	}
	targeted := map[string]bool{}
	for _, a := range p.state.Assignments {
		targeted[a.AppID] = true
	}
	if p.answered == nil {
		p.answered = map[string]postboxAnswered{}
	}
	outcomes := maps.Clone(p.state.Outcomes) // copy on write: decide reads the old map unlocked
	for _, s := range req.GetCurrentStates() {
		app := s.GetAppId()
		o := outcomes[app]
		if o == nil || s.GetReportedState() != cloudpbv2.Running_ABSENT || o.EntryID != s.GetPostboxEntryId() || targeted[app] {
			delete(p.answered, app)
			continue
		}
		n := p.answered[app]
		if n.entryID != o.EntryID {
			n = postboxAnswered{entryID: o.EntryID}
		}
		if n.reports++; n.reports < 2 {
			p.answered[app] = n
			continue
		}
		delete(p.answered, app)
		delete(outcomes, app)
	}
	dropped := len(outcomes) != len(p.state.Outcomes)
	p.state.Outcomes = outcomes
	p.mu.Unlock()
	if dropped {
		if err := p.save(); err != nil {
			p.logger.Warn("postbox: saving reported outcomes failed", zap.Error(err))
		}
	}
}

// Outcomes returns the latest postbox decision per app for the reported state.
func (p *Postbox) Outcomes() map[string]postboxOutcome {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]postboxOutcome{}
	if p.state != nil {
		for app, o := range p.state.Outcomes {
			out[app] = *o
		}
	}
	return out
}

// ack signs PostboxHeadAck for the newest verified entry until cloud takes it.
func (p *Postbox) ack(ctx context.Context, client cloudpbv2.DevicePostboxServiceClient, id *postboxIdentity) error {
	st := p.snapshot()
	if st.HighWater <= st.Acked || id.signer == nil {
		return nil
	}
	signed, err := id.signer.Sign(cloudpbv2.DevicePostboxService_AckHead_FullMethodName, &cloudpbv2.PostboxHeadAck{
		EntryId: st.HighWater, EntryHash: st.HeadHash, ObservedAt: timestamppb.New(p.now()),
	}, uuid.NewString())
	if err != nil {
		return err
	}
	if _, err := client.AckHead(ctx, signed); err != nil {
		return fmt.Errorf("acking postbox head %d: %w", st.HighWater, err)
	}
	p.mu.Lock()
	p.state.Acked = max(p.state.Acked, st.HighWater)
	p.mu.Unlock()
	return p.save()
}

// settleInterrupted reports the entry a crash left between advance and
// decide: claimed past the mark, never decided. It is not retried. A decided
// entry never reaches it: decide holds until its record is on disk.
func (p *Postbox) settleInterrupted(ctx context.Context) error {
	p.mu.Lock()
	pend := p.state.Pending
	p.mu.Unlock()
	if pend == nil {
		return nil
	}
	return p.decide(ctx, &cloudpbv2.PostboxEntry{EntryId: pend.EntryID}, pend.CorrelationID, pend.AppID,
		cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "interrupted",
		errors.New("agent stopped after claiming this entry and before deciding it; not retried"))
}

// advance moves the mark past a chain-verified entry, claims its nonce and
// records it as pending. Memory changes only once the new state is on disk:
// a failed write leaves the entry to be processed again next cycle.
func (p *Postbox) advance(e *cloudpbv2.PostboxEntry, nonce string, nonceExpiry int64, pending *postboxPending) error {
	p.mu.Lock()
	st := *p.state
	st.Nonces = maps.Clone(p.state.Nonces)
	p.mu.Unlock()
	replayed := false
	if nonceExpiry > 0 {
		if _, seen := st.Nonces[nonce]; seen {
			replayed = true
		} else {
			st.Nonces[nonce] = nonceExpiry
		}
	}
	// Pruned against the persisted floor, never the clock: a clock run past
	// valid_until and set back would otherwise drop a live nonce and let the
	// re-appended request apply twice.
	for n, exp := range st.Nonces {
		if exp <= st.TimeFloor {
			delete(st.Nonces, n)
		}
	}
	st.HighWater, st.HeadHash, st.Pending = e.GetEntryId(), e.GetEntryHash(), pending
	p.mu.Lock()
	data, err := json.Marshal(&st)
	p.mu.Unlock()
	if err == nil {
		err = p.write(p.configPath, postboxStateFile, data)
	}
	if err != nil {
		return fmt.Errorf("persisting postbox high-water mark: %w", err)
	}
	p.mu.Lock()
	p.state = &st
	p.mu.Unlock()
	if replayed {
		return errReplayed
	}
	return nil
}

var errReplayed = errors.New("nonce replayed")

func (p *Postbox) snapshot() postboxState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return *p.state
}

// load reads the persisted state for principal; another principal's state
// (a re-enrolled device has a new UUID and a new chain) is discarded.
func (p *Postbox) load(principal string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != nil && p.state.Principal == principal {
		return nil
	}
	st := &postboxState{}
	data, err := os.ReadFile(filepath.Join(p.configPath, postboxStateFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("reading postbox state: %w", err)
	default:
		if err := json.Unmarshal(data, st); err != nil {
			return fmt.Errorf("decoding postbox state: %w", err)
		}
	}
	if st.Principal != principal {
		st = &postboxState{Principal: principal}
	}
	if st.Nonces == nil {
		st.Nonces = map[string]int64{}
	}
	if st.Assignments == nil {
		st.Assignments = map[string]*postboxAssignment{}
	}
	if st.Outcomes == nil {
		st.Outcomes = map[string]*postboxOutcome{}
	}
	p.state = st
	return nil
}

func (p *Postbox) save() error {
	p.mu.Lock()
	data, err := json.Marshal(p.state)
	p.mu.Unlock()
	if err != nil {
		return err
	}
	return p.write(p.configPath, postboxStateFile, data)
}

func (p *Postbox) count(reason string) {
	p.mu.Lock()
	p.refusals[reason]++
	n := p.refusals[reason]
	p.mu.Unlock()
	if p.pub == nil {
		return
	}
	p.pub.PublishMetrics(&colmetricspb.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: newAgentResource(),
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Scope: &commonpb.InstrumentationScope{Name: "wendy.postbox"},
				Metrics: []*metricspb.Metric{{
					Name: "postbox.refusals",
					Unit: "1",
					Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
						IsMonotonic:            true,
						AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
						DataPoints: []*metricspb.NumberDataPoint{{
							TimeUnixNano: uint64(p.now().UnixNano()),
							Value:        &metricspb.NumberDataPoint_AsInt{AsInt: n},
							Attributes: []*commonpb.KeyValue{{
								Key: "reason", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: reason}},
							}},
						}},
					}},
				}},
			}},
		}},
	})
}

// postboxApp is the app a verified request names; "" for none. App ids are
// reverse-DNS: a container name is resolved against this device's apps,
// never cut at a '.'.
func (p *Postbox) postboxApp(ctx context.Context, req proto.Message) string {
	switch r := req.(type) {
	case *cloudpbv2.ControlContainerRequest:
		return p.control.AppOf(ctx, r.GetContainerName())
	case *cloudpbv2.DeviceAssignment:
		return r.GetAppId()
	}
	return ""
}

// peekX5C parses the x5c of a JWS header without verifying anything.
func peekX5C(jws []byte) []*x509.Certificate {
	header, _, ok := strings.Cut(string(jws), ".")
	if !ok {
		return nil
	}
	raw, err := b64url(header)
	if err != nil {
		return nil
	}
	var h struct {
		X5C []string `json:"x5c"`
	}
	_ = json.Unmarshal(raw, &h)
	var out []*x509.Certificate
	for _, enc := range h.X5C {
		der, err := b64std(enc)
		if err != nil {
			return out
		}
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return out
		}
		out = append(out, c)
	}
	return out
}

func peekLeaf(jws []byte) *x509.Certificate {
	if x5c := peekX5C(jws); len(x5c) > 0 {
		return x5c[0]
	}
	return nil
}

func peekIntermediates(jws []byte) []*x509.Certificate {
	if x5c := peekX5C(jws); len(x5c) > 1 {
		return x5c[1:]
	}
	return nil
}

func peekCorrelation(jws []byte) string {
	parts := strings.Split(string(jws), ".")
	if len(parts) != 3 {
		return ""
	}
	claims, err := b64url(parts[1])
	if err != nil {
		return ""
	}
	var d struct {
		Correlation string `json:"correlation_id"`
	}
	_ = json.Unmarshal(claims, &d)
	return d.Correlation
}

// containerControl drives the same ContainerService handlers the CLI calls:
// a start is the CLI's detached start, which returns once the app started.
type containerControl struct{ svc *ContainerService }

// NewPostboxControl adapts the agent's container service for the postbox.
func NewPostboxControl(svc *ContainerService) postboxControl { return containerControl{svc: svc} }

func (c containerControl) StopApp(ctx context.Context, name string) error {
	_, err := c.svc.StopContainer(ctx, &agentpb.StopContainerRequest{AppName: name})
	return err
}

// AppOf maps a service container ("{appID}_{service}") to its app by this
// device's own listing; any other name is the app id itself.
func (c containerControl) AppOf(ctx context.Context, name string) string {
	apps, _ := c.svc.containerd.ListContainers(ctx) // no listing: the name is the app id
	for _, a := range apps {
		for _, s := range a.GetServices() {
			if a.GetAppName()+"_"+s.GetName() == name {
				return a.GetAppName()
			}
		}
	}
	return name
}

func (c containerControl) StartApp(ctx context.Context, name string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := &detachedStart{ctx: ctx, cancel: cancel}
	err := c.svc.StartContainer(&agentpb.StartContainerRequest{AppName: name}, stream)
	if stream.started {
		return nil // the handler ends with the detached client's cancellation
	}
	if err == nil {
		err = errors.New("container did not report a start")
	}
	return err
}

type detachedStart struct {
	grpc.ServerStream // unused by the start handler; nil
	ctx               context.Context
	cancel            context.CancelFunc
	started           bool
}

func (s *detachedStart) Context() context.Context { return s.ctx }

func (s *detachedStart) Send(m *agentpb.RunContainerLayersResponse) error {
	if m.GetStarted() != nil {
		s.started = true
		s.cancel()
	}
	return nil
}

func b64url(s string) ([]byte, error) { return base64.RawURLEncoding.Strict().DecodeString(s) }

func b64urlEncode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func b64std(s string) ([]byte, error) { return base64.StdEncoding.Strict().DecodeString(s) }

func strictJSON(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// verifyMLDSA65 checks a JWS signature (raw FIPS 204, empty context, as the
// signers produce it) against an ML-DSA-65 leaf.
func verifyMLDSA65(leaf *x509.Certificate, input []byte, sigB64 string) error {
	pk, ok := leaf.PublicKey.(*mldsa.PublicKey)
	if !ok || pk.Parameters() != mldsa.MLDSA65() {
		return errors.New("signing leaf does not hold an ML-DSA-65 key")
	}
	sig, err := b64url(sigB64)
	if err != nil {
		return err
	}
	return mldsa.Verify(pk, input, sig, nil)
}
