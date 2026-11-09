package services

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/wendylabsinc/wendy/go/internal/shared/registrycrypto"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// testRelease is what an operator client publishes and assigns: an OCI image
// encrypted under a content key, sealed under a deployment key wrapped to the
// device (the CLI half, T12, does the same with registrycrypto).
type testRelease struct {
	assign    *cloudpbv2.DeviceAssignment
	material  *cloudpbv2.GetDeploymentMaterialResponse
	plain     map[string][]byte // plaintext digest -> bytes
	cipher    map[string][]byte // ciphertext digest -> bytes
	chunkSize int
}

func (f *pbFixture) release(t *testing.T) *testRelease {
	t.Helper()
	return f.releaseOf(t, randomBytes(10000), randomBytes(0))
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// releaseOf publishes an image with these layers, in order. As the CLI
// publisher does, the blob list names each digest once: config first, then
// layers in manifest order.
func (f *pbFixture) releaseOf(t *testing.T, layers ...[]byte) *testRelease {
	t.Helper()
	priv, err := loadOrCreateDeviceEncryptionKey(f.dir, f.uri)
	if err != nil {
		t.Fatal(err)
	}
	r := &testRelease{plain: map[string][]byte{}, cipher: map[string][]byte{}, chunkSize: minChunkSize}
	add := func(mediaType string, data []byte) ocispec.Descriptor {
		r.plain[digest.FromBytes(data).String()] = data
		return ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
	}
	om := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    add(ocispec.MediaTypeImageConfig, []byte(`{"architecture":"arm64","os":"linux"}`)),
	}
	for _, l := range layers {
		om.Layers = append(om.Layers, add(ocispec.MediaTypeImageLayer, l))
	}
	om.SchemaVersion = 2
	oci, err := json.Marshal(om)
	if err != nil {
		t.Fatal(err)
	}
	contentKey, deploymentKey := registrycrypto.NewKey(), registrycrypto.NewKey()
	m := &cloudpbv2.ArtifactManifest{
		ManifestDigest: sha256Digest(oci), OciManifest: oci, Kind: cloudpbv2.ReleaseType_CONTAINER,
		ContentKeyId: registrycrypto.KeyID(contentKey), Aead: cloudpbv2.AeadSuite_AEAD_SUITE_AES_256_GCM_CHUNKED,
		ChunkSize: uint32(r.chunkSize),
	}
	listed := map[digest.Digest]bool{}
	for _, d := range append([]ocispec.Descriptor{om.Config}, om.Layers...) {
		if listed[d.Digest] {
			continue
		}
		listed[d.Digest] = true
		var ct bytes.Buffer
		b, err := registrycrypto.EncryptBlob(&ct, bytes.NewReader(r.plain[d.Digest.String()]), contentKey, d.Digest.String(), r.chunkSize)
		if err != nil {
			t.Fatal(err)
		}
		r.cipher[b.CiphertextDigest] = ct.Bytes()
		m.Blobs = append(m.Blobs, &cloudpbv2.EncryptedBlob{
			PlaintextDigest: b.PlaintextDigest, CiphertextDigest: b.CiphertextDigest,
			PlaintextSize: b.PlaintextSize, CiphertextSize: b.CiphertextSize,
		})
	}
	dep := uuid.NewString()
	r.assign = &cloudpbv2.DeviceAssignment{
		OrganizationId: f.tenant, DeploymentId: dep, Device: f.uri, AppId: "web", AppReleaseId: uuid.NewString(),
		ManifestDigest: m.GetManifestDigest(), DeploymentKeyId: registrycrypto.KeyID(deploymentKey),
		DesiredState: cloudpbv2.Running_RUNNING, ExpiresAt: timestamppb.New(f.now.Add(time.Hour)),
	}
	pub := priv.PublicKey().Bytes()
	wrap := registrycrypto.Wrap{
		OrganizationID: f.tenant, Scope: registrycrypto.ScopeDeployment, ScopeID: dep, KeyID: r.assign.GetDeploymentKeyId(),
		RecipientKind: byte(cloudpbv2.RecipientKind_RECIPIENT_KIND_DEVICE), Recipient: f.uri, RecipientKeyID: registrycrypto.KeyID(pub),
	}
	enc, ct, err := wrap.Seal(pub, deploymentKey)
	if err != nil {
		t.Fatal(err)
	}
	r.assign.DeviceWrap = &cloudpbv2.KeyWrap{
		Scope:         &cloudpbv2.WrapScope{Scope: &cloudpbv2.WrapScope_DeploymentId{DeploymentId: dep}, KeyId: wrap.KeyID},
		RecipientKind: cloudpbv2.RecipientKind_RECIPIENT_KIND_DEVICE, Recipient: f.uri, RecipientKeyId: wrap.RecipientKeyID,
		Suite: cloudpbv2.KemSuite_KEM_SUITE_MLKEM768_X25519, Enc: enc, Ciphertext: ct,
	}
	sealed := registrycrypto.SealedKey{
		OrganizationID: f.tenant, DeploymentID: dep, ManifestDigest: m.GetManifestDigest(),
		ContentKeyID: m.GetContentKeyId(), DeploymentKeyID: r.assign.GetDeploymentKeyId(),
	}
	nonce, sct, err := sealed.Seal(deploymentKey, contentKey)
	if err != nil {
		t.Fatal(err)
	}
	r.material = &cloudpbv2.GetDeploymentMaterialResponse{Manifest: m, SealedContentKey: &cloudpbv2.SealedContentKey{
		DeploymentId: dep, ManifestDigest: m.GetManifestDigest(), ContentKeyId: m.GetContentKeyId(),
		DeploymentKeyId: r.assign.GetDeploymentKeyId(), Nonce: nonce, Ciphertext: sct,
	}}
	return r
}

// fakeArtifacts is Cloud's DeviceArtifactService: it serves the release and
// lets a test fail any call.
type fakeArtifacts struct {
	r *testRelease
	// materialErr / blobErr, when set, answer a call before serving it (the
	// call counts from 1); cut ends one stream after that many bytes.
	materialErr func(call int) error
	blobErr     func(call int, req *cloudpbv2.GetBlobRequest) error
	cut         map[string]int

	mu            sync.Mutex
	materialCalls int
	blobCalls     int
	offsets       map[string][]uint64
}

func (a *fakeArtifacts) GetDeploymentMaterial(_ context.Context, in *cloudpbv2.GetDeploymentMaterialRequest, _ ...grpc.CallOption) (*cloudpbv2.GetDeploymentMaterialResponse, error) {
	a.mu.Lock()
	a.materialCalls++
	call := a.materialCalls
	a.mu.Unlock()
	if a.materialErr != nil {
		if err := a.materialErr(call); err != nil {
			return nil, err
		}
	}
	if in.GetDeploymentId() != a.r.assign.GetDeploymentId() {
		return nil, status.Error(codes.NotFound, "no live assignment of this deployment")
	}
	return a.r.material, nil
}

func (a *fakeArtifacts) GetBlob(_ context.Context, in *cloudpbv2.GetBlobRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[cloudpbv2.GetBlobResponse], error) {
	a.mu.Lock()
	a.blobCalls++
	call := a.blobCalls
	if a.offsets == nil {
		a.offsets = map[string][]uint64{}
	}
	a.offsets[in.GetCiphertextDigest()] = append(a.offsets[in.GetCiphertextDigest()], in.GetOffset())
	cut, cutNow := a.cut[in.GetCiphertextDigest()]
	delete(a.cut, in.GetCiphertextDigest())
	a.mu.Unlock()
	if a.blobErr != nil {
		if err := a.blobErr(call, in); err != nil {
			return nil, err
		}
	}
	data, ok := a.r.cipher[in.GetCiphertextDigest()]
	if !ok || in.GetDeploymentId() != a.r.assign.GetDeploymentId() {
		return nil, status.Error(codes.NotFound, "no live assignment of this deployment")
	}
	s := &blobStream{data: data[in.GetOffset():], piece: 1000}
	if cutNow {
		s.data, s.end = s.data[:cut], status.Error(codes.Unavailable, "connection reset")
	}
	return s, nil
}

type blobStream struct {
	grpc.ClientStream // unused
	data              []byte
	piece             int
	end               error // after data; nil = io.EOF
}

func (s *blobStream) Recv() (*cloudpbv2.GetBlobResponse, error) {
	if len(s.data) == 0 {
		if s.end != nil {
			return nil, s.end
		}
		return nil, io.EOF
	}
	n := min(s.piece, len(s.data))
	out := &cloudpbv2.GetBlobResponse{Data: s.data[:n]}
	s.data = s.data[n:]
	return out, nil
}

// importRecorder is the container control with a containerd import that
// records what it was given, reading the staged blobs while they exist.
type importRecorder struct {
	postboxControl
	imported []ReleaseImage
	blobs    map[string][]byte
	err      error
}

func (r *importRecorder) ImportRelease(_ context.Context, img ReleaseImage) error {
	r.imported = append(r.imported, img)
	r.blobs = map[string][]byte{}
	for d, path := range img.Blobs {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		r.blobs[d] = b
	}
	return r.err
}

func (crashingControl) ImportRelease(context.Context, ReleaseImage) error { panic("agent killed") }

// refuseArtifacts is the fixture's default registry: no live assignment.
func refuseArtifacts(string, string, string, string) (cloudpbv2.DeviceArtifactServiceClient, io.Closer, error) {
	return &fakeArtifacts{r: &testRelease{assign: &cloudpbv2.DeviceAssignment{}}}, io.NopCloser(nil), nil
}

func (f *pbFixture) withArtifacts(a *fakeArtifacts) *importRecorder {
	rec := &importRecorder{postboxControl: f.p.control}
	f.p.control = rec
	f.p.retry = time.Millisecond
	f.p.dialArtifacts = func(string, string, string, string) (cloudpbv2.DeviceArtifactServiceClient, io.Closer, error) {
		return a, io.NopCloser(nil), nil
	}
	return rec
}

func (f *pbFixture) assignEntry(a *cloudpbv2.DeviceAssignment) {
	resource := "org/" + f.tenant + "/deployment/" + a.GetDeploymentId() + "/device/" + f.uuid
	jws, payload, typ := f.sign(cloudpbv2.ArtifactRegistryService_AssignDevice_FullMethodName, a, resource, []*x509.Certificate{f.opLeaf, f.opCA}, f.opKey, nil)
	f.cloud.add(jws, payload, typ)
}

func TestRegistryPullImportsAVerifiedReleaseAndResumesACutBlob(t *testing.T) {
	f := newPBFixture(t)
	r := f.release(t)
	big := r.material.GetManifest().GetBlobs()[1] // the 10000-byte layer: 3 chunks
	arts := &fakeArtifacts{r: r, cut: map[string]int{big.GetCiphertextDigest(): 5000}}
	rec := f.withArtifacts(arts)
	f.assignEntry(r.assign)
	f.cycle()

	if o := f.outcome("web"); o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED {
		t.Fatalf("outcome = %+v, want APPLIED", o)
	}
	if len(rec.imported) != 1 {
		t.Fatalf("imports = %d, want 1", len(rec.imported))
	}
	img := rec.imported[0]
	if img.AppID != "web" || img.ReleaseID != r.assign.GetAppReleaseId() || img.ManifestDigest != r.assign.GetManifestDigest() ||
		!bytes.Equal(img.Manifest, r.material.GetManifest().GetOciManifest()) {
		t.Fatalf("imported %+v, want the signed release identity", img)
	}
	for d, want := range r.plain {
		if !bytes.Equal(rec.blobs[d], want) {
			t.Fatalf("blob %s imported as %x, want %x", d, rec.blobs[d], want)
		}
	}
	if got := arts.offsets[big.GetCiphertextDigest()]; !slices.Equal(got, []uint64{0, 5000}) {
		t.Fatalf("GetBlob offsets = %v, want a resume at the cut [0 5000]", got)
	}
	if _, err := os.Stat(filepath.Join(f.dir, registryPullDir)); !os.IsNotExist(err) {
		t.Fatalf("staging directory left behind: %v", err)
	}
}

// A layer the image manifest names twice is one blob in the release: it is
// fetched once and imported under its digest.
func TestRegistryPullRepeatedLayerIsOneBlob(t *testing.T) {
	f := newPBFixture(t)
	shared := randomBytes(5000)
	r := f.releaseOf(t, shared, randomBytes(300), shared)
	if n := len(r.material.GetManifest().GetBlobs()); n != 3 {
		t.Fatalf("fixture lists %d blobs, want 3 (config and two distinct layers)", n)
	}
	arts := &fakeArtifacts{r: r}
	rec := f.withArtifacts(arts)
	f.assignEntry(r.assign)
	f.cycle()
	if o := f.outcome("web"); o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED || len(rec.imported) != 1 {
		t.Fatalf("outcome %v, %d imports: want the release imported", o.Outcome, len(rec.imported))
	}
	if !bytes.Equal(rec.blobs[digest.FromBytes(shared).String()], shared) || len(rec.blobs) != 3 || arts.blobCalls != 3 {
		t.Fatalf("imported %d blobs after %d fetches: want the repeated layer fetched and imported once", len(rec.blobs), arts.blobCalls)
	}
}

func TestRegistryPullRefusesBeforeImport(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(f *pbFixture, r *testRelease)
		want   cloudpbv2.ApplyOutcome
	}{
		{"ciphertext digest mismatch", func(_ *pbFixture, r *testRelease) {
			b := r.material.GetManifest().GetBlobs()[1]
			r.cipher[b.GetCiphertextDigest()] = slices.Clone(r.cipher[b.GetCiphertextDigest()])
			r.cipher[b.GetCiphertextDigest()][3] ^= 1
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_DIGEST},
		{"chunk does not authenticate under a matching ciphertext digest", func(_ *pbFixture, r *testRelease) {
			b := r.material.GetManifest().GetBlobs()[1]
			ct := slices.Clone(r.cipher[b.GetCiphertextDigest()])
			ct[3] ^= 1
			b.CiphertextDigest = sha256Digest(ct)
			r.cipher[b.CiphertextDigest] = ct
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_DIGEST},
		{"served manifest is not the signed one", func(_ *pbFixture, r *testRelease) {
			m := proto.Clone(r.material.GetManifest()).(*cloudpbv2.ArtifactManifest)
			m.OciManifest = append(slices.Clone(m.GetOciManifest()), ' ')
			m.ManifestDigest = sha256Digest(m.GetOciManifest())
			r.material.Manifest = m
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_DIGEST},
		{"blob the manifest does not name", func(_ *pbFixture, r *testRelease) {
			r.material.GetManifest().GetBlobs()[2].PlaintextDigest = sha256Digest([]byte("other"))
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_DIGEST},
		{"ciphertext size the chunking does not imply", func(_ *pbFixture, r *testRelease) {
			r.material.GetManifest().GetBlobs()[1].CiphertextSize += 1 << 30
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_DIGEST},
		{"registry streams past the blob", func(_ *pbFixture, r *testRelease) {
			b := r.material.GetManifest().GetBlobs()[1]
			r.cipher[b.GetCiphertextDigest()] = append(slices.Clone(r.cipher[b.GetCiphertextDigest()]), 0)
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_DIGEST},
		{"sealed key for another manifest", func(_ *pbFixture, r *testRelease) {
			r.material.GetManifest().ContentKeyId = registrycrypto.KeyID(registrycrypto.NewKey())
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_UNWRAP},
		{"wrap for another device key", func(f *pbFixture, _ *testRelease) {
			if err := os.Remove(filepath.Join(f.dir, deviceEncryptionKeyFile)); err != nil {
				f.t.Fatal(err)
			}
			if _, err := loadOrCreateDeviceEncryptionKey(f.dir, f.uri); err != nil {
				f.t.Fatal(err)
			}
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_UNWRAP},
		{"no device encryption key", func(f *pbFixture, _ *testRelease) {
			if err := os.Remove(filepath.Join(f.dir, deviceEncryptionKeyFile)); err != nil {
				f.t.Fatal(err)
			}
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_UNWRAP},
		{"chunk size below the floor", func(_ *pbFixture, r *testRelease) {
			m := r.material.GetManifest()
			m.ChunkSize = 1 // re-chunking inflates the download 17x
			for _, b := range m.GetBlobs() {
				b.CiphertextSize = b.GetPlaintextSize() * 17
			}
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_DIGEST},
		{"not a container release", func(_ *pbFixture, r *testRelease) {
			r.material.GetManifest().Kind = cloudpbv2.ReleaseType_WASM
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPBFixture(t)
			r := f.release(t)
			tc.mutate(f, r)
			rec := f.withArtifacts(&fakeArtifacts{r: r})
			f.assignEntry(r.assign)
			f.cycle()
			if o := f.outcome("web"); o.Outcome != tc.want {
				t.Fatalf("outcome = %v, want %v", o.Outcome, tc.want)
			}
			if len(rec.imported) != 0 {
				t.Fatal("a refused release reached containerd")
			}
		})
	}
}

func TestRegistryPullAssignmentRemovedMidPull(t *testing.T) {
	f := newPBFixture(t)
	r := f.release(t)
	// Cloud's live check fails once the operator unassigns: every later call
	// is NOT_FOUND, the same answer as for a deployment never assigned.
	arts := &fakeArtifacts{r: r, blobErr: func(call int, _ *cloudpbv2.GetBlobRequest) error {
		if call > 1 {
			return status.Error(codes.NotFound, "no live assignment of this deployment")
		}
		return nil
	}}
	rec := f.withArtifacts(arts)
	f.assignEntry(r.assign)
	f.cycle()
	if o := f.outcome("web"); o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_PULL_REFUSED {
		t.Fatalf("outcome = %v, want PULL_REFUSED", o.Outcome)
	}
	if len(rec.imported) != 0 || arts.blobCalls != 2 {
		t.Fatalf("imports = %d, blob calls = %d: want nothing imported after the refusal", len(rec.imported), arts.blobCalls)
	}
	if f.p.refusals["pull"] != 1 {
		t.Fatalf("refusals = %v, want the pull refusal counted", f.p.refusals)
	}
}

func TestRegistryPullRetriesUnavailableUntilTheCheckRuns(t *testing.T) {
	f := newPBFixture(t)
	r := f.release(t)
	arts := &fakeArtifacts{r: r, materialErr: func(call int) error {
		if call < 3 {
			return status.Error(codes.Unavailable, "the assignment check could not be made; retry")
		}
		return nil
	}}
	rec := f.withArtifacts(arts)
	f.assignEntry(r.assign)
	f.cycle()
	if o := f.outcome("web"); o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED || arts.materialCalls != 3 || len(rec.imported) != 1 {
		t.Fatalf("outcome %v after %d calls, %d imports: want APPLIED on the third", o.Outcome, arts.materialCalls, len(rec.imported))
	}
}

func TestRegistryPullGivesUpWhenTheAssignmentExpires(t *testing.T) {
	f := newPBFixture(t)
	r := f.release(t)
	arts := &fakeArtifacts{r: r, materialErr: func(int) error {
		f.now = f.now.Add(time.Hour) // the outage outlives the assignment
		return status.Error(codes.Unavailable, "the assignment check could not be made; retry")
	}}
	rec := f.withArtifacts(arts)
	f.assignEntry(r.assign)
	f.cycle()
	if o := f.outcome("web"); o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_PULL_REFUSED || len(rec.imported) != 0 {
		t.Fatalf("outcome = %v, imports = %d: want PULL_REFUSED, nothing imported", o.Outcome, len(rec.imported))
	}
}

func TestRegistryPullImportFailureIsApplyFailed(t *testing.T) {
	f := newPBFixture(t)
	r := f.release(t)
	rec := f.withArtifacts(&fakeArtifacts{r: r})
	rec.err = errors.New("content store full")
	f.assignEntry(r.assign)
	f.cycle()
	if o := f.outcome("web"); o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED {
		t.Fatalf("outcome = %v, want APPLY_FAILED", o.Outcome)
	}
}

// TestRegistryPullConformanceVectors runs the service-protos registry vectors
// through the agent's own openers: the device wrap and sealed content key are
// bound by what the assignment and this device's identity say, and each
// vector made under another binding is refused.
func TestRegistryPullConformanceVectors(t *testing.T) {
	raw, err := os.ReadFile("../../shared/registrycrypto/testdata/registry-vectors-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	type binding struct {
		OrganizationID  string `json:"organization_id"`
		ScopeKind       byte   `json:"scope_kind"`
		ScopeID         string `json:"scope_id"`
		KeyID           string `json:"key_id"`
		RecipientKind   byte   `json:"recipient_kind"`
		Recipient       string `json:"recipient"`
		RecipientKeyID  string `json:"recipient_key_id"`
		DeploymentID    string `json:"deployment_id"`
		ManifestDigest  string `json:"manifest_digest"`
		ContentKeyID    string `json:"content_key_id"`
		DeploymentKeyID string `json:"deployment_key_id"`
	}
	var v struct {
		ChunkAEAD struct {
			ContentKeyHex string `json:"content_key_hex"`
			ChunkSize     int    `json:"chunk_size"`
			Vectors       []struct {
				Name             string `json:"name"`
				Expect           string `json:"expect"`
				PlaintextHex     string `json:"plaintext_hex"`
				PlaintextDigest  string `json:"plaintext_digest"`
				PlaintextSize    uint64 `json:"plaintext_size"`
				CiphertextHex    string `json:"ciphertext_hex"`
				CiphertextDigest string `json:"ciphertext_digest"`
			} `json:"vectors"`
		} `json:"chunk_aead"`
		KeyWrap struct {
			RecipientIKMHex string `json:"recipient_ikm_hex"`
			Vectors         []struct {
				Name          string  `json:"name"`
				Expect        string  `json:"expect"`
				Binding       binding `json:"binding"`
				EncHex        string  `json:"enc_hex"`
				CiphertextHex string  `json:"ciphertext_hex"`
				KeyHex        string  `json:"wrapped_key_hex"`
			} `json:"vectors"`
		} `json:"key_wrap"`
		SealedContentKey struct {
			DeploymentKeyHex string `json:"deployment_key_hex"`
			Vectors          []struct {
				Name          string  `json:"name"`
				Expect        string  `json:"expect"`
				Binding       binding `json:"binding"`
				NonceHex      string  `json:"nonce_hex"`
				CiphertextHex string  `json:"ciphertext_hex"`
				ContentKeyHex string  `json:"content_key_hex"`
			} `json:"vectors"`
		} `json:"sealed_content_key"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	unhex := func(s string) []byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	priv, err := registrycrypto.KEM.DeriveKeyPair(unhex(v.KeyWrap.RecipientIKMHex))
	if err != nil {
		t.Fatal(err)
	}
	wraps := 0
	for _, wv := range v.KeyWrap.Vectors {
		b := wv.Binding
		// The device builds only device, deployment-scope bindings, and the
		// recipient key id from its own key: the other vectors bind inputs
		// the device never takes from outside.
		if b.ScopeKind != registrycrypto.ScopeDeployment || b.RecipientKind != byte(cloudpbv2.RecipientKind_RECIPIENT_KIND_DEVICE) ||
			b.RecipientKeyID != registrycrypto.KeyID(priv.PublicKey().Bytes()) {
			continue
		}
		wraps++
		a := &cloudpbv2.DeviceAssignment{DeploymentId: b.ScopeID, DeploymentKeyId: b.KeyID, DeviceWrap: &cloudpbv2.KeyWrap{
			Suite: cloudpbv2.KemSuite_KEM_SUITE_MLKEM768_X25519, Enc: unhex(wv.EncHex), Ciphertext: unhex(wv.CiphertextHex),
		}}
		key, err := openDeploymentKey(priv, b.OrganizationID, b.Recipient, a)
		if (err == nil) != (wv.Expect == "valid") || err == nil && hex.EncodeToString(key) != wv.KeyHex {
			t.Fatalf("wrap %s: expect %s, got %v", wv.Name, wv.Expect, err)
		}
	}
	if wraps < 5 {
		t.Fatalf("only %d wrap vectors reached the device path", wraps)
	}

	for _, sv := range v.SealedContentKey.Vectors {
		b := sv.Binding
		a := &cloudpbv2.DeviceAssignment{DeploymentId: b.DeploymentID, ManifestDigest: b.ManifestDigest, DeploymentKeyId: b.DeploymentKeyID}
		key, err := openContentKey(unhex(v.SealedContentKey.DeploymentKeyHex), b.OrganizationID, a, b.ContentKeyID,
			&cloudpbv2.SealedContentKey{Nonce: unhex(sv.NonceHex), Ciphertext: unhex(sv.CiphertextHex)})
		if (err == nil) != (sv.Expect == "valid") || err == nil && hex.EncodeToString(key) != sv.ContentKeyHex {
			t.Fatalf("seal %s: expect %s, got %v", sv.Name, sv.Expect, err)
		}
	}

	c := v.ChunkAEAD
	dir := t.TempDir()
	for _, cv := range c.Vectors {
		ct := filepath.Join(dir, cv.Name+".enc")
		pt := filepath.Join(dir, cv.Name)
		ciphertext := unhex(cv.CiphertextHex)
		if err := os.WriteFile(ct, ciphertext, 0o600); err != nil {
			t.Fatal(err)
		}
		err := decryptStaged(ct, pt, unhex(c.ContentKeyHex), &cloudpbv2.EncryptedBlob{
			PlaintextDigest: cv.PlaintextDigest, CiphertextDigest: cv.CiphertextDigest,
			PlaintextSize: cv.PlaintextSize, CiphertextSize: uint64(len(ciphertext)),
		}, c.ChunkSize)
		if cv.Expect == "valid" {
			got, rerr := os.ReadFile(pt)
			if err != nil || rerr != nil || hex.EncodeToString(got) != cv.PlaintextHex {
				t.Fatalf("chunks %s: %v", cv.Name, err)
			}
			continue
		}
		var f *pullFailure
		if !errors.As(err, &f) || f.outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_DIGEST {
			t.Fatalf("chunks %s: want REJECTED_DIGEST, got %v", cv.Name, err)
		}
	}
}
