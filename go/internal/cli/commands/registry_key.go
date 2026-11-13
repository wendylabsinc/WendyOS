package commands

import (
	"context"
	"crypto/hpke"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/registrycrypto"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// The registry encryption key is per operator DEVICE (sem 2026-10-20 21:29):
// this machine generates its own for each operator session, right after the
// operator leaf is minted, and registers it with Cloud's key directory under
// the operator's signature. Wraps to it open on this machine only. A known
// device re-wraps onward for a new one with registrycrypto.Rewrap (T12/T8).
//
// Every wrap also goes to the custodian (registrycrypto.CustodianKey, recipient
// kind RecipientCustodian): dev is pinned, prod is refused until its key exists.

func newRegistryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Manage this machine's encrypted-registry key",
	}
	var cloudGRPC string
	key := &cobra.Command{
		Use:   "key",
		Short: "Show or rotate this machine's registry encryption key",
	}
	key.PersistentFlags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint of the session to use")
	key.AddCommand(
		&cobra.Command{
			Use:   "show",
			Short: "Show this machine's registry key, registering it if Cloud does not hold it",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runRegistryKey(cmd.Context(), cloudGRPC, false)
			},
		},
		&cobra.Command{
			Use:   "rotate",
			Short: "Replace this machine's registry key with a new one and register it",
			Long: "Generates a new registry key for this machine and registers it with Wendy Cloud.\n" +
				"Releases wrapped to the old key must be wrapped again to the new one by another of your devices.",
			Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return runRegistryKey(cmd.Context(), cloudGRPC, true)
			},
		},
	)
	cmd.AddCommand(key)
	return cmd
}

func runRegistryKey(ctx context.Context, cloudGRPC string, rotate bool) error {
	auth, err := pickAuthEntry(cloudGRPC)
	if err != nil {
		return err
	}
	if len(auth.Certificates) == 0 || auth.Certificates[0].TenantUUID() == "" {
		return fmt.Errorf("the registry key needs an operator session; run 'wendy auth login'")
	}
	if rotate {
		auth.RegistryPrivateKey, auth.RegistryKeyID = "", ""
	}
	priv, err := sessionRegistryKey(auth)
	if err != nil {
		return err
	}
	keyID := registrycrypto.KeyID(priv.PublicKey().Bytes())
	status := "registered"
	regErr := withCloud(ctx, auth, func(ctx context.Context, conn grpc.ClientConnInterface) error {
		// The local marker is not proof: Cloud may have dropped the key. When
		// Cloud does not list it, or the lookup fails, register it again
		// (registering the same key is idempotent).
		if auth.RegistryKeyID == keyID {
			if registryKeyListed(ctx, conn, auth, keyID) {
				return nil
			}
			auth.RegistryKeyID, status = "", "re-registered"
		}
		return bindRegistryKey(ctx, conn, auth, priv)
	})
	if err := persistRegistryKey(auth); err != nil {
		return err
	}
	if regErr != nil {
		cliLogln("Registry key %s: unregistered (cloud refused: %v)", keyID, regErr)
		return regErr
	}
	cliLogln("Registry key %s: %s with Cloud", keyID, status)
	return nil
}

// registryKeyListed reports whether Cloud's key directory serves keyID for
// this operator. A served registration is not verified here: this only
// decides whether to register again, never what to wrap to.
func registryKeyListed(ctx context.Context, conn grpc.ClientConnInterface, auth *config.AuthConfig, keyID string) bool {
	principal := auth.Certificates[0].PrincipalURI
	resp, err := cloudpb.NewKeyDirectoryServiceClient(conn).GetRecipientKeys(ctx, &cloudpb.GetRecipientKeysRequest{
		OrganizationId: auth.Certificates[0].TenantUUID(), Principals: []string{principal},
	})
	if err != nil {
		return false
	}
	for _, key := range resp.GetKeys() {
		var req cloudpb.RegisterEncryptionKeyRequest
		if key.GetPrincipal() == principal && proto.Unmarshal(key.GetRegistration().GetPayload(), &req) == nil && registrycrypto.KeyID(req.GetPublicKey()) == keyID {
			return true
		}
	}
	return false
}

// registerRegistryKeyAfterMint registers the session's registry key after an
// operator leaf was minted or renewed, generating it on first use. A failure
// is reported and leaves the key unbound: the next sign-in, certificate
// refresh or 'wendy registry key show' retries. The caller persists auth.
func registerRegistryKeyAfterMint(ctx context.Context, auth *config.AuthConfig) {
	if err := registerRegistryKey(ctx, auth); err != nil {
		cliLogln("Registry key not registered yet (retried at the next sign-in, certificate refresh or 'wendy registry key show'): %v", err)
	}
}

// retryRegistryKey registers an unbound registry key after the pre-flight
// renewed the operator leaf, and saves the result. A bound key costs nothing.
func retryRegistryKey(auth *config.AuthConfig) {
	before := auth.RegistryPrivateKey + auth.RegistryKeyID
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	registerRegistryKeyAfterMint(ctx, auth)
	if auth.RegistryPrivateKey+auth.RegistryKeyID == before {
		return
	}
	if err := persistRegistryKey(auth); err != nil {
		cliLogln("Registry key not saved: %v", err)
	}
}

func registerRegistryKey(ctx context.Context, auth *config.AuthConfig) error {
	priv, err := sessionRegistryKey(auth)
	if err != nil {
		return err
	}
	if auth.RegistryKeyID == registrycrypto.KeyID(priv.PublicKey().Bytes()) {
		return nil
	}
	return withCloud(ctx, auth, func(ctx context.Context, conn grpc.ClientConnInterface) error {
		return bindRegistryKey(ctx, conn, auth, priv)
	})
}

// bindRegistryKey sends the operator-signed RegisterUserEncryptionKey and
// marks the key bound once Cloud acknowledges this key's id.
func bindRegistryKey(ctx context.Context, conn grpc.ClientConnInterface, auth *config.AuthConfig, priv hpke.PrivateKey) error {
	keyID := registrycrypto.KeyID(priv.PublicKey().Bytes())
	var resp cloudpb.RegisterEncryptionKeyResponse
	err := cloudrequest.Invoke(ctx, conn, auth, cloudpb.KeyDirectoryService_RegisterUserEncryptionKey_FullMethodName,
		registrycrypto.Registration(auth.Certificates[0].TenantUUID(), priv.PublicKey()), &resp)
	if err != nil {
		return fmt.Errorf("registering the registry key with Cloud: %w", err)
	}
	if resp.GetKeyId() != keyID {
		return fmt.Errorf("Cloud registered registry key %q, not this machine's %q", resp.GetKeyId(), keyID)
	}
	auth.RegistryKeyID = keyID
	return nil
}

// sessionRegistryKey returns the session's registry key, generating one (and
// clearing the bound marker) when the session has none.
func sessionRegistryKey(auth *config.AuthConfig) (hpke.PrivateKey, error) {
	stored, err := auth.RegistryKey()
	if err != nil {
		return nil, fmt.Errorf("loading the registry key: %w", err)
	}
	if stored == "" {
		priv, err := registrycrypto.KEM.GenerateKey()
		if err != nil {
			return nil, err
		}
		raw, err := priv.Bytes()
		if err != nil {
			return nil, err
		}
		auth.RegistryPrivateKey, auth.RegistryKeyID = base64.StdEncoding.EncodeToString(raw), ""
		return priv, nil
	}
	raw, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return nil, errors.New("stored registry key is invalid; run 'wendy registry key rotate'")
	}
	priv, err := registrycrypto.KEM.NewPrivateKey(raw)
	if err != nil {
		return nil, errors.New("stored registry key is invalid; run 'wendy registry key rotate'")
	}
	return priv, nil
}

// persistRegistryKey writes the session's registry key and bound marker back
// to the stored session.
func persistRegistryKey(auth *config.AuthConfig) error {
	unlock, err := acquireAuthRefreshLock(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	for i := range cfg.Auth {
		if sameOAuthSession(&cfg.Auth[i], auth) {
			cfg.Auth[i].RegistryPrivateKey, cfg.Auth[i].RegistryKeyID = auth.RegistryPrivateKey, auth.RegistryKeyID
			return config.Save(cfg)
		}
	}
	return errors.New("stored session for the registry key no longer exists")
}
