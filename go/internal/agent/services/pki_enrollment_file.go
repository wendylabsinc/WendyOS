package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/pkienroll"
	"github.com/wendylabsinc/wendy/go/internal/shared/enrolltoken"
)

// PKIEnrollmentFileName is the staged credential file that hands the agent a
// pki-core enrollment token once.
//
// PRECEDENT. This is deliberately the same mechanism as enrollment.json, read
// by ApplyEnrollmentFile in enrollment_file.go and written by the installer
// (go/internal/cli/assets/docs/agent.sh). The properties that made it the right
// shape there are the same ones needed here: the credential is single-use and
// short-lived, it has to survive being staged before the agent is running, it
// must not be added to a long-lived configuration file, and it must be deleted
// once redeemed.
//
// WHO WRITES IT. Two callers, and they are not alternatives. On a device with
// shell access the installer or an operator writes the file directly, with
// `wendy data enroll --local`. On a device with no shell access - which is every
// WendyOS device in the dev fleet - the file cannot be written from outside at
// all, so the token arrives over the agent's own gRPC surface instead:
// StagePKIEnrollment on WendyProvisioningService writes exactly this file and
// then applies it, the same way StartProvisioning is the remote face of
// enrollment.json. The file stays the single mechanism; only the way it is
// written differs.
//
// The two files never overlap: enrollment.json carries a Wendy Cloud asset
// token and produces the Certificate Authority Service (CAS) triple, this one
// carries a pki-core enrollment token and produces the pki-core triple.
const PKIEnrollmentFileName = pkienroll.StagedFileName

// PKIEnrollmentStatus is the outcome of one attempt to redeem a staged
// credential. It exists because StagePKIEnrollment has to answer its caller
// with what happened, and "error or not" is the wrong shape: a deferred
// enrolment is not a failure, and a refusal is not something a retry fixes.
type PKIEnrollmentStatus int

const (
	// PKIEnrollmentNone means there was no staged credential to redeem.
	PKIEnrollmentNone PKIEnrollmentStatus = iota
	// PKIEnrollmentEnrolled means a pki-core leaf was issued and stored.
	PKIEnrollmentEnrolled
	// PKIEnrollmentAlreadyEnrolled means the device already holds a pki-core
	// identity, so the staged token was discarded unredeemed.
	PKIEnrollmentAlreadyEnrolled
	// PKIEnrollmentDeferred means the device is not enrolled with Wendy Cloud
	// yet, so no certificate Common Name can be built. The staged token is
	// kept and retried on the next agent start.
	PKIEnrollmentDeferred
	// PKIEnrollmentRefused means pki-core rejected the credential or returned
	// an identity this device must not store. A retry with the same token
	// cannot succeed.
	PKIEnrollmentRefused
	// PKIEnrollmentFailed covers everything else: a malformed file, a missing
	// tenant, a network error, a storage error.
	PKIEnrollmentFailed
)

// String is what the RPC and the logs print.
func (s PKIEnrollmentStatus) String() string {
	switch s {
	case PKIEnrollmentNone:
		return "none"
	case PKIEnrollmentEnrolled:
		return "enrolled"
	case PKIEnrollmentAlreadyEnrolled:
		return "already_enrolled"
	case PKIEnrollmentDeferred:
		return "deferred"
	case PKIEnrollmentRefused:
		return "refused"
	case PKIEnrollmentFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// PKIEnrollmentOutcome reports what one ApplyStagedFile did. It deliberately
// carries the issued SPIFFE Uniform Resource Identifier and never the token:
// the caller supplied the credential and has no reason to be handed it back,
// and a response is logged in far more places than a request body is.
type PKIEnrollmentOutcome struct {
	Status     PKIEnrollmentStatus
	SPIFFEURI  string
	DeviceName string
	NotAfter   time.Time
	// Reason is a human-readable explanation for the deferred, refused and
	// failed statuses, and empty otherwise.
	Reason string
}

// PKIEnrollment owns the device's pki-core identity: the staged credential
// file, the PEM triple under <configPath>/pki, and the renewal loop.
//
// It never reads or writes the CAS triple. It borrows exactly one thing from
// the CAS side, the "sh/wendy/<org>/<asset>" Common Name, so that the two CSRs
// state the same subject and pki-core's dev shim (which derives device_id from
// the CN) lines up with no special case.
type PKIEnrollment struct {
	logger     *zap.Logger
	configPath string
	store      *pkienroll.Store

	// provisioningSvc supplies the org and asset the Common Name is built
	// from. A device that is not yet enrolled with Wendy Cloud has no such
	// pair, so pki enrolment waits for it rather than inventing one.
	provisioningSvc *ProvisioningService

	// enroll is indirected for tests.
	enroll func(context.Context, pkienroll.EnrollRequest) (pkienroll.Result, error)
	// httpClient, when set, is passed to enroll. Tests point it at an
	// httptest server; production leaves it nil for the package default.
	httpClient *http.Client

	// applyMu serialises redemption. Without it a StagePKIEnrollment call
	// arriving while the startup pass is still running would race it for the
	// same staged file and the same single-use token, and one of the two would
	// burn a credential the other had already spent.
	applyMu sync.Mutex

	// renewMu guards renewerRunning. EnsureRenewer is called from startup and
	// again from every successful StagePKIEnrollment, and a second renewal
	// loop over the same PEM triple would renew twice per interval.
	renewMu        sync.Mutex
	renewerRunning bool
}

// NewPKIEnrollment builds the pki identity manager for configPath.
func NewPKIEnrollment(logger *zap.Logger, configPath string, provisioningSvc *ProvisioningService) *PKIEnrollment {
	return &PKIEnrollment{
		logger:          logger,
		configPath:      configPath,
		store:           pkienroll.NewStore(configPath),
		provisioningSvc: provisioningSvc,
		enroll:          pkienroll.Enroll,
	}
}

// Store is the PEM triple the data platform dialer reads.
func (p *PKIEnrollment) Store() *pkienroll.Store { return p.store }

// StagedFilePath is where the credential is expected.
func (p *PKIEnrollment) StagedFilePath() string {
	return filepath.Join(p.configPath, PKIEnrollmentFileName)
}

// ApplyStagedFile enrolls the device against pki-core from a staged token, then
// deletes the file. It is best-effort and safe to call unconditionally at
// startup: an absent file is a no-op, an already-enrolled store keeps its
// identity and clears the file, and a malformed or refused token is logged and
// the file removed because the token's own short lifetime self-limits it
// anyway. This mirrors ApplyEnrollmentFile, including the bounded three
// attempts for a slow boot-time network.
//
// The returned outcome is for StagePKIEnrollment, which has to tell a remote
// caller what became of the credential it just handed over. The startup path
// ignores it and reads the log instead.
func (p *PKIEnrollment) ApplyStagedFile(ctx context.Context) PKIEnrollmentOutcome {
	// One redemption at a time: the token is single-use, and the startup pass
	// and a StagePKIEnrollment call can otherwise overlap on the same file.
	p.applyMu.Lock()
	defer p.applyMu.Unlock()

	path := p.StagedFilePath()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return PKIEnrollmentOutcome{Status: PKIEnrollmentNone}
	}
	if err != nil {
		p.logger.Error("Failed to read pki enrollment file", zap.String("path", path), zap.Error(err))
		p.removeStagedFile(path)
		return PKIEnrollmentOutcome{Status: PKIEnrollmentFailed, Reason: fmt.Sprintf("reading %s: %v", path, err)}
	}

	if p.store.Has() {
		// Renewal, not re-enrolment, keeps an existing identity alive. Burning
		// a fresh single-use token to replace a working certificate would also
		// discard the renewal lineage pki-core tracks against it.
		p.logger.Info("pki identity already present; discarding staged pki enrollment token",
			zap.String("leaf", p.store.LeafPath()))
		p.removeStagedFile(path)
		outcome := PKIEnrollmentOutcome{
			Status: PKIEnrollmentAlreadyEnrolled,
			Reason: "the device already holds a pki-core identity at " + p.store.LeafPath() +
				"; it is renewed in place, not re-enrolled",
		}
		if meta, metaErr := p.store.LoadMetadata(); metaErr == nil {
			outcome.DeviceName = meta.DeviceName
		}
		if uri, uriErr := p.store.SPIFFEURI(); uriErr == nil {
			outcome.SPIFFEURI = uri
		}
		return outcome
	}

	var staged pkienroll.StagedEnrollment
	if err := json.Unmarshal(data, &staged); err != nil {
		p.logger.Error("Failed to parse pki enrollment file, removing", zap.Error(err))
		p.removeStagedFile(path)
		return PKIEnrollmentOutcome{Status: PKIEnrollmentFailed, Reason: "the staged pki enrollment file is not valid JSON"}
	}
	if staged.Token == "" {
		p.logger.Error("pki enrollment file carries no token, removing")
		p.removeStagedFile(path)
		return PKIEnrollmentOutcome{Status: PKIEnrollmentFailed, Reason: "the staged pki enrollment file carries no token"}
	}

	tenantUUID, ok := p.resolveTenant(staged)
	if !ok {
		const reason = "no tenant: the token carries no tenant_uuid claim and no tenantUUID was staged"
		p.logger.Error("pki enrollment file has no tenant: the token carries no tenant_uuid claim " +
			"and no tenantUUID was staged; removing")
		p.removeStagedFile(path)
		return PKIEnrollmentOutcome{Status: PKIEnrollmentFailed, Reason: reason}
	}
	commonName, ok := p.commonName()
	if !ok {
		// Left in place on purpose: this is the one failure a later retry can
		// fix without a new token, because the cloud enrolment that supplies
		// the org and asset pair may simply not have happened yet.
		p.logger.Warn("pki enrollment deferred: the device is not enrolled with Wendy Cloud yet, " +
			"so the certificate Common Name cannot be built; the staged token is kept")
		return PKIEnrollmentOutcome{
			Status: PKIEnrollmentDeferred,
			Reason: "the device is not enrolled with Wendy Cloud yet, so the certificate Common Name " +
				"cannot be built; the token stays staged and is retried on the next agent start",
		}
	}

	frontendURL := pkienroll.CSRFrontendURL(staged.CSREndpoint, staged.Environment)

	key, err := p.store.LoadOrGenerateKey()
	if err != nil {
		p.logger.Error("Failed to prepare pki identity key", zap.Error(err))
		p.removeStagedFile(path)
		return PKIEnrollmentOutcome{Status: PKIEnrollmentFailed, Reason: fmt.Sprintf("preparing the pki identity key: %v", err)}
	}
	defer zeroBytes(key)

	outcome := PKIEnrollmentOutcome{Status: PKIEnrollmentFailed}
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		var result pkienroll.Result
		result, lastErr = p.enroll(ctx, pkienroll.EnrollRequest{
			CSRFrontendURL:  frontendURL,
			TenantUUID:      tenantUUID,
			EnrollmentToken: staged.Token,
			Key:             key,
			CommonName:      commonName,
			DeviceID:        staged.DeviceID,
			HTTPClient:      p.httpClient,
		})
		if lastErr == nil {
			if err := p.store.Save(result); err != nil {
				lastErr = fmt.Errorf("storing pki identity: %w", err)
				break
			}
			if err := p.store.SaveMetadata(pkienroll.Metadata{
				TenantUUID:  tenantUUID,
				CSREndpoint: frontendURL,
				DeviceName:  result.DeviceName,
			}); err != nil {
				// The identity is usable; only renewal loses its bearings, so
				// this is reported and not treated as an enrolment failure.
				p.logger.Warn("pki identity stored but its metadata could not be written; "+
					"renewal will not start until this is fixed", zap.Error(err))
				outcome.Reason = "the identity is stored but its metadata could not be written, so renewal will not start"
			}
			p.logger.Info("Enrolled pki-core device identity",
				zap.String("spiffe_uri", result.SPIFFEURI),
				zap.String("device_name", result.DeviceName),
				zap.Time("not_after", result.NotAfter),
				zap.String("leaf", p.store.LeafPath()))
			outcome.Status = PKIEnrollmentEnrolled
			outcome.SPIFFEURI = result.SPIFFEURI
			outcome.DeviceName = result.DeviceName
			outcome.NotAfter = result.NotAfter
			break
		}

		// A refused credential will be refused identically on a retry: the
		// token is single-use and consumed atomically with issuance, so a 401
		// means it is wrong, already spent or revoked. Retrying only delays
		// the message that a fresh token is needed.
		var statusErr *pkienroll.StatusError
		if errors.As(lastErr, &statusErr) && statusErr.Unauthorized() {
			outcome.Status = PKIEnrollmentRefused
			break
		}
		var identityErr *pkienroll.IdentityError
		if errors.As(lastErr, &identityErr) {
			outcome.Status = PKIEnrollmentRefused
			break
		}
		p.logger.Warn("pki enrollment attempt failed", zap.Int("attempt", attempt), zap.Error(lastErr))
		if attempt < 3 {
			select {
			case <-ctx.Done():
				lastErr = ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}
	if lastErr != nil {
		p.logger.Error("pki-core enrollment from staged token failed; "+
			"a fresh enrollment token has to be minted and staged again",
			zap.String("tenant", tenantUUID), zap.String("frontend", frontendURL), zap.Error(lastErr))
		outcome.Reason = lastErr.Error()
	}
	p.removeStagedFile(path)
	return outcome
}

// StageAndApply writes the credential file and redeems it in one step, which is
// what a caller with no shell on the device needs. The file is still written
// first, and still deleted by ApplyStagedFile, so a crash between the two
// leaves exactly the state the installer path leaves: a staged token the next
// agent start redeems.
//
// ctx bounds the enrolment round trips only. Starting the renewal loop is the
// caller's job, with the agent's lifetime context - a loop bounded by an RPC
// context would be cancelled the moment the response was written.
func (p *PKIEnrollment) StageAndApply(ctx context.Context, staged pkienroll.StagedEnrollment) (string, PKIEnrollmentOutcome, error) {
	path, err := pkienroll.Stage(p.configPath, staged)
	if err != nil {
		return "", PKIEnrollmentOutcome{}, err
	}
	return path, p.ApplyStagedFile(ctx), nil
}

// EnsureRenewer starts the renewal loop if there is an identity to renew and no
// loop already running. It is what makes a remote enrolment complete without a
// restart: the startup path builds its renewer before any certificate exists,
// and a device enrolled minutes later would otherwise hold a leaf nothing
// renews until the next reboot - on a 30-day leaf, a fuse.
//
// ctx bounds the loop; it is the agent's lifetime context, not the RPC's.
func (p *PKIEnrollment) EnsureRenewer(ctx context.Context) {
	p.renewMu.Lock()
	defer p.renewMu.Unlock()
	if p.renewerRunning {
		return
	}
	renewer := p.Renewer()
	if renewer == nil {
		return
	}
	p.renewerRunning = true
	// Said out loud because the loop is otherwise entirely silent until it
	// renews - it sleeps to roughly two thirds of a thirty-day leaf. Without
	// this line there is no way to tell a running renewer from one that never
	// started, which is the difference between a device that keeps its
	// identity and one whose certificate quietly expires.
	p.logger.Info("pki identity renewal started", zap.String("leaf", p.store.LeafPath()))
	go func() {
		defer func() {
			p.renewMu.Lock()
			p.renewerRunning = false
			p.renewMu.Unlock()
		}()
		renewer.Run(ctx)
	}()
}

// Renewer builds the renewal loop for the stored identity, or nil when there is
// nothing to renew. nil is an ordinary answer: a device with no pki identity
// has no renewal to run.
func (p *PKIEnrollment) Renewer() *pkienroll.Renewer {
	meta, err := p.store.LoadMetadata()
	if err != nil {
		p.logger.Warn("Failed to read pki identity metadata; renewal is not started", zap.Error(err))
		return nil
	}
	if meta.TenantUUID == "" || meta.CSREndpoint == "" {
		return nil
	}
	return pkienroll.NewRenewer(p.logger, p.store, meta.CSREndpoint, meta.TenantUUID)
}

// resolveTenant prefers the token's own tenant_uuid claim over the staged
// field, so an operator who staged the wrong tenant alongside a Wendy-minted
// token cannot send the enrolment to a tenant the token was not for. The
// staged field is the fallback, and the only source for pki-core's own opaque
// tokens.
func (p *PKIEnrollment) resolveTenant(staged pkienroll.StagedEnrollment) (string, bool) {
	if claimed, ok := enrolltoken.TenantUUIDFromToken(staged.Token); ok {
		if staged.TenantUUID != "" && staged.TenantUUID != claimed {
			p.logger.Warn("staged tenantUUID disagrees with the token's tenant_uuid claim; using the claim",
				zap.String("staged", staged.TenantUUID), zap.String("claim", claimed))
		}
		return claimed, true
	}
	if staged.TenantUUID != "" {
		return staged.TenantUUID, true
	}
	return "", false
}

// commonName is the "sh/wendy/<org>/<asset>" subject the agent already builds
// for its CAS certificate.
func (p *PKIEnrollment) commonName() (string, bool) {
	if p.provisioningSvc == nil {
		return "", false
	}
	_, orgID, assetID, enrolled := p.provisioningSvc.ProvisioningInfo()
	if !enrolled || orgID == 0 || assetID == 0 {
		return "", false
	}
	return fmt.Sprintf("sh/wendy/%d/%d", orgID, assetID), true
}

func (p *PKIEnrollment) removeStagedFile(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		p.logger.Warn("Failed to remove pki enrollment file", zap.String("path", path), zap.Error(err))
	}
}
