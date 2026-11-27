// Package operator resolves an explicitly selected existing Wendy operator
// credential. It has no defaults, migration, credential writes or renewal.
package operator

import (
	"bytes"
	"context"
	"crypto/mldsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/world/production/internal/upstreamcerts"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const keychainService = "wendy-credentials"

var keyAccount = regexp.MustCompile(`^key-[0-9a-f]{16}$`)

type Selection struct{ ConfigPath, Context, Tenant, Principal string }
type Metadata struct {
	Context    string `json:"context"`
	Tenant     string `json:"tenant"`
	Principal  string `json:"principal"`
	KeyStorage string `json:"key_storage"`
	Status     string `json:"status"`
}

// Keychain is read-only custody. It must honor cancellation and return bounded
// PEM bytes. No login, unlock, migration, writes or cross-invocation cache.
type Keychain interface {
	Get(context.Context, string) ([]byte, error)
}
type selected struct {
	selection    Selection
	metadata     Metadata
	chain        [][]byte
	keyField     string
	configDigest [32]byte
}

// Credential deliberately has no key-export or generic signing method.
type Credential struct {
	selected *selected
	key      *mldsa.PrivateKey
	identity wendycert.Identity
}

func (c *Credential) Metadata() Metadata {
	if c == nil {
		return Metadata{}
	}
	return c.selected.metadata
}

// Describe returns metadata without keychain access or a present-time trust claim.
func Describe(s Selection) (Metadata, error) {
	chosen, err := selectCredential(s)
	if err != nil {
		return Metadata{}, err
	}
	return chosen.metadata, nil
}
func validText(s string, max int) bool {
	if len(s) == 0 || len(s) > max || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func validateSelection(s Selection) error {
	tenant, err := uuid.Parse(s.Tenant)
	if err != nil || tenant.String() != s.Tenant || s.ConfigPath == "" || !validText(s.Context, 128) || !canonicalOperator(s.Principal, s.Tenant) {
		return errors.New("explicit config, named context, canonical tenant and operator principal required")
	}
	return nil
}
func canonicalOperator(p, tenant string) bool {
	if !validText(p, 1024) {
		return false
	}
	id, err := upstreamcerts.ParsePrincipal(p)
	if err != nil || id.TenantUUID != tenant || id.EntityType != upstreamcerts.EntityUser || p != "spiffe://wendy.sh/tenant/"+tenant+"/operator/"+id.EntityID {
		return false
	}
	u, err := url.Parse(p)
	return err == nil && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.RawPath == "" && u.String() == p
}
func stringField(m map[string]json.RawMessage, key string) (string, error) {
	raw, ok := m[key]
	if !ok {
		return "", nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", errors.New("invalid credential field type")
	}
	return value, nil
}
func selectCredential(s Selection) (*selected, error) {
	if err := validateSelection(s); err != nil {
		return nil, err
	}
	raw, err := readRegular(s.ConfigPath, MaxConfigBytes, true)
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	cfg, err := decodeConfigJSON(raw)
	if err != nil {
		return nil, err
	}
	var contexts []map[string]json.RawMessage
	if json.Unmarshal(cfg["auth"], &contexts) != nil || len(contexts) == 0 || len(contexts) > 32 {
		return nil, errors.New("named contexts unavailable or over bound")
	}
	var chosen map[string]json.RawMessage
	matches := 0
	for _, candidate := range contexts {
		name, err := stringField(candidate, "name")
		if err != nil {
			return nil, err
		}
		if name == s.Context {
			matches++
			chosen = candidate
		}
	}
	if matches != 1 {
		return nil, errors.New("explicit context absent or ambiguous; no defaults or migration")
	}
	var records []map[string]json.RawMessage
	if json.Unmarshal(chosen["certificates"], &records) != nil || len(records) == 0 || len(records) > 32 {
		return nil, errors.New("context certificate list unavailable or over bound")
	}
	var result *selected
	for _, record := range records {
		leafPEM, err := stringField(record, "pemCertificate")
		if err != nil {
			return nil, err
		}
		chainPEM, err := stringField(record, "pemCertificateChain")
		if err != nil {
			return nil, err
		}
		declared, err := stringField(record, "principalUri")
		if err != nil {
			return nil, err
		}
		chain, err := presentationChain(leafPEM, chainPEM)
		if err != nil {
			return nil, errors.New("selected context contains invalid certificate bundle")
		}
		leaf, err := x509.ParseCertificate(chain[0])
		if err != nil {
			return nil, errors.New("invalid operator leaf")
		}
		id, found, err := upstreamcerts.IdentityFromCert(leaf)
		if err != nil || !found {
			return nil, errors.New("context certificate identity absent or ambiguous")
		}
		if declared != "" && declared != id.Principal {
			return nil, errors.New("certificate metadata disagrees with identity")
		}
		if id.Principal != s.Principal || id.TenantUUID != s.Tenant {
			continue
		}
		if !canonicalOperator(id.Principal, id.TenantUUID) {
			return nil, errors.New("selected certificate is not a canonical operator")
		}
		key, ok := leaf.PublicKey.(*mldsa.PublicKey)
		if !ok || key.Parameters() != mldsa.MLDSA65() {
			return nil, errors.New("selected operator must use ML-DSA-65")
		}
		field, err := stringField(record, "pemPrivateKey")
		if err != nil {
			return nil, err
		}
		kind, err := storageKind(field)
		if err != nil {
			return nil, err
		}
		if result != nil {
			return nil, errors.New("multiple certificates match operator; automatic rotation selection refused")
		}
		result = &selected{selection: s, metadata: Metadata{Context: s.Context, Tenant: s.Tenant, Principal: s.Principal, KeyStorage: kind, Status: "certificate-metadata-only; key access not checked"}, chain: chain, keyField: field, configDigest: sha256.Sum256(raw)}
	}
	if result == nil {
		return nil, errors.New("requested operator certificate absent from explicit context")
	}
	return result, nil
}
func storageKind(field string) (string, error) {
	if strings.HasPrefix(field, "keychain:") {
		account, ok := strings.CutPrefix(field, "keychain:v1:")
		if !ok || !keyAccount.MatchString(account) {
			return "", errors.New("unsupported or non-key credential reference")
		}
		return "macos-keychain", nil
	}
	if len(field) == 0 || len(field) > MaxKeyPEMBytes {
		return "", errors.New("operator key absent or over bound")
	}
	block, rest := pem.Decode([]byte(field))
	if block == nil {
		return "", errors.New("operator key must be PKCS8 or supported keychain reference")
	}
	defer clear(block.Bytes)
	if !bytes.HasPrefix(bytes.TrimSpace([]byte(field)), []byte("-----BEGIN "+block.Type+"-----")) || block.Type != "PRIVATE KEY" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return "", errors.New("one unencrypted PKCS8 key required")
	}
	return "inline-pkcs8", nil
}

// Preserve Wendy's outer-ASN.1 normalization, while refusing skipped blocks.
func parsePEM(data []byte, max int) ([]*x509.Certificate, error) {
	rest := bytes.TrimSpace(data)
	out := []*x509.Certificate{}
	for len(rest) > 0 {
		if !bytes.HasPrefix(rest, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, errors.New("unexpected certificate bundle data")
		}
		block, next := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, errors.New("invalid certificate PEM")
		}
		one, err := upstreamcerts.ParseCertsFromPEM(pem.EncodeToMemory(block))
		if err != nil || len(one) != 1 {
			return nil, errors.New("invalid certificate DER")
		}
		out = append(out, one[0])
		if len(out) > max {
			return nil, errors.New("certificate count exceeds bound")
		}
		rest = bytes.TrimSpace(next)
	}
	if len(out) == 0 {
		return nil, errors.New("empty certificate bundle")
	}
	return out, nil
}

// Adapted from Wendy TLSKeyPair: preserve issuer order, deduplicate identical
// certificates and omit proven self-signed CAs from the presented chain.
func presentationChain(leafPEM, chainPEM string) ([][]byte, error) {
	if len(leafPEM)+len(chainPEM) > 128<<10 {
		return nil, errors.New("certificate PEM exceeds bound")
	}
	all, err := parsePEM([]byte(leafPEM), 16)
	if err != nil {
		return nil, err
	}
	if chainPEM != "" {
		additional, err := parsePEM([]byte(chainPEM), 16)
		if err != nil {
			return nil, err
		}
		all = append(all, additional...)
	}
	seen := map[string]bool{}
	chain := [][]byte{}
	total := 0
	for i, c := range all {
		if seen[string(c.Raw)] {
			continue
		}
		seen[string(c.Raw)] = true
		if i != 0 && c.IsCA && bytes.Equal(c.RawSubject, c.RawIssuer) && c.CheckSignatureFrom(c) == nil {
			continue
		}
		total += len(c.Raw)
		if len(chain) >= 8 || total > 64<<10 {
			return nil, errors.New("presented chain exceeds World bound")
		}
		chain = append(chain, append([]byte(nil), c.Raw...))
	}
	return chain, nil
}
func unchanged(s *selected) error {
	raw, err := readRegular(s.selection.ConfigPath, MaxConfigBytes, true)
	if err != nil {
		return err
	}
	defer clear(raw)
	if sha256.Sum256(raw) != s.configDigest {
		return errors.New("credential configuration changed during signing; retry explicit selection")
	}
	return nil
}

// Resolve verifies certificate trust before custody access. It loads a fresh
// key, checks the leaf match and detects config replacement, without exporting it.
func Resolve(ctx context.Context, s Selection, custody Keychain, certificates *wendycert.Verifier, window trustedtime.Interval) (*Credential, error) {
	if ctx == nil || ctx.Err() != nil || certificates == nil || window.Validate() != nil {
		return nil, errors.New("explicit trust, context and authenticated physical time required")
	}
	chosen, err := selectCredential(s)
	if err != nil {
		return nil, err
	}
	identity, err := certificates.Verify(chosen.chain, window.Earliest, window.Latest)
	if err != nil || identity.Principal != s.Principal || identity.Tenant != s.Tenant {
		return nil, errors.New("selected operator failed trust or validity")
	}
	var secret []byte
	defer func() { clear(secret) }()
	if chosen.metadata.KeyStorage == "macos-keychain" {
		if custody == nil {
			return nil, errors.New("keychain custody unavailable")
		}
		secret, err = custody.Get(ctx, strings.TrimPrefix(chosen.keyField, "keychain:v1:"))
		if err != nil {
			return nil, errors.New("keychain key unavailable; no unlock or renewal attempted")
		}
	} else {
		secret = []byte(chosen.keyField)
	}
	if ctx.Err() != nil || len(secret) == 0 || len(secret) > MaxKeyPEMBytes {
		return nil, errors.New("operator key unavailable or over bound")
	}
	block, rest := pem.Decode(secret)
	if block == nil || !bytes.HasPrefix(bytes.TrimSpace(secret), []byte("-----BEGIN "+block.Type+"-----")) || block.Type != "PRIVATE KEY" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("one unencrypted PKCS8 key required")
	}
	defer clear(block.Bytes)
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("operator PKCS8 key cannot be parsed")
	}
	key, ok := parsed.(*mldsa.PrivateKey)
	if !ok || key.PublicKey().Parameters() != mldsa.MLDSA65() {
		return nil, errors.New("operator requires ML-DSA-65; algorithm fallback refused")
	}
	leaf, _ := x509.ParseCertificate(chosen.chain[0])
	public, err := x509.MarshalPKIXPublicKey(key.Public())
	leafPublic, leafErr := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil || leafErr != nil || !bytes.Equal(public, leafPublic) {
		return nil, errors.New("resolved key does not match selected certificate")
	}
	if err = unchanged(chosen); err != nil {
		return nil, err
	}
	chosen.keyField = ""
	return &Credential{selected: chosen, key: key, identity: identity}, nil
}
