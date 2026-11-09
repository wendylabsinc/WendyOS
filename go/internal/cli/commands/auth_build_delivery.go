package commands

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/secretstore"
)

const deliveryCredentialService = "wendy-build-delivery"

func clearDeliveryCredentials() error {
	root, err := config.ConfigDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(root, "build-delivery-credentials")
	files, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, file := range files {
		account, ok := strings.CutSuffix(file.Name(), ".json")
		if !ok || len(account) != 64 {
			continue
		}
		if _, err := hex.DecodeString(account); err != nil {
			continue
		}
		path := filepath.Join(dir, file.Name())
		unlock, err := flock.Acquire(path+".lock", 10*time.Second)
		if err != nil {
			return err
		}
		err = os.Remove(path)
		if err == nil || os.IsNotExist(err) {
			if store := secretstore.NewKeychain(deliveryCredentialService); store != nil {
				store.Delete(account)
			}
		}
		unlock()
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func deliveryCredentialPath(operator string, device meshid.Device) (string, string, error) {
	root, err := config.ConfigDir()
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(operator + "\n" + device.Principal()))
	account := hex.EncodeToString(sum[:])
	dir := filepath.Join(root, "build-delivery-credentials")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	return filepath.Join(dir, account+".json"), account, nil
}

func loadDeliveryCredential(operator string, device meshid.Device) (*config.CertificateInfo, error) {
	path, account, err := deliveryCredentialPath(operator, device)
	if err != nil {
		return nil, err
	}
	unlock, err := flock.Acquire(path+".lock", 10*time.Second)
	if err != nil {
		return nil, err
	}
	defer unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var credential config.CertificateInfo
	if len(data) > 256<<10 || json.Unmarshal(data, &credential) != nil {
		return nil, fmt.Errorf("invalid cached delivery credential")
	}
	chain, err := certs.ParseCertsFromPEM([]byte(credential.PemCertificate))
	if err != nil || len(chain) != 1 || credential.CertificatePrincipal() != operator || time.Now().Add(30*time.Second).After(chain[0].NotAfter) {
		os.Remove(path)
		if store := secretstore.NewKeychain(deliveryCredentialService); store != nil {
			store.Delete(account)
		}
		return nil, fmt.Errorf("cached delivery credential expired or mismatched")
	}
	if credential.PemPrivateKey == "keychain" {
		store := secretstore.NewKeychain(deliveryCredentialService)
		if store == nil {
			return nil, fmt.Errorf("delivery signing key unavailable")
		}
		credential.PemPrivateKey = string(store.Get(account))
	}
	if !strings.HasPrefix(credential.PemPrivateKey, "-----BEGIN PRIVATE KEY-----") {
		return nil, fmt.Errorf("delivery signing key unavailable")
	}
	return &credential, nil
}

func saveDeliveryCredential(operator string, device meshid.Device, credential *config.CertificateInfo) error {
	path, account, err := deliveryCredentialPath(operator, device)
	if err != nil {
		return err
	}
	unlock, err := flock.Acquire(path+".lock", 10*time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	copy := *credential
	if os.Getenv("WENDY_SECRET_STORE") != "file" {
		if store := secretstore.NewKeychain(deliveryCredentialService); store != nil {
			if store.Put(account, []byte(copy.PemPrivateKey)) == nil {
				copy.PemPrivateKey = "keychain"
			}
		}
	}
	data, err := json.Marshal(copy)
	if err != nil {
		return err
	}
	return atomicfile.WritePreservingOwner(path, data, 0o600)
}

func newAuthPrepareBuildDeliveryCmd() *cobra.Command {
	var device string
	cmd := &cobra.Command{Use: "prepare-build-delivery", Short: "Prepare target-scoped deployment authority before an airgap", Long: "Cache an operator deployment certificate for one target (up to 15 minutes). Create and admit the build job before it expires; admitted delivery can then continue offline for up to 24 hours.", RunE: func(cmd *cobra.Command, _ []string) error {
		conn, err := connectFleetDevice(cmd.Context(), device)
		if err != nil {
			return err
		}
		defer conn.Close()
		base, err := targetPushTargetBase(cmd.Context(), conn)
		if err != nil {
			return err
		}
		id, verified := conn.ObservedServerIdentity()
		target, err := meshid.DeviceFromPrincipal(id.Principal)
		if !verified || err != nil || target.UUID().String() != base.GetAssetUuid() {
			return fmt.Errorf("authenticated target identity unavailable")
		}
		credential, err := deliveryCredential(cmd.Context(), conn, target, true)
		if err != nil {
			return err
		}
		chain, err := certs.ParseCertsFromPEM([]byte(credential.PemCertificate))
		if err != nil || len(chain) != 1 {
			return fmt.Errorf("invalid delivery certificate")
		}
		if jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"target": target.Principal(), "admit_before": chain[0].NotAfter.UTC().Format(time.RFC3339)})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Build delivery authority prepared for %s. Admit jobs before %s.\n", device, chain[0].NotAfter.UTC().Format(time.RFC3339))
		return nil
	}}
	cmd.Flags().StringVar(&device, "device", "", "Target device selector")
	_ = cmd.MarkFlagRequired("device")
	return cmd
}
