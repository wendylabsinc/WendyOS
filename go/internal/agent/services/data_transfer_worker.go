package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/pkienroll"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

const (
	// transferChunkBytes is the payload size per UploadEpisodeChunk message. The
	// cloud ingest contract caps a chunk at 1 MiB of data; we send exactly that
	// (except the final short chunk of each file).
	transferChunkBytes = 1 << 20

	// transferMaxAttempts bounds how many times a single episode is retried on
	// retryable (transport) failures before the worker gives up and records the
	// episode as permanently "failed". Verification failures are terminal on the
	// first occurrence and do not consume the retry budget.
	transferMaxAttempts = 5

	// transferMaxBackoff caps the per-pass exponential backoff between failed
	// upload passes, matching the telemetry flusher's ceiling.
	transferMaxBackoff = 60 * time.Second

	// transferIdlePause is the pause between successful passes when there is no
	// backlog, to avoid busy-looping the disk scan.
	transferIdlePause = 10 * time.Second
)

// errIngestBlocked wraps a failure that says the ROUTE is wrong rather than the
// episode. Retrying cannot fix it and every other episode in the backlog will
// fail identically, so the pass aborts instead of walking the queue and burning
// each episode's retry budget against the same wall.
type errIngestBlocked struct {
	code  codes.Code
	cause error
}

func (e *errIngestBlocked) Error() string {
	return fmt.Sprintf("ingest endpoint rejected the request as %s: %v", e.code, e.cause)
}
func (e *errIngestBlocked) Unwrap() error { return e.cause }

// ingestBlocked reports whether err is a route/configuration failure rather
// than a transport one.
//
// These three codes share a property that no amount of retrying changes: the
// endpoint understood us and refused. Unimplemented is the one that matters
// most in practice, because dialling a host that does not serve
// DataIngestService (the enrolled cloud host, when no ingest endpoint is
// configured) returns exactly that, and treating it as transient quietly
// converts every sealed episode on the device into a permanent failure.
func ingestBlocked(err error) *errIngestBlocked {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return nil
	}
	switch st.Code() {
	case codes.Unimplemented, codes.Unauthenticated, codes.PermissionDenied:
		return &errIngestBlocked{code: st.Code(), cause: err}
	}
	return nil
}

// Upload workflow states persisted in Manifest.Upload.State. These mirror the
// vocabulary the data manager already understands (see awaitingUpload).
const (
	uploadStatePending   = "pending"
	uploadStateUploading = "uploading"
	uploadStateUploaded  = "uploaded"
	uploadStateFailed    = "failed"
)

// ingestClientFactory produces a DataIngestService client bound to the device's
// asset identity. It returns the org and asset IDs the identity asserts (so the
// worker can populate the manifest the server cross-checks against the cert),
// plus a closeFn the caller must invoke when the pass is done. Production dials
// the cloud over mTLS; tests inject an in-process client.
type ingestClientFactory func(ctx context.Context) (client cloudpb.DataIngestServiceClient, closeFn func(), err error)

// DataTransferWorker uploads sealed episodes to the cloud DataIngestService. It
// consumes Manifest.Upload as a durable queue: episodes marked "pending" (and,
// after a crash, "uploading") are streamed to the cloud and the manifest is
// resealed at every state transition, so the worker is at-least-once and
// crash-safe. It connects to the same cloud host as the telemetry flusher using
// the same asset mTLS identity, over a distinct DataIngestService client.
type DataTransferWorker struct {
	logger          *zap.Logger
	manager         *data.Manager
	provisioningSvc *ProvisioningService // nil in tests
	factory         ingestClientFactory  // set in tests; production builds one from provisioningSvc

	maxAttempts int
	// lastBlockedCause is the most recent route-level rejection, so a blocked
	// endpoint is reported once rather than once a minute forever.
	lastBlockedCause string
	// ingestHost is the DataIngestService endpoint (host[:port]) uploads dial,
	// set from WENDY_DATA_INGEST_URL in main. There is no fallback to the
	// enrolled cloud host: the broker does not serve DataIngestService, so
	// dialling it only ever produced Unimplemented. Empty means uploads are
	// disabled and Run says so once. Enrollment still provides the asset
	// identity and client certificate; this is only the destination.
	ingestHost string
	// pkiIdentity, when set, is the device's pki-core identity: a leaf whose
	// only URI Subject Alternative Name is
	// spiffe://wendy.sh/tenant/<tenant>/device/<name>. The data platform's
	// ingest interceptor prefers that principal and rejects any SPIFFE kind
	// other than "device"; Wendy Cloud's interceptors, by contrast, read only
	// the legacy Wendy organization URN that the enrolled asset certificate
	// carries (see certs.AssetURN). So this identity is used HERE and nowhere
	// else, and the asset certificate stays what every cloud dialer presents.
	// When no pki identity is stored the worker behaves exactly as before,
	// presenting the asset certificate, which the ingest surface still accepts
	// as a legacy identity.
	pkiIdentity pkiIdentityReader
	// onWiFi reports whether the device is currently on Wi-Fi, gating campaigns
	// whose upload.when is "wifi". When nil, no network-type signal is wired and
	// "wifi" is treated as "always" (see resolveShouldUpload).
	onWiFi func() bool
	// now and newSleeper are injection points for deterministic tests; both
	// default to real time.
	now        func() time.Time
	newSleeper func(ctx context.Context) func(time.Duration)
}

// NewDataTransferWorker builds a worker that reads cloud credentials and the
// asset identity from the ProvisioningService at run time, dialling the cloud
// over mTLS once per pass (mirroring CloudFlusher).
func NewDataTransferWorker(logger *zap.Logger, manager *data.Manager, provisioningSvc *ProvisioningService) *DataTransferWorker {
	w := &DataTransferWorker{
		logger:          logger,
		manager:         manager,
		provisioningSvc: provisioningSvc,
		maxAttempts:     transferMaxAttempts,
		now:             time.Now,
		newSleeper:      contextSleeper,
	}
	w.factory = w.dialFactory
	return w
}

// SetIngestEndpoint sets the DataIngestService endpoint uploads dial. endpoint
// may be a bare host, host:port, or an http(s):// URL; the scheme and any path
// are stripped and the default port (443) is applied by the dialer. Empty
// disables uploads (see Run).
//
// Identity is the enrolled asset certificate, presented in the TLS handshake
// and read by the ingest service from the validated leaf. No request header
// carries identity: the header this worker once attached on the override path
// was a self-asserted identity, and the service no longer reads it.
func (w *DataTransferWorker) SetIngestEndpoint(endpoint string) {
	w.ingestHost = normalizeEndpoint(endpoint)
}

// IngestEndpoint reports the configured endpoint, empty when uploads are
// disabled.
func (w *DataTransferWorker) IngestEndpoint() string { return w.ingestHost }

// normalizeEndpoint reduces an endpoint value to the host[:port] form
// dialCloudMTLS expects, tolerating URL-shaped input.
func normalizeEndpoint(host string) string {
	host = strings.TrimSpace(host)
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	return host
}

// contextSleeper returns a sleep function that returns early when ctx is done,
// so backoff and bandwidth pacing stay responsive to shutdown.
func contextSleeper(ctx context.Context) func(time.Duration) {
	return func(d time.Duration) {
		if d <= 0 {
			return
		}
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		}
	}
}

// pkiIdentityReader reads the device's stored pki-core identity.
// *pkienroll.Store implements it; tests substitute a stub. ErrNoIdentity (or
// any error) means "no pki identity", which is a supported state and not a
// failure.
type pkiIdentityReader interface {
	Load() (pkienroll.Material, error)
}

// SetPKIIdentity gives the worker a pki-core device identity to present to the
// data platform in preference to the enrolled asset certificate. Passing nil
// restores the previous behaviour.
func (w *DataTransferWorker) SetPKIIdentity(r pkiIdentityReader) { w.pkiIdentity = r }

// identitySource names which of the device's two identities a dial used. It
// exists so the choice is testable and so the log says which certificate was
// presented — a question that is otherwise unanswerable after the fact.
type identitySource string

const (
	identitySourcePKI   identitySource = "pki-core-spiffe"
	identitySourceAsset identitySource = "cloud-asset-urn"
)

// ingestIdentity picks the client certificate for an ingest dial: the pki-core
// device identity when one is stored, otherwise the enrolled asset
// certificate.
//
// The fallback is unconditional and silent by design. A device that has not
// been enrolled against pki-core must keep uploading exactly as it does today,
// so an absent pki identity is not a warning; and a stored-but-unreadable one
// is reported rather than papered over, because that is a real fault.
//
// The caller owns keyData and must zero it.
func (w *DataTransferWorker) ingestIdentity() (certPEM, chainPEM string, keyData []byte, source identitySource, err error) {
	if w.pkiIdentity != nil {
		material, loadErr := w.pkiIdentity.Load()
		switch {
		case loadErr == nil && material.LeafPEM != "" && len(material.KeyData) > 0:
			return material.LeafPEM, material.ChainPEM, material.KeyData, identitySourcePKI, nil
		case loadErr != nil && !errors.Is(loadErr, pkienroll.ErrNoIdentity):
			w.logger.Warn("data transfer worker: pki identity is present but unreadable; "+
				"falling back to the enrolled asset certificate", zap.Error(loadErr))
		}
	}
	certPEM, chainPEM, keyData = w.provisioningSvc.ProvisioningCerts()
	return certPEM, chainPEM, keyData, identitySourceAsset, nil
}

// dialFactory is the production ingestClientFactory: it waits for provisioning,
// dials the cloud over mTLS, and returns a DataIngestService client.
func (w *DataTransferWorker) dialFactory(ctx context.Context) (cloudpb.DataIngestServiceClient, func(), error) {
	if _, _, _, enrolled := w.provisioningSvc.ProvisioningInfo(); !enrolled {
		return nil, nil, errors.New("data transfer worker: not provisioned")
	}
	if w.ingestHost == "" {
		return nil, nil, errors.New("data transfer worker: no ingest endpoint configured")
	}
	certPEM, chainPEM, keyData, source, err := w.ingestIdentity()
	if err != nil {
		return nil, nil, err
	}
	// The identity is a client certificate, presented in the TLS handshake and
	// read by the service from the validated leaf. Nothing else is sent: no
	// header, nothing from the environment, nothing an app on the device can
	// influence. The server side is verified against the system roots either
	// way, which is unchanged: the ingest endpoint presents a publicly trusted
	// certificate.
	conn, err := func() (*grpc.ClientConn, error) {
		defer zeroBytes(keyData)
		return dialCloudMTLS(w.ingestHost, certPEM, chainPEM, keyData)
	}()
	if err != nil {
		return nil, nil, err
	}
	w.logger.Debug("data transfer worker: dialled ingest",
		zap.String("host", w.ingestHost), zap.String("identity", string(source)))
	return cloudpb.NewDataIngestServiceClient(conn), func() { _ = conn.Close() }, nil
}

// Run drives the transfer worker until ctx is cancelled. It waits for the agent
// to be provisioned, then continuously drains the upload backlog with
// exponential backoff between failed passes (1s to 60s). It blocks; start it in
// a goroutine.
func (w *DataTransferWorker) Run(ctx context.Context) {
	if w.factory == nil || w.manager == nil {
		return
	}
	if w.provisioningSvc != nil {
		for {
			if _, _, _, enrolled := w.provisioningSvc.ProvisioningInfo(); enrolled {
				break
			}
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}

	// No endpoint means no uploads. Said once, at error level, and then the
	// worker stops rather than dialling a host that cannot serve it: sealed
	// episodes stay queued on the device (bounded by the store's quota and its
	// eviction order) until the endpoint is configured and the agent restarts.
	if w.ingestHost == "" {
		w.logger.Error("data transfer worker: WENDY_DATA_INGEST_URL is not set; " +
			"episode uploads are disabled and sealed episodes stay queued on the device")
		return
	}

	// A restart is usually what follows fixing whatever broke uploads, so give
	// the episodes that exhausted their budget one more life. Bounded: this
	// runs once per process, not per pass.
	if moved, err := w.manager.RequeueFailedUploads(); err != nil {
		w.logger.Warn("data transfer worker: requeue of failed episodes failed", zap.Error(err))
	} else if moved > 0 {
		w.logger.Info("data transfer worker: requeued previously failed episodes", zap.Int("episodes", moved))
	}

	w.logger.Info("data transfer worker: started")
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		err := w.runPass(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// A blocked route persists until someone changes configuration, so
			// it would otherwise log identically every minute forever. Say it
			// loudly once, then keep it at debug until the cause changes.
			if blocked := ingestBlocked(err); blocked != nil {
				if w.lastBlockedCause != blocked.Error() {
					w.lastBlockedCause = blocked.Error()
					w.logger.Error("data transfer worker: ingest endpoint is not accepting uploads; "+
						"no episode will be retried against it and none has been marked failed",
						zap.String("code", blocked.code.String()), zap.Error(blocked))
				} else {
					w.logger.Debug("data transfer worker: ingest still blocked", zap.Error(blocked))
				}
			} else {
				w.lastBlockedCause = ""
				w.logger.Warn("data transfer worker: pass failed", zap.Error(err))
			}
			w.backoff(ctx, attempt)
			if attempt < 6 { // 2^6 = 64s > 60s cap
				attempt++
			}
			continue
		}
		attempt = 0
		select {
		case <-time.After(transferIdlePause):
		case <-ctx.Done():
			return
		}
	}
}

func (w *DataTransferWorker) backoff(ctx context.Context, attempt int) {
	d := time.Second << attempt
	if d > transferMaxBackoff || d <= 0 {
		d = transferMaxBackoff
	}
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// runPass dials the cloud, enumerates the upload backlog, and uploads each
// episode. A failure to dial or enumerate fails the whole pass (retried with
// backoff); a failure uploading one episode is recorded on that episode's
// manifest and does not abort the pass.
func (w *DataTransferWorker) runPass(ctx context.Context) error {
	client, closeFn, err := w.factory(ctx)
	if err != nil {
		return fmt.Errorf("acquire ingest client: %w", err)
	}
	defer closeFn()

	episodes, err := w.manager.EpisodesAwaitingUpload()
	if err != nil {
		return fmt.Errorf("enumerate upload backlog: %w", err)
	}
	now := w.now()
	for _, mf := range episodes {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Honor backoff: a pending episode whose next-attempt time is in the
		// future is skipped this pass.
		if mf.Upload.State == uploadStatePending && mf.Upload.NextAttemptUnixNanos > now.UnixNano() {
			continue
		}
		if err := w.processEpisode(ctx, client, mf); err != nil {
			// Route failure: the rest of the backlog would fail identically.
			return err
		}
	}
	return nil
}

// resolveShouldUpload applies the campaign upload.when policy. It returns
// whether the episode should upload now and a human-readable reason when it
// should not.
func (w *DataTransferWorker) resolveShouldUpload(mf data.Manifest) (bool, string) {
	when := "always"
	if name := mf.Trigger.CampaignName; name != "" {
		if c, err := w.manager.Campaign(name); err == nil {
			if c.Upload.When != "" {
				when = c.Upload.When
			}
		} else {
			// Campaign plan gone (deleted after capture). The episode was queued
			// for upload, so default to uploading it rather than stranding data.
			w.logger.Warn("data transfer worker: campaign plan not found, defaulting to always",
				zap.String("episode", mf.ID), zap.String("campaign", name), zap.Error(err))
		}
	}
	switch when {
	case "manual":
		return false, "campaign upload.when is manual"
	case "wifi":
		if w.onWiFi == nil {
			// TODO(data-platform): wire a device network-type signal so "wifi"
			// gates on actually being on an unmetered Wi-Fi link. No such signal
			// is plumbed to the worker yet, so treat "wifi" as "always".
			w.logger.Debug("data transfer worker: no network-type signal, treating upload.when=wifi as always",
				zap.String("episode", mf.ID))
			return true, ""
		}
		if !w.onWiFi() {
			return false, "campaign upload.when is wifi and device is not on Wi-Fi"
		}
		return true, ""
	default:
		return true, ""
	}
}

// uploadRate returns the campaign's bandwidth ceiling in bytes/sec for the
// episode, or 0 (unlimited) when there is no campaign or no configured cap.
func (w *DataTransferWorker) uploadRate(mf data.Manifest) int64 {
	if name := mf.Trigger.CampaignName; name != "" {
		if c, err := w.manager.Campaign(name); err == nil {
			return c.UploadMaxRateBytes()
		}
	}
	return 0
}

// processEpisode uploads a single episode. All outcomes are persisted to the
// manifest; transient failures move the episode back to "pending" with a
// backoff (or to "failed" once the retry ceiling is hit), verification failures
// move it straight to "failed", and success marks it "uploaded".
//
// It returns a non-nil error ONLY when the failure is the route rather than the
// episode, which is the caller's signal to abandon the pass. The episode is
// left exactly as it was found in that case: it did nothing wrong, so it keeps
// its attempt count and its place in the queue.
func (w *DataTransferWorker) processEpisode(ctx context.Context, client cloudpb.DataIngestServiceClient, mf data.Manifest) error {
	if ok, reason := w.resolveShouldUpload(mf); !ok {
		w.logger.Debug("data transfer worker: skipping episode", zap.String("episode", mf.ID), zap.String("reason", reason))
		return nil
	}

	// Mark uploading (durably) before any network work so a crash mid-transfer
	// is recoverable and the quota manager knows the payload is in flight.
	if _, err := w.manager.UpdateUploadState(mf.ID, func(ws *data.WorkflowState) {
		ws.State = uploadStateUploading
	}); err != nil {
		w.logger.Warn("data transfer worker: mark uploading failed", zap.String("episode", mf.ID), zap.Error(err))
		return nil
	}
	w.logger.Info("data transfer worker: uploading episode", zap.String("episode", mf.ID),
		zap.Int("files", len(mf.Files)), zap.String("campaign", mf.Trigger.CampaignName))

	verifyErr, retryErr := w.uploadEpisode(ctx, client, mf)
	switch {
	case verifyErr != nil:
		// Server-side verification failed: the stored bytes do not match the
		// manifest checksum. This is local corruption; retrying re-sends the
		// same bytes and fails identically, so fail permanently.
		w.markFailed(mf.ID, fmt.Sprintf("verification failed: %v", verifyErr))
		w.logger.Error("data transfer worker: episode failed verification (marked failed, not retrying)",
			zap.String("episode", mf.ID), zap.Error(verifyErr))
	case retryErr != nil:
		if ctx.Err() != nil {
			// Shutdown mid-transfer: leave the episode "uploading" so the next
			// run resumes it. Nothing is silently dropped.
			w.logger.Info("data transfer worker: shutdown mid-upload, episode left for resume", zap.String("episode", mf.ID))
			return nil
		}
		if blocked := ingestBlocked(retryErr); blocked != nil {
			// The route is wrong, not the episode. Put it back exactly as it
			// was and let the caller stop the pass.
			if _, err := w.manager.UpdateUploadState(mf.ID, func(ws *data.WorkflowState) {
				ws.State = mf.Upload.State
				ws.LastError = blocked.Error()
			}); err != nil {
				w.logger.Warn("data transfer worker: restore state failed", zap.String("episode", mf.ID), zap.Error(err))
			}
			return blocked
		}
		w.handleRetryable(mf, retryErr)
	default:
		if _, err := w.manager.UpdateUploadState(mf.ID, func(ws *data.WorkflowState) {
			ws.State = uploadStateUploaded
			ws.LastError = ""
			ws.NextAttemptUnixNanos = 0
		}); err != nil {
			w.logger.Warn("data transfer worker: mark uploaded failed", zap.String("episode", mf.ID), zap.Error(err))
			return nil
		}
		w.logger.Info("data transfer worker: episode uploaded", zap.String("episode", mf.ID))
	}
	return nil
}

// handleRetryable records a retryable failure: it bumps the attempt count and
// either re-queues the episode with a backoff or, once the ceiling is reached,
// marks it permanently failed.
func (w *DataTransferWorker) handleRetryable(mf data.Manifest, cause error) {
	attempts := mf.Upload.Attempts + 1
	if attempts >= w.maxAttempts {
		w.markFailed(mf.ID, fmt.Sprintf("gave up after %d attempts: %v", attempts, cause))
		w.logger.Error("data transfer worker: episode failed after retry ceiling",
			zap.String("episode", mf.ID), zap.Int("attempts", attempts), zap.Error(cause))
		return
	}
	// Exponential backoff on the retry clock, capped.
	delay := time.Second << attempts
	if delay > transferMaxBackoff || delay <= 0 {
		delay = transferMaxBackoff
	}
	next := w.now().Add(delay).UnixNano()
	if _, err := w.manager.UpdateUploadState(mf.ID, func(ws *data.WorkflowState) {
		ws.State = uploadStatePending
		ws.Attempts = attempts
		ws.LastError = cause.Error()
		ws.NextAttemptUnixNanos = next
	}); err != nil {
		w.logger.Warn("data transfer worker: requeue failed", zap.String("episode", mf.ID), zap.Error(err))
		return
	}
	w.logger.Warn("data transfer worker: episode upload failed, requeued",
		zap.String("episode", mf.ID), zap.Int("attempts", attempts), zap.Duration("backoff", delay), zap.Error(cause))
}

func (w *DataTransferWorker) markFailed(id, detail string) {
	if _, err := w.manager.UpdateUploadState(id, func(ws *data.WorkflowState) {
		ws.State = uploadStateFailed
		ws.Attempts++
		ws.LastError = detail
		ws.NextAttemptUnixNanos = 0
	}); err != nil {
		w.logger.Warn("data transfer worker: mark failed failed", zap.String("episode", id), zap.Error(err))
	}
}

// uploadEpisode runs the Begin -> chunk stream -> Commit protocol for one
// episode. It returns (verifyErr, retryErr): verifyErr is a terminal
// verification/corruption failure, retryErr is a retryable transport failure.
// At most one is non-nil; both nil means success.
func (w *DataTransferWorker) uploadEpisode(ctx context.Context, client cloudpb.DataIngestServiceClient, mf data.Manifest) (verifyErr, retryErr error) {
	manifest, err := buildEpisodeManifest(mf)
	if err != nil {
		// A manifest we cannot even serialize is corrupt local state; do not
		// spin on it.
		return err, nil
	}

	begin, err := client.BeginEpisodeUpload(ctx, &cloudpb.BeginEpisodeUploadRequest{Manifest: manifest})
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	if begin.GetState() == cloudpb.EpisodeState_EPISODE_STATE_COMPLETE {
		// The server already has a complete, verified copy (idempotent replay).
		return nil, nil
	}

	// Per-file committed offsets from the server. C3 reports 0 (restart) or the
	// full size (already stored, skip); we resume at file granularity. Mid-file
	// resume is a server-side follow-up.
	committed := make(map[string]int64, len(begin.GetFiles()))
	for _, f := range begin.GetFiles() {
		// Offsets are unsigned on the wire and signed here, because that is
		// what the file APIs use. Any value a correct server can report fits;
		// one that does not is treated as "nothing committed" so the file
		// restarts from 0 rather than wrapping to a negative offset.
		if v := f.GetCommittedOffset(); v <= math.MaxInt64 {
			committed[f.GetPath()] = int64(v)
		}
	}

	rate := w.uploadRate(mf)
	sleep := w.newSleeper(ctx)
	limiter := &byteRateLimiter{ratePerSec: float64(rate), sleep: sleep}

	stream, err := client.UploadEpisodeChunk(ctx)
	if err != nil {
		return nil, fmt.Errorf("open upload stream: %w", err)
	}

	// Drain acks concurrently so large uploads do not deadlock on flow control.
	//
	// The acks are read and discarded, not matched. That is deliberate, and it
	// is why the (episode_id, path) matching rule the wire contract states for
	// EpisodeChunkAck does not apply to this client today.
	//
	// Nothing here is keyed on an ack. Resume offsets come from
	// BeginEpisodeUpload's per-file FileUploadState, above, which is scoped to
	// one episode by construction; the acks contribute no state. Draining them
	// serves two other purposes: gRPC flow control stalls a large upload if the
	// receive side is never read, and a server-side stream error arrives here
	// rather than being lost.
	//
	// The contract's hazard is a client that matches acks on `path` alone while
	// one stream carries chunks for several episodes, which credits one
	// episode's committed offset to another. This client cannot hit it from
	// either direction: it matches on nothing, and it opens exactly one
	// UploadEpisodeChunk stream per episode, here inside uploadEpisode, which
	// processEpisode calls once per manifest. That is precisely the "keep to one
	// stream per episode" fallback the contract requires of a client that cannot
	// rely on the new episode_id field, and a server built before that field
	// leaves it empty anyway.
	//
	// TestUploadStreamCarriesOneEpisode pins the one-stream-per-episode
	// invariant. Batching several episodes onto a single stream, or using ack
	// offsets to drive resume, means matching on the (episode_id, path) pair
	// first; do not do one without the other.
	ackErrCh := make(chan error, 1)
	go func() {
		for {
			_, rerr := stream.Recv()
			if rerr == io.EOF {
				ackErrCh <- nil
				return
			}
			if rerr != nil {
				ackErrCh <- rerr
				return
			}
		}
	}()

	sendErr := w.streamFiles(ctx, stream, mf, committed, limiter)
	// Always close the send direction so the ack reader observes EOF.
	closeErr := stream.CloseSend()
	ackErr := <-ackErrCh

	if sendErr != nil {
		return nil, fmt.Errorf("stream chunks: %w", sendErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close send: %w", closeErr)
	}
	if ackErr != nil {
		return nil, fmt.Errorf("chunk ack: %w", ackErr)
	}

	commit, err := client.CommitEpisode(ctx, &cloudpb.CommitEpisodeRequest{EpisodeId: mf.ID})
	if err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	if commit.GetState() == cloudpb.EpisodeState_EPISODE_STATE_COMPLETE {
		return nil, nil
	}
	// Not complete: collect per-file verification detail. Any failed file is a
	// terminal corruption verdict.
	var details []string
	for _, v := range commit.GetFiles() {
		if !v.GetOk() {
			details = append(details, fmt.Sprintf("%s: %s (expected %s, actual %s)",
				v.GetPath(), v.GetDetail(), v.GetExpectedSha256(), v.GetActualSha256()))
		}
	}
	if len(details) > 0 {
		return errors.New(strings.Join(details, "; ")), nil
	}
	// Commit returned a non-complete state with no per-file failure detail:
	// treat as retryable so we do not permanently fail on an ambiguous verdict.
	return nil, fmt.Errorf("commit returned state %s without completion", commit.GetState())
}

// streamFiles sends every file that still needs bytes, honoring the committed
// offset and the bandwidth ceiling.
func (w *DataTransferWorker) streamFiles(ctx context.Context, stream grpc.BidiStreamingClient[cloudpb.EpisodeChunk, cloudpb.EpisodeChunkAck], mf data.Manifest, committed map[string]int64, limiter *byteRateLimiter) error {
	buf := make([]byte, transferChunkBytes)
	for _, file := range mf.Files {
		start := committed[file.Path]
		// A non-empty file whose committed offset already covers it is durably
		// stored, so skip it. A zero-length file must NOT be skipped on a zero
		// offset: streamOneFile sends it as a single empty EOF chunk so the server
		// persists the (empty) object. Skipping it here would silently drop a file
		// the manifest lists, leaving the server unaware it exists.
		if file.Size > 0 && start >= file.Size {
			continue
		}
		if err := w.streamOneFile(ctx, stream, mf.ID, file, start, buf, limiter); err != nil {
			return err
		}
	}
	return nil
}

func (w *DataTransferWorker) streamOneFile(ctx context.Context, stream grpc.BidiStreamingClient[cloudpb.EpisodeChunk, cloudpb.EpisodeChunkAck], episodeID string, file data.File, start int64, buf []byte, limiter *byteRateLimiter) error {
	f, _, err := w.manager.OpenFile(episodeID, file.Path, start)
	if err != nil {
		return fmt.Errorf("open %s: %w", file.Path, err)
	}
	defer f.Close()

	offset := start
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, readErr := f.Read(buf)
		if n > 0 {
			limiter.take(n)
			eof := offset+int64(n) >= file.Size
			if err := stream.Send(&cloudpb.EpisodeChunk{
				EpisodeId: episodeID,
				Path:      file.Path,
				Offset:    uint64(offset),
				Data:      buf[:n],
				Eof:       eof,
			}); err != nil {
				return fmt.Errorf("send %s @%d: %w", file.Path, offset, err)
			}
			offset += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read %s @%d: %w", file.Path, offset, readErr)
		}
	}
	// Zero-length files: send a single empty eof chunk so the server persists
	// the object (Read returned EOF immediately, no chunk sent above).
	if file.Size == 0 && start == 0 {
		if err := stream.Send(&cloudpb.EpisodeChunk{EpisodeId: episodeID, Path: file.Path, Offset: 0, Data: nil, Eof: true}); err != nil {
			return fmt.Errorf("send empty %s: %w", file.Path, err)
		}
	}
	return nil
}

// buildEpisodeManifest projects the device manifest onto the cloud
// EpisodeManifest. The full device manifest JSON rides in attributes_json for
// fidelity; the typed fields are what the catalog indexes on.
//
// The manifest carries no org or asset. The cloud reads both from the client
// certificate presented on the connection, so there is nothing here for the
// device to assert and nothing for the server to cross-check.
func buildEpisodeManifest(mf data.Manifest) (*cloudpb.EpisodeManifest, error) {
	raw, err := json.Marshal(mf)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	files := make([]*cloudpb.EpisodeFileManifest, 0, len(mf.Files))
	for _, f := range mf.Files {
		role, err := episodeFileRole(f.Role)
		if err != nil {
			return nil, fmt.Errorf("file %s: %w", f.Path, err)
		}
		files = append(files, &cloudpb.EpisodeFileManifest{
			Path:      f.Path,
			SizeBytes: uint64(f.Size),
			Sha256:    f.SHA256,
			MediaType: f.MediaType,
			Format:    f.Format,
			SourceId:  f.SourceID,
			Role:      role,
		})
	}
	var lower, upper int64
	if len(mf.UTCObservations) > 0 {
		lower = mf.UTCObservations[0].OffsetLowerNanos
		upper = mf.UTCObservations[0].OffsetUpperNanos
	}
	trigger := mf.Trigger.Reason
	if mf.Trigger.Expression != "" {
		trigger = mf.Trigger.Expression
	}
	return &cloudpb.EpisodeManifest{
		EpisodeId:            mf.ID,
		Campaign:             mf.Trigger.CampaignName,
		CampaignRevision:     mf.Trigger.CampaignRevision,
		Trigger:              trigger,
		StartedBoottimeNanos: mf.StartedEpisodeNS,
		StoppedBoottimeNanos: mf.StoppedEpisodeNS,
		StartedUnixNanos:     mf.StartedUnixNanos,
		UtcOffsetLowerNanos:  lower,
		UtcOffsetUpperNanos:  upper,
		SystemClockStatus:    mf.SystemClockStatus,
		Files:                files,
		AttributesJson:       raw,
	}, nil
}

// episodeFileRole maps the device manifest's per-file role onto the wire enum.
//
// The manifest omits the role for capture payload and capture metadata, so an
// empty role means captured. This sends CAPTURED explicitly rather than
// leaving the field at its default, so a manifest that went through this
// mapper is distinguishable on the wire from one written by an agent built
// before the field existed.
//
// Any other string is a hard error rather than a value on the wire. The wire
// contract gives UNSPECIFIED exactly one meaning, "the sender predates this
// field", and states that a sender holding a role it cannot express MUST NOT
// send UNSPECIFIED: doing so books the file as capture payload, corrupts
// capture-only accounting, and tells the reader nothing was lost, so no
// reader can flag it. The enum has no "unknown" member to fall back to, and
// inventing one is not ours to do.
//
// Failing is the honest option because the role string is produced by this
// same repository. Every value comes from a data.FileRole constant that seal
// wrote, so a role this mapper does not know is a programming error on our
// own side: someone added a role to the manifest and did not extend this
// switch or the wire enum. It is deterministic, so a retry re-sends the same
// manifest and fails identically. buildEpisodeManifest's error reaches
// processEpisode as verifyErr, which marks the episode failed with the
// offending role recorded and does not spin on it. The gap then surfaces on
// the first episode that carries the new role, which is the whole point;
// silently mis-accounting every such file forever is the alternative.
func episodeFileRole(role string) (cloudpb.EpisodeFileRole, error) {
	switch role {
	case "":
		return cloudpb.EpisodeFileRole_EPISODE_FILE_ROLE_CAPTURED, nil
	case data.FileRoleDerived:
		return cloudpb.EpisodeFileRole_EPISODE_FILE_ROLE_DERIVED, nil
	default:
		return cloudpb.EpisodeFileRole_EPISODE_FILE_ROLE_UNSPECIFIED,
			fmt.Errorf("unmappable manifest file role %q: add it to the EpisodeFileRole wire enum and to episodeFileRole", role)
	}
}

// byteRateLimiter paces a byte stream to at most ratePerSec bytes per second by
// sleeping for the transmission time of each batch of bytes after sending it.
// A non-positive rate means unlimited. sleep is injected so tests can pace
// deterministically; production passes a context-aware sleeper.
type byteRateLimiter struct {
	ratePerSec float64
	sleep      func(time.Duration)
}

func (l *byteRateLimiter) take(n int) {
	if l == nil || l.ratePerSec <= 0 || n <= 0 || l.sleep == nil {
		return
	}
	d := time.Duration(float64(n) / l.ratePerSec * float64(time.Second))
	l.sleep(d)
}
