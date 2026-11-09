package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/shared/registrycrypto"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// The conformance vectors' device with a valid key registration (service-protos
// conformance/reqsig, recipient-key-valid) in testOperatorTenant.
const (
	vectorDevice      = "7c1d3e5f-2a4b-4c6d-8e9f-0a1b2c3d4e5f"
	vectorOtherDevice = "3f8e2d1c-9b7a-4e6f-8d5c-4b3a2f1e0d9c"
)

type recipientVectors struct {
	Root    string `json:"root_cert_der_b64"`
	Vectors []struct {
		Name          string `json:"name"`
		RecipientKey  string `json:"recipient_key_b64"`
		ExpectedKeyID string `json:"expected_key_id"`
	} `json:"recipient_key_vectors"`
}

func loadRecipientVectors(t *testing.T) recipientVectors {
	t.Helper()
	raw, err := os.ReadFile("../../shared/registrycrypto/testdata/device-signed-request-vectors-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var v recipientVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func vectorRecipientKey(t *testing.T, v recipientVectors, name string) (*cloudpb.RecipientKey, string) {
	t.Helper()
	for _, rv := range v.Vectors {
		if rv.Name == name {
			raw, _ := base64.StdEncoding.DecodeString(rv.RecipientKey)
			var k cloudpb.RecipientKey
			if err := proto.Unmarshal(raw, &k); err != nil {
				t.Fatal(err)
			}
			return &k, rv.ExpectedKeyID
		}
	}
	t.Fatalf("no recipient vector %s", name)
	return nil, ""
}

// signedCall is one operator-signed request the fake cloud received.
type signedCall struct {
	method   string
	header   map[string]any
	claims   map[string]any
	payload  proto.Message
	resource string
}

// fakeRegistry is cloud's registry, deployments and key directory as far as
// the CLI sees them. It enforces what cloud's handlers enforce on the CLI's
// input: payload types, signed resources, manifest digest, unique blobs,
// promised digest and size on upload, write-once releases and wrap slots.
type fakeRegistry struct {
	cloudpb.UnimplementedArtifactRegistryServiceServer
	cloudpb.UnimplementedDeploymentServiceServer
	cloudpb.UnimplementedKeyDirectoryServiceServer

	t        *testing.T
	mu       sync.Mutex
	keys     []*cloudpb.RecipientKey
	calls    []signedCall
	missing  func([]string) []string // which ciphertext digests to ask for; nil = all
	releases map[string]*cloudpb.ArtifactManifest
	blobs    map[string][]byte // ciphertext digest -> stored bytes
	wraps    []*cloudpb.KeyWrap
	sealed   []*cloudpb.SealedContentKey
	deploys  map[string]*cloudpb.Deployment
	assigned []*cloudpb.DeviceAssignment
}

func newFakeRegistry(t *testing.T, keys ...*cloudpb.RecipientKey) *fakeRegistry {
	return &fakeRegistry{t: t, keys: keys, releases: map[string]*cloudpb.ArtifactManifest{}, blobs: map[string][]byte{}, deploys: map[string]*cloudpb.Deployment{}}
}

// accept decodes a signed request into msg and records it.
func (f *fakeRegistry) accept(method string, in *cloudpb.SignedRequest, msg proto.Message) error {
	if in.GetPayloadType() != string(msg.ProtoReflect().Descriptor().FullName()) {
		return status.Errorf(codes.InvalidArgument, "payload_type %q", in.GetPayloadType())
	}
	if err := proto.Unmarshal(in.GetPayload(), msg); err != nil {
		return err
	}
	parts := strings.Split(string(in.GetSignature()), ".")
	if len(parts) != 3 {
		return status.Error(codes.Unauthenticated, "no JWS")
	}
	claims := decodeJWSPart(f.t, parts[1])
	sum := sha256.Sum256(in.GetPayload())
	if claims["operation"] != strings.TrimPrefix(method, "/") || claims["body_sha256"] != base64.RawURLEncoding.EncodeToString(sum[:]) {
		return status.Error(codes.PermissionDenied, "descriptor does not bind the call")
	}
	target, _ := claims["target"].(map[string]any)
	f.calls = append(f.calls, signedCall{method: method, header: decodeJWSPart(f.t, parts[0]), claims: claims, payload: msg, resource: fmt.Sprint(target["resource"])})
	return nil
}

func (f *fakeRegistry) signed(method string) []signedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []signedCall
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeRegistry) GetRecipientKeys(_ context.Context, in *cloudpb.GetRecipientKeysRequest) (*cloudpb.GetRecipientKeysResponse, error) {
	resp := &cloudpb.GetRecipientKeysResponse{}
	for _, k := range f.keys {
		for _, p := range in.GetPrincipals() {
			if k.GetPrincipal() == p {
				resp.Keys = append(resp.Keys, k)
			}
		}
	}
	return resp, nil
}

func (f *fakeRegistry) CreateAppRelease(_ context.Context, in *cloudpb.SignedRequest) (*cloudpb.AppRelease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var req cloudpb.CreateAppReleaseRequest
	if err := f.accept(cloudpb.DeploymentService_CreateAppRelease_FullMethodName, in, &req); err != nil {
		return nil, err
	}
	return &cloudpb.AppRelease{Id: uuid.NewString(), OrganizationId: req.GetOrganizationId(), AppId: req.GetAppId(), Name: req.GetName()}, nil
}

func (f *fakeRegistry) PublishArtifact(_ context.Context, in *cloudpb.SignedRequest) (*cloudpb.PublishArtifactResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var req cloudpb.PublishArtifactRequest
	if err := f.accept(cloudpb.ArtifactRegistryService_PublishArtifact_FullMethodName, in, &req); err != nil {
		return nil, err
	}
	m := req.GetManifest()
	sum := sha256.Sum256(m.GetOciManifest())
	if m.GetManifestDigest() != "sha256:"+hex.EncodeToString(sum[:]) || m.GetAead() != cloudpb.AeadSuite_AEAD_SUITE_AES_256_GCM_CHUNKED {
		return nil, status.Error(codes.InvalidArgument, "manifest_digest must be sha256:<hex> of oci_manifest")
	}
	seen := map[string]bool{}
	var digests []string
	for _, b := range m.GetBlobs() {
		if seen[b.GetCiphertextDigest()] {
			return nil, status.Error(codes.InvalidArgument, "ciphertext digests must be unique within an artifact")
		}
		seen[b.GetCiphertextDigest()] = true
		digests = append(digests, b.GetCiphertextDigest())
	}
	if prior, ok := f.releases[req.GetAppReleaseId()]; ok && !proto.Equal(prior, m) {
		return nil, status.Error(codes.FailedPrecondition, "this release already has a different artifact")
	}
	f.releases[req.GetAppReleaseId()] = m
	if f.missing != nil {
		digests = f.missing(digests)
	}
	return &cloudpb.PublishArtifactResponse{UploadId: req.GetAppReleaseId(), MissingCiphertextDigests: digests}, nil
}

func (f *fakeRegistry) PutBlob(stream grpc.ClientStreamingServer[cloudpb.PutBlobRequest, cloudpb.PutBlobResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	header := first.GetHeader()
	if header == nil {
		return status.Error(codes.InvalidArgument, "the first message is the header")
	}
	f.mu.Lock()
	m := f.releases[header.GetUploadId()]
	f.mu.Unlock()
	var promised *cloudpb.EncryptedBlob
	for _, b := range m.GetBlobs() {
		if b.GetCiphertextDigest() == header.GetCiphertextDigest() {
			promised = b
		}
	}
	if promised == nil {
		return status.Error(codes.NotFound, "upload or blob not found")
	}
	if header.GetCiphertextSize() != promised.GetCiphertextSize() {
		return status.Error(codes.InvalidArgument, "ciphertext_size differs from the published manifest")
	}
	var got bytes.Buffer
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if msg.GetHeader() != nil {
			return status.Error(codes.InvalidArgument, "only the first message is a header")
		}
		got.Write(msg.GetData())
	}
	sum := sha256.Sum256(got.Bytes())
	if uint64(got.Len()) != header.GetCiphertextSize() || "sha256:"+hex.EncodeToString(sum[:]) != header.GetCiphertextDigest() {
		return status.Error(codes.InvalidArgument, "the bytes do not match ciphertext_digest and ciphertext_size")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, stored := f.blobs[header.GetCiphertextDigest()]; !stored { // write-once: its digest is its address
		f.blobs[header.GetCiphertextDigest()] = got.Bytes()
	}
	return stream.SendAndClose(&cloudpb.PutBlobResponse{CiphertextDigest: header.GetCiphertextDigest()})
}

func (f *fakeRegistry) AddWraps(_ context.Context, in *cloudpb.SignedRequest) (*cloudpb.AddWrapsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var req cloudpb.AddWrapsRequest
	if err := f.accept(cloudpb.ArtifactRegistryService_AddWraps_FullMethodName, in, &req); err != nil {
		return nil, err
	}
	for _, w := range req.GetWraps() {
		if w.GetRecipientKind() == cloudpb.RecipientKind_RECIPIENT_KIND_DEVICE {
			return nil, status.Error(codes.InvalidArgument, "a device's wrap travels in its DeviceAssignment (AssignDevice)")
		}
		for _, held := range f.wraps {
			if proto.Equal(held.GetScope(), w.GetScope()) && held.GetRecipientKeyId() == w.GetRecipientKeyId() && !bytes.Equal(held.GetEnc(), w.GetEnc()) {
				return nil, status.Error(codes.AlreadyExists, "wrap slot taken")
			}
		}
	}
	f.wraps = append(f.wraps, req.GetWraps()...)
	f.sealed = append(f.sealed, req.GetSealedContentKeys()...)
	return &cloudpb.AddWrapsResponse{Stored: uint32(len(req.GetWraps()) + len(req.GetSealedContentKeys()))}, nil
}

func (f *fakeRegistry) GetArtifact(_ context.Context, in *cloudpb.GetArtifactRequest) (*cloudpb.GetArtifactResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.releases[in.GetAppReleaseId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "app release not found")
	}
	return &cloudpb.GetArtifactResponse{Manifest: m}, nil
}

// GetMyWraps serves every user wrap in scope; the CLI picks its own.
func (f *fakeRegistry) GetMyWraps(_ context.Context, in *cloudpb.GetMyWrapsRequest) (*cloudpb.GetMyWrapsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resp := &cloudpb.GetMyWrapsResponse{}
	for _, w := range f.wraps {
		s := w.GetScope()
		if w.GetRecipientKind() == cloudpb.RecipientKind_RECIPIENT_KIND_USER && s.GetDeploymentId() == in.GetScope().GetDeploymentId() &&
			s.GetAppReleaseId() == in.GetScope().GetAppReleaseId() && (in.GetScope().GetKeyId() == "" || s.GetKeyId() == in.GetScope().GetKeyId()) {
			resp.Wraps = append(resp.Wraps, w)
		}
	}
	for _, s := range f.sealed {
		if s.GetDeploymentId() == in.GetScope().GetDeploymentId() && len(resp.Wraps) > 0 {
			resp.SealedContentKeys = append(resp.SealedContentKeys, s)
		}
	}
	return resp, nil
}

func (f *fakeRegistry) CreateDeployment(_ context.Context, in *cloudpb.SignedRequest) (*cloudpb.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var req cloudpb.CreateDeploymentRequest
	if err := f.accept(cloudpb.DeploymentService_CreateDeployment_FullMethodName, in, &req); err != nil {
		return nil, err
	}
	d := &cloudpb.Deployment{Id: uuid.NewString(), OrganizationId: req.GetOrganizationId(), AppReleaseId: req.GetAppReleaseId(), TargetDeviceIds: req.GetTargetDeviceIds()}
	f.deploys[d.Id] = d
	return d, nil
}

func (f *fakeRegistry) GetDeployment(_ context.Context, in *cloudpb.GetDeploymentRequest) (*cloudpb.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.deploys[in.GetId()]; ok {
		return d, nil
	}
	return nil, status.Error(codes.NotFound, "deployment not found")
}

func (f *fakeRegistry) AssignDevice(_ context.Context, in *cloudpb.SignedRequest) (*cloudpb.AssignDeviceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var a cloudpb.DeviceAssignment
	if err := f.accept(cloudpb.ArtifactRegistryService_AssignDevice_FullMethodName, in, &a); err != nil {
		return nil, err
	}
	f.assigned = append(f.assigned, &a)
	return &cloudpb.AssignDeviceResponse{PostboxEntryId: int64(len(f.assigned))}, nil
}

func (f *fakeRegistry) UnassignDevice(_ context.Context, in *cloudpb.SignedRequest) (*cloudpb.UnassignDeviceResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var req cloudpb.UnassignDeviceRequest
	if err := f.accept(cloudpb.ArtifactRegistryService_UnassignDevice_FullMethodName, in, &req); err != nil {
		return nil, err
	}
	return &cloudpb.UnassignDeviceResponse{PostboxEntryId: 1}, nil
}

// registryFixture is an operator session on the dev cloud (whose custodian is
// pinned) dialed to a fake registry, with the vectors' root as its chain.
func registryFixture(t *testing.T, keys ...*cloudpb.RecipientKey) (*registryOp, *fakeRegistry) {
	t.Helper()
	v := loadRecipientVectors(t)
	root, _ := base64.StdEncoding.DecodeString(v.Root)
	session := tenantOperatorSession(t, defaultDevCloudGRPC)
	session.Certificates[0].PemCertificateChain = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root}))
	priv, err := sessionRegistryKey(&session)
	if err != nil {
		t.Fatal(err)
	}
	session.RegistryKeyID = registrycrypto.KeyID(priv.PublicKey().Bytes()) // bound: no registration call

	fake := newFakeRegistry(t, keys...)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	cloudpb.RegisterArtifactRegistryServiceServer(srv, fake)
	cloudpb.RegisterDeploymentServiceServer(srv, fake)
	cloudpb.RegisterKeyDirectoryServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	op, err := newRegistryOp(context.Background(), conn, &session)
	if err != nil {
		t.Fatal(err)
	}
	return op, fake
}

// publishFixture publishes an image whose two layers are the same blob.
func publishFixture(t *testing.T, op *registryOp) (string, *cloudpb.ArtifactManifest) {
	t.Helper()
	layer, err := random.Layer(200_000, "application/vnd.oci.image.layer.v1.tar+gzip")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := random.Layer(10, "application/vnd.oci.image.layer.v1.tar+gzip")
	img, err := mutate.AppendLayers(empty.Image, layer, other, layer)
	if err != nil {
		t.Fatal(err)
	}
	releaseID := uuid.NewString()
	m, err := op.publish(context.Background(), releaseID, img, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return releaseID, m
}

// Publish uploads only ciphertext that decrypts to the image's blobs under a
// content key wrapped to this machine and the custodian; the manifest is the
// image's own, each blob once.
func TestPublishUploadsCiphertextAndWrapsTheReleaseKey(t *testing.T) {
	op, fake := registryFixture(t)
	releaseID, m := publishFixture(t, op)

	calls := fake.signed(cloudpb.ArtifactRegistryService_PublishArtifact_FullMethodName)
	if len(calls) != 1 || calls[0].resource != "org/"+testOperatorTenant+"/release/"+releaseID {
		t.Fatalf("PublishArtifact calls %+v", calls)
	}
	if len(m.GetBlobs()) != 3 || len(fake.blobs) != 3 { // config + two distinct layers
		t.Fatalf("%d blobs in the manifest, %d stored", len(m.GetBlobs()), len(fake.blobs))
	}
	if m.GetChunkSize() != registrycrypto.DefaultChunkSize || m.GetKind() != cloudpb.ReleaseType_CONTAINER {
		t.Fatalf("manifest %v", m)
	}

	// Two release-scope wraps: this machine and the custodian.
	custodianID := op.custodianID
	if len(fake.wraps) != 2 || fake.wraps[0].GetRecipientKeyId() != op.keyID || fake.wraps[0].GetRecipient() != op.me ||
		fake.wraps[1].GetRecipientKind() != cloudpb.RecipientKind_RECIPIENT_KIND_CUSTODIAN || fake.wraps[1].GetRecipientKeyId() != custodianID || fake.wraps[1].GetRecipient() != "" {
		t.Fatalf("wraps %v", fake.wraps)
	}
	contentKey, err := op.contentKey(context.Background(), releaseID, m)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range m.GetBlobs() {
		var plain bytes.Buffer
		want := registrycrypto.Blob{PlaintextDigest: b.PlaintextDigest, CiphertextDigest: b.CiphertextDigest, PlaintextSize: b.PlaintextSize, CiphertextSize: b.CiphertextSize}
		if err := registrycrypto.DecryptBlob(&plain, bytes.NewReader(fake.blobs[b.CiphertextDigest]), contentKey, want, int(m.GetChunkSize())); err != nil {
			t.Fatalf("stored blob %s does not decrypt: %v", b.CiphertextDigest, err)
		}
	}
}

// Publish sends only what Cloud reports missing, and a release that already
// holds another artifact is refused (write-once): nothing more is uploaded.
func TestPublishResumesAndIsWriteOnce(t *testing.T) {
	op, fake := registryFixture(t)
	fake.missing = func(d []string) []string { return d[1:] }
	releaseID, m := publishFixture(t, op)
	if len(fake.blobs) != 2 {
		t.Fatalf("%d blobs uploaded, want the 2 missing", len(fake.blobs))
	}
	if _, ok := fake.blobs[m.GetBlobs()[0].GetCiphertextDigest()]; ok {
		t.Fatal("a blob Cloud already held was uploaded again")
	}

	img, _ := random.Image(64, 1)
	if _, err := op.publish(context.Background(), releaseID, img, t.TempDir()); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("second artifact for one release: %v", err)
	}
	if len(fake.blobs) != 2 || len(fake.wraps) != 2 {
		t.Fatalf("the refused publish uploaded %d blobs, %d wraps", len(fake.blobs), len(fake.wraps))
	}
}

// The upload carries the promised digest and size; a blob whose bytes differ
// from them is refused by the registry and the refusal reaches the caller.
func TestPutBlobRefusedOnDigestMismatch(t *testing.T) {
	op, fake := registryFixture(t)
	releaseID, m := publishFixture(t, op)
	b := proto.Clone(m.GetBlobs()[0]).(*cloudpb.EncryptedBlob)
	path := t.TempDir() + "/tampered"
	if err := os.WriteFile(path, bytes.Repeat([]byte{1}, int(b.GetCiphertextSize())), 0o600); err != nil {
		t.Fatal(err)
	}
	delete(fake.blobs, b.GetCiphertextDigest())
	if err := op.putBlob(context.Background(), releaseID, b, path); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("tampered upload: %v", err)
	}
	if _, ok := fake.blobs[b.GetCiphertextDigest()]; ok {
		t.Fatal("tampered bytes were stored")
	}
}

// Deploy verifies every device key first, records the deployment for its
// picked devices, seals the release key under a new deployment key wrapped
// to this machine and the custodian, and signs one assignment per device
// (x5c, the device's own resource, its own wrap).
func TestDeployAssignsEachDeviceWithItsOwnWrap(t *testing.T) {
	v := loadRecipientVectors(t)
	deviceKey, deviceKeyID := vectorRecipientKey(t, v, "recipient-key-valid")
	op, fake := registryFixture(t, deviceKey)
	releaseID, m := publishFixture(t, op)

	if err := op.deploy(context.Background(), "sh.wendy.example", releaseID, "sha256:"+strings.Repeat("0", 64), []string{vectorDevice}); err == nil {
		t.Fatal("a release whose digest is not the pinned one was deployed")
	}
	if err := op.deploy(context.Background(), "sh.wendy.example", releaseID, m.GetManifestDigest(), []string{vectorDevice}); err != nil {
		t.Fatal(err)
	}
	created := fake.signed(cloudpb.DeploymentService_CreateDeployment_FullMethodName)
	if len(created) != 1 || created[0].resource != "org/"+testOperatorTenant+"/release/"+releaseID || created[0].header["x5c"] == nil {
		t.Fatalf("CreateDeployment %+v", created)
	}
	req := created[0].payload.(*cloudpb.CreateDeploymentRequest)
	if len(req.GetTargetDeviceIds()) != 1 || req.GetTargetDeviceIds()[0] != vectorDevice || len(req.GetTargetTags()) != 0 || req.GetValidUntil() == nil {
		t.Fatalf("deployment targets %v", req)
	}
	var depID string
	for id := range fake.deploys {
		depID = id
	}

	if len(fake.sealed) != 1 || len(fake.wraps) != 4 {
		t.Fatalf("%d sealed keys, %d wraps", len(fake.sealed), len(fake.wraps))
	}
	kd, err := op.current(context.Background(), depID)
	if err != nil {
		t.Fatal(err)
	}
	s := fake.sealed[0]
	contentKey, err := registrycrypto.SealedKey{OrganizationID: testOperatorTenant, DeploymentID: depID, ManifestDigest: m.GetManifestDigest(), ContentKeyID: m.GetContentKeyId(), DeploymentKeyID: s.GetDeploymentKeyId()}.Open(kd.key, s.GetNonce(), s.GetCiphertext())
	if err != nil || registrycrypto.KeyID(contentKey) != m.GetContentKeyId() {
		t.Fatalf("sealed key does not open to the content key: %v", err)
	}

	assigns := fake.signed(cloudpb.ArtifactRegistryService_AssignDevice_FullMethodName)
	device := deviceURI(testOperatorTenant, vectorDevice)
	if len(assigns) != 1 || assigns[0].resource != "org/"+testOperatorTenant+"/deployment/"+depID+"/device/"+vectorDevice || assigns[0].header["x5c"] == nil || assigns[0].header["kid"] != nil {
		t.Fatalf("AssignDevice %+v", assigns)
	}
	a := assigns[0].payload.(*cloudpb.DeviceAssignment)
	w := a.GetDeviceWrap()
	if a.GetDevice() != device || a.GetAppId() != "sh.wendy.example" || a.GetAppReleaseId() != releaseID || a.GetManifestDigest() != m.GetManifestDigest() ||
		a.GetDeploymentKeyId() != s.GetDeploymentKeyId() || a.GetDesiredState() != cloudpb.Running_RUNNING || a.GetExpiresAt() == nil ||
		w.GetRecipientKind() != cloudpb.RecipientKind_RECIPIENT_KIND_DEVICE || w.GetRecipient() != device || w.GetRecipientKeyId() != deviceKeyID ||
		w.GetScope().GetDeploymentId() != depID || w.GetScope().GetKeyId() != s.GetDeploymentKeyId() || len(w.GetEnc()) == 0 {
		t.Fatalf("assignment %v", a)
	}
}

// A device with no verifiable key (unregistered, or a registration that does
// not verify) refuses the whole deploy before anything is created.
func TestDeployRefusesUnverifiedDevicesBeforeCreating(t *testing.T) {
	v := loadRecipientVectors(t)
	valid, _ := vectorRecipientKey(t, v, "recipient-key-valid")
	substituted, _ := vectorRecipientKey(t, v, "recipient-key-substituted-key-invalid")
	substituted.Principal = deviceURI(testOperatorTenant, vectorOtherDevice)
	op, fake := registryFixture(t, valid, substituted)
	releaseID, m := publishFixture(t, op)
	for _, devices := range [][]string{{vectorDevice, vectorOtherDevice}, {vectorDevice, uuid.NewString()}} {
		err := op.deploy(context.Background(), "sh.wendy.example", releaseID, m.GetManifestDigest(), devices)
		if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
			t.Fatalf("%v: %v", devices, err)
		}
	}
	if len(fake.deploys) != 0 || len(fake.assigned) != 0 || len(fake.sealed) != 0 {
		t.Fatal("a refused deploy changed something")
	}
}

// Tag targets are refused with the reason; picked devices are required.
func TestDeployRefusesTagTargets(t *testing.T) {
	cmd := newAppDeployCmd()
	cmd.SetArgs([]string{"--release", uuid.NewString(), "--target-tag", "fleet"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "tag targets are not supported") {
		t.Fatalf("tag target: %v", err)
	}
	cmd = newAppDeployCmd()
	cmd.SetArgs([]string{"--release", uuid.NewString()})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--device") {
		t.Fatalf("no devices: %v", err)
	}
}

// Rotation keeps the content key, seals it under a new deployment key and
// re-assigns the deployment's devices at it; assign uses the newest key.
func TestRotateAndAssignUseTheNewestDeploymentKey(t *testing.T) {
	v := loadRecipientVectors(t)
	deviceKey, _ := vectorRecipientKey(t, v, "recipient-key-valid")
	op, fake := registryFixture(t, deviceKey)
	releaseID, m := publishFixture(t, op)
	ctx := context.Background()
	if err := op.deploy(ctx, "sh.wendy.example", releaseID, "", []string{vectorDevice}); err != nil {
		t.Fatal(err)
	}
	var depID string
	for id := range fake.deploys {
		depID = id
	}
	first := fake.sealed[0].GetDeploymentKeyId()
	if err := op.rotate(ctx, depID, "sh.wendy.example", nil); err != nil {
		t.Fatal(err)
	}
	if len(fake.sealed) != 2 || fake.sealed[1].GetDeploymentKeyId() == first || fake.sealed[1].GetContentKeyId() != m.GetContentKeyId() {
		t.Fatalf("rotation sealed %v", fake.sealed)
	}
	if len(fake.assigned) != 2 || fake.assigned[1].GetDeploymentKeyId() != fake.sealed[1].GetDeploymentKeyId() {
		t.Fatalf("rotation re-assigned %v", fake.assigned)
	}
	kd, err := op.current(ctx, depID)
	if err != nil || registrycrypto.KeyID(kd.key) != fake.sealed[1].GetDeploymentKeyId() {
		t.Fatalf("current key after rotation: %v", err)
	}
	if err := op.assign(ctx, kd, "sh.wendy.example", []string{vectorDevice}); err != nil {
		t.Fatal(err)
	}
	if fake.assigned[2].GetDeploymentKeyId() != fake.sealed[1].GetDeploymentKeyId() {
		t.Fatal("assign used a stale deployment key")
	}
}

// Unassign signs one UnassignDeviceRequest per device over the device's own
// resource, with a validity, by x5c.
func TestUnassignRequestShape(t *testing.T) {
	op, fake := registryFixture(t)
	depID := uuid.NewString()
	if err := op.unassign(context.Background(), depID, []string{vectorDevice, vectorOtherDevice}); err != nil {
		t.Fatal(err)
	}
	calls := fake.signed(cloudpb.ArtifactRegistryService_UnassignDevice_FullMethodName)
	if len(calls) != 2 {
		t.Fatalf("%d unassignments", len(calls))
	}
	for i, d := range []string{vectorDevice, vectorOtherDevice} {
		req := calls[i].payload.(*cloudpb.UnassignDeviceRequest)
		if calls[i].resource != "org/"+testOperatorTenant+"/deployment/"+depID+"/device/"+d || calls[i].header["x5c"] == nil ||
			req.GetDevice() != deviceURI(testOperatorTenant, d) || req.GetOrganizationId() != testOperatorTenant || req.GetValidUntil() == nil {
			t.Fatalf("unassignment %d: %+v %v", i, calls[i], req)
		}
	}
}

// The registry refuses a session whose cloud has no pinned custodian key,
// rather than publishing without the custodian.
func TestRegistryRefusesWithoutCustodianPin(t *testing.T) {
	session := tenantOperatorSession(t, "cloud.example:443")
	if _, err := newRegistryOp(context.Background(), nil, &session); err == nil || !strings.Contains(err.Error(), "custodian") {
		t.Fatalf("no custodian pin: %v", err)
	}
	session = tenantOperatorSession(t, defaultDevCloudGRPC)
	if _, err := newRegistryOp(context.Background(), nil, &session); err == nil || !strings.Contains(err.Error(), "CA chain") {
		t.Fatalf("no chain: %v", err)
	}
}
