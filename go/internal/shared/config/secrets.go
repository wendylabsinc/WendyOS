package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/shared/secretstore"
)

// credentialService names the Keychain items holding wendy credentials —
// distinct from tlscache's "wendy-tls-session" (tickets are droppable,
// credentials are not, so the two keep separate policy).
const credentialService = "wendy-credentials"

// Secret fields in config.json hold either a real value or a reference of
// this form. Values never collide with references: private keys are PEM
// ("-----BEGIN...") and API tokens are opaque strings that do not start
// with "keychain:".
const (
	refPrefix   = "keychain:"
	refPrefixV1 = "keychain:v1:"
)

// secretStoreIface aliases the store interface so tests can swap fakes
// without importing secretstore.
type secretStoreIface = secretstore.Store

// newCredentialStore returns the platform credential store (nil = no
// backend, secrets stay inline). Package variable so tests inject fakes.
var newCredentialStore = defaultCredentialStore

var (
	secretMu    sync.Mutex
	secretCache = map[string]string{} // ref → resolved secret, per process
)

func resetSecretCacheForTest() {
	secretMu.Lock()
	defer secretMu.Unlock()
	secretCache = map[string]string{}
}

func cacheSecret(ref, value string) {
	secretMu.Lock()
	defer secretMu.Unlock()
	secretCache[ref] = value
}

func isRef(v string) bool { return strings.HasPrefix(v, refPrefix) }

// resolveSecret turns a keychain reference into the stored secret,
// memoized per process so repeated config loads cost one security(1)
// invocation per secret.
func resolveSecret(ref string) (string, error) {
	secretMu.Lock()
	if v, ok := secretCache[ref]; ok {
		secretMu.Unlock()
		return v, nil
	}
	secretMu.Unlock()

	account, ok := strings.CutPrefix(ref, refPrefixV1)
	if !ok {
		return "", resolveError(fmt.Errorf("unrecognized credential reference %q (written by a newer wendy?)", ref))
	}
	store := newCredentialStore()
	if store == nil {
		return "", resolveError(fmt.Errorf("no credential store on this platform (config migrated on macOS?)"))
	}
	secret := store.Get(account)
	if secret == nil {
		return "", resolveError(fmt.Errorf("keychain item %s/%s not readable", credentialService, account))
	}
	cacheSecret(ref, string(secret))
	return string(secret), nil
}

func resolveError(cause error) error {
	return fmt.Errorf("credential is stored in the macOS Keychain but could not be read (keychain locked?): %w\n"+
		"Unlock with 'security unlock-keychain', or re-run 'wendy auth login' with WENDY_SECRET_STORE=file to keep credentials in config.json.", cause)
}

// keyAccount derives the deterministic Keychain account for a client
// private key, so re-login for the same identity overwrites one item. It
// hashes cloudGRPC|orgID|userID|assetID|principalURI. userID alone is not
// enough because asset certs (from performLocalLogin) carry no UserID, only an
// AssetID, so two asset certs on the same endpoint+org with different AssetID
// must not collide. principalURI distinguishes newer pki-core SPIFFE
// identities, which do not carry the legacy numeric org ID.
func keyAccount(cloudGRPC string, orgID int, userID string, assetID int, principalURI string) string {
	identity := cloudGRPC + "|" + strconv.Itoa(orgID) + "|" + userID + "|" + strconv.Itoa(assetID)
	// Preserve the established account derivation for legacy certificates;
	// append the new identity component only when it exists.
	if principalURI != "" {
		identity += "|" + principalURI
	}
	sum := sha256.Sum256([]byte(identity))
	return "key-" + hex.EncodeToString(sum[:8])
}

// tokenAccount derives the deterministic Keychain account for a cloud API
// token. It hashes cloudDashboard|cloudGRPC|orgID — the same triple AddAuth
// dedups auth entries on — because a browser login (CloudDashboard set) and
// an --api-key login (CloudDashboard empty, see performLocalLogin) against
// the same endpoint+org coexist as two distinct AddAuth entries; omitting
// cloudDashboard would let one entry's dehydrated token overwrite the
// other's Keychain item even though both entries' references kept pointing
// at the same account. orgID matters for the analogous reason: several orgs
// can share one endpoint (e.g. multiple orgs on the production cloud), each
// with its own AddAuth entry.
func tokenAccount(cloudDashboard, cloudGRPC string, orgID int) string {
	sum := sha256.Sum256([]byte(cloudDashboard + "|" + cloudGRPC + "|" + strconv.Itoa(orgID)))
	return "token-" + hex.EncodeToString(sum[:8])
}

func oauthSecretAccount(kind string, a AuthConfig) string {
	sum := sha256.Sum256([]byte(kind + "|" + a.OAuthIssuer + "|" + a.OAuthClientID + "|" + a.CloudGRPC))
	return kind + "-" + hex.EncodeToString(sum[:8])
}

// HasPrivateKey reports whether key material exists — inline or by
// reference — without touching the Keychain.
func (c CertificateInfo) HasPrivateKey() bool { return c.PemPrivateKey != "" }

// PrivateKeyPEM returns the client private key, resolving a Keychain
// reference if necessary.
func (c CertificateInfo) PrivateKeyPEM() (string, error) {
	if !isRef(c.PemPrivateKey) {
		return c.PemPrivateKey, nil
	}
	return resolveSecret(c.PemPrivateKey)
}

// HasAPIKey reports whether an API token exists — inline or by reference —
// without touching the Keychain.
func (a AuthConfig) HasAPIKey() bool { return a.APIKey != "" }

// BearerToken returns the cloud API token, resolving a Keychain reference
// if necessary.
func (a AuthConfig) BearerToken() (string, error) {
	if !isRef(a.APIKey) {
		return a.APIKey, nil
	}
	return resolveSecret(a.APIKey)
}

// OAuthRefreshToken and OAuthDPoPKey resolve the long-lived OAuth session
// secrets without exposing their storage representation to callers.
func (a AuthConfig) OAuthRefreshToken() (string, error) {
	if !isRef(a.RefreshToken) {
		return a.RefreshToken, nil
	}
	return resolveSecret(a.RefreshToken)
}

func (a AuthConfig) OAuthDPoPKey() (string, error) {
	if !isRef(a.DPoPPrivateKey) {
		return a.DPoPPrivateKey, nil
	}
	return resolveSecret(a.DPoPPrivateKey)
}

// InvalidateCachedSecrets forces subsequent reads to consult the credential
// store. Use after reloading a session under the refresh lock: another process
// can rotate a secret while its stored Keychain reference remains unchanged.
// This only clears the in-process cache; it does not delete stored credentials.
func (a AuthConfig) InvalidateCachedSecrets() {
	secretMu.Lock()
	defer secretMu.Unlock()
	for _, ref := range []string{a.APIKey, a.RefreshToken, a.DPoPPrivateKey} {
		delete(secretCache, ref)
	}
	for _, cert := range a.Certificates {
		delete(secretCache, cert.PemPrivateKey)
	}
}

// dehydrateEnabled reports whether Save should move inline secrets into the
// platform store. WENDY_SECRET_STORE=file forces inline writes (and
// de-migration); everything else uses the platform default.
func dehydrateEnabled() bool {
	if os.Getenv("WENDY_SECRET_STORE") == "file" {
		return false
	}
	return secretsPlatformDefault
}

// clone deep-copies a Config via JSON round-trip so Save can rewrite secret
// fields without mutating the caller's struct. An error here must not fall
// back to returning c itself — that would silently hand Save the caller's
// live struct and violate the never-mutate contract on the very path that
// handles secrets.
func (c *Config) clone() (*Config, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var out Config
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// dehydrate pushes every inline secret into the credential store and
// replaces the field with its reference. A failed Put keeps the value
// inline — the store never holds the only copy until a write succeeded.
func dehydrate(cfg *Config) {
	store := newCredentialStore()
	if store == nil {
		return
	}
	for i := range cfg.Auth {
		a := &cfg.Auth[i]
		if a.APIKey != "" && !isRef(a.APIKey) {
			dashboardKey := a.CloudDashboard
			if a.OAuthIssuer != "" {
				dashboardKey += "|" + a.OAuthIssuer
			}
			acct := tokenAccount(dashboardKey, a.CloudGRPC, authEntryOrgID(*a))
			if store.Put(acct, []byte(a.APIKey)) == nil {
				cacheSecret(refPrefixV1+acct, a.APIKey)
				a.APIKey = refPrefixV1 + acct
			}
		}
		for value, kind := range map[string]string{
			a.RefreshToken:   "refresh",
			a.DPoPPrivateKey: "dpop-key",
		} {
			if value == "" || isRef(value) {
				continue
			}
			acct := oauthSecretAccount(kind, *a)
			if store.Put(acct, []byte(value)) == nil {
				cacheSecret(refPrefixV1+acct, value)
				if kind == "refresh" {
					a.RefreshToken = refPrefixV1 + acct
				} else {
					a.DPoPPrivateKey = refPrefixV1 + acct
				}
			}
		}
		for j := range a.Certificates {
			c := &a.Certificates[j]
			if c.PemPrivateKey != "" && !isRef(c.PemPrivateKey) {
				acct := keyAccount(a.CloudGRPC, c.OrganizationID, c.UserID, c.AssetID, c.PrincipalURI)
				if store.Put(acct, []byte(c.PemPrivateKey)) == nil {
					cacheSecret(refPrefixV1+acct, c.PemPrivateKey)
					c.PemPrivateKey = refPrefixV1 + acct
				}
			}
		}
	}
}

// inlineSecrets is the de-migration path (WENDY_SECRET_STORE=file):
// references that resolve are written back inline; unresolvable references
// are kept — never drop a secret.
func inlineSecrets(cfg *Config) {
	for i := range cfg.Auth {
		a := &cfg.Auth[i]
		if isRef(a.APIKey) {
			if v, err := resolveSecret(a.APIKey); err == nil {
				a.APIKey = v
			}
		}
		if isRef(a.RefreshToken) {
			if v, err := resolveSecret(a.RefreshToken); err == nil {
				a.RefreshToken = v
			}
		}
		if isRef(a.DPoPPrivateKey) {
			if v, err := resolveSecret(a.DPoPPrivateKey); err == nil {
				a.DPoPPrivateKey = v
			}
		}
		for j := range a.Certificates {
			c := &a.Certificates[j]
			if isRef(c.PemPrivateKey) {
				if v, err := resolveSecret(c.PemPrivateKey); err == nil {
					c.PemPrivateKey = v
				}
			}
		}
	}
}

// hasInlineSecrets reports whether any secret field holds a real value
// (as opposed to a reference or empty).
func hasInlineSecrets(cfg *Config) bool {
	return countInlineSecrets(cfg) > 0
}

// MigrateSecretsIfNeeded reconciles pre-existing credentials with the current
// storage policy. When dehydration is enabled it moves inline secrets into the
// platform store. When file storage is enabled it resolves legacy Keychain
// references back into config.json. References that cannot be resolved are
// preserved by Save, so migration never discards the only copy of a secret.
//
// Called once per invocation from the root command's synchronous pre-run zone;
// organic Saves elsewhere also apply the selected policy. Returns true when it
// changed the on-disk representation.
func MigrateSecretsIfNeeded(cfg *Config) bool {
	beforeInline := countInlineSecrets(cfg)
	beforeRefs := countSecretRefs(cfg)
	if dehydrateEnabled() {
		if beforeInline == 0 {
			return false
		}
	} else if beforeRefs == 0 {
		return false
	}
	if err := Save(cfg); err != nil {
		return false
	}
	reloaded, err := Load()
	if err != nil {
		return false
	}
	if dehydrateEnabled() {
		return countInlineSecrets(reloaded) < beforeInline
	}
	return countSecretRefs(reloaded) < beforeRefs
}

func countInlineSecrets(cfg *Config) int {
	n := 0
	for _, a := range cfg.Auth {
		if a.APIKey != "" && !isRef(a.APIKey) {
			n++
		}
		if a.RefreshToken != "" && !isRef(a.RefreshToken) {
			n++
		}
		if a.DPoPPrivateKey != "" && !isRef(a.DPoPPrivateKey) {
			n++
		}
		for _, c := range a.Certificates {
			if c.PemPrivateKey != "" && !isRef(c.PemPrivateKey) {
				n++
			}
		}
	}
	return n
}

func countSecretRefs(cfg *Config) int {
	n := 0
	for _, a := range cfg.Auth {
		if isRef(a.APIKey) {
			n++
		}
		if isRef(a.RefreshToken) {
			n++
		}
		if isRef(a.DPoPPrivateKey) {
			n++
		}
		for _, c := range a.Certificates {
			if isRef(c.PemPrivateKey) {
				n++
			}
		}
	}
	return n
}

// DeleteStoredSecrets removes every Keychain item this config references —
// called when credentials are being discarded (logout). Best-effort: an
// orphaned item is inert once nothing references it, but tidy-up is cheap.
func DeleteStoredSecrets(cfg *Config) {
	store := newCredentialStore()
	if store == nil {
		return
	}
	deleteRef := func(ref string) {
		account, ok := strings.CutPrefix(ref, refPrefixV1)
		if !ok {
			return
		}
		store.Delete(account)
		secretMu.Lock()
		delete(secretCache, ref)
		secretMu.Unlock()
	}
	for _, a := range cfg.Auth {
		if isRef(a.APIKey) {
			deleteRef(a.APIKey)
		}
		if isRef(a.RefreshToken) {
			deleteRef(a.RefreshToken)
		}
		if isRef(a.DPoPPrivateKey) {
			deleteRef(a.DPoPPrivateKey)
		}
		for _, c := range a.Certificates {
			if isRef(c.PemPrivateKey) {
				deleteRef(c.PemPrivateKey)
			}
		}
	}
}
