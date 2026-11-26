package services

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hpke"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/registrycrypto"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
)

// AppSecretOpener decrypts a previously verified assignment for one start.
// The caller owns the returned buffers and must clear them after use.
type AppSecretOpener func(*cloudpbv2.DeviceAssignment) (map[string][]byte, error)

func NewAppSecretOpener(configPath string) AppSecretOpener {
	return func(a *cloudpbv2.DeviceAssignment) (map[string][]byte, error) {
		key, err := loadDeviceEncryptionKey(configPath, a.GetDevice())
		if err != nil {
			return nil, errors.New("app secrets: device encryption key unavailable")
		}
		return openAppSecrets(a, key)
	}
}

func ClearAppSecrets(values map[string][]byte) {
	for _, value := range values {
		clear(value)
	}
}

// openAppSecrets validates all bindings against the verified assignment, not
// the server's wrap metadata. Error messages never include secret values.
func openAppSecrets(a *cloudpbv2.DeviceAssignment, key hpke.PrivateKey) (values map[string][]byte, err error) {
	values = make(map[string][]byte)
	defer func() {
		if err != nil {
			ClearAppSecrets(values)
		}
	}()
	refuse := func() error { return errors.New("app secrets: invalid value, name, size or device binding") }
	principal, parseErr := certs.ParsePrincipal(a.GetDevice())
	if parseErr != nil || principal.TenantUUID != a.GetOrganizationId() || a.GetDevice() != certs.DeviceSPIFFEURI(principal.TenantUUID, principal.EntityID) {
		return values, refuse()
	}
	if len(a.GetSecrets()) > 64 {
		return values, refuse()
	}
	env, configErr := appconfig.DeploymentConfigEnv(a.GetConfigJson())
	if configErr != nil {
		return values, refuse()
	}
	configNames := map[string]bool{}
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		configNames[name] = true
	}
	total := 0
	for _, secret := range a.GetSecrets() {
		v, w := secret.GetValue(), secret.GetDeviceWrap()
		name := v.GetName()
		_, duplicate := values[name]
		if !appconfig.IsEnvVarName(name) || appconfig.IsAgentEnvName(name) || duplicate || configNames[name] || v.GetAppId() != a.GetAppId() || v.GetVersion() < 1 || len(v.GetCiphertext()) < 16 || len(v.GetCiphertext()) > (16<<10)+16 || len(v.GetNonce()) != 12 {
			return values, refuse()
		}
		total += len(v.GetCiphertext()) - 16
		if total > 64<<10 {
			return values, refuse()
		}
		scope := a.GetAppId() + "/" + name + "/" + strconv.Itoa(int(v.GetVersion()))
		if w.GetSuite() != cloudpbv2.KemSuite_KEM_SUITE_MLKEM768_X25519 || w.GetRecipientKind() != cloudpbv2.RecipientKind_RECIPIENT_KIND_DEVICE || w.GetRecipient() != a.GetDevice() || w.GetRecipientKeyId() != registrycrypto.KeyID(key.PublicKey().Bytes()) || w.GetScope().GetSecretVersion() != scope || w.GetScope().GetKeyId() != v.GetSecretKeyId() {
			return values, refuse()
		}
		secretKey, openErr := (registrycrypto.Wrap{OrganizationID: a.GetOrganizationId(), Scope: registrycrypto.ScopeSecret, ScopeID: scope, KeyID: v.GetSecretKeyId(), RecipientKind: 1, Recipient: a.GetDevice(), RecipientKeyID: w.GetRecipientKeyId()}).Open(key, w.GetEnc(), w.GetCiphertext())
		if openErr != nil {
			return values, refuse()
		}
		block, blockErr := aes.NewCipher(secretKey)
		clear(secretKey)
		if blockErr != nil {
			return values, refuse()
		}
		gcm, gcmErr := cipher.NewGCM(block)
		if gcmErr != nil {
			return values, refuse()
		}
		aad := []byte("wendy-secret-seal-v1")
		for _, s := range []string{a.GetOrganizationId(), a.GetAppId(), name, strconv.Itoa(int(v.GetVersion())), v.GetSecretKeyId()} {
			aad = binary.BigEndian.AppendUint16(aad, uint16(len(s)))
			aad = append(aad, s...)
		}
		value, openErr := gcm.Open(nil, v.GetNonce(), v.GetCiphertext(), aad)
		if openErr != nil {
			return values, refuse()
		}
		values[name] = value
		if !utf8.Valid(value) || bytes.IndexByte(value, 0) >= 0 {
			return values, refuse()
		}
	}
	return values, nil
}

// SecretContentID identifies only sealed values, so rewrapping an equal
// revision does not restart the app. It never hashes plaintext.
func SecretContentID(a *cloudpbv2.DeviceAssignment) string {
	if len(a.GetSecrets()) == 0 {
		return ""
	}
	values := append([]*cloudpbv2.DeviceSecret(nil), a.GetSecrets()...)
	sort.Slice(values, func(i, j int) bool { return values[i].GetValue().GetName() < values[j].GetValue().GetName() })
	h := sha256.New()
	for _, s := range values {
		b, _ := (proto.MarshalOptions{Deterministic: true}).Marshal(s.GetValue())
		h.Write(binary.BigEndian.AppendUint32(nil, uint32(len(b))))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

type releaseSecretsKey struct{}

// WithReleaseSecrets is internal to the verified deployment path. It carries
// ciphertext through local validation/create calls, never over the run RPC.
func WithReleaseSecrets(ctx context.Context, a *cloudpbv2.DeviceAssignment) context.Context {
	return context.WithValue(ctx, releaseSecretsKey{}, a)
}
func ReleaseSecretsFromContext(ctx context.Context) *cloudpbv2.DeviceAssignment {
	a, _ := ctx.Value(releaseSecretsKey{}).(*cloudpbv2.DeviceAssignment)
	return a
}

func (p *Postbox) checkAppSecrets(a *cloudpbv2.DeviceAssignment) error {
	if len(a.GetSecrets()) == 0 {
		return nil
	}
	key, err := p.encryptionKey(a.GetDevice())
	if err != nil {
		return unwrapRefused(errors.New("app secrets: device encryption key unavailable"))
	}
	values, err := openAppSecrets(a, key)
	ClearAppSecrets(values)
	if err != nil {
		return unwrapRefused(err)
	}
	return nil
}

func (c containerControl) SupportsAppSecrets() bool {
	runtime, ok := c.svc.containerd.(interface{ SupportsAppSecrets() bool })
	return ok && runtime.SupportsAppSecrets()
}

func secretStartContext(ctx context.Context, s ReleaseStart) (context.Context, error) {
	if len(s.Secrets.GetSecrets()) == 0 {
		return ctx, nil
	}
	if s.Secrets.GetAppId() != s.AppID {
		return nil, fmt.Errorf("secret assignment does not match app")
	}
	return WithReleaseSecrets(ctx, s.Secrets), nil
}

func (p *Postbox) acceptedSecretFields() []string {
	c, ok := p.control.(interface{ SupportsAppSecrets() bool })
	if ok && c.SupportsAppSecrets() {
		return []string{"wendycloud.v2.DeviceAssignment.secrets"}
	}
	return nil
}

func (c containerControl) UpdateAppSecretWraps(ctx context.Context, a *cloudpbv2.DeviceAssignment) error {
	runtime, ok := c.svc.containerd.(interface {
		UpdateAppSecretWraps(context.Context, *cloudpbv2.DeviceAssignment) error
	})
	if !ok {
		return errors.New("container runtime cannot refresh app secret wraps")
	}
	return runtime.UpdateAppSecretWraps(ctx, a)
}
