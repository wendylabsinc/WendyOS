package pkienroll

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
)

// StagedEnrollment is the on-disk shape of StagedFileName. Only Token is always
// required: TenantUUID may instead come from the token's own tenant_uuid claim,
// and CSREndpoint may instead be derived from Environment.
//
// It lives in this package, not in the agent's services package, because three
// callers need the same shape and one of them is the wendy command line
// interface, which is cross-compiled for Windows where the services package
// does not build. The three are: the command line interface writing the file on
// the device (the installer path), the agent's StagePKIEnrollment handler
// writing it on behalf of a remote caller, and the agent reading it back.
type StagedEnrollment struct {
	// Token is the pki-core enrollment token an operator minted through
	// pki-core's fabric relay. The agent only consumes it.
	Token string `json:"token"`
	// TenantUUID is the pki-core tenant. Optional when the token is a Wendy
	// JavaScript Object Notation Web Token (JWT) carrying tenant_uuid;
	// required when it is one of pki-core's own opaque tokens, which carry no
	// claims at all.
	TenantUUID string `json:"tenantUUID,omitempty"`
	// DeviceID must equal the device_id the token was minted with, byte for
	// byte. Empty means "use the certificate signing request Common Name",
	// which is the pairing pki-core's own token minting follows.
	DeviceID string `json:"deviceID,omitempty"`
	// CSREndpoint overrides the derived frontend URL.
	CSREndpoint string `json:"csrEndpoint,omitempty"`
	// Environment is "dev" or "prod" and selects the derived frontend host
	// when CSREndpoint is empty.
	Environment string `json:"environment,omitempty"`
}

// ErrNoToken is returned by Stage when the credential is empty. Staging a
// tokenless file would only produce a file the agent deletes unread.
var ErrNoToken = errors.New("pki enrollment: no token")

// ErrMalformedToken is returned when the credential cannot be carried in an
// HTTP Authorization header.
var ErrMalformedToken = errors.New("pki enrollment: malformed token")

// ValidateToken rejects a credential the enrolment request could not carry.
//
// WHY THIS IS NOT PEDANTRY. The token rides in "Authorization: Bearer <token>",
// and Go's net/http refuses to send a header value containing a newline or any
// other control character - with "invalid header field value", raised in the
// client before a single byte reaches pki-core. That failure is not a status
// code, so it is not a 401, so the agent's retry ladder treats it as transient
// and tries three times over ten seconds before reporting that the token was
// refused and a fresh one must be minted. Every part of that is wrong: nothing
// was sent, nothing was spent, and the fix is to pass the token correctly. It
// is an easy mistake to make, because the credential is normally pasted out of
// a file that also carries a heading and metadata lines.
func ValidateToken(token string) error {
	if strings.TrimSpace(token) == "" {
		return ErrNoToken
	}
	for _, r := range token {
		// The set an HTTP field value admits: visible ASCII, plus space and
		// horizontal tab. Anything else - a newline above all - is the paste
		// accident this guards.
		if r == ' ' || r == '\t' {
			continue
		}
		if r < 0x21 || r > 0x7e {
			return fmt.Errorf("%w: it carries a character that cannot be sent in an "+
				"Authorization header (a newline, a control character, or a non-ASCII byte); "+
				"pass the token value alone, without any surrounding text", ErrMalformedToken)
		}
	}
	return nil
}

// Stage writes the credential file into configPath and returns its path.
//
// 0600 and atomic: it holds a single-use bearer credential, and a half-written
// one would be redeemed and burned for nothing.
func Stage(configPath string, staged StagedEnrollment) (string, error) {
	if err := ValidateToken(staged.Token); err != nil {
		return "", err
	}
	if err := os.MkdirAll(configPath, 0o700); err != nil {
		return "", fmt.Errorf("creating agent config directory %s: %w", configPath, err)
	}
	data, err := json.MarshalIndent(staged, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encoding pki enrollment file: %w", err)
	}
	path := filepath.Join(configPath, StagedFileName)
	if err := atomicfile.Write(path, append(data, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return path, nil
}
