package services

import (
	"context"
	"crypto/hpke"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"time"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/registrycrypto"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// The registry pull (registry plan T14): a verified DeviceAssignment's
// release is fetched from Cloud's DeviceArtifactService over the device's own
// mTLS, opened with keys only the operator's signature and this device's key
// can produce, checked against the digests the operator signed and imported
// into containerd. The registry is blind and untrusted: it can withhold
// material, never substitute it.
//
// Fail-safe table (each row is the reported ApplyOutcome; decide writes the
// audit line, and nothing is imported unless every check passed):
//
//	registry refuses (NOT_FOUND: no live assignment, also when it is removed
//	mid-pull; PERMISSION_DENIED, FAILED_PRECONDITION, ...)   PULL_REFUSED
//	assignment check could not be made (UNAVAILABLE), attempt
//	timed out or blob stream cut                             retried with backoff,
//	                                                         resumed at the offset held;
//	                                                         PULL_REFUSED once the
//	                                                         assignment expires
//	manifest, blob list, ciphertext or plaintext digest or
//	size differs from what the operator signed, or a chunk
//	does not authenticate                                    REJECTED_DIGEST
//	device wrap or sealed content key does not open, unknown
//	suite, no device encryption key for this identity        REJECTED_UNWRAP
//	release kind other than CONTAINER, unreadable OCI
//	manifest, local staging or containerd import fails       APPLY_FAILED

const (
	// registryPullDir stages one pull's ciphertext and plaintext under the
	// agent config directory; it is emptied before and after every pull.
	registryPullDir = "registry-pull"
	// materialTimeout bounds GetDeploymentMaterial; blobAttemptTimeout bounds
	// one GetBlob stream, which a later attempt resumes.
	materialTimeout    = 30 * time.Second
	blobAttemptTimeout = 5 * time.Minute
	chunkTagSize       = 16 // AES-256-GCM tag per chunk (registrycrypto)
	mediaTypeDockerV2  = "application/vnd.docker.distribution.manifest.v2+json"
)

// ReleaseImage is a registry release whose every byte verified: the OCI
// manifest the operator signed (by ManifestDigest) and the decrypted blobs it
// names, ready for containerd under the release identity.
type ReleaseImage struct {
	AppID          string
	ReleaseID      string
	ManifestDigest string // "sha256:<hex>"
	Manifest       []byte // byte-exact; its SHA-256 is ManifestDigest
	// Blobs maps each config and layer digest of Manifest to the file that
	// holds its verified plaintext.
	Blobs map[string]string
}

// ReleaseImporter is the containerd capability that imports a verified
// release (containerd.Client.ImportRelease).
type ReleaseImporter interface {
	ImportRelease(ctx context.Context, img ReleaseImage) error
}

// pullFailure carries the outcome and audit reason of a failed pull.
type pullFailure struct {
	outcome cloudpbv2.ApplyOutcome
	reason  string
	err     error
}

func (f *pullFailure) Error() string { return f.err.Error() }
func (f *pullFailure) Unwrap() error { return f.err }

func pullRefused(reason string, err error) error {
	return &pullFailure{cloudpbv2.ApplyOutcome_APPLY_OUTCOME_PULL_REFUSED, reason, err}
}

func digestRefused(err error) error {
	return &pullFailure{cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_DIGEST, "digest", err}
}

func unwrapRefused(err error) error {
	return &pullFailure{cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_UNWRAP, "unwrap", err}
}

func pullFailed(reason string, err error) error {
	return &pullFailure{cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, reason, err}
}

// errStreamCut is a GetBlob stream that ended before the blob did.
var errStreamCut = errors.New("blob stream ended before the blob")

func dialDeviceArtifacts(cloudURL, certPEM, keyPEM, chainPEM string) (cloudpbv2.DeviceArtifactServiceClient, io.Closer, error) {
	conn, closer, err := dialDeviceCloud(cloudURL, certPEM, keyPEM, chainPEM)
	if err != nil {
		return nil, nil, err
	}
	return cloudpbv2.NewDeviceArtifactServiceClient(conn), closer, nil
}

// pullAssignment fetches, opens, verifies and imports a verified assignment's
// release. An ABSENT assignment is held with nothing to fetch.
func (p *Postbox) pullAssignment(ctx context.Context, id *postboxIdentity, a *cloudpbv2.DeviceAssignment) (cloudpbv2.ApplyOutcome, string, error) {
	if a.GetDesiredState() == cloudpbv2.Running_ABSENT {
		return cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED, "", nil
	}
	dir := filepath.Join(p.configPath, registryPullDir)
	// The postbox applies one entry at a time: anything here is an
	// interrupted pull's, whose entry is settled and never retried.
	if err := os.RemoveAll(dir); err != nil {
		return cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "stage", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "stage", err
	}
	img, err := p.fetchRelease(ctx, id, a, dir)
	if err == nil {
		if err = p.control.ImportRelease(ctx, img); err != nil {
			err = pullFailed("import", err)
		}
	}
	if err != nil {
		var f *pullFailure
		if errors.As(err, &f) {
			return f.outcome, f.reason, err
		}
		return cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED, "pull", err
	}
	return cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED, "", nil
}

func (p *Postbox) fetchRelease(ctx context.Context, id *postboxIdentity, a *cloudpbv2.DeviceAssignment, dir string) (ReleaseImage, error) {
	p.mu.Lock()
	cloudURL, certPEM, keyPEM, chainPEM := p.cloudURL, p.certPEM, p.keyPEM, p.chainPEM
	p.mu.Unlock()
	client, closer, err := p.dialArtifacts(cloudURL, certPEM, keyPEM, chainPEM)
	if err != nil {
		return ReleaseImage{}, pullRefused("dial", err)
	}
	defer func() { _ = closer.Close() }()

	var mat *cloudpbv2.GetDeploymentMaterialResponse
	err = p.pullRetry(ctx, a, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, materialTimeout)
		defer cancel()
		var err error
		mat, err = client.GetDeploymentMaterial(ctx, &cloudpbv2.GetDeploymentMaterialRequest{DeploymentId: a.GetDeploymentId()})
		return err
	})
	if err != nil {
		return ReleaseImage{}, err
	}
	m := mat.GetManifest()
	blobs, chunkSize, err := releaseBlobs(a, m)
	if err != nil {
		return ReleaseImage{}, err
	}
	priv, err := p.encryptionKey(id.uri)
	if err != nil {
		return ReleaseImage{}, unwrapRefused(fmt.Errorf("device encryption key: %w", err))
	}
	deploymentKey, err := openDeploymentKey(priv, id.tenant, id.uri, a)
	if err != nil {
		return ReleaseImage{}, unwrapRefused(err)
	}
	contentKey, err := openContentKey(deploymentKey, id.tenant, a, m.GetContentKeyId(), mat.GetSealedContentKey())
	clear(deploymentKey)
	if err != nil {
		return ReleaseImage{}, unwrapRefused(err)
	}
	defer clear(contentKey)

	img := ReleaseImage{
		AppID: a.GetAppId(), ReleaseID: a.GetAppReleaseId(), ManifestDigest: a.GetManifestDigest(),
		Manifest: m.GetOciManifest(), Blobs: map[string]string{},
	}
	for i, b := range blobs {
		ct := filepath.Join(dir, strconv.Itoa(i)+".enc")
		pt := filepath.Join(dir, strconv.Itoa(i))
		if err := p.pullRetry(ctx, a, func(ctx context.Context) error {
			return fetchBlob(ctx, client, a.GetDeploymentId(), b, ct)
		}); err != nil {
			return ReleaseImage{}, err
		}
		if err := decryptStaged(ct, pt, contentKey, b, chunkSize); err != nil {
			return ReleaseImage{}, err
		}
		img.Blobs[b.GetPlaintextDigest()] = pt
	}
	return img, nil
}

// pullRetry runs call until it succeeds or fails for good. A transient
// failure (the registry could not make the assignment check, an attempt timed
// out, a stream was cut) backs off with jitter and retries until the
// assignment's signed validity ends: availability degrades, the check is
// never skipped. Any other registry status is its refusal.
func (p *Postbox) pullRetry(ctx context.Context, a *cloudpbv2.DeviceAssignment, call func(context.Context) error) error {
	for failures := 0; ; failures++ {
		err := call(ctx)
		if err == nil {
			return nil
		}
		var f *pullFailure
		switch {
		case errors.As(err, &f):
			return err
		case ctx.Err() != nil:
			return pullFailed("pull", err)
		case status.Code(err) != codes.Unavailable && status.Code(err) != codes.DeadlineExceeded && !errors.Is(err, errStreamCut):
			return pullRefused("pull", err)
		case !p.trustedNow().Before(a.GetExpiresAt().AsTime()):
			return pullRefused("unavailable", fmt.Errorf("assignment expired while the registry was unavailable: %w", err))
		}
		wait := min(p.retry<<min(failures, 6), 30*time.Minute)
		wait = wait/2 + rand.N(wait/2+1) // a fleet does not retry in one wave
		p.logger.Warn("registry pull unavailable; retrying", zap.String("audit", "postbox"),
			zap.String("deployment_id", a.GetDeploymentId()), zap.String("app_id", a.GetAppId()),
			zap.Duration("retry_in", wait), zap.Error(err))
		select {
		case <-ctx.Done():
			return pullFailed("pull", ctx.Err())
		case <-time.After(wait):
		}
	}
}

// releaseBlobs checks the served manifest against the signed assignment
// before anything else is used: the manifest digest must be the signed one,
// and the blob list must be exactly the OCI manifest's config and layers with
// their sizes, each with the ciphertext size its chunking implies. Only then
// are the served sizes allowed to size a download.
func releaseBlobs(a *cloudpbv2.DeviceAssignment, m *cloudpbv2.ArtifactManifest) ([]*cloudpbv2.EncryptedBlob, int, error) {
	if m.GetManifestDigest() != a.GetManifestDigest() || digest.FromBytes(m.GetOciManifest()).String() != a.GetManifestDigest() {
		return nil, 0, digestRefused(fmt.Errorf("served manifest is not the signed release %s", a.GetManifestDigest()))
	}
	if m.GetKind() != cloudpbv2.ReleaseType_CONTAINER {
		return nil, 0, pullFailed("kind", fmt.Errorf("release kind %v is not a container image", m.GetKind()))
	}
	if m.GetAead() != cloudpbv2.AeadSuite_AEAD_SUITE_AES_256_GCM_CHUNKED {
		return nil, 0, unwrapRefused(fmt.Errorf("unknown AEAD suite %v", m.GetAead()))
	}
	chunkSize := int(m.GetChunkSize())
	if chunkSize == 0 {
		chunkSize = registrycrypto.DefaultChunkSize
	}
	if chunkSize > registrycrypto.MaxChunkSize {
		return nil, 0, digestRefused(fmt.Errorf("%w: %d", registrycrypto.ErrChunkSize, chunkSize))
	}
	var om ocispec.Manifest
	if err := json.Unmarshal(m.GetOciManifest(), &om); err != nil {
		return nil, 0, pullFailed("manifest", fmt.Errorf("reading the OCI manifest: %w", err))
	}
	if om.MediaType != ocispec.MediaTypeImageManifest && om.MediaType != mediaTypeDockerV2 {
		return nil, 0, pullFailed("manifest", fmt.Errorf("manifest media type %q is not an image manifest", om.MediaType))
	}
	want := map[string]int64{}
	for _, d := range append([]ocispec.Descriptor{om.Config}, om.Layers...) {
		want[d.Digest.String()] = d.Size
	}
	if len(m.GetBlobs()) != len(want) {
		return nil, 0, digestRefused(fmt.Errorf("served %d blobs for the %d the manifest names", len(m.GetBlobs()), len(want)))
	}
	seen := map[string]bool{}
	for _, b := range m.GetBlobs() {
		size, ok := want[b.GetPlaintextDigest()]
		switch {
		case !ok || seen[b.GetPlaintextDigest()] || size < 0 || uint64(size) != b.GetPlaintextSize():
			return nil, 0, digestRefused(fmt.Errorf("served blob %s is not one the manifest names", b.GetPlaintextDigest()))
		case b.GetCiphertextSize() != ciphertextSize(b.GetPlaintextSize(), chunkSize):
			return nil, 0, digestRefused(fmt.Errorf("blob %s ciphertext size %d does not fit its plaintext size", b.GetPlaintextDigest(), b.GetCiphertextSize()))
		}
		seen[b.GetPlaintextDigest()] = true
	}
	return m.GetBlobs(), chunkSize, nil
}

// ciphertextSize is a blob's size after chunked AEAD: one tag per chunk, and
// an empty blob is one empty chunk.
func ciphertextSize(plaintext uint64, chunkSize int) uint64 {
	chunks := max(1, (plaintext+uint64(chunkSize)-1)/uint64(chunkSize))
	return plaintext + chunks*chunkTagSize
}

// openDeploymentKey unwraps the assignment's device wrap; openContentKey
// opens the served sealed content key with the result. Every binding comes
// from the signed assignment and this device's own identity and key, never
// from the served material, so a wrap or seal for another tenant, deployment,
// key, manifest or device does not open.
func openDeploymentKey(priv hpke.PrivateKey, tenant, device string, a *cloudpbv2.DeviceAssignment) ([]byte, error) {
	w := a.GetDeviceWrap()
	if w.GetSuite() != cloudpbv2.KemSuite_KEM_SUITE_MLKEM768_X25519 {
		return nil, fmt.Errorf("device wrap suite %v", w.GetSuite())
	}
	key, err := registrycrypto.Wrap{
		OrganizationID: tenant, Scope: registrycrypto.ScopeDeployment, ScopeID: a.GetDeploymentId(),
		KeyID: a.GetDeploymentKeyId(), RecipientKind: byte(cloudpbv2.RecipientKind_RECIPIENT_KIND_DEVICE),
		Recipient: device, RecipientKeyID: registrycrypto.KeyID(priv.PublicKey().Bytes()),
	}.Open(priv, w.GetEnc(), w.GetCiphertext())
	if err != nil {
		return nil, fmt.Errorf("device wrap: %w", err)
	}
	return key, nil
}

func openContentKey(deploymentKey []byte, tenant string, a *cloudpbv2.DeviceAssignment, contentKeyID string, sealed *cloudpbv2.SealedContentKey) ([]byte, error) {
	key, err := registrycrypto.SealedKey{
		OrganizationID: tenant, DeploymentID: a.GetDeploymentId(), ManifestDigest: a.GetManifestDigest(),
		ContentKeyID: contentKeyID, DeploymentKeyID: a.GetDeploymentKeyId(),
	}.Open(deploymentKey, sealed.GetNonce(), sealed.GetCiphertext())
	if err != nil {
		return nil, fmt.Errorf("sealed content key: %w", err)
	}
	return key, nil
}

// fetchBlob downloads b's ciphertext to path, resuming at what path already
// holds. It never writes past the blob's size.
func fetchBlob(ctx context.Context, client cloudpbv2.DeviceArtifactServiceClient, deploymentID string, b *cloudpbv2.EncryptedBlob, path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return pullFailed("stage", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return pullFailed("stage", err)
	}
	offset, size := uint64(info.Size()), b.GetCiphertextSize()
	if offset == size {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, blobAttemptTimeout)
	defer cancel()
	stream, err := client.GetBlob(ctx, &cloudpbv2.GetBlobRequest{DeploymentId: deploymentID, CiphertextDigest: b.GetCiphertextDigest(), Offset: offset})
	if err != nil {
		return err
	}
	for offset < size {
		msg, err := stream.Recv()
		if err == io.EOF {
			return errStreamCut
		}
		if err != nil {
			return err
		}
		data := msg.GetData()
		if uint64(len(data)) > size-offset {
			return digestRefused(fmt.Errorf("registry served more than blob %s's %d bytes", b.GetCiphertextDigest(), size))
		}
		if _, err := f.Write(data); err != nil {
			return pullFailed("stage", err)
		}
		offset += uint64(len(data))
	}
	return nil
}

// decryptStaged checks the staged ciphertext's digest, then decrypts it to pt
// (which DecryptBlob checks against the plaintext digest and size), and drops
// the ciphertext.
func decryptStaged(ct, pt string, contentKey []byte, b *cloudpbv2.EncryptedBlob, chunkSize int) error {
	in, err := os.Open(ct)
	if err != nil {
		return pullFailed("stage", err)
	}
	defer func() { _ = in.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, in); err != nil {
		return pullFailed("stage", err)
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != b.GetCiphertextDigest() {
		return digestRefused(fmt.Errorf("blob ciphertext is %s, the manifest names %s", got, b.GetCiphertextDigest()))
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return pullFailed("stage", err)
	}
	out, err := os.OpenFile(pt, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return pullFailed("stage", err)
	}
	defer func() { _ = out.Close() }()
	err = registrycrypto.DecryptBlob(out, in, contentKey, registrycrypto.Blob{
		PlaintextDigest: b.GetPlaintextDigest(), CiphertextDigest: b.GetCiphertextDigest(),
		PlaintextSize: b.GetPlaintextSize(), CiphertextSize: b.GetCiphertextSize(),
	}, chunkSize)
	var pathErr *fs.PathError
	switch {
	case errors.As(err, &pathErr):
		return pullFailed("stage", err)
	case err != nil:
		return digestRefused(fmt.Errorf("blob %s: %w", b.GetPlaintextDigest(), err))
	}
	if err := out.Close(); err != nil {
		return pullFailed("stage", err)
	}
	return os.Remove(ct)
}

// loadDeviceEncryptionKey is principal's stored registry encryption key
// (T10). The pull never creates one: a wrap is only ever made to a key Cloud
// registered.
func loadDeviceEncryptionKey(configPath, principal string) (hpke.PrivateKey, error) {
	raw, err := os.ReadFile(filepath.Join(configPath, deviceEncryptionKeyFile))
	if err != nil {
		return nil, err
	}
	var stored storedEncryptionKey
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, err
	}
	if stored.Principal != principal {
		return nil, errors.New("the stored key belongs to another enrollment")
	}
	return registrycrypto.KEM.NewPrivateKey(stored.PrivateKey)
}
