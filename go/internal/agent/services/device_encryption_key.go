package services

import (
	"context"
	"crypto/hpke"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/devicesign"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/registrycrypto"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// deviceEncryptionKeyFile holds the device's registry encryption key next to
// device-key.pem, with the same custody: mode 0600 in the config directory,
// removed with the other credentials when the device is reset.
const deviceEncryptionKeyFile = "device-encryption-key.json"

// DeviceEncryptionKey is the device's registry encryption key (ML-KEM-768 +
// X25519, registrycrypto.KEM) and its registration with Cloud's key directory
// (registry plan T10, wendycloud/v2/registry.proto). Operators wrap
// deployment keys to it; the device unwraps them (T14).
//
// The key is generated the first time the agent holds an enrolled identity
// and belongs to that device principal: a new enrollment gets a new key, a
// certificate renewal keeps it so existing wraps still open.
//
// For every identity the agent loads (enrollment, start, and so after each
// renewal, which restarts the agent) the device registers the public half in a
// request it signs itself, over its own mTLS
// (KeyDirectoryService.RegisterDeviceEncryptionKey). A device cannot read the
// key directory (GetRecipientKeys is operator-only), so the key counts as
// unbound until Cloud answers with this key's id; until then the
// registration is retried with jittered backoff. Registering the same key
// again is idempotent in Cloud.
type DeviceEncryptionKey struct {
	logger     *zap.Logger
	configPath string
	retry      time.Duration
	dial       func(cloudURL, certPEM, keyPEM, chainPEM string) (cloudpbv2.KeyDirectoryServiceClient, io.Closer, error)

	mu                                  sync.Mutex
	cloudURL, certPEM, keyPEM, chainPEM string
	registered                          string // the certPEM Cloud acknowledged this key under
	identityChanged                     chan struct{}
}

// NewDeviceEncryptionKey builds the registrar; it does nothing until
// UpdateIdentity supplies an enrolled identity.
func NewDeviceEncryptionKey(logger *zap.Logger, configPath string) *DeviceEncryptionKey {
	return &DeviceEncryptionKey{
		logger:          logger,
		configPath:      configPath,
		retry:           30 * time.Second,
		dial:            dialKeyDirectory,
		identityChanged: make(chan struct{}, 1),
	}
}

func dialKeyDirectory(cloudURL, certPEM, keyPEM, chainPEM string) (cloudpbv2.KeyDirectoryServiceClient, io.Closer, error) {
	conn, closer, err := dialDeviceCloud(cloudURL, certPEM, keyPEM, chainPEM)
	if err != nil {
		return nil, nil, err
	}
	return cloudpbv2.NewKeyDirectoryServiceClient(conn), closer, nil
}

// UpdateIdentity swaps the device identity; empty values stop registering.
// cloudURL is the device's configured Cloud endpoint, as for every other
// device-to-Cloud call.
func (k *DeviceEncryptionKey) UpdateIdentity(cloudURL, certPEM, keyPEM, chainPEM string) {
	k.mu.Lock()
	k.cloudURL, k.certPEM, k.keyPEM, k.chainPEM = cloudURL, certPEM, keyPEM, chainPEM
	k.mu.Unlock()
	select {
	case k.identityChanged <- struct{}{}:
	default:
	}
}

// Run registers the key for each identity until ctx is done. A failure is
// logged and retried with jittered exponential backoff (capped at 30
// minutes); it never stops the agent.
func (k *DeviceEncryptionKey) Run(ctx context.Context) {
	failures := 0
	for {
		var retry <-chan time.Time
		if err := k.register(ctx); err != nil && ctx.Err() == nil {
			failures++
			wait := min(k.retry<<min(failures-1, 6), 30*time.Minute)
			wait = wait/2 + rand.N(wait/2+1) // a fleet does not retry in one wave
			k.logger.Warn("Device encryption key not registered with Cloud; retrying",
				zap.String("event", "device_encryption_key_unbound"),
				zap.Duration("retry_in", wait), zap.Error(err))
			retry = time.After(wait)
		} else {
			failures = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-k.identityChanged:
			failures = 0
		case <-retry:
		}
	}
}

// register loads or creates the key for the current identity and registers
// it unless Cloud already acknowledged it under this identity.
func (k *DeviceEncryptionKey) register(ctx context.Context) error {
	k.mu.Lock()
	cloudURL, certPEM, keyPEM, chainPEM, registered := k.cloudURL, k.certPEM, k.keyPEM, k.chainPEM, k.registered
	k.mu.Unlock()
	if certPEM == "" || keyPEM == "" || certPEM == registered {
		return nil
	}
	leaf, err := storedDeviceLeaf(certPEM)
	if err != nil {
		return err
	}
	principal, _ := certs.TenantPrincipalFromCert(leaf)
	id, err := certs.ParsePrincipal(principal)
	if err != nil || principal != certs.DeviceSPIFFEURI(id.TenantUUID, id.EntityID) {
		return fmt.Errorf("device certificate names %q, not a device principal", principal)
	}
	priv, err := loadOrCreateDeviceEncryptionKey(k.configPath, principal)
	if err != nil {
		return err
	}
	signer, err := devicesign.New(certPEM, keyPEM)
	if err != nil {
		// A P-256 device registers once its ML-DSA-65 renewal restarts the agent.
		k.logger.Warn("Device encryption key cannot be registered until the device key is ML-DSA-65",
			zap.String("event", "device_encryption_key_unsignable"), zap.Error(err))
		return nil
	}

	pub := priv.PublicKey()
	keyID := registrycrypto.KeyID(pub.Bytes())
	client, closer, err := k.dial(cloudURL, certPEM, keyPEM, chainPEM)
	if err != nil {
		return err
	}
	defer func() { _ = closer.Close() }()
	correlationID := uuid.NewString()
	signed, err := signer.Sign(cloudpbv2.KeyDirectoryService_RegisterDeviceEncryptionKey_FullMethodName,
		registrycrypto.Registration(id.TenantUUID, pub), correlationID)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := client.RegisterDeviceEncryptionKey(callCtx, signed)
	if err != nil {
		return fmt.Errorf("registering device encryption key %s: %w", keyID, err)
	}
	if resp.GetKeyId() != keyID {
		return fmt.Errorf("Cloud registered key %q, not this device's %q", resp.GetKeyId(), keyID)
	}
	k.mu.Lock()
	if k.certPEM == certPEM {
		k.registered = certPEM
	}
	k.mu.Unlock()
	k.logger.Info("Device encryption key registered with Cloud",
		zap.String("event", "device_encryption_key_registered"),
		zap.String("principal", principal), zap.String("key_id", keyID),
		zap.String("correlation_id", correlationID))
	return nil
}

type storedEncryptionKey struct {
	Principal  string `json:"principal"`
	PrivateKey []byte `json:"privateKey"` // registrycrypto.KEM private key bytes
}

// loadOrCreateDeviceEncryptionKey returns principal's key, generating and
// persisting a new one when there is none or the stored one belongs to another
// enrollment (or is unreadable as a key). A file that cannot be read is an
// error, never replaced.
func loadOrCreateDeviceEncryptionKey(configPath, principal string) (hpke.PrivateKey, error) {
	raw, err := os.ReadFile(filepath.Join(configPath, deviceEncryptionKeyFile))
	switch {
	case err == nil:
		var stored storedEncryptionKey
		if json.Unmarshal(raw, &stored) == nil && stored.Principal == principal {
			if priv, err := registrycrypto.KEM.NewPrivateKey(stored.PrivateKey); err == nil {
				return priv, nil
			}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("reading device encryption key: %w", err)
	}
	priv, err := registrycrypto.KEM.GenerateKey()
	if err != nil {
		return nil, err
	}
	privBytes, err := priv.Bytes()
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(storedEncryptionKey{Principal: principal, PrivateKey: privBytes})
	if err != nil {
		return nil, err
	}
	if err := writeProvisioningFile(configPath, deviceEncryptionKeyFile, data); err != nil {
		return nil, err
	}
	return priv, nil
}
