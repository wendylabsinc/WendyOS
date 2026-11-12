package services

import (
	"context"
	"crypto/hpke"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/registrycrypto"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// The registry pull (registry plan T14): a verified DeviceAssignment's
// release is fetched from Cloud's DeviceArtifactService over the device's own
// mTLS, opened with keys only the operator's signature and this device's key
// can produce, checked against the digests the operator signed and imported
// into containerd. The registry is blind and untrusted: it can withhold
// material, never substitute it.
//
// Fail-safe table (each row is the reported ApplyOutcome; decide writes the
// audit line, and nothing is imported unless every check passed):
//
//	registry refuses (NOT_FOUND: no live assignment, also when it is removed
//	mid-pull; PERMISSION_DENIED, FAILED_PRECONDITION, ...)   PULL_REFUSED
//	assignment check could not be made (UNAVAILABLE), attempt
//	timed out, stream reset or cut (INTERNAL, CANCELLED,
//	ABORTED), rate limited (RESOURCE_EXHAUSTED)              retried with backoff,
//	                                                         resumed at the offset held;
//	                                                         PULL_REFUSED once the
//	                                                         assignment expires
//	manifest, blob list, ciphertext or plaintext digest or
//	size differs from what the operator signed, or a chunk
//	does not authenticate                                    REJECTED_DIGEST
//	device wrap or sealed content key does not open, unknown
//	suite, no device encryption key for this identity        REJECTED_UNWRAP
//	release kind other than CONTAINER, unreadable OCI
//	manifest, local staging or containerd import fails       APPLY_FAILED
//	assignment expired before the pull (re)started           PULL_REFUSED
//	imported, desired state RUNNING: nothing starts the app
//	until starting it with its configuration exists          outcomeNotStarted
//	imported, desired state STOPPED (install only)           APPLIED
//
// The pull runs beside the postbox's command queue (pullJob): the
// assignment's entry is decided at once as outcomePulling, and the pull's
// outcome replaces it when it ends. Removing or replacing the deployment
// cancels its pull; a pull the agent's stop interrupted is resumed from the
// held assignment on the next cycle. A removed deployment's releases are
// deleted (RemoveReleases), and so is a release a newer one supersedes.

// The three transient states of a deployment on the device (service-protos
// #184: APPLY_OUTCOME_PULLING = 10, APPLY_OUTCOME_NOT_STARTED = 11,
// APPLY_OUTCOME_PULL_RETRYING = 12). Each is reported from one place:
// pulling by apply when the pull starts and by pullRetry when an attempt
// succeeds after a retry, retrying by pullRetry, not started by
// pullAssignment's result.
//
// sendTransientOutcomes is the switch: cloud refuses an outcome it does not
// know, so until cloud on prod accepts the three values all of them are
// sent as no outcome (UNSPECIFIED). Turn it on only once that is confirmed.
const sendTransientOutcomes = false

var (
	outcomePulling      = transientOutcome(cloudpbv2.ApplyOutcome_APPLY_OUTCOME_PULLING)
	outcomeNotStarted   = transientOutcome(cloudpbv2.ApplyOutcome_APPLY_OUTCOME_NOT_STARTED)
	outcomePullRetrying = transientOutcome(cloudpbv2.ApplyOutcome_APPLY_OUTCOME_PULL_RETRYING)
)

func transientOutcome(o cloudpbv2.ApplyOutcome) cloudpbv2.ApplyOutcome {
	if sendTransientOutcomes {
		return o
	}
	return cloudpbv2.ApplyOutcome_APPLY_OUTCOME_UNSPECIFIED
}

// pullReport records a transient state of the pull it is given to.
type pullReport func(cloudpbv2.ApplyOutcome)

const (
	// registryPullDir stages each pull's ciphertext and plaintext under the
	// agent config directory, one folder per deployment. A folder is emptied
	// before and after its pull; the whole directory when the postbox starts
	// and on reset. A kill or power loss mid-pull leaves it until the next of
	// those.
	registryPullDir = "registry-pull"
	// materialTimeout bounds GetDeploymentMaterial; blobAttemptTimeout bounds
	// one GetBlob stream, which a later attempt resumes.
	materialTimeout    = 30 * time.Second
	blobAttemptTimeout = 5 * time.Minute
	chunkTagSize       = 16 // AES-256-GCM tag per chunk (registrycrypto)
	// minChunkSize floors the served chunk_size, which the manifest digest
	// does not cover: tiny chunks would let the registry inflate a download
	// (17x at 1 byte) past the sizes the operator signed. At the floor the
	// tags add under 0.4 %.
	minChunkSize      = 4096
	mediaTypeDockerV2 = "application/vnd.docker.distribution.manifest.v2+json"
)

// ReleaseImage is a registry release whose every byte verified: the OCI
// manifest the operator signed (by ManifestDigest) and the decrypted blobs it
// names, ready for containerd under the release identity.
type ReleaseImage struct {
	AppID          string
	ReleaseID      string
	ManifestDigest string // "sha256:<hex>"
	Manifest       []byte // byte-exact; its SHA-256 is ManifestDigest
	// Blobs maps each config and layer digest of Manifest to the file that
	// holds its verified plaintext.
	Blobs map[string]string
}

// ReleaseImporter is the containerd capability that imports a verified
// release (containerd.Client.ImportRelease).
type ReleaseImporter interface {
	ImportRelease(ctx context.Context, img ReleaseImage) error
}

// pullFailure carries the outcome and audit reason of a failed pull.
type pullFailure struct {
	outcome cloudpbv2.ApplyOutcome
	reason  string
	err     error
}

func (f *pullFailure) Error() string { return f.err.Error() }
func (f *pullFailure) Unwrap() error { return f.err }

func pullRefused(reason string, err error) error {
	return &pullFailure{cloudpbv2.ApplyOutcome_APPLY_OUTCOME_PULL_REFUSED, reason, err}
}

func digestRefused(err error) error {
	return &pullFailure{cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_DIGEST, "digest", err}
}

func unwrapRefused(err error) error {
	return &pullFailure{cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_UNWRAP, "unwrap", err}
}

func pullFailed(reason string, err error) error {
	return &pullFailure{cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, reason, err}
}

// errStreamCut is a GetBlob stream that ended before the blob did.
var errStreamCut = errors.New("blob stream ended before the blob")

func dialDeviceArtifacts(cloudURL, certPEM, keyPEM, chainPEM string) (cloudpbv2.DeviceArtifactServiceClient, io.Closer, error) {
	conn, closer, err := dialDeviceCloud(cloudURL, certPEM, keyPEM, chainPEM)
	if err != nil {
		return nil, nil, err
	}
	return cloudpbv2.NewDeviceArtifactServiceClient(conn), closer, nil
}

// checkAssignment runs the checks that need no download (the containerd
// import repeats them), so a malformed assignment is decided with its entry.
func checkAssignment(a *cloudpbv2.DeviceAssignment) (cloudpbv2.ApplyOutcome, string, error) {
	if err := appconfig.ValidateAppID(a.GetAppId()); err != nil {
		return cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "assignment", fmt.Errorf("invalid app ID: %w", err)
	}
	if u, err := uuid.Parse(a.GetAppReleaseId()); err != nil || u.String() != a.GetAppReleaseId() {
		return cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "assignment", errors.New("invalid release id: want a canonical lowercase UUID")
	}
	return cloudpbv2.ApplyOutcome_APPLY_OUTCOME_UNSPECIFIED, "", nil
}

// pullAssignment fetches, opens, verifies and imports a checked assignment's
// release, then removes the app's releases no held assignment uses.
func (p *Postbox) pullAssignment(ctx context.Context, id *postboxIdentity, a *cloudpbv2.DeviceAssignment, report pullReport) (cloudpbv2.ApplyOutcome, string, error) {
	// A resumed pull starts without a failure, where pullRetry checks the
	// expiry: nothing is fetched under a statement that has expired.
	if !p.trustedNow().Before(a.GetExpiresAt().AsTime()) {
		return cloudpbv2.ApplyOutcome_APPLY_OUTCOME_PULL_REFUSED, "expired", errors.New("the assignment expired before its pull started")
	}
	// Named by a hash: the deployment id is cloud's string, not a path.
	dir := filepath.Join(p.configPath, registryPullDir, fmt.Sprintf("%x", sha256.Sum256([]byte(a.GetDeploymentId()))))
	// One pull per deployment runs at a time: anything here is an earlier,
	// interrupted pull's.
	if err := os.RemoveAll(dir); err != nil {
		return cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "stage", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "stage", err
	}
	// A release already on the device (a configuration-only revision, or an
	// import a restart interrupted after it finished) is not fetched again.
	var err error
	if !p.control.HasRelease(ctx, a.GetAppId(), a.GetAppReleaseId(), a.GetManifestDigest()) {
		var img ReleaseImage
		img, err = p.fetchRelease(ctx, id, a, dir, report)
		if err == nil {
			if err = p.control.ImportRelease(ctx, img); err != nil {
				err = pullFailed("import", err)
			}
		}
	}
	if err == nil {
		err = p.startRelease(ctx, a)
	}
	if err != nil {
		var f *pullFailure
		if errors.As(err, &f) {
			return f.outcome, f.reason, err
		}
		return cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "pull", err
	}
	// Only now, with the new release running (or installed), do the
	// superseded releases go; a failure is retried (Removals).
	p.removeReleases(ctx, a.GetAppId())
	return cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED, "", nil
}

// pullJob is one deployment's pull, running beside the command queue.
type pullJob struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startPull runs a held assignment's pull in the background. Its outcome is
// recorded against entryID when it ends, unless it was cancelled: a removed
// or replaced deployment is settled by its own entry, and one the agent's
// stop interrupted stays unpulled and is resumed.
func (p *Postbox) startPull(ctx context.Context, id *postboxIdentity, a *cloudpbv2.DeviceAssignment, entryID int64, correlation string) {
	dep := a.GetDeploymentId()
	ctx, cancel := context.WithCancel(ctx)
	job := &pullJob{cancel: cancel, done: make(chan struct{})}
	p.mu.Lock()
	if p.pulls == nil {
		p.pulls = map[string]*pullJob{}
	}
	p.pulls[dep] = job
	p.mu.Unlock()
	// ponytail: one goroutine per held deployment, no cap; add a semaphore if
	// devices come to hold many deployments at once.
	go func() {
		defer close(job.done)
		defer func() {
			p.mu.Lock()
			if p.pulls[dep] == job {
				delete(p.pulls, dep)
			}
			p.mu.Unlock()
		}()
		defer cancel()
		report := func(o cloudpbv2.ApplyOutcome) { p.recordPullState(a, entryID, correlation, o) }
		outcome, reason, err := p.pullAssignment(ctx, id, a, report)
		if ctx.Err() != nil {
			return
		}
		p.finishPull(ctx, a, entryID, correlation, outcome, reason, err)
	}()
}

// stopPull cancels a deployment's pull and waits until it has stopped, so
// nothing it imports outlives the caller's next step.
func (p *Postbox) stopPull(dep string) {
	p.mu.Lock()
	job := p.pulls[dep]
	delete(p.pulls, dep)
	p.mu.Unlock()
	if job != nil {
		job.cancel()
		<-job.done
	}
}

// waitPulls waits for every running pull to end.
func (p *Postbox) waitPulls() {
	p.mu.Lock()
	jobs := slices.Collect(maps.Values(p.pulls))
	p.mu.Unlock()
	for _, job := range jobs {
		<-job.done
	}
}

// recordPullState records a transient state of a running pull as its
// entry's outcome. It writes only when the entry's recorded state changes,
// and never over a newer entry or a replaced assignment.
func (p *Postbox) recordPullState(a *cloudpbv2.DeviceAssignment, entryID int64, correlation string, outcome cloudpbv2.ApplyOutcome) {
	current := func(st *postboxState) bool {
		held, o := st.Assignments[a.GetDeploymentId()], st.Outcomes[a.GetAppId()]
		return held != nil && held.EntryID == entryID && o != nil && o.EntryID == entryID && o.Outcome != outcome
	}
	if st := p.snapshot(); !current(&st) {
		return
	}
	if err := p.commit(func(st *postboxState) {
		if current(st) {
			st.Outcomes = maps.Clone(st.Outcomes)
			st.Outcomes[a.GetAppId()] = &postboxOutcome{EntryID: entryID, CorrelationID: correlation, Outcome: outcome}
		}
	}); err != nil {
		p.logger.Warn("postbox: saving a pull's state failed", zap.String("deployment_id", a.GetDeploymentId()), zap.Error(err))
	}
}

// finishPull audits a pull's end and records it: the assignment is marked
// pulled (never resumed), and the outcome replaces the app's unless a newer
// entry decided the app since. The reported state carries one outcome per
// app, so the newer entry's stands; the pull's stays in the audit line. A
// failed save is retried until it lands or the pull is cancelled, so a
// finished pull does not run again each cycle.
func (p *Postbox) finishPull(ctx context.Context, a *cloudpbv2.DeviceAssignment, entryID int64, correlation string, outcome cloudpbv2.ApplyOutcome, reason string, err error) {
	fields := []zap.Field{
		zap.String("audit", "postbox"), zap.Int64("entry_id", entryID), zap.String("correlation_id", correlation),
		zap.String("deployment_id", a.GetDeploymentId()), zap.String("app_id", a.GetAppId()),
		zap.String("outcome", outcome.String()), zap.String("reason", reason),
	}
	if err != nil {
		p.count(reason)
		p.logger.Warn("registry pull ended without applying", append(fields, zap.Error(err))...)
	} else {
		p.logger.Info("registry pull ended", fields...)
	}
	record := func(st *postboxState) {
		held := st.Assignments[a.GetDeploymentId()]
		if held == nil || held.EntryID != entryID {
			return // removed or replaced while it ended
		}
		done := *held
		done.Pulled = true
		st.Assignments = maps.Clone(st.Assignments)
		st.Assignments[a.GetDeploymentId()] = &done
		if o := st.Outcomes[a.GetAppId()]; o == nil || o.EntryID <= entryID {
			st.Outcomes = maps.Clone(st.Outcomes)
			st.Outcomes[a.GetAppId()] = &postboxOutcome{EntryID: entryID, CorrelationID: correlation, Outcome: outcome}
		}
	}
	for wait := min(time.Second, p.retry); ; wait = min(2*wait, p.retry) {
		serr := p.commit(record)
		if serr == nil {
			return
		}
		p.logger.Warn("postbox: saving a pull's outcome failed; holding until it lands", append(fields, zap.Error(serr))...)
		select {
		case <-ctx.Done():
			return // removed, replaced or stopping: settled elsewhere or resumed
		case <-time.After(wait):
		}
	}
}

// resumePulls restarts the pull of every held assignment whose pull neither
// ended nor runs, and retries the removals that failed.
func (p *Postbox) resumePulls(ctx context.Context, id *postboxIdentity) {
	st := p.snapshot()
	for dep, held := range st.Assignments {
		p.mu.Lock()
		_, running := p.pulls[dep]
		p.mu.Unlock()
		if held.Pulled || running || len(held.Assignment) == 0 {
			continue
		}
		a := &cloudpbv2.DeviceAssignment{}
		if err := proto.Unmarshal(held.Assignment, a); err != nil {
			p.logger.Warn("postbox: held assignment unreadable; not resumed", zap.String("deployment_id", dep), zap.Error(err))
			continue
		}
		p.logger.Info("resuming registry pull", zap.String("audit", "postbox"), zap.String("deployment_id", dep), zap.String("app_id", held.AppID))
		p.startPull(ctx, id, a, held.EntryID, held.CorrelationID)
	}
	for _, app := range st.Removals {
		p.removeReleases(ctx, app)
	}
}

// removeReleases deletes app's releases that no held assignment uses. While
// the app has a live held deployment it only prunes: a release a container
// still uses stays, so the running app is never removed by an update. With
// none, the app's containers running a release go too. A failure is kept in
// Removals and retried each cycle, so no decrypted release stays on the
// device.
func (p *Postbox) removeReleases(ctx context.Context, app string) error {
	// "" means every app's releases to RemoveReleases: only reset asks that.
	if err := appconfig.ValidateAppID(app); err != nil {
		return fmt.Errorf("invalid app ID: %w", err)
	}
	p.mu.Lock()
	var keep []string
	for _, a := range p.state.Assignments {
		// An ABSENT statement holds the app's removal, not its release.
		if a.AppID == app && a.DesiredState != cloudpbv2.Running_ABSENT.String() {
			keep = append(keep, a.AppReleaseID)
		}
	}
	p.mu.Unlock()
	slices.Sort(keep)
	remove := p.control.RemoveReleases
	if len(keep) > 0 {
		remove = p.control.PruneReleases
	}
	err := remove(ctx, app, keep)
	if err != nil {
		p.logger.Warn("removing an app's registry releases failed; retrying next cycle", zap.String("audit", "postbox"), zap.String("app_id", app), zap.Error(err))
	}
	serr := p.commit(func(st *postboxState) {
		queued := slices.Contains(st.Removals, app)
		switch {
		case err != nil && !queued:
			st.Removals = append(slices.Clone(st.Removals), app)
		case err == nil && queued:
			st.Removals = slices.DeleteFunc(slices.Clone(st.Removals), func(a string) bool { return a == app })
		}
	})
	return errors.Join(err, serr)
}

// RemoveAllReleases stops every pull and removes every registry release and
// the containers running one: the reset path. imagesOnly leaves the
// containers to a later run (the reset's restart recovery).
func (p *Postbox) RemoveAllReleases(ctx context.Context, imagesOnly bool) error {
	p.mu.Lock()
	deps := slices.Collect(maps.Keys(p.pulls))
	p.mu.Unlock()
	for _, dep := range deps {
		p.stopPull(dep)
	}
	if imagesOnly {
		return p.control.RemoveReleaseImages(ctx)
	}
	return p.control.RemoveReleases(ctx, "", nil)
}

func (p *Postbox) fetchRelease(ctx context.Context, id *postboxIdentity, a *cloudpbv2.DeviceAssignment, dir string, report pullReport) (ReleaseImage, error) {
	p.mu.Lock()
	cloudURL, certPEM, keyPEM, chainPEM := p.cloudURL, p.certPEM, p.keyPEM, p.chainPEM
	p.mu.Unlock()
	client, closer, err := p.dialArtifacts(cloudURL, certPEM, keyPEM, chainPEM)
	if err != nil {
		return ReleaseImage{}, pullRefused("dial", err)
	}
	defer func() { _ = closer.Close() }()

	var mat *cloudpbv2.GetDeploymentMaterialResponse
	err = p.pullRetry(ctx, a, report, func(ctx context.Context) (bool, error) {
		ctx, cancel := context.WithTimeout(ctx, materialTimeout)
		defer cancel()
		var err error
		mat, err = client.GetDeploymentMaterial(ctx, &cloudpbv2.GetDeploymentMaterialRequest{DeploymentId: a.GetDeploymentId()})
		return false, err
	})
	if err != nil {
		return ReleaseImage{}, err
	}
	m := mat.GetManifest()
	blobs, chunkSize, err := releaseBlobs(a, m)
	if err != nil {
		return ReleaseImage{}, err
	}
	priv, err := p.encryptionKey(id.uri)
	if err != nil {
		return ReleaseImage{}, unwrapRefused(fmt.Errorf("device encryption key: %w", err))
	}
	deploymentKey, err := openDeploymentKey(priv, id.tenant, id.uri, a)
	if err != nil {
		return ReleaseImage{}, unwrapRefused(err)
	}
	contentKey, err := openContentKey(deploymentKey, id.tenant, a, m.GetContentKeyId(), mat.GetSealedContentKey())
	clear(deploymentKey)
	if err != nil {
		return ReleaseImage{}, unwrapRefused(err)
	}
	defer clear(contentKey)

	img := ReleaseImage{
		AppID: a.GetAppId(), ReleaseID: a.GetAppReleaseId(), ManifestDigest: a.GetManifestDigest(),
		Manifest: m.GetOciManifest(), Blobs: map[string]string{},
	}
	for i, b := range blobs {
		ct := filepath.Join(dir, strconv.Itoa(i)+".enc")
		pt := filepath.Join(dir, strconv.Itoa(i))
		if err := p.pullRetry(ctx, a, report, func(ctx context.Context) (bool, error) {
			before := stagedSize(ct)
			err := fetchBlob(ctx, client, a.GetDeploymentId(), b, ct)
			return stagedSize(ct) > before, err
		}); err != nil {
			return ReleaseImage{}, err
		}
		if err := decryptStaged(ct, pt, contentKey, b, chunkSize); err != nil {
			return ReleaseImage{}, err
		}
		img.Blobs[b.GetPlaintextDigest()] = pt
	}
	return img, nil
}

// pullRetry runs call until it succeeds or fails for good. A transient
// failure (the registry could not make the assignment check, an attempt timed
// out, a stream was reset or cut, a rate limit) backs off with jitter and
// retries until the
// assignment's signed validity ends: availability degrades, the check is
// never skipped. An attempt that made progress starts the backoff again.
// Any other registry status is its refusal. The pull is reported retrying
// while it waits, and pulling again once an attempt succeeds.
func (p *Postbox) pullRetry(ctx context.Context, a *cloudpbv2.DeviceAssignment, report pullReport, call func(context.Context) (bool, error)) error {
	for failures := 0; ; failures++ {
		progressed, err := call(ctx)
		if err == nil {
			if failures > 0 {
				report(outcomePulling)
			}
			return nil
		}
		if progressed {
			failures = 0
		}
		var f *pullFailure
		switch {
		case errors.As(err, &f):
			return err
		case ctx.Err() != nil:
			return pullFailed("pull", err)
		case !retryablePull(err):
			return pullRefused("pull", err)
		case !p.trustedNow().Before(a.GetExpiresAt().AsTime()):
			return pullRefused("unavailable", fmt.Errorf("assignment expired while the registry was unavailable: %w", err))
		}
		report(outcomePullRetrying)
		wait := min(p.retry<<min(failures, 6), 30*time.Minute)
		wait = wait/2 + rand.N(wait/2+1) // a fleet does not retry in one wave
		p.logger.Warn("registry pull unavailable; retrying", zap.String("audit", "postbox"),
			zap.String("deployment_id", a.GetDeploymentId()), zap.String("app_id", a.GetAppId()),
			zap.Duration("retry_in", wait), zap.Error(err))
		select {
		case <-ctx.Done():
			return pullFailed("pull", ctx.Err())
		case <-time.After(wait):
		}
	}
}

// retryablePull reports a failure a later attempt can get past. grpc-go
// reports most HTTP/2 stream resets (e.g. a proxy resetting a long GetBlob) as
// INTERNAL or CANCELLED; pullRetry already returned for the agent's own
// cancellation, so CANCELLED here came from the connection. Explicit refusals
// (NOT_FOUND, PERMISSION_DENIED, FAILED_PRECONDITION, ...) are final.
func retryablePull(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Internal, codes.ResourceExhausted, codes.Aborted, codes.Canceled:
		return true
	}
	return errors.Is(err, errStreamCut)
}

// releaseBlobs checks the served manifest against the signed assignment
// before anything else is used: the manifest digest must be the signed one,
// and the blob list must be exactly the set of digests the OCI manifest's
// config and layers name (a digest repeated there is listed once), each at
// its descriptor size and with the ciphertext size its chunking implies. Only then
// are the served sizes allowed to size a download.
func releaseBlobs(a *cloudpbv2.DeviceAssignment, m *cloudpbv2.ArtifactManifest) ([]*cloudpbv2.EncryptedBlob, int, error) {
	if m.GetManifestDigest() != a.GetManifestDigest() || digest.FromBytes(m.GetOciManifest()).String() != a.GetManifestDigest() {
		return nil, 0, digestRefused(fmt.Errorf("served manifest is not the signed release %s", a.GetManifestDigest()))
	}
	if m.GetKind() != cloudpbv2.ReleaseType_CONTAINER {
		return nil, 0, pullFailed("kind", fmt.Errorf("release kind %v is not a container image", m.GetKind()))
	}
	if m.GetAead() != cloudpbv2.AeadSuite_AEAD_SUITE_AES_256_GCM_CHUNKED {
		return nil, 0, unwrapRefused(fmt.Errorf("unknown AEAD suite %v", m.GetAead()))
	}
	chunkSize := int(m.GetChunkSize())
	if chunkSize == 0 {
		chunkSize = registrycrypto.DefaultChunkSize
	}
	if chunkSize < minChunkSize || chunkSize > registrycrypto.MaxChunkSize {
		return nil, 0, digestRefused(fmt.Errorf("%w: %d", registrycrypto.ErrChunkSize, chunkSize))
	}
	var om ocispec.Manifest
	if err := json.Unmarshal(m.GetOciManifest(), &om); err != nil {
		return nil, 0, pullFailed("manifest", fmt.Errorf("reading the OCI manifest: %w", err))
	}
	if om.MediaType != ocispec.MediaTypeImageManifest && om.MediaType != mediaTypeDockerV2 {
		return nil, 0, pullFailed("manifest", fmt.Errorf("manifest media type %q is not an image manifest", om.MediaType))
	}
	want := map[string]int64{}
	for _, d := range append([]ocispec.Descriptor{om.Config}, om.Layers...) {
		if size, ok := want[d.Digest.String()]; ok && size != d.Size {
			return nil, 0, pullFailed("manifest", fmt.Errorf("manifest names %s at two sizes", d.Digest))
		}
		want[d.Digest.String()] = d.Size
	}
	if len(m.GetBlobs()) != len(want) {
		return nil, 0, digestRefused(fmt.Errorf("served %d blobs for the %d the manifest names", len(m.GetBlobs()), len(want)))
	}
	seen := map[string]bool{}
	for _, b := range m.GetBlobs() {
		size, ok := want[b.GetPlaintextDigest()]
		switch {
		case !ok || seen[b.GetPlaintextDigest()] || size < 0 || uint64(size) != b.GetPlaintextSize():
			return nil, 0, digestRefused(fmt.Errorf("served blob %s is not one the manifest names", b.GetPlaintextDigest()))
		case b.GetCiphertextSize() != ciphertextSize(b.GetPlaintextSize(), chunkSize):
			return nil, 0, digestRefused(fmt.Errorf("blob %s ciphertext size %d does not fit its plaintext size", b.GetPlaintextDigest(), b.GetCiphertextSize()))
		}
		seen[b.GetPlaintextDigest()] = true
	}
	return m.GetBlobs(), chunkSize, nil
}

// ciphertextSize is a blob's size after chunked AEAD: one tag per chunk, and
// an empty blob is one empty chunk.
func ciphertextSize(plaintext uint64, chunkSize int) uint64 {
	chunks := max(1, (plaintext+uint64(chunkSize)-1)/uint64(chunkSize))
	return plaintext + chunks*chunkTagSize
}

// openDeploymentKey unwraps the assignment's device wrap; openContentKey
// opens the served sealed content key with the result. Every binding comes
// from the signed assignment and this device's own identity and key, never
// from the served material, so a wrap or seal for another tenant, deployment,
// key, manifest or device does not open.
func openDeploymentKey(priv hpke.PrivateKey, tenant, device string, a *cloudpbv2.DeviceAssignment) ([]byte, error) {
	w := a.GetDeviceWrap()
	if w.GetSuite() != cloudpbv2.KemSuite_KEM_SUITE_MLKEM768_X25519 {
		return nil, fmt.Errorf("device wrap suite %v", w.GetSuite())
	}
	key, err := registrycrypto.Wrap{
		OrganizationID: tenant, Scope: registrycrypto.ScopeDeployment, ScopeID: a.GetDeploymentId(),
		KeyID: a.GetDeploymentKeyId(), RecipientKind: byte(cloudpbv2.RecipientKind_RECIPIENT_KIND_DEVICE),
		Recipient: device, RecipientKeyID: registrycrypto.KeyID(priv.PublicKey().Bytes()),
	}.Open(priv, w.GetEnc(), w.GetCiphertext())
	if err != nil {
		return nil, fmt.Errorf("device wrap: %w", err)
	}
	return key, nil
}

func openContentKey(deploymentKey []byte, tenant string, a *cloudpbv2.DeviceAssignment, contentKeyID string, sealed *cloudpbv2.SealedContentKey) ([]byte, error) {
	key, err := registrycrypto.SealedKey{
		OrganizationID: tenant, DeploymentID: a.GetDeploymentId(), ManifestDigest: a.GetManifestDigest(),
		ContentKeyID: contentKeyID, DeploymentKeyID: a.GetDeploymentKeyId(),
	}.Open(deploymentKey, sealed.GetNonce(), sealed.GetCiphertext())
	if err != nil {
		return nil, fmt.Errorf("sealed content key: %w", err)
	}
	return key, nil
}

// fetchBlob downloads b's ciphertext to path, resuming at what path already
// holds. It never writes past the blob's size.
func fetchBlob(ctx context.Context, client cloudpbv2.DeviceArtifactServiceClient, deploymentID string, b *cloudpbv2.EncryptedBlob, path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return pullFailed("stage", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return pullFailed("stage", err)
	}
	offset, size := uint64(info.Size()), b.GetCiphertextSize()
	if offset == size {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, blobAttemptTimeout)
	defer cancel()
	stream, err := client.GetBlob(ctx, &cloudpbv2.GetBlobRequest{DeploymentId: deploymentID, CiphertextDigest: b.GetCiphertextDigest(), Offset: offset})
	if err != nil {
		return err
	}
	for offset < size {
		msg, err := stream.Recv()
		if err == io.EOF {
			return errStreamCut
		}
		if err != nil {
			return err
		}
		data := msg.GetData()
		if uint64(len(data)) > size-offset {
			return digestRefused(fmt.Errorf("registry served more than blob %s's %d bytes", b.GetCiphertextDigest(), size))
		}
		if _, err := f.Write(data); err != nil {
			return pullFailed("stage", err)
		}
		offset += uint64(len(data))
	}
	return nil
}

func stagedSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// decryptStaged decrypts the staged ciphertext to pt, which DecryptBlob checks
// against the ciphertext and plaintext digests and sizes, and drops the
// ciphertext. On a refusal the caller's staging cleanup removes pt.
func decryptStaged(ct, pt string, contentKey []byte, b *cloudpbv2.EncryptedBlob, chunkSize int) error {
	in, err := os.Open(ct)
	if err != nil {
		return pullFailed("stage", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(pt, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return pullFailed("stage", err)
	}
	defer func() { _ = out.Close() }()
	err = registrycrypto.DecryptBlob(out, in, contentKey, registrycrypto.Blob{
		PlaintextDigest: b.GetPlaintextDigest(), CiphertextDigest: b.GetCiphertextDigest(),
		PlaintextSize: b.GetPlaintextSize(), CiphertextSize: b.GetCiphertextSize(),
	}, chunkSize)
	var pathErr *fs.PathError
	switch {
	case errors.As(err, &pathErr):
		return pullFailed("stage", err)
	case err != nil:
		return digestRefused(fmt.Errorf("blob %s: %w", b.GetPlaintextDigest(), err))
	}
	if err := out.Close(); err != nil {
		return pullFailed("stage", err)
	}
	return os.Remove(ct)
}

// loadDeviceEncryptionKey is principal's stored registry encryption key
// (T10). The pull never creates one: a wrap is only ever made to a key Cloud
// registered.
func loadDeviceEncryptionKey(configPath, principal string) (hpke.PrivateKey, error) {
	raw, err := os.ReadFile(filepath.Join(configPath, deviceEncryptionKeyFile))
	if err != nil {
		return nil, err
	}
	var stored storedEncryptionKey
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, err
	}
	if stored.Principal != principal {
		return nil, errors.New("the stored key belongs to another enrollment")
	}
	return registrycrypto.KEM.NewPrivateKey(stored.PrivateKey)
}
