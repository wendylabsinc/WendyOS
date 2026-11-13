package commands

import (
	"bytes"
	"context"
	"crypto/hpke"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/registrycrypto"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// The encrypted registry, operator side (T12, registry.proto). Publish
// encrypts an OCI image under a fresh content key and uploads only
// ciphertext; the content key reaches this machine and the custodian as
// release-scope wraps. A deployment seals the content key under its own
// deployment key, wrapped to this machine and the custodian, and each target
// device gets one operator-signed DeviceAssignment carrying its own wrap.
// Device keys are trusted only after their signed registration verifies up
// to a CA of this operator's own pki-core chain; cloud is a courier.

const putBlobChunk = 1 << 20

func newAppPublishCmd() *cobra.Command {
	var tag, appID, layoutDir, platform, dockerfile, cloudGRPC string
	var untagged bool
	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Publish an encrypted release of this project's app to the Wendy Cloud registry",
		Long: "Builds the project's container image (or takes --oci-layout), encrypts every blob on this\n" +
			"machine and uploads only ciphertext as a new release tagged --tag. The release key is\n" +
			"wrapped to this machine's registry key and to the Wendy custodian key. An app that does\n" +
			"not exist yet is created first.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if tag != "" && untagged {
				return usageErrorf("Give --tag or --untagged, not both.")
			}
			raw, err := os.ReadFile("wendy.json")
			if err != nil {
				return fmt.Errorf("reading wendy.json: %w", err)
			}
			appCfg, def, err := appDefinition(raw, appID)
			if err != nil {
				return err
			}
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			var branch string
			switch {
			case untagged:
				if branch, err = gitBranch(cwd); err != nil {
					return err
				}
			case tag == "" && !isInteractiveTerminal():
				return usageErrorf("No tag given. Add --tag <version>, or run in a terminal to answer.")
			case tag == "":
				if tag, err = askText("Tag", "The version you want to give this release, for example 1.0.0.", "", func(s string) error { return validateReleaseTag(s) }); err != nil {
					return err
				}
			}
			defaults := []byte("{}")
			if appCfg.Env != nil {
				defaults, _ = json.Marshal(appCfg.Env)
			}
			auth, err := pickAppAuth(cloudGRPC)
			if err != nil {
				return err
			}
			if err := withAppCloud(cmd.Context(), auth, func(ctx context.Context, conn grpc.ClientConnInterface) error {
				return ensureApp(ctx, cmd.OutOrStdout(), conn, auth, appCfg.AppID)
			}); err != nil {
				return err
			}
			work, err := os.MkdirTemp("", "wendy-publish-")
			if err != nil {
				return err
			}
			defer func() { _ = os.RemoveAll(work) }()
			if layoutDir == "" {
				resolved, err := resolveDockerfile(cwd, dockerfile, false, "")
				if err != nil {
					return err
				}
				layoutDir = filepath.Join(work, "oci")
				if err := buildImageToOCILayoutDir(cmd.Context(), cwd, resolved, platform, nil, "", layoutDir, cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
					return err
				}
			}
			img, err := ociLayoutManifestForPlatform(layoutDir, platform)
			if err != nil {
				return err
			}
			if img, err = withAppDefinition(img, def); err != nil {
				return err
			}
			if untagged {
				digest, err := img.Digest()
				if err != nil {
					return err
				}
				tag = branch + "@" + digest.String()
			}
			return withAppCloud(cmd.Context(), auth, func(ctx context.Context, conn grpc.ClientConnInterface) error {
				op, err := newRegistryOp(ctx, conn, auth)
				if err != nil {
					return err
				}
				releaseID, err := createAppRelease(ctx, conn, auth, op.org, appCfg.AppID, tag, string(defaults))
				if err != nil {
					return err
				}
				manifest, err := op.publish(ctx, releaseID, img, work)
				if err != nil {
					return fmt.Errorf("release %s was created but not published: %w", releaseID, err)
				}
				cliLogln("Published release %s of %s: %s = %s", releaseID, appCfg.AppID, tag, manifest.GetManifestDigest())
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&tag, "tag", "", "Release tag, the version you give this release, e.g. 1.0.0")
	cmd.Flags().BoolVar(&untagged, "untagged", false, "Name the release <git branch>@<manifest digest> instead of a tag")
	cmd.Flags().StringVar(&appID, "app", "", "App id (default: appId in wendy.json, which it must match)")
	cmd.Flags().StringVar(&layoutDir, "oci-layout", "", "Publish this OCI image layout directory instead of building")
	cmd.Flags().StringVar(&platform, "platform", "linux/arm64", "Image platform to build or pick from the layout")
	cmd.Flags().StringVar(&dockerfile, "dockerfile", "", "Dockerfile to build (default: detected)")
	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint of the session to use")
	return cmd
}

// appConfigLabel carries the release's app definition to the device
// (agent services.AppConfigLabel), at most maxAppDefinition bytes.
const (
	appConfigLabel   = "sh.wendy/app-config"
	maxAppDefinition = 64 << 10
)

// appDefinition is wendy.json as a release carries it: compact JSON holding
// only the fields AppConfig knows. What the device would refuse is refused
// here, in its words, before anything is built or uploaded.
func appDefinition(raw []byte, app string) (*appconfig.AppConfig, []byte, error) {
	cfg, err := appconfig.LoadFromBytes(raw)
	if err != nil {
		return nil, nil, err
	}
	if app != "" && app != cfg.AppID {
		return nil, nil, usageErrorf("--app must match appId in wendy.json (%s).", cfg.AppID)
	}
	if len(cfg.Services) > 0 || cfg.ServiceName != "" {
		return nil, nil, errors.New("apps with several services cannot be deployed yet; deploy each service as its own app")
	}
	if err := appconfig.ValidateAppID(cfg.AppID); err != nil {
		return nil, nil, fmt.Errorf("wendy.json: %w", err)
	}
	def, err := json.Marshal(cfg)
	if err != nil {
		return nil, nil, err
	}
	if warnings := append(appconfig.ValidateJSON(raw), appconfig.ValidateJSON(def)...); len(warnings) > 0 {
		return nil, nil, fmt.Errorf("%s. A device refuses an app with warnings; fix wendy.json first", strings.Join(warnings, "; "))
	}
	if len(def) > maxAppDefinition {
		return nil, nil, fmt.Errorf("wendy.json is %d bytes as published, over the %d a device accepts", len(def), maxAppDefinition)
	}
	return cfg, def, nil
}

// withAppDefinition labels img's config with def; the manifest digest the
// operator signs covers it.
func withAppDefinition(img v1.Image, def []byte) (v1.Image, error) {
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	cfg := cf.Config
	cfg.Labels = maps.Clone(cfg.Labels)
	if cfg.Labels == nil {
		cfg.Labels = map[string]string{}
	}
	cfg.Labels[appConfigLabel] = string(def)
	return mutate.Config(img, cfg)
}

// gitBranch is the branch checked out in dir.
func gitBranch(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "branch", "--show-current").Output()
	if branch := strings.TrimSpace(string(out)); err == nil && branch != "" {
		return branch, nil
	}
	return "", usageErrorf("--untagged names the release after the current git branch, and this folder has none checked out.")
}

// ensureApp creates appID first, asking the create questions, when Cloud
// does not have it.
func ensureApp(ctx context.Context, out io.Writer, conn grpc.ClientConnInterface, auth *config.AuthConfig, appID string) error {
	org, err := operatorOrg(auth)
	if err != nil {
		return err
	}
	_, err = cloudpb.NewAppServiceClient(conn).GetApp(ctx, &cloudpb.GetAppRequest{Id: appID, OrganizationId: org})
	if status.Code(err) != codes.NotFound {
		if err != nil {
			return fmt.Errorf("reading app %s: %w", appID, err)
		}
		return nil
	}
	if !isInteractiveTerminal() {
		return fmt.Errorf("%s doesn't exist yet. Run wendy app create --app %s first, or run this in a terminal.", appID, appID)
	}
	a := &appAnswers{id: appID, name: appID}
	if err := a.ask(func(f string) bool { return f == "app" }); err != nil {
		return err
	}
	return createApp(ctx, out, conn, auth, a, false)
}

// registryFlags are the flags deploy, assign, unassign and rotate share.
type registryFlags struct {
	devices    []string
	cloudGRPC  string
	deployment string
}

func (f *registryFlags) bind(cmd *cobra.Command, deployment bool) {
	cmd.Flags().StringArrayVar(&f.devices, "device", nil, "Target device UUID (repeatable)")
	cmd.Flags().StringVar(&f.cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint of the session to use")
	if deployment {
		cmd.Flags().StringVar(&f.deployment, "deployment", "", "Deployment id (required)")
	}
}

// run validates the flags and runs call with this project's app id against
// the session's registry.
func (f *registryFlags) run(cmd *cobra.Command, call func(context.Context, *registryOp, string, []string) error) error {
	if cmd.Flags().Lookup("deployment") != nil && !canonicalUUID(f.deployment) {
		return usageErrorf("--deployment must be a deployment id (lower-case UUID)")
	}
	if len(f.devices) == 0 {
		return usageErrorf("name each target device with --device <uuid>")
	}
	for _, d := range f.devices {
		if !canonicalUUID(d) {
			return usageErrorf("--device must be a device UUID (lower-case), got %q", d)
		}
	}
	appCfg, err := appconfig.LoadFromFile("wendy.json")
	if err != nil {
		return err
	}
	auth, err := pickAuthEntry(f.cloudGRPC)
	if err != nil {
		return err
	}
	return withCloud(cmd.Context(), auth, func(ctx context.Context, conn grpc.ClientConnInterface) error {
		op, err := newRegistryOp(ctx, conn, auth)
		if err != nil {
			return err
		}
		return call(ctx, op, appCfg.AppID, f.devices)
	})
}

func newAppRotateCmd() *cobra.Command {
	var f registryFlags
	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Replace a deployment's key and re-assign the named devices",
		Long: "Seals the release key under a new deployment key and re-assigns each --device at it.\n" +
			"Name every device that should keep the deployment: devices not named stay on the old key.\n" +
			"Only the deployment's creator can rotate (Cloud accepts its custodian wrap from them only).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return f.run(cmd, func(ctx context.Context, op *registryOp, appID string, devices []string) error {
				return op.rotate(ctx, f.deployment, appID, devices)
			})
		},
	}
	f.bind(cmd, true)
	return cmd
}

// registryOp is one operator session's view of the registry.
type registryOp struct {
	conn        grpc.ClientConnInterface
	auth        *config.AuthConfig
	org, me     string
	key         hpke.PrivateKey
	keyID       string
	roots       []*x509.Certificate
	custodian   []byte
	custodianID string
}

// newRegistryOp loads this machine's registry key (registering it when Cloud
// has not acknowledged it), the custodian pin and the CAs device keys are
// verified against.
func newRegistryOp(ctx context.Context, conn grpc.ClientConnInterface, auth *config.AuthConfig) (*registryOp, error) {
	if len(auth.Certificates) == 0 || auth.Certificates[0].TenantUUID() == "" {
		return nil, errors.New("the registry needs an operator session; run 'wendy auth login'")
	}
	custodian, custodianID, err := registrycrypto.CustodianKey(auth.CloudGRPC == defaultDevCloudGRPC)
	if err != nil {
		return nil, fmt.Errorf("%w (%s)", err, auth.CloudGRPC)
	}
	roots, err := sessionCAs(auth.Certificates[0].PemCertificateChain)
	if err != nil {
		return nil, err
	}
	key, err := sessionRegistryKey(auth)
	if err != nil {
		return nil, err
	}
	keyID := registrycrypto.KeyID(key.PublicKey().Bytes())
	if auth.RegistryKeyID != keyID {
		if err := bindRegistryKey(ctx, conn, auth, key); err != nil {
			return nil, err
		}
		if err := persistRegistryKey(auth); err != nil {
			return nil, err
		}
	}
	return &registryOp{
		conn: conn, auth: auth, org: auth.Certificates[0].TenantUUID(), me: auth.Certificates[0].PrincipalURI,
		key: key, keyID: keyID, roots: roots, custodian: custodian, custodianID: custodianID,
	}, nil
}

// sessionCAs are the CA certificates of the operator's own chain, which
// pki-core issued at sign-in: the anchors a served key registration must
// chain to. Cloud never supplies them.
func sessionCAs(chainPEM string) ([]*x509.Certificate, error) {
	chain, err := certs.ParseCertsFromPEM([]byte(chainPEM))
	if err != nil {
		chain = nil
	}
	var cas []*x509.Certificate
	for _, c := range chain {
		if c.BasicConstraintsValid && c.IsCA {
			cas = append(cas, c)
		}
	}
	if len(cas) == 0 {
		return nil, errors.New("the operator session has no CA chain to verify device keys against; run 'wendy auth login'")
	}
	return cas, nil
}

func deviceURI(org, device string) string {
	return "spiffe://wendy.sh/tenant/" + org + "/device/" + device
}

// verified returns the registered public key of principal from keys, verified
// against the session's CAs, and refuses a principal with none or several.
func (op *registryOp) verified(keys []*cloudpb.RecipientKey, principal string) ([]byte, error) {
	var found [][]byte
	for _, k := range keys {
		if k.GetPrincipal() != principal {
			continue
		}
		for _, root := range op.roots {
			pub, err := registrycrypto.VerifyRegistration(k.GetRegistration(), k.GetChainDer(), root, principal, op.org)
			if err == nil {
				if !slices.ContainsFunc(found, func(f []byte) bool { return string(f) == string(pub) }) {
					found = append(found, pub)
				}
				break
			}
		}
	}
	switch len(found) {
	case 0:
		return nil, fmt.Errorf("%s has no verifiable registry key", principal)
	case 1:
		return found[0], nil
	}
	return nil, fmt.Errorf("%s has %d registry keys; cannot tell which one it holds", principal, len(found))
}

// deviceKeys verifies the key of every device before anything is changed.
func (op *registryOp) deviceKeys(ctx context.Context, devices []string) (map[string][]byte, error) {
	principals := make([]string, len(devices))
	for i, d := range devices {
		principals[i] = deviceURI(op.org, d)
	}
	resp, err := cloudpb.NewKeyDirectoryServiceClient(op.conn).GetRecipientKeys(ctx, &cloudpb.GetRecipientKeysRequest{OrganizationId: op.org, Principals: principals})
	if err != nil {
		return nil, fmt.Errorf("fetching device keys: %w", err)
	}
	keys := map[string][]byte{}
	var errs []error
	for _, p := range principals {
		pub, err := op.verified(resp.GetKeys(), p)
		if err != nil {
			errs = append(errs, err)
		}
		keys[p] = pub
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("refusing to target devices without a verified key (nothing was changed): %w", err)
	}
	return keys, nil
}

// wrap seals key (named keyID) for one recipient public key in scope.
func (op *registryOp) wrap(scope byte, scopeID, keyID string, key []byte, kind cloudpb.RecipientKind, recipient string, pub []byte) (*cloudpb.KeyWrap, error) {
	w := registrycrypto.Wrap{
		OrganizationID: op.org, Scope: scope, ScopeID: scopeID, KeyID: keyID,
		RecipientKind: byte(kind), Recipient: recipient, RecipientKeyID: registrycrypto.KeyID(pub),
	}
	enc, ciphertext, err := w.Seal(pub, key)
	if err != nil {
		return nil, err
	}
	ws := &cloudpb.WrapScope{KeyId: keyID}
	if scope == registrycrypto.ScopeDeployment {
		ws.Scope = &cloudpb.WrapScope_DeploymentId{DeploymentId: scopeID}
	} else {
		ws.Scope = &cloudpb.WrapScope_AppReleaseId{AppReleaseId: scopeID}
	}
	return &cloudpb.KeyWrap{
		Scope: ws, RecipientKind: kind, Recipient: recipient, RecipientKeyId: w.RecipientKeyID,
		Suite: cloudpb.KemSuite_KEM_SUITE_MLKEM768_X25519, Enc: enc, Ciphertext: ciphertext,
	}, nil
}

// ownWraps wraps key to this machine and to the custodian.
func (op *registryOp) ownWraps(scope byte, scopeID string, key []byte) ([]*cloudpb.KeyWrap, error) {
	keyID := registrycrypto.KeyID(key)
	mine, err := op.wrap(scope, scopeID, keyID, key, cloudpb.RecipientKind_RECIPIENT_KIND_USER, op.me, op.key.PublicKey().Bytes())
	if err != nil {
		return nil, err
	}
	custodian, err := op.wrap(scope, scopeID, keyID, key, cloudpb.RecipientKind_RECIPIENT_KIND_CUSTODIAN, "", op.custodian)
	if err != nil {
		return nil, err
	}
	return []*cloudpb.KeyWrap{mine, custodian}, nil
}

func (op *registryOp) addWraps(ctx context.Context, wraps []*cloudpb.KeyWrap, sealed []*cloudpb.SealedContentKey) error {
	var resp cloudpb.AddWrapsResponse
	err := cloudrequest.Invoke(ctx, op.conn, op.auth, cloudpb.ArtifactRegistryService_AddWraps_FullMethodName,
		&cloudpb.AddWrapsRequest{OrganizationId: op.org, Wraps: wraps, SealedContentKeys: sealed}, &resp)
	if err != nil {
		return fmt.Errorf("adding key wraps: %w", err)
	}
	return nil
}

// imageBlob is one OCI blob: the descriptor digest and its plaintext.
type imageBlob struct {
	digest string
	open   func() (io.ReadCloser, error)
}

// imageBlobs lists the config and layer blobs of img, each digest once (one
// content key gives equal plaintexts equal ciphertexts, which the registry
// refuses twice in one artifact).
func imageBlobs(img v1.Image) ([]imageBlob, error) {
	configName, err := img.ConfigName()
	if err != nil {
		return nil, err
	}
	rawConfig, err := img.RawConfigFile()
	if err != nil {
		return nil, err
	}
	blobs := []imageBlob{{configName.String(), func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(rawConfig)), nil }}}
	layers, err := img.Layers()
	if err != nil {
		return nil, err
	}
	for _, l := range layers {
		d, err := l.Digest()
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(blobs, func(b imageBlob) bool { return b.digest == d.String() }) {
			blobs = append(blobs, imageBlob{d.String(), l.Compressed})
		}
	}
	return blobs, nil
}

// publish encrypts img into work, opens the artifact for releaseID, uploads
// the ciphertext Cloud is missing and wraps the content key to this machine,
// the custodian and every auto-grant key that verifies.
func (op *registryOp) publish(ctx context.Context, releaseID string, img v1.Image, work string) (*cloudpb.ArtifactManifest, error) {
	rawManifest, err := img.RawManifest()
	if err != nil {
		return nil, err
	}
	blobs, err := imageBlobs(img)
	if err != nil {
		return nil, err
	}
	contentKey := registrycrypto.NewKey()
	defer clear(contentKey)
	sum := sha256.Sum256(rawManifest)
	manifest := &cloudpb.ArtifactManifest{
		ManifestDigest: "sha256:" + hex.EncodeToString(sum[:]), OciManifest: rawManifest, Kind: cloudpb.ReleaseType_CONTAINER,
		ContentKeyId: registrycrypto.KeyID(contentKey), Aead: cloudpb.AeadSuite_AEAD_SUITE_AES_256_GCM_CHUNKED,
		// Always 64 KiB: the manifest digest does not cover chunk_size, so the
		// agent refuses chunks below 4 KiB (T14) and nothing here varies it.
		ChunkSize: registrycrypto.DefaultChunkSize,
	}
	files := map[string]string{} // ciphertext digest -> encrypted file
	for i, b := range blobs {
		path := filepath.Join(work, fmt.Sprintf("blob-%d", i))
		enc, err := encryptBlobFile(path, b, contentKey)
		if err != nil {
			return nil, fmt.Errorf("encrypting %s: %w", b.digest, err)
		}
		files[enc.CiphertextDigest] = path
		manifest.Blobs = append(manifest.Blobs, &cloudpb.EncryptedBlob{
			PlaintextDigest: enc.PlaintextDigest, CiphertextDigest: enc.CiphertextDigest,
			PlaintextSize: enc.PlaintextSize, CiphertextSize: enc.CiphertextSize,
		})
	}

	var resp cloudpb.PublishArtifactResponse
	if err := cloudrequest.Invoke(ctx, op.conn, op.auth, cloudpb.ArtifactRegistryService_PublishArtifact_FullMethodName,
		&cloudpb.PublishArtifactRequest{OrganizationId: op.org, AppReleaseId: releaseID, Manifest: manifest}, &resp); err != nil {
		return nil, fmt.Errorf("publishing the artifact: %w", err)
	}
	for _, digest := range resp.GetMissingCiphertextDigests() {
		i := slices.IndexFunc(manifest.Blobs, func(b *cloudpb.EncryptedBlob) bool { return b.CiphertextDigest == digest })
		if i < 0 {
			return nil, fmt.Errorf("Cloud asked for blob %s, which is not in this artifact", digest)
		}
		if err := op.putBlob(ctx, resp.GetUploadId(), manifest.Blobs[i], files[digest]); err != nil {
			return nil, err
		}
	}

	wraps, err := op.ownWraps(registrycrypto.ScopeRelease, releaseID, contentKey)
	if err != nil {
		return nil, err
	}
	granted := map[string]bool{op.keyID: true}
	for _, k := range resp.GetAutoGrantKeys() {
		pub, err := op.verified([]*cloudpb.RecipientKey{k}, k.GetPrincipal())
		if err != nil {
			cliLogln("Not granting %s: %v", k.GetPrincipal(), err)
			continue
		}
		if granted[registrycrypto.KeyID(pub)] {
			continue
		}
		granted[registrycrypto.KeyID(pub)] = true
		w, err := op.wrap(registrycrypto.ScopeRelease, releaseID, manifest.ContentKeyId, contentKey, cloudpb.RecipientKind_RECIPIENT_KIND_USER, k.GetPrincipal(), pub)
		if err != nil {
			return nil, err
		}
		wraps = append(wraps, w)
	}
	if err := op.addWraps(ctx, wraps, nil); err != nil {
		return nil, err
	}
	return manifest, nil
}

// encryptBlobFile writes b's ciphertext to path.
func encryptBlobFile(path string, b imageBlob, contentKey []byte) (registrycrypto.Blob, error) {
	src, err := b.open()
	if err != nil {
		return registrycrypto.Blob{}, err
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return registrycrypto.Blob{}, err
	}
	enc, err := registrycrypto.EncryptBlob(dst, src, contentKey, b.digest, registrycrypto.DefaultChunkSize)
	if closeErr := dst.Close(); err == nil {
		err = closeErr
	}
	return enc, err
}

// putBlob streams one encrypted blob under the upload.
func (op *registryOp) putBlob(ctx context.Context, uploadID string, blob *cloudpb.EncryptedBlob, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	stream, err := cloudpb.NewArtifactRegistryServiceClient(op.conn).PutBlob(ctx)
	if err != nil {
		return fmt.Errorf("uploading %s: %w", blob.CiphertextDigest, err)
	}
	err = stream.Send(&cloudpb.PutBlobRequest{Part: &cloudpb.PutBlobRequest_Header{Header: &cloudpb.PutBlobHeader{
		UploadId: uploadID, CiphertextDigest: blob.CiphertextDigest, CiphertextSize: blob.CiphertextSize,
	}}})
	buf := make([]byte, putBlobChunk)
	for err == nil {
		var n int
		n, err = f.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&cloudpb.PutBlobRequest{Part: &cloudpb.PutBlobRequest_Data{Data: buf[:n]}}); sendErr != nil {
				err = sendErr
			}
		}
	}
	if err != io.EOF {
		// A failed Send is explained by the stream's status.
		if _, recvErr := stream.CloseAndRecv(); recvErr != nil {
			err = recvErr
		}
		return fmt.Errorf("uploading %s: %w", blob.CiphertextDigest, err)
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("uploading %s: %w", blob.CiphertextDigest, err)
	}
	if resp.GetCiphertextDigest() != blob.CiphertextDigest {
		return fmt.Errorf("Cloud stored %q for blob %s", resp.GetCiphertextDigest(), blob.CiphertextDigest)
	}
	return nil
}

// artifact is the release's manifest, checked against its own digest (the
// one thing a served manifest can be checked against here) and, when set,
// against the digest the operator expects.
func (op *registryOp) artifact(ctx context.Context, releaseID, want string) (*cloudpb.ArtifactManifest, error) {
	resp, err := cloudpb.NewArtifactRegistryServiceClient(op.conn).GetArtifact(ctx, &cloudpb.GetArtifactRequest{OrganizationId: op.org, AppReleaseId: releaseID})
	if err != nil {
		return nil, fmt.Errorf("reading release %s: %w", releaseID, err)
	}
	m := resp.GetManifest()
	sum := sha256.Sum256(m.GetOciManifest())
	if m.GetManifestDigest() != "sha256:"+hex.EncodeToString(sum[:]) {
		return nil, fmt.Errorf("release %s: the served manifest does not match its digest", releaseID)
	}
	if want != "" && m.GetManifestDigest() != want {
		return nil, fmt.Errorf("release %s is %s, not %s", releaseID, m.GetManifestDigest(), want)
	}
	return m, nil
}

// contentKey opens this machine's release-scope wrap of the content key.
func (op *registryOp) contentKey(ctx context.Context, releaseID string, m *cloudpb.ArtifactManifest) ([]byte, error) {
	resp, err := cloudpb.NewArtifactRegistryServiceClient(op.conn).GetMyWraps(ctx, &cloudpb.GetMyWrapsRequest{
		OrganizationId: op.org, Scope: &cloudpb.WrapScope{Scope: &cloudpb.WrapScope_AppReleaseId{AppReleaseId: releaseID}, KeyId: m.GetContentKeyId()},
	})
	if err != nil {
		return nil, fmt.Errorf("reading this machine's wraps of release %s: %w", releaseID, err)
	}
	for _, w := range resp.GetWraps() {
		if w.GetRecipientKeyId() != op.keyID {
			continue
		}
		return op.open(w, registrycrypto.ScopeRelease, releaseID)
	}
	return nil, fmt.Errorf("release %s has no key wrapped to this machine's registry key %s", releaseID, op.keyID)
}

func (op *registryOp) open(w *cloudpb.KeyWrap, scope byte, scopeID string) ([]byte, error) {
	return registrycrypto.Wrap{
		OrganizationID: op.org, Scope: scope, ScopeID: scopeID, KeyID: w.GetScope().GetKeyId(),
		RecipientKind: byte(cloudpb.RecipientKind_RECIPIENT_KIND_USER), Recipient: op.me, RecipientKeyID: op.keyID,
	}.Open(op.key, w.GetEnc(), w.GetCiphertext())
}

// keyedDeployment is a deployment and its deployment key.
type keyedDeployment struct {
	deployment     *cloudpb.Deployment
	manifestDigest string
	key            []byte
	sealed         *cloudpb.SealedContentKey
}

// newDeploymentKey seals the content key under a fresh deployment key and
// wraps that key to this machine and the custodian.
func (op *registryOp) newDeploymentKey(ctx context.Context, d *cloudpb.Deployment, m *cloudpb.ArtifactManifest, contentKey []byte) (*keyedDeployment, error) {
	key := registrycrypto.NewKey()
	sk := registrycrypto.SealedKey{
		OrganizationID: op.org, DeploymentID: d.GetId(), ManifestDigest: m.GetManifestDigest(),
		ContentKeyID: m.GetContentKeyId(), DeploymentKeyID: registrycrypto.KeyID(key),
	}
	nonce, ciphertext, err := sk.Seal(key, contentKey)
	if err != nil {
		return nil, err
	}
	sealed := &cloudpb.SealedContentKey{
		DeploymentId: d.GetId(), ManifestDigest: sk.ManifestDigest, ContentKeyId: sk.ContentKeyID,
		DeploymentKeyId: sk.DeploymentKeyID, Nonce: nonce, Ciphertext: ciphertext,
	}
	wraps, err := op.ownWraps(registrycrypto.ScopeDeployment, d.GetId(), key)
	if err != nil {
		return nil, err
	}
	if err := op.addWraps(ctx, wraps, []*cloudpb.SealedContentKey{sealed}); err != nil {
		return nil, err
	}
	return &keyedDeployment{deployment: d, manifestDigest: m.GetManifestDigest(), key: key, sealed: sealed}, nil
}

// deployPlan is a deploy as the operator confirmed it.
type deployPlan struct {
	appID, releaseID, digest, name, config, note string
	desired                                      cloudpb.Running
	validity                                     time.Duration
	deviceIDs, tags                              []string          // the selection as given
	devices                                      []string          // every device it resolved to, as shown
	moves                                        map[string]string // device UUID -> the deployment it moves out of
}

// deploy creates the deployment at revision 1 and signs each device's
// statement.
func (op *registryOp) deploy(ctx context.Context, p deployPlan) (*cloudpb.Deployment, error) {
	m, err := op.artifact(ctx, p.releaseID, p.digest)
	if err != nil {
		return nil, err
	}
	contentKey, err := op.contentKey(ctx, p.releaseID, m)
	if err != nil {
		return nil, err
	}
	defer clear(contentKey)
	keys, err := op.deviceKeys(ctx, p.devices)
	if err != nil {
		return nil, err
	}
	moves := map[string]string{}
	for dev, from := range p.moves {
		moves[deviceURI(op.org, dev)] = from
	}
	resp := new(cloudpb.DeploymentChangeResponse)
	if err := cloudrequest.Invoke(ctx, op.conn, op.auth, cloudpb.DeploymentService_CreateDeploymentV2_FullMethodName, &cloudpb.CreateDeploymentRequest{
		OrganizationId: op.org, AppReleaseId: p.releaseID, Running: p.desired, TargetDeviceIds: p.deviceIDs, TargetTags: p.tags,
		Name: p.name, ConfigJson: p.config, Note: p.note, MoveDevices: len(moves) > 0, Validity: durationpb.New(p.validity),
	}, resp); err != nil {
		return nil, fmt.Errorf("creating the deployment: %w", err)
	}
	d := resp.GetDeployment()
	if !canonicalUUID(d.GetId()) || d.GetAppReleaseId() != p.releaseID || d.GetAppId() != p.appID || d.GetName() != p.name || d.GetRevision() != 1 ||
		d.GetConfigJson() != p.config || d.GetDesiredState() != p.desired {
		return nil, fmt.Errorf("Cloud created a deployment that differs from the one asked for (%s, revision %d); nothing was sent to devices", d.GetId(), d.GetRevision())
	}
	kd, err := op.newDeploymentKey(ctx, d, m, contentKey)
	if err != nil {
		return d, fmt.Errorf("deployment %s: %w", d.GetId(), err)
	}
	must := map[string]bool{}
	for dev := range keys {
		must[dev] = true
	}
	return d, op.signPending(ctx, confirmed{d: d, kd: kd, appID: p.appID, revision: 1, assign: keys, moves: moves, must: must}, resp.GetToSign(), resp.GetNextPageToken())
}

// releases lists appID's releases, newest first.
func (op *registryOp) releases(ctx context.Context, appID string) ([]*cloudpb.AppRelease, error) {
	var out []*cloudpb.AppRelease
	for {
		stream, err := cloudpb.NewDeploymentServiceClient(op.conn).ListAppReleases(ctx, &cloudpb.ListAppReleasesRequest{OrganizationId: op.org, AppId: appID, Offset: int32(len(out))})
		if err != nil {
			return nil, fmt.Errorf("reading the releases of %s: %w", appID, err)
		}
		var total int32
		before := len(out)
		for {
			page, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("reading the releases of %s: %w", appID, err)
			}
			total = page.GetTotal()
			for _, r := range page.GetAppReleases() {
				if r.GetAppId() == appID {
					out = append(out, r)
				}
			}
		}
		if len(out) == before || int32(len(out)) >= total {
			break
		}
	}
	slices.SortStableFunc(out, func(a, b *cloudpb.AppRelease) int {
		return b.GetCreatedAt().AsTime().Compare(a.GetCreatedAt().AsTime())
	})
	return out, nil
}

// current is a deployment with this machine's newest deployment key that
// has a sealed content key.
func (op *registryOp) current(ctx context.Context, deploymentID string) (*keyedDeployment, error) {
	d, err := cloudpb.NewDeploymentServiceClient(op.conn).GetDeployment(ctx, &cloudpb.GetDeploymentRequest{Id: deploymentID})
	if err != nil {
		return nil, fmt.Errorf("reading deployment %s: %w", deploymentID, err)
	}
	resp, err := cloudpb.NewArtifactRegistryServiceClient(op.conn).GetMyWraps(ctx, &cloudpb.GetMyWrapsRequest{
		OrganizationId: op.org, Scope: &cloudpb.WrapScope{Scope: &cloudpb.WrapScope_DeploymentId{DeploymentId: deploymentID}},
	})
	if err != nil {
		return nil, fmt.Errorf("reading this machine's wraps of deployment %s: %w", deploymentID, err)
	}
	wraps := resp.GetWraps()
	for i := len(wraps) - 1; i >= 0; i-- { // newest last
		w := wraps[i]
		j := slices.IndexFunc(resp.GetSealedContentKeys(), func(s *cloudpb.SealedContentKey) bool { return s.GetDeploymentKeyId() == w.GetScope().GetKeyId() })
		if w.GetRecipientKeyId() != op.keyID || j < 0 {
			continue
		}
		key, err := op.open(w, registrycrypto.ScopeDeployment, deploymentID)
		if err != nil {
			return nil, fmt.Errorf("deployment %s: %w", deploymentID, err)
		}
		// Open the sealed key so the digest signed into assignments is the
		// one it is bound to, not just what Cloud lists beside it.
		sealed := resp.GetSealedContentKeys()[j]
		contentKey, err := registrycrypto.SealedKey{
			OrganizationID: op.org, DeploymentID: deploymentID, ManifestDigest: sealed.GetManifestDigest(),
			ContentKeyID: sealed.GetContentKeyId(), DeploymentKeyID: sealed.GetDeploymentKeyId(),
		}.Open(key, sealed.GetNonce(), sealed.GetCiphertext())
		if err != nil {
			return nil, fmt.Errorf("deployment %s: its sealed release key does not open: %w", deploymentID, err)
		}
		clear(contentKey)
		return &keyedDeployment{deployment: d, manifestDigest: sealed.GetManifestDigest(), key: key, sealed: sealed}, nil
	}
	return nil, fmt.Errorf("deployment %s has no key wrapped to this machine's registry key %s", deploymentID, op.keyID)
}

// confirmed is one revision as the operator confirmed it: every statement
// cloud lists must be part of it.
type confirmed struct {
	d        *cloudpb.Deployment
	kd       *keyedDeployment // nil when no device gets the deployment
	appID    string
	revision int32             // what the statements carry: d's, or the maximum for a terminal removal
	assign   map[string][]byte // device URI -> verified key: may get the deployment
	remove   map[string]bool   // device URIs that may leave it
	moves    map[string]string // device URI -> the deployment of the app it moves out of
	must     map[string]bool   // device URIs cloud must ask for
}

// signPending signs every statement cloud lists (rows, then the pending
// list from next), refusing any the operator did not confirm. Every device
// is tried.
func (op *registryOp) signPending(ctx context.Context, c confirmed, rows []*cloudpb.PendingDeviceStatement, next string) error {
	signed := map[string]bool{}
	var errs []error
	for {
		for _, row := range rows {
			if err := op.signRow(ctx, c, row); err != nil {
				cliLogln("Not sent to %s: %v", row.GetDevice(), err)
				errs = append(errs, fmt.Errorf("%s: %w", row.GetDevice(), err))
				continue
			}
			signed[row.GetDevice()] = true
		}
		if next == "" {
			break
		}
		page, err := cloudpb.NewDeploymentServiceClient(op.conn).ListPendingDeviceStatements(ctx, &cloudpb.ListPendingDeviceStatementsRequest{DeploymentId: c.d.GetId(), PageToken: next})
		if err != nil {
			errs = append(errs, fmt.Errorf("reading the rest of the devices: %w", err))
			break
		}
		rows, next = page.GetStatements(), page.GetNextPageToken()
	}
	for _, dev := range slices.Sorted(maps.Keys(c.must)) {
		if !signed[dev] {
			errs = append(errs, fmt.Errorf("%s: Cloud did not ask for its instruction", dev))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("deployment %s revision %d: %d devices not sent their instruction: %w", c.d.GetId(), c.d.GetRevision(), len(errs), err)
	}
	return nil
}

// signRow signs one listed statement if it is one the operator confirmed.
func (op *registryOp) signRow(ctx context.Context, c confirmed, row *cloudpb.PendingDeviceStatement) error {
	if row.GetDeploymentId() != c.d.GetId() || row.GetRevision() != c.revision {
		return fmt.Errorf("Cloud asked for deployment %s revision %d, not %s revision %d", row.GetDeploymentId(), row.GetRevision(), c.d.GetId(), c.revision)
	}
	switch row.GetKind() {
	case cloudpb.PendingDeviceStatement_KIND_ASSIGN:
		key, ok := c.assign[row.GetDevice()]
		if !ok || c.kd == nil || c.revision == maxRevision {
			return errors.New("Cloud asked to give it the deployment, which was not asked for")
		}
		move := row.GetReplacesDeploymentId() != "" || row.GetReplacesRevision() != 0
		if move && (c.moves[row.GetDevice()] == "" || c.moves[row.GetDevice()] != row.GetReplacesDeploymentId() || row.GetReplacesRevision() < 1 || row.GetReplacesRevision() == maxRevision) {
			return fmt.Errorf("Cloud asked to move it from deployment %s, which was not asked for", row.GetReplacesDeploymentId())
		}
		return op.signAssign(ctx, c.d, c.kd, c.appID, row.GetDevice(), key, row.GetReplacesDeploymentId(), row.GetReplacesRevision())
	case cloudpb.PendingDeviceStatement_KIND_UNASSIGN:
		if !c.remove[row.GetDevice()] {
			return errors.New("Cloud asked to remove it, which was not asked for")
		}
		valid, _ := cloudrequest.ValidUntil(time.Now(), cloudrequest.MaxValidity) // removals: the platform cap, not the tenant's
		var resp cloudpb.DeviceStatementResponse
		if err := cloudrequest.Invoke(ctx, op.conn, op.auth, cloudpb.DeploymentService_UnassignDevice_FullMethodName, &cloudpb.UnassignDeviceRequest{
			OrganizationId: op.org, DeploymentId: c.d.GetId(), Device: row.GetDevice(), Revision: c.revision, ValidUntil: timestamppb.New(valid),
		}, &resp); err != nil {
			return err
		}
		cliLogln("Sent %s its removal from deployment %s (postbox entry %d)", row.GetDevice(), c.d.GetId(), resp.GetPostboxEntryId())
		return nil
	}
	return fmt.Errorf("Cloud asked for a statement of kind %s, which was not asked for", row.GetKind())
}

// maxRevision is the revision of a terminal removal (UnassignDeviceRequest).
const maxRevision = 2147483647

// signAssign signs device's statement for revision d at kd's key, carrying
// the device's own wrap of that key.
func (op *registryOp) signAssign(ctx context.Context, d *cloudpb.Deployment, kd *keyedDeployment, appID, device string, pub []byte, replaces string, replacesRevision int32) error {
	keyID := registrycrypto.KeyID(kd.key)
	w, err := op.wrap(registrycrypto.ScopeDeployment, d.GetId(), keyID, kd.key, cloudpb.RecipientKind_RECIPIENT_KIND_DEVICE, device, pub)
	if err != nil {
		return err
	}
	expires, err := cloudrequest.ValidUntil(time.Now(), d.GetValidity().AsDuration())
	if err != nil {
		return err
	}
	var resp cloudpb.DeviceStatementResponse
	if err := cloudrequest.Invoke(ctx, op.conn, op.auth, cloudpb.DeploymentService_AssignDevice_FullMethodName, &cloudpb.DeviceAssignment{
		OrganizationId: op.org, DeploymentId: d.GetId(), Device: device, AppId: appID, AppReleaseId: d.GetAppReleaseId(),
		ManifestDigest: kd.manifestDigest, DeploymentKeyId: keyID, DesiredState: d.GetDesiredState(), DeviceWrap: w,
		ExpiresAt: timestamppb.New(expires), ConfigJson: d.GetConfigJson(), Revision: d.GetRevision(),
		ReplacesDeploymentId: replaces, ReplacesRevision: replacesRevision,
	}, &resp); err != nil {
		return err
	}
	cliLogln("Sent %s deployment %s revision %d (postbox entry %d)", device, d.GetId(), d.GetRevision(), resp.GetPostboxEntryId())
	return nil
}

// rotate seals the same content key under a new deployment key (Q6: blobs
// are not re-encrypted) and re-signs the named devices' statements at it, at
// the deployment's current revision with its current content.
func (op *registryOp) rotate(ctx context.Context, deploymentID, appID string, devices []string) error {
	old, err := op.current(ctx, deploymentID)
	if err != nil {
		return err
	}
	// Named, never defaulted: the deployment's create-time targets would
	// re-grant devices unassigned since and miss devices assigned since.
	if len(devices) == 0 {
		return usageErrorf("name each device to re-assign with --device")
	}
	d := old.deployment
	if d.GetAppId() != appID {
		return fmt.Errorf("deployment %s is of app %s, not %s (wendy.json)", deploymentID, d.GetAppId(), appID)
	}
	if d.GetRevision() < 1 || d.GetRemovedAt() != nil {
		return fmt.Errorf("deployment %s cannot be changed: it has no current revision", deploymentID)
	}
	keys, err := op.deviceKeys(ctx, devices)
	if err != nil {
		return err
	}
	s := old.sealed
	contentKey, err := registrycrypto.SealedKey{
		OrganizationID: op.org, DeploymentID: deploymentID, ManifestDigest: s.GetManifestDigest(),
		ContentKeyID: s.GetContentKeyId(), DeploymentKeyID: s.GetDeploymentKeyId(),
	}.Open(old.key, s.GetNonce(), s.GetCiphertext())
	if err != nil {
		return fmt.Errorf("deployment %s: opening its sealed release key: %w", deploymentID, err)
	}
	defer clear(contentKey)
	m := &cloudpb.ArtifactManifest{ManifestDigest: s.GetManifestDigest(), ContentKeyId: s.GetContentKeyId()}
	kd, err := op.newDeploymentKey(ctx, d, m, contentKey)
	if err != nil {
		return fmt.Errorf("deployment %s: %w", deploymentID, err)
	}
	cliLogln("Deployment %s now uses key %s", deploymentID, registrycrypto.KeyID(kd.key))
	var errs []error
	for _, device := range slices.Sorted(maps.Keys(keys)) {
		if err := op.signAssign(ctx, d, kd, appID, device, keys[device], "", 0); err != nil {
			cliLogln("Not sent to %s: %v", device, err)
			errs = append(errs, fmt.Errorf("%s: %w", device, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("deployment %s: %d of %d devices not re-assigned: %w", deploymentID, len(errs), len(keys), err)
	}
	return nil
}

func canonicalUUID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}
