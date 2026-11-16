package builddelivery

import (
	"crypto/x509"
	"errors"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// VerifyBuildBinding checks the signed build assignment, including an operator
// credential that may have expired since target admission. This is NOT proof
// of timely authorization: only the target's durable admission establishes
// that, and delivery must present that exact session to the target.
func VerifyBuildBinding(compact, session, input string, roots []*x509.Certificate, target meshid.Device, builder *x509.Certificate, now time.Time) (Claims, error) {
	var empty Claims
	if len(compact) > MaxContractBytes || session != sessionID(compact) {
		return empty, errors.New("delivery session does not name the signed contract")
	}
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		return empty, errors.New("invalid delivery contract")
	}
	payload, err := decode(parts[1])
	if err != nil {
		return empty, err
	}
	var c Claims
	if err = strictJSON(payload, &c); err != nil {
		return empty, err
	}
	verified, err := Admit(compact, roots, target, time.Unix(c.IssuedAt, 0))
	if err != nil {
		return empty, err
	}
	c = verified.Claims()
	if now.Unix() < c.IssuedAt || now.Unix() >= c.ValidUntil || input != c.InputSHA256 || builder == nil {
		return empty, errors.New("expired delivery or changed build inputs")
	}
	id, found, err := certs.IdentityFromCert(builder)
	if err != nil || !found || id.Principal != c.Builder || Fingerprint(builder) != c.BuilderSPKI {
		return empty, errors.New("delivery is assigned to another builder identity or key")
	}
	return c, nil
}
