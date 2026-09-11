package pkienroll

import (
	"context"
	"errors"
	"math/rand"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

// Renewer keeps the stored pki-core device identity fresh.
//
// It is the agent's FIRST certificate renewal loop of any kind. The CAS leaf
// has never been renewed by the device either, so this does not replace a
// working mechanism — but a device-tier pki-core leaf lasts 30 days where the
// CAS leaf lasts 365, and the same silence that was survivable for a year is
// not survivable for a month.
type Renewer struct {
	logger *zap.Logger
	store  *Store

	// frontendURL and tenantUUID are resolved once, when the identity is
	// enrolled, and persisted alongside it. Renewal does not re-derive them:
	// a leaf must be renewed at the tenant that issued it.
	frontendURL string
	tenantUUID  string

	// Injection points for deterministic tests. now and jitter replace the
	// clock and the random source; renew replaces the network call; sleep
	// replaces the wait so a test does not spend the interval it is asserting.
	now    func() time.Time
	jitter func() float64
	renew  func(context.Context, RenewRequest) (Result, error)
	sleep  func(context.Context, time.Duration)
}

// NewRenewer builds a renewer for the identity in store.
func NewRenewer(logger *zap.Logger, store *Store, frontendURL, tenantUUID string) *Renewer {
	return &Renewer{
		logger:      logger,
		store:       store,
		frontendURL: frontendURL,
		tenantUUID:  tenantUUID,
		now:         time.Now,
		jitter:      rand.Float64,
		renew:       Renew,
		sleep:       sleepCtx,
	}
}

// Run drives renewal until ctx is cancelled. It blocks; start it in a
// goroutine. A store holding no identity is not an error: the loop waits, so
// that enrolling a device later does not need an agent restart to start
// renewing it.
func (r *Renewer) Run(ctx context.Context) {
	if r.store == nil || r.frontendURL == "" || r.tenantUUID == "" {
		return
	}
	failures := 0
	for {
		if ctx.Err() != nil {
			return
		}

		material, err := r.store.Load()
		if err != nil {
			// No identity yet. Poll rather than exit: the staged credential
			// file may arrive after the agent came up.
			r.sleep(ctx, pollFloor)
			continue
		}
		notBefore, notAfter, err := leafValidity(material.LeafPEM)
		zeroBytes(material.KeyData)
		if err != nil {
			r.logger.Error("pki identity renewal: stored leaf is unreadable; re-enrolment is required",
				zap.String("leaf", r.store.LeafPath()), zap.Error(err))
			r.sleep(ctx, backoffMax)
			continue
		}

		decision := NextRenewal(notBefore, notAfter, r.now(), failures, r.jitter())
		if decision.Critical {
			// Loud, and it says what to do. A WARN that only reports a failed
			// attempt would be indistinguishable from the transient failures
			// the backoff exists to absorb.
			r.logger.Error("pki identity renewal keeps failing and the certificate is about to expire; "+
				"episode uploads to the data platform will stop until the device is re-enrolled",
				zap.Time("not_after", notAfter),
				zap.Duration("remaining", notAfter.Sub(r.now())),
				zap.Int("consecutive_failures", failures),
				zap.Bool("already_expired", decision.Expired))
		}
		if !decision.Attempt {
			r.sleep(ctx, decision.Wait)
			continue
		}

		if err := r.renewOnce(ctx); err != nil {
			failures++
			r.logger.Warn("pki identity renewal attempt failed",
				zap.Int("consecutive_failures", failures), zap.Error(err))
			r.sleep(ctx, backoffFor(failures))
			continue
		}
		failures = 0
	}
}

// renewOnce performs a single renewal and persists the result.
//
// The store is written only after the new leaf has been parsed and its SPIFFE
// SAN verified, so a refusal or a surprise identity leaves the still-valid
// current certificate in place. That ordering is the reason a failed renewal
// costs nothing but a retry.
func (r *Renewer) renewOnce(ctx context.Context) error {
	material, err := r.store.Load()
	if err != nil {
		return err
	}
	defer zeroBytes(material.KeyData)

	result, err := r.renew(ctx, RenewRequest{
		CSRFrontendURL:  r.frontendURL,
		TenantUUID:      r.tenantUUID,
		CurrentLeafPEM:  material.LeafPEM,
		CurrentChainPEM: material.ChainPEM,
		Key:             material.KeyData,
	})
	if err != nil {
		var identityErr *IdentityError
		if errors.As(err, &identityErr) {
			// A renewed leaf with the wrong principal is not a transport
			// problem and retrying will reproduce it exactly. Say so at error
			// level; the backoff still applies, because stopping would remove
			// the only signal.
			r.logger.Error("pki identity renewal returned an unexpected identity; not stored",
				zap.Error(identityErr))
		}
		return err
	}
	if err := r.store.Save(result); err != nil {
		return err
	}
	r.logger.Info("pki identity renewed",
		zap.String("spiffe_uri", result.SPIFFEURI),
		zap.String("device_name", result.DeviceName),
		zap.Time("not_after", result.NotAfter))
	return nil
}

// leafValidity reads the stored leaf's window through the same ML-DSA-aware
// parser the handshake uses, so the schedule is computed from the certificate
// that will actually be presented.
func leafValidity(leafPEM string) (notBefore, notAfter time.Time, err error) {
	normalized, err := certs.LeafCertificatePEM(leafPEM)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	parsed, err := certs.ParseCertsFromPEM([]byte(normalized))
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if len(parsed) == 0 {
		return time.Time{}, time.Time{}, errors.New("stored leaf PEM carried no certificate")
	}
	return parsed[0].NotBefore, parsed[0].NotAfter, nil
}

// sleepCtx waits for d, returning early when ctx is done so shutdown is not
// held up by a renewal interval measured in days.
func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		d = pollFloor
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// zeroBytes overwrites b in place as best-effort key-material hygiene, matching
// what the cloud dialers already do with the CAS key.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
