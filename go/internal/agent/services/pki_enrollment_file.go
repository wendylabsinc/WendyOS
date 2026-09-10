package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
// once redeemed. Choosing it over a new provisioning RPC also keeps the change
// free of a proto addition, so nothing regenerates and no cloud surface grows.
//
// The two files never overlap: enrollment.json carries a Wendy Cloud asset
// token and produces the Certificate Authority Service (CAS) triple, this one
// carries a pki-core enrollment token and produces the pki-core triple.
const PKIEnrollmentFileName = pkienroll.StagedFileName

// stagedPKIEnrollment is the on-disk shape. Only token is always required:
// tenantUUID may instead come from the token's own tenant_uuid claim, and
// csrEndpoint may instead be derived from the environment.
type stagedPKIEnrollment struct {
	// Token is the pki-core enrollment token an operator minted through
	// pki-core's fabric relay. The agent only consumes it.
	Token string `json:"token"`
	// TenantUUID is the pki-core tenant. Optional when the token is a Wendy
	// JWT carrying tenant_uuid; required when it is one of pki-core's own
	// opaque tokens, which carry no claims at all.
	TenantUUID string `json:"tenantUUID"`
	// DeviceID must equal the device_id the token was minted with, byte for
	// byte. Empty means "use the CSR Common Name", which is the pairing
	// pki-core's own token minting follows.
	DeviceID string `json:"deviceID"`
	// CSREndpoint overrides the derived frontend URL.
	CSREndpoint string `json:"csrEndpoint"`
	// Environment is "dev" or "prod" and selects the derived frontend host
	// when CSREndpoint is empty.
	Environment string `json:"environment"`
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
func (p *PKIEnrollment) ApplyStagedFile(ctx context.Context) {
	path := p.StagedFilePath()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		p.logger.Error("Failed to read pki enrollment file", zap.String("path", path), zap.Error(err))
		p.removeStagedFile(path)
		return
	}

	if p.store.Has() {
		// Renewal, not re-enrolment, keeps an existing identity alive. Burning
		// a fresh single-use token to replace a working certificate would also
		// discard the renewal lineage pki-core tracks against it.
		p.logger.Info("pki identity already present; discarding staged pki enrollment token",
			zap.String("leaf", p.store.LeafPath()))
		p.removeStagedFile(path)
		return
	}

	var staged stagedPKIEnrollment
	if err := json.Unmarshal(data, &staged); err != nil {
		p.logger.Error("Failed to parse pki enrollment file, removing", zap.Error(err))
		p.removeStagedFile(path)
		return
	}
	if staged.Token == "" {
		p.logger.Error("pki enrollment file carries no token, removing")
		p.removeStagedFile(path)
		return
	}

	tenantUUID, ok := p.resolveTenant(staged)
	if !ok {
		p.logger.Error("pki enrollment file has no tenant: the token carries no tenant_uuid claim " +
			"and no tenantUUID was staged; removing")
		p.removeStagedFile(path)
		return
	}
	commonName, ok := p.commonName()
	if !ok {
		// Left in place on purpose: this is the one failure a later retry can
		// fix without a new token, because the cloud enrolment that supplies
		// the org and asset pair may simply not have happened yet.
		p.logger.Warn("pki enrollment deferred: the device is not enrolled with Wendy Cloud yet, " +
			"so the certificate Common Name cannot be built; the staged token is kept")
		return
	}

	frontendURL := pkienroll.CSRFrontendURL(staged.CSREndpoint, staged.Environment)

	key, err := p.store.LoadOrGenerateKey()
	if err != nil {
		p.logger.Error("Failed to prepare pki identity key", zap.Error(err))
		p.removeStagedFile(path)
		return
	}
	defer zeroBytes(key)

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
			}
			p.logger.Info("Enrolled pki-core device identity",
				zap.String("spiffe_uri", result.SPIFFEURI),
				zap.String("device_name", result.DeviceName),
				zap.Time("not_after", result.NotAfter),
				zap.String("leaf", p.store.LeafPath()))
			break
		}

		// A refused credential will be refused identically on a retry: the
		// token is single-use and consumed atomically with issuance, so a 401
		// means it is wrong, already spent or revoked. Retrying only delays
		// the message that a fresh token is needed.
		var statusErr *pkienroll.StatusError
		if errors.As(lastErr, &statusErr) && statusErr.Unauthorized() {
			break
		}
		var identityErr *pkienroll.IdentityError
		if errors.As(lastErr, &identityErr) {
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
	}
	p.removeStagedFile(path)
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
func (p *PKIEnrollment) resolveTenant(staged stagedPKIEnrollment) (string, bool) {
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
