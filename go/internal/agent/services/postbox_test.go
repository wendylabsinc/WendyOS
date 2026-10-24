package services

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// Cloud's pinned chain vectors (cloud swift/Tests/FabricKitTests/
// PostboxHashChainTests.swift, the SQL trigger is checked against them).
func TestPostboxEntryHashMatchesCloudVectors(t *testing.T) {
	tenant := "0b9df273-8b39-4f4e-9d5e-1a2b3c4d5e6f"
	h1 := postboxEntryHash(make([]byte, 32), tenant, "", 1, []byte("jws-bytes"), []byte("tsr-bytes"))
	if got := hex.EncodeToString(h1); got != "5071dd914f504c02a1ac8f1c465ffc0854183293558775847085295d5cd4e279" {
		t.Fatalf("vector 1 = %s", got)
	}
	h2 := postboxEntryHash(h1, tenant, "spiffe://wendy.sh/tenant/"+tenant+"/device/edge-01", 2, []byte("jws-2"), []byte("tsr-2"))
	if got := hex.EncodeToString(h2); got != "ad0f76c11b5ccc409f3b189d37c22a30ff15eb38e9df4de6dd59498ea14a894e" {
		t.Fatalf("vector 2 = %s", got)
	}
}

type fakePostboxCloud struct {
	mu      sync.Mutex
	tenant  string
	target  string
	entries []*cloudpbv2.PostboxEntry
	acks    []*cloudpbv2.SignedRequest
	pulls   chan int64
	// orgCAs is what GetOrgCAs serves per issuer Name; absent = NOT_FOUND.
	orgCAs    map[string][]*x509.Certificate
	orgCAAsks int
	genTime   time.Time
	stamp     func(jws []byte) []byte // the TSA token cloud takes at accept
	inter     []*x509.Certificate     // the checkpoint x5c beyond its leaf
}

func (c *fakePostboxCloud) GetOrgCAs(_ context.Context, in *cloudpbv2.GetOrgCAsRequest, _ ...grpc.CallOption) (*cloudpbv2.GetOrgCAsResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.orgCAAsks++
	cas, ok := c.orgCAs[string(in.GetIssuer())]
	if !ok {
		return nil, status.Error(codes.NotFound, "no CA with this subject")
	}
	resp := &cloudpbv2.GetOrgCAsResponse{}
	for _, ca := range cas {
		resp.CaDer = append(resp.CaDer, ca.Raw)
	}
	return resp, nil
}

func (c *fakePostboxCloud) PullChanges(_ context.Context, in *cloudpbv2.PullChangesRequest, _ ...grpc.CallOption) (*cloudpbv2.PullChangesResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case c.pulls <- in.GetAfterEntryId():
	default:
	}
	resp := &cloudpbv2.PullChangesResponse{Checkpoint: []byte("checkpoint")}
	for _, e := range c.entries {
		if e.GetEntryId() > in.GetAfterEntryId() {
			resp.Entries = append(resp.Entries, e)
		}
	}
	return resp, nil
}

func (c *fakePostboxCloud) AckHead(_ context.Context, in *cloudpbv2.SignedRequest, _ ...grpc.CallOption) (*cloudpbv2.AckHeadResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.acks = append(c.acks, in)
	return &cloudpbv2.AckHeadResponse{}, nil
}

// add appends an entry the way cloud's trigger chains it.
func (c *fakePostboxCloud) add(jws, payload []byte, payloadType string) *cloudpbv2.PostboxEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev := make([]byte, 32)
	if n := len(c.entries); n > 0 {
		prev = c.entries[n-1].GetEntryHash()
	}
	id := int64(len(c.entries) + 1)
	tsr := c.stamp(jws)
	e := &cloudpbv2.PostboxEntry{
		EntryId: id, RequestJws: jws, TsrResponse: tsr, PrevHash: prev,
		EntryHash:      postboxEntryHash(prev, c.tenant, c.target, id, jws, tsr),
		RequestPayload: payload, PayloadType: payloadType,
	}
	c.entries = append(c.entries, e)
	return e
}

// notarize is the test checkpoint seam: the cloud tip, as a verified
// checkpoint would bind it, notarized at genTime (zero: now).
func (c *fakePostboxCloud) notarize(_ []byte, _ *x509.CertPool, _, _ string, mark int64, now time.Time) (postboxCheckpoint, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) == 0 {
		return postboxCheckpoint{}, nil
	}
	last := c.entries[len(c.entries)-1]
	if last.GetEntryId() < mark {
		return postboxCheckpoint{}, cpRefuse("rollback", "max_id below the mark")
	}
	gen := c.genTime
	if gen.IsZero() {
		gen = now
	}
	return postboxCheckpoint{maxID: last.GetEntryId(), head: last.GetEntryHash(), genTime: gen, inter: c.inter}, nil
}

type pbFixture struct {
	t                         *testing.T
	tenant, uuid, uri         string
	root, opCA                *x509.Certificate
	tenantCA, deviceCA        *x509.Certificate
	tenantCAKey, deviceCAKey  *ecdsa.PrivateKey
	rootKey, opCAKey          *ecdsa.PrivateKey
	opKey                     *mldsa.PrivateKey
	opLeaf                    *x509.Certificate
	certPEM, keyPEM, chainPEM string
	dir                       string
	alg                       string // JWS header alg; "" = ML-DSA-65
	now                       time.Time
	stampAt                   time.Time // entry token genTime; zero = now
	tsaCA, tsaSigner          *x509.Certificate
	tsaKey                    *mldsa.PrivateKey
	cloud                     *fakePostboxCloud
	ctr                       *mockContainerdClient
	p                         *Postbox
}

func mkCert(t *testing.T, tmpl, parent *x509.Certificate, pub any, key any) *x509.Certificate {
	t.Helper()
	tmpl.SerialNumber = big.NewInt(time.Now().UnixNano())
	if tmpl.NotBefore.IsZero() {
		tmpl.NotBefore, tmpl.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	}
	if parent == nil {
		parent = tmpl
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func mkCA(t *testing.T, cn string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if parentKey == nil {
		parentKey = key
	}
	ca := mkCert(t, &x509.Certificate{
		Subject: pkix.Name{CommonName: cn}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign,
	}, parent, &key.PublicKey, parentKey)
	return ca, key
}

func mkLeaf(t *testing.T, uri string, pub any, issuer *x509.Certificate, issuerKey *ecdsa.PrivateKey) *x509.Certificate {
	u, _ := url.Parse(uri)
	return mkCert(t, &x509.Certificate{
		Subject: pkix.Name{CommonName: "leaf"}, URIs: []*url.URL{u},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, issuer, pub, issuerKey)
}

func pemOf(certs ...*x509.Certificate) string {
	var b strings.Builder
	for _, c := range certs {
		b.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
	}
	return b.String()
}

func newPBFixture(t *testing.T) *pbFixture {
	t.Helper()
	f := &pbFixture{t: t, tenant: uuid.NewString(), uuid: uuid.NewString(), dir: t.TempDir(), now: time.Now().Truncate(time.Second)}
	f.uri = "spiffe://wendy.sh/tenant/" + f.tenant + "/device/" + f.uuid
	f.root, f.rootKey = mkCA(t, "platform root", nil, nil)
	f.tenantCA, f.tenantCAKey = mkCA(t, "tenant intermediate", f.root, f.rootKey)
	f.deviceCA, f.deviceCAKey = mkCA(t, "device identity CA", f.tenantCA, f.tenantCAKey)
	f.opCA, f.opCAKey = mkCA(t, "operator authority", f.tenantCA, f.tenantCAKey)

	var err error
	if f.keyPEM, err = certs.GenerateMLDSAKeyPair(); err != nil {
		t.Fatal(err)
	}
	signer, err := certs.ParseSigningPrivateKeyPEM([]byte(f.keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	f.certPEM = pemOf(mkLeaf(t, f.uri, signer.Public(), f.deviceCA, f.deviceCAKey))
	f.chainPEM = pemOf(f.deviceCA, f.tenantCA, f.root)

	if f.opKey, err = mldsa.GenerateKey(mldsa.MLDSA65()); err != nil {
		t.Fatal(err)
	}
	f.opLeaf = mkLeaf(t, "spiffe://wendy.sh/tenant/"+f.tenant+"/operator/alice", f.opKey.PublicKey(), f.opCA, f.opCAKey)

	// The platform TSA, as pki-core's: an ML-DSA signer under a platform
	// intermediate that only the checkpoint x5c carries.
	var tsaCAKey *ecdsa.PrivateKey
	f.tsaCA, tsaCAKey = mkCA(t, "platform TSA intermediate", f.root, f.rootKey)
	if f.tsaKey, err = mldsa.GenerateKey(mldsa.MLDSA65()); err != nil {
		t.Fatal(err)
	}
	f.tsaSigner = mkCert(t, &x509.Certificate{
		Subject: pkix.Name{CommonName: "platform TSA"}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping},
	}, f.tsaCA, f.tsaKey.PublicKey(), tsaCAKey)

	f.cloud = &fakePostboxCloud{tenant: f.tenant, target: f.uri, pulls: make(chan int64, 16), inter: []*x509.Certificate{f.tsaCA}}
	f.cloud.stamp = func(jws []byte) []byte {
		at := f.stampAt
		if at.IsZero() {
			at = f.now
		}
		return f.timestamp(jws, at)
	}
	f.ctr = &mockContainerdClient{resolvedIDs: map[string][]string{"web": {"web"}}}
	f.p = f.newPostbox()
	return f
}

// newPostbox is a fresh client over the same config dir, as after a restart.
func (f *pbFixture) newPostbox() *Postbox {
	p := NewPostbox(zap.NewNop(), nil, f.dir, NewPostboxControl(NewContainerService(zap.NewNop(), f.ctr)))
	p.now = func() time.Time { return f.now }
	p.dial = func(string, string, string, string) (cloudpbv2.DevicePostboxServiceClient, io.Closer, error) {
		return f.cloud, io.NopCloser(nil), nil
	}
	p.checkpoint = f.cloud.notarize
	p.UpdateIdentity("https://cloud.test", f.certPEM, f.keyPEM, f.chainPEM)
	return p
}

// sign builds an operator-signed request as the CLI does (x5c = leaf plus
// the issuing CA here; mutate edits the claims).
func (f *pbFixture) sign(method string, msg proto.Message, resource string, x5c []*x509.Certificate, key *mldsa.PrivateKey, mutate func(map[string]any)) ([]byte, []byte, string) {
	f.t.Helper()
	payload, err := proto.Marshal(msg)
	if err != nil {
		f.t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	claims := map[string]any{
		"aud": postboxAudience, "body_sha256": base64.RawURLEncoding.EncodeToString(sum[:]),
		"correlation_id": uuid.NewString(), "iat": f.now.Unix() - 5, "expiry": f.now.Unix() + 300,
		"nonce": uuid.NewString(), "operation": strings.TrimPrefix(method, "/"),
		"target": map[string]any{"tenant": f.tenant, "resource": resource},
	}
	if mutate != nil {
		mutate(claims)
	}
	chain := make([]string, len(x5c))
	for i, c := range x5c {
		chain[i] = base64.StdEncoding.EncodeToString(c.Raw)
	}
	alg := f.alg
	if alg == "" {
		alg = "ML-DSA-65"
	}
	header, _ := json.Marshal(map[string]any{"alg": alg, "x5c": chain})
	body, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	sig, err := key.Sign(rand.Reader, []byte(input), &mldsa.Options{})
	if err != nil {
		f.t.Fatal(err)
	}
	return []byte(input + "." + base64.RawURLEncoding.EncodeToString(sig)), payload, string(msg.ProtoReflect().Descriptor().FullName())
}

// timestamp is an RFC 3161 TimeStampResp over SHA-256(stamped) at gen,
// signed by the fixture TSA with its certificate embedded, as cloud's accept
// takes it from pki-core.
func (f *pbFixture) timestamp(stamped []byte, gen time.Time) []byte {
	f.t.Helper()
	der := func(v any) []byte {
		b, err := asn1.Marshal(v)
		if err != nil {
			f.t.Fatal(err)
		}
		return b
	}
	type alg struct{ Algorithm asn1.ObjectIdentifier }
	imprint := sha256.Sum256(stamped)
	var tst struct {
		Version        int
		Policy         asn1.ObjectIdentifier
		MessageImprint struct {
			HashAlgorithm alg
			HashedMessage []byte
		}
		SerialNumber *big.Int
		GenTime      time.Time `asn1:"generalized"`
	}
	tst.Version, tst.Policy, tst.SerialNumber, tst.GenTime = 1, asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 65441, 2, 1}, big.NewInt(1), gen.UTC()
	tst.MessageImprint.HashAlgorithm, tst.MessageImprint.HashedMessage = alg{oidSHA256}, imprint[:]
	tstDER := der(tst)
	md := sha256.Sum256(tstDER)
	attr := func(t asn1.ObjectIdentifier, v any) []byte {
		return der(struct {
			Type  asn1.ObjectIdentifier
			Value asn1.RawValue
		}{t, asn1.RawValue{Tag: asn1.TagSet, IsCompound: true, Bytes: der(v)}})
	}
	attrs := append(attr(oidAttrContentType, oidTSTInfo), attr(oidAttrMessageDigest, md[:])...)
	sig, err := f.tsaKey.Sign(rand.Reader, der(asn1.RawValue{Tag: asn1.TagSet, IsCompound: true, Bytes: attrs}), &mldsa.Options{})
	if err != nil {
		f.t.Fatal(err)
	}
	type signerInfo struct {
		Version int
		SID     struct {
			Issuer       asn1.RawValue
			SerialNumber *big.Int
		}
		DigestAlgorithm    alg
		SignedAttrs        asn1.RawValue
		SignatureAlgorithm alg
		Signature          []byte
	}
	si := signerInfo{Version: 1, DigestAlgorithm: alg{oidSHA256}, SignatureAlgorithm: alg{mldsaSigOIDs[1952]}, Signature: sig,
		SignedAttrs: asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: attrs}}
	si.SID.Issuer, si.SID.SerialNumber = asn1.RawValue{FullBytes: f.tsaSigner.RawIssuer}, f.tsaSigner.SerialNumber
	var sd struct {
		Version          int
		DigestAlgorithms []alg `asn1:"set"`
		EncapContentInfo struct {
			EContentType asn1.ObjectIdentifier
			EContent     []byte `asn1:"explicit,tag:0"`
		}
		Certificates asn1.RawValue
		SignerInfos  []signerInfo `asn1:"set"`
	}
	sd.Version, sd.DigestAlgorithms, sd.SignerInfos = 3, []alg{{oidSHA256}}, []signerInfo{si}
	sd.EncapContentInfo.EContentType, sd.EncapContentInfo.EContent = oidTSTInfo, tstDER
	sd.Certificates = asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: f.tsaSigner.Raw}
	token := der(struct {
		ContentType asn1.ObjectIdentifier
		Content     asn1.RawValue
	}{oidCMSSignedData, asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: der(sd)}})
	return der(struct {
		Status         struct{ Status int }
		TimeStampToken asn1.RawValue
	}{TimeStampToken: asn1.RawValue{FullBytes: token}})
}

func (f *pbFixture) control(app string, action cloudpbv2.ContainerAction, mutate func(map[string]any)) *cloudpbv2.PostboxEntry {
	return f.controlUntil(app, action, timestamppb.New(f.now.Add(time.Hour)), []*x509.Certificate{f.opLeaf, f.opCA}, mutate)
}

// controlUntil signs a ControlContainer with the given valid_until (nil: none)
// and x5c.
func (f *pbFixture) controlUntil(app string, action cloudpbv2.ContainerAction, validUntil *timestamppb.Timestamp, x5c []*x509.Certificate, mutate func(map[string]any)) *cloudpbv2.PostboxEntry {
	msg := &cloudpbv2.ControlContainerRequest{AssetId: f.uuid, OrganizationId: f.tenant, ContainerName: app, Action: action, ValidUntil: validUntil}
	jws, payload, typ := f.sign(cloudpbv2.DeploymentService_ControlContainer_FullMethodName, msg,
		"asset/"+f.uuid+"/container/"+app, x5c, f.opKey, mutate)
	return f.cloud.add(jws, payload, typ)
}

func (f *pbFixture) cycle() {
	f.t.Helper()
	if err := f.p.cycle(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *pbFixture) outcome(app string) postboxOutcome {
	return f.p.Outcomes()[app]
}

func TestPostboxAppliesVerifiedControlThroughContainerServiceAndAcks(t *testing.T) {
	f := newPBFixture(t)
	e := f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil)
	f.cycle()

	if len(f.ctr.stopCalls) != 1 || f.ctr.stopCalls[0] != "web" {
		t.Fatalf("stop calls = %v, want the existing StopContainer path for web", f.ctr.stopCalls)
	}
	if o := f.outcome("web"); o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED || o.EntryID != 1 || o.CorrelationID == "" {
		t.Fatalf("outcome = %+v", o)
	}
	st := f.p.snapshot()
	if st.HighWater != 1 || st.Acked != 1 || string(st.HeadHash) != string(e.GetEntryHash()) {
		t.Fatalf("state = %+v", st)
	}
	if len(f.cloud.acks) != 1 {
		t.Fatalf("acks = %d", len(f.cloud.acks))
	}
	assertAckShape(t, f, f.cloud.acks[0], e)
}

// assertAckShape checks the AckHead against the device-signer profile and the
// service-protos cfa7f45 vector conformance/reqsig/device-signed-request-
// vectors-v1.json "mldsa65-ack-head-valid": protected header {alg, x5c} with
// x5c = [device leaf], claims {aud, body_sha256, correlation_id, expiry, iat,
// nonce, operation, target{resource, tenant}}, payload_type
// wendycloud.v2.PostboxHeadAck, resource = the device's own SPIFFE URI.
func assertAckShape(t *testing.T, f *pbFixture, ack *cloudpbv2.SignedRequest, e *cloudpbv2.PostboxEntry) {
	t.Helper()
	if ack.GetPayloadType() != "wendycloud.v2.PostboxHeadAck" {
		t.Fatalf("payload_type = %q", ack.GetPayloadType())
	}
	var head cloudpbv2.PostboxHeadAck
	if err := proto.Unmarshal(ack.GetPayload(), &head); err != nil || head.GetEntryId() != e.GetEntryId() || string(head.GetEntryHash()) != string(e.GetEntryHash()) || head.GetObservedAt() == nil {
		t.Fatalf("ack payload = %v (%v)", &head, err)
	}
	parts := strings.Split(string(ack.GetSignature()), ".")
	if len(parts) != 3 {
		t.Fatal("ack is not a compact JWS")
	}
	rawHeader, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var header map[string]any
	_ = json.Unmarshal(rawHeader, &header)
	leaves, _ := certs.ParseCertsFromPEM([]byte(f.certPEM))
	x5c, _ := header["x5c"].([]any)
	if len(header) != 2 || header["alg"] != "ML-DSA-65" || len(x5c) != 1 || x5c[0] != base64.StdEncoding.EncodeToString(leaves[0].Raw) {
		t.Fatalf("ack header = %s", rawHeader)
	}
	if err := verifyMLDSA65(leaves[0], []byte(parts[0]+"."+parts[1]), parts[2]); err != nil {
		t.Fatalf("ack signature: %v", err)
	}
	rawClaims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims postboxDescriptor
	if err := strictJSON(rawClaims, &claims); err != nil {
		t.Fatalf("ack claims %s: %v", rawClaims, err)
	}
	sum := sha256.Sum256(ack.GetPayload())
	if claims.Operation != "wendycloud.v2.DevicePostboxService/AckHead" || claims.Target.Resource != f.uri || claims.Target.Tenant != f.tenant ||
		claims.Target.CA != "" || claims.Audience != postboxAudience || claims.BodySHA256 != b64urlEncode(sum[:]) || claims.Nonce == "" || claims.Expiry <= claims.Iat {
		t.Fatalf("ack claims = %s", rawClaims)
	}
	// The vector's payload decodes as this message (entry_id 42).
	vector, _ := base64.StdEncoding.DecodeString("CCoSINQP0+1Joutjp+kKntJlbRlA5u8/4ryNyhqh9OnzSiXDGgYI9vbE1QY=")
	var v cloudpbv2.PostboxHeadAck
	if err := proto.Unmarshal(vector, &v); err != nil || v.GetEntryId() != 42 || len(v.GetEntryHash()) != 32 {
		t.Fatalf("vector payload = %v (%v)", &v, err)
	}
}

func TestPostboxStartGoesThroughDetachedStart(t *testing.T) {
	f := newPBFixture(t)
	f.ctr.startOutputCh = make(chan ContainerOutput)
	f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_START, nil)
	f.cycle()
	if !f.ctr.startedNames["web"] || f.outcome("web").Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED {
		t.Fatalf("started = %v, outcome = %+v", f.ctr.startedNames, f.outcome("web"))
	}
}

func TestPostboxRefusesNamedAndRunsNothing(t *testing.T) {
	otherRoot, otherRootKey := mkCA(t, "other root", nil, nil)
	otherCA, otherCAKey := mkCA(t, "other operator authority", otherRoot, otherRootKey)

	for _, tc := range []struct {
		name    string
		entry   func(f *pbFixture)
		want    cloudpbv2.ApplyOutcome
		advance bool // the chain verified, so the mark moves past the refusal
	}{
		{"wrong target asset", func(f *pbFixture) {
			// The resource names this device; the signed payload names another.
			msg := &cloudpbv2.ControlContainerRequest{AssetId: uuid.NewString(), OrganizationId: f.tenant, ContainerName: "web", Action: cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP}
			jws, payload, typ := f.sign(cloudpbv2.DeploymentService_ControlContainer_FullMethodName, msg, "asset/"+f.uuid+"/container/web", []*x509.Certificate{f.opLeaf, f.opCA}, f.opKey, nil)
			f.cloud.add(jws, payload, typ)
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_TARGET, true},
		{"wrong target resource", func(f *pbFixture) {
			f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, func(c map[string]any) {
				c["target"] = map[string]any{"tenant": f.tenant, "resource": "asset/" + f.uuid + "/container/db"}
			})
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_TARGET, true},
		{"past valid_until", func(f *pbFixture) {
			f.controlUntil("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, timestamppb.New(f.now), []*x509.Certificate{f.opLeaf, f.opCA}, nil)
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE, true},
		{"valid_until missing", func(f *pbFixture) {
			f.controlUntil("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil, []*x509.Certificate{f.opLeaf, f.opCA}, nil)
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE, true},
		{"wrong root", func(f *pbFixture) {
			leaf := mkLeaf(t, "spiffe://wendy.sh/tenant/"+f.tenant+"/operator/mallory", f.opKey.PublicKey(), otherCA, otherCAKey)
			msg := &cloudpbv2.ControlContainerRequest{AssetId: f.uuid, OrganizationId: f.tenant, ContainerName: "web", Action: cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP}
			jws, payload, typ := f.sign(cloudpbv2.DeploymentService_ControlContainer_FullMethodName, msg, "asset/"+f.uuid+"/container/web", []*x509.Certificate{leaf, otherCA}, f.opKey, nil)
			f.cloud.add(jws, payload, typ)
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_SIGNATURE, true},
		{"device signs as operator", func(f *pbFixture) {
			devKey, _ := mldsa.GenerateKey(mldsa.MLDSA65())
			leaf := mkLeaf(t, "spiffe://wendy.sh/tenant/"+f.tenant+"/device/"+uuid.NewString(), devKey.PublicKey(), f.opCA, f.opCAKey)
			msg := &cloudpbv2.ControlContainerRequest{AssetId: f.uuid, OrganizationId: f.tenant, ContainerName: "web", Action: cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP}
			jws, payload, typ := f.sign(cloudpbv2.DeploymentService_ControlContainer_FullMethodName, msg, "asset/"+f.uuid+"/container/web", []*x509.Certificate{leaf, f.opCA}, devKey, nil)
			f.cloud.add(jws, payload, typ)
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_SIGNATURE, true},
		{"tampered payload", func(f *pbFixture) {
			e := f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil)
			msg := &cloudpbv2.ControlContainerRequest{AssetId: f.uuid, OrganizationId: f.tenant, ContainerName: "web", Action: cloudpbv2.ContainerAction_CONTAINER_ACTION_START}
			e.RequestPayload, _ = proto.Marshal(msg) // payload is outside the chain hash; body_sha256 binds it
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_SIGNATURE, true},
		{"another tenant's operator authority under the same root", func(f *pbFixture) {
			otherTenant, otherTenantKey := mkCA(t, "other tenant intermediate", f.root, f.rootKey)
			otherOp, otherOpKey := mkCA(t, "other operator authority", otherTenant, otherTenantKey)
			leaf := mkLeaf(t, "spiffe://wendy.sh/tenant/"+f.tenant+"/operator/mallory", f.opKey.PublicKey(), otherOp, otherOpKey)
			msg := &cloudpbv2.ControlContainerRequest{AssetId: f.uuid, OrganizationId: f.tenant, ContainerName: "web", Action: cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP}
			jws, payload, typ := f.sign(cloudpbv2.DeploymentService_ControlContainer_FullMethodName, msg, "asset/"+f.uuid+"/container/web", []*x509.Certificate{leaf, otherOp, otherTenant}, f.opKey, nil)
			f.cloud.add(jws, payload, typ)
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_SIGNATURE, true},
		{"valid_until beyond the platform cap", func(f *pbFixture) {
			f.controlUntil("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, timestamppb.New(f.now.Add(postboxPlatformCap+time.Second)), []*x509.Certificate{f.opLeaf, f.opCA}, nil)
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE, true},
		{"header alg is not the leaf's", func(f *pbFixture) {
			f.alg = "ML-DSA-87"
			f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil)
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_SIGNATURE, true},
		{"entry id gap", func(f *pbFixture) {
			e := f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil)
			e.EntryId = 2 // chained from genesis, but entry 1 is missing
			e.EntryHash = postboxEntryHash(e.PrevHash, f.tenant, f.uri, 2, e.RequestJws, e.TsrResponse)
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_SIGNATURE, false},
		{"tampered entry", func(f *pbFixture) {
			e := f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil)
			e.TsrResponse = []byte("forged")
			f.control("db", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil) // the checkpoint covers the next entry, not this one
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_SIGNATURE, false},
		{"chain break", func(f *pbFixture) {
			e := f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil)
			e.PrevHash = make([]byte, 31)
			e.PrevHash = append(e.PrevHash, 1)
			e.EntryHash = postboxEntryHash(e.PrevHash, f.tenant, f.uri, 1, e.RequestJws, e.TsrResponse)
		}, cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_SIGNATURE, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPBFixture(t)
			tc.entry(f)
			f.cycle()
			if len(f.ctr.stopCalls)+len(f.ctr.startedNames) != 0 {
				t.Fatalf("ran something: stop %v start %v", f.ctr.stopCalls, f.ctr.startedNames)
			}
			// Only a request verified as signed for this device reports against
			// an app; every other refusal is its audit line and counter.
			want := postboxOutcome{}
			if tc.want == cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE {
				want = postboxOutcome{EntryID: 1, Outcome: tc.want}
			}
			if o := f.outcome("web"); o.Outcome != want.Outcome || o.EntryID != want.EntryID {
				t.Fatalf("outcome = %+v, want %+v", o, want)
			}
			n := int64(0)
			for _, v := range f.p.refusals {
				n += v
			}
			if n != 1 {
				t.Fatalf("refusals = %v, want one", f.p.refusals)
			}
			st := f.p.snapshot()
			if want := map[bool]int64{true: 1, false: 0}[tc.advance]; st.HighWater != want || st.Acked != want || len(f.cloud.acks) != int(want) {
				t.Fatalf("high water %d, acked %d, acks %d; want %d (never ack an unverified entry)", st.HighWater, st.Acked, len(f.cloud.acks), want)
			}
		})
	}
}

func TestPostboxRefusesReplayedNonceAcrossRestart(t *testing.T) {
	f := newPBFixture(t)
	nonce := uuid.NewString()
	f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, func(c map[string]any) { c["nonce"] = nonce })
	f.cycle()

	f.p = f.newPostbox() // restart: mark, head and nonce set come back from disk
	f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, func(c map[string]any) { c["nonce"] = nonce })
	f.cycle()
	if after := <-f.cloud.pulls; after != 0 {
		t.Fatalf("first pull after %d", after)
	}
	if after := <-f.cloud.pulls; after != 1 {
		t.Fatalf("pull after restart started at %d, want the persisted mark 1", after)
	}
	if len(f.ctr.stopCalls) != 1 {
		t.Fatalf("stop calls = %v, want the replay refused", f.ctr.stopCalls)
	}
	if o := f.outcome("web"); o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE || o.EntryID != 2 {
		t.Fatalf("outcome = %+v", o)
	}
	if st := f.p.snapshot(); st.HighWater != 2 || st.Acked != 2 {
		t.Fatalf("state = %+v", st)
	}
}

func TestPostboxForgedEntryKeepsAppOutcome(t *testing.T) {
	f := newPBFixture(t)
	f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil)
	f.cycle()
	f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_START, nil).RequestPayload = []byte("forged")
	f.cycle()
	if o := f.outcome("web"); o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED || o.EntryID != 1 {
		t.Fatalf("outcome = %+v, want entry 1's APPLIED kept", o)
	}
	if f.p.refusals["signature"] != 1 || f.p.snapshot().HighWater != 2 {
		t.Fatalf("refusals %v, state %+v", f.p.refusals, f.p.snapshot())
	}
}

func TestPostboxFailedSaveLeavesEntryForNextCycle(t *testing.T) {
	f := newPBFixture(t)
	f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil)
	f.p.write = func(string, string, []byte) error { return errors.New("disk full") }
	f.cycle()
	if st := f.p.snapshot(); st.HighWater != 0 || st.HeadHash != nil || len(st.Nonces) != 0 || st.Pending != nil || len(f.ctr.stopCalls) != 0 || len(f.cloud.acks) != 0 {
		t.Fatalf("failed save moved state %+v / stop %v / acks %d", st, f.ctr.stopCalls, len(f.cloud.acks))
	}

	f.p.write = writeProvisioningFile
	f.cycle()
	if len(f.ctr.stopCalls) != 1 || f.outcome("web").Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED || f.p.snapshot().HighWater != 1 {
		t.Fatalf("entry not reprocessed: stop %v, outcome %+v", f.ctr.stopCalls, f.outcome("web"))
	}
}

type crashingControl struct{}

func (crashingControl) StartApp(context.Context, string) error { panic("agent killed") }
func (crashingControl) StopApp(context.Context, string) error  { panic("agent killed") }

func TestPostboxReportsEntryInterruptedByCrash(t *testing.T) {
	f := newPBFixture(t)
	f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil)
	f.p.control = crashingControl{}
	func() {
		defer func() { _ = recover() }()
		_ = f.p.cycle(context.Background())
	}()

	f.p = f.newPostbox() // restart: the claim is on disk, no decision is
	f.cycle()
	o := f.outcome("web")
	if o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED || o.EntryID != 1 || o.CorrelationID == "" {
		t.Fatalf("outcome = %+v, want entry 1 reported APPLY_FAILED (interrupted)", o)
	}
	if st := f.p.snapshot(); st.Pending != nil || st.HighWater != 1 || len(f.ctr.stopCalls) != 0 || f.p.refusals["interrupted"] != 1 {
		t.Fatalf("state %+v, stop %v, refusals %v: want reported once, never retried", st, f.ctr.stopCalls, f.p.refusals)
	}

	f.p = f.newPostbox()
	f.cycle()
	if f.p.refusals["interrupted"] != 0 {
		t.Fatal("interrupted entry reported again after it was settled")
	}
}

func TestPostboxHoldsWithoutVerifiedCheckpoint(t *testing.T) {
	f := newPBFixture(t)
	f.p.checkpoint = verifyCheckpoint // the production verifier; the fake's bytes are no checkpoint
	f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil)
	f.cycle()
	if len(f.ctr.stopCalls) != 0 || f.p.snapshot().HighWater != 0 || len(f.cloud.acks) != 0 {
		t.Fatalf("applied or acked without a verified checkpoint")
	}
	if f.p.refusals["checkpoint"] != 1 {
		t.Fatalf("refusals = %v", f.p.refusals)
	}
}

func TestPostboxAssignmentHeldUntilMaterialPull(t *testing.T) {
	f := newPBFixture(t)
	dep := uuid.NewString()
	resource := "org/" + f.tenant + "/deployment/" + dep + "/device/" + f.uuid
	assign := &cloudpbv2.DeviceAssignment{
		OrganizationId: f.tenant, DeploymentId: dep, Device: f.uri, AppId: "web", AppReleaseId: "rel",
		ManifestDigest: "sha256:ab", DeploymentKeyId: "k1", DesiredState: cloudpbv2.Running_RUNNING,
		ExpiresAt: timestamppb.New(f.now.Add(time.Hour)),
	}
	jws, payload, typ := f.sign(cloudpbv2.ArtifactRegistryService_AssignDevice_FullMethodName, assign, resource, []*x509.Certificate{f.opLeaf, f.opCA}, f.opKey, nil)
	f.cloud.add(jws, payload, typ)
	f.cycle()
	if a := f.p.snapshot().Assignments[dep]; a == nil || a.AppID != "web" || a.ManifestDigest != "sha256:ab" {
		t.Fatalf("assignment not held: %+v", f.p.snapshot().Assignments)
	}
	if o := f.outcome("web"); o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_PULL_REFUSED {
		t.Fatalf("outcome = %+v, want the T14 seam's pull refusal", o)
	}

	unassign := &cloudpbv2.UnassignDeviceRequest{OrganizationId: f.tenant, DeploymentId: dep, Device: f.uri, ValidUntil: timestamppb.New(f.now.Add(time.Hour))}
	jws, payload, typ = f.sign(cloudpbv2.ArtifactRegistryService_UnassignDevice_FullMethodName, unassign, resource, []*x509.Certificate{f.opLeaf, f.opCA}, f.opKey, nil)
	f.cloud.add(jws, payload, typ)
	f.cycle()
	if len(f.p.snapshot().Assignments) != 0 || f.outcome("web").Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED {
		t.Fatalf("unassign: %+v / %+v", f.p.snapshot().Assignments, f.outcome("web"))
	}
}

func TestPostboxDeploymentMustNameThisDevice(t *testing.T) {
	f := newPBFixture(t)
	for _, req := range []*cloudpbv2.CreateDeploymentRequest{
		{OrganizationId: f.tenant, AppReleaseId: "rel", TargetTags: []string{"lab"}},
		{OrganizationId: f.tenant, AppReleaseId: "rel", TargetDeviceIds: []string{f.uuid}, ValidUntil: timestamppb.New(f.now.Add(time.Hour))},
	} {
		jws, payload, typ := f.sign(cloudpbv2.DeploymentService_CreateDeployment_FullMethodName, req, "org/"+f.tenant+"/release/rel", []*x509.Certificate{f.opLeaf, f.opCA}, f.opKey, nil)
		f.cloud.add(jws, payload, typ)
	}
	entries := f.cloud.entries
	id := &postboxIdentity{tenant: f.tenant, uuid: f.uuid, uri: f.uri, anchors: []*x509.Certificate{f.tenantCA}}
	if _, _, o, _, _ := verifyPostboxRequest(entries[0], id, f.now, f.now); o != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_TARGET {
		t.Fatalf("tag-only deployment = %v, want refused: tags are cloud-resolved", o)
	}
	if _, _, o, _, err := verifyPostboxRequest(entries[1], id, f.now, f.now); err != nil {
		t.Fatalf("deployment naming this device = %v: %v", o, err)
	}
}

func TestPostboxNudgeWakesRun(t *testing.T) {
	f := newPBFixture(t)
	f.p.poll = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.p.Run(ctx)
	<-f.cloud.pulls // the first cycle runs at once
	f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil)
	f.p.Nudge(1)
	select {
	case <-f.cloud.pulls:
	case <-time.After(5 * time.Second):
		t.Fatal("nudge did not wake the pull")
	}
}

func TestReportedStateCarriesPostboxOutcomes(t *testing.T) {
	f := newPBFixture(t)
	f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil)
	f.controlUntil("db", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, timestamppb.New(f.now.Add(-time.Second)), []*x509.Certificate{f.opLeaf, f.opCA}, nil)
	f.cycle()
	r := NewReportedState(zap.NewNop(), &fakeAppReporter{apps: []AppReport{{AppID: "web"}}}, nil, "", "", "", "")
	r.AttachPostbox(f.p)
	req, err := r.snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*cloudpbv2.CurrentState{}
	for _, s := range req.GetCurrentStates() {
		got[s.GetAppId()] = s
	}
	if s := got["web"]; s == nil || s.GetPostboxEntryId() != 1 || s.GetOutcome() != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED || s.GetCorrelationId() == "" {
		t.Fatalf("web = %v", s)
	}
	if s := got["db"]; s == nil || s.GetReportedState() != cloudpbv2.Running_ABSENT || s.GetOutcome() != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE {
		t.Fatalf("db = %v", s)
	}
}

func TestPostboxFetchesOperatorCAOnceAndCachesIt(t *testing.T) {
	f := newPBFixture(t)
	f.cloud.orgCAs = map[string][]*x509.Certificate{string(f.opLeaf.RawIssuer): {f.opCA}}
	leafOnly := []*x509.Certificate{f.opLeaf}
	f.controlUntil("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, timestamppb.New(f.now.Add(time.Hour)), leafOnly, nil)
	f.cycle()
	st := f.p.snapshot()
	if len(f.ctr.stopCalls) != 1 || f.cloud.orgCAAsks != 1 || len(st.OrgCAs) != 1 || string(st.OrgCAs[0]) != string(f.opCA.Raw) {
		t.Fatalf("stop %v, asks %d, cached %d", f.ctr.stopCalls, f.cloud.orgCAAsks, len(st.OrgCAs))
	}
	if st.CheckpointMark != 1 || st.TimeFloor != f.now.Unix() {
		t.Fatalf("checkpoint mark %d / time floor %d not persisted", st.CheckpointMark, st.TimeFloor)
	}

	f.p = f.newPostbox() // restart: the cache comes back from disk
	f.controlUntil("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, timestamppb.New(f.now.Add(time.Hour)), leafOnly, nil)
	f.cycle()
	if len(f.ctr.stopCalls) != 2 || f.cloud.orgCAAsks != 1 {
		t.Fatalf("stop %v, asks %d: want the cached CA used", f.ctr.stopCalls, f.cloud.orgCAAsks)
	}
}

// An operator CA rotated under the same subject is a different issuer: the
// cached old CA must not pass for it, and the entry is held until the new CA
// is served and verifies.
func TestPostboxRefetchesARotatedOperatorCA(t *testing.T) {
	f := newPBFixture(t)
	stop := cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP
	f.cloud.orgCAs = map[string][]*x509.Certificate{string(f.opLeaf.RawIssuer): {f.opCA}}
	f.controlUntil("web", stop, timestamppb.New(f.now.Add(time.Hour)), []*x509.Certificate{f.opLeaf}, nil)
	f.cycle() // caches the old CA

	rotated, rotatedKey := mkCA(t, "operator authority", f.tenantCA, f.tenantCAKey)
	leaf := mkLeaf(t, "spiffe://wendy.sh/tenant/"+f.tenant+"/operator/alice", f.opKey.PublicKey(), rotated, rotatedKey)
	f.controlUntil("web", stop, timestamppb.New(f.now.Add(time.Hour)), []*x509.Certificate{leaf}, nil)
	stray, _ := mkCA(t, "operator authority", f.tenantCA, f.tenantCAKey) // same subject, did not issue the leaf
	for i, served := range [][]*x509.Certificate{nil, {f.opCA, stray}} { // NOT_FOUND, then CAs other than the leaf's
		f.cloud.orgCAs = map[string][]*x509.Certificate{}
		if served != nil {
			f.cloud.orgCAs[string(leaf.RawIssuer)] = served
		}
		f.cycle()
		if st := f.p.snapshot(); st.HighWater != 1 || len(f.ctr.stopCalls) != 1 || f.cloud.orgCAAsks != 2+i || len(st.OrgCAs) != 1 {
			t.Fatalf("answer %d: entry not held: high water %d, stop %v, asks %d, cached %d", i, st.HighWater, f.ctr.stopCalls, f.cloud.orgCAAsks, len(st.OrgCAs))
		}
	}

	f.cloud.orgCAs = map[string][]*x509.Certificate{string(leaf.RawIssuer): {rotated}}
	f.cycle()
	if st := f.p.snapshot(); st.HighWater != 2 || len(f.ctr.stopCalls) != 2 || len(st.OrgCAs) != 2 {
		t.Fatalf("rotated CA not used: high water %d, stop %v, cached %d", st.HighWater, f.ctr.stopCalls, len(st.OrgCAs))
	}
}

func TestPostboxHoldsEntryUntilAnOperatorCAVerifies(t *testing.T) {
	f := newPBFixture(t)
	f.controlUntil("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, timestamppb.New(f.now.Add(time.Hour)), []*x509.Certificate{f.opLeaf}, nil)
	held := func(step string) {
		t.Helper()
		st := f.p.snapshot()
		if st.HighWater != 0 || len(st.OrgCAs) != 0 || len(f.ctr.stopCalls) != 0 || len(f.cloud.acks) != 0 || len(f.p.Outcomes()) != 0 {
			t.Fatalf("%s: entry not held: state %+v, stop %v, acks %d, outcomes %v", step, st, f.ctr.stopCalls, len(f.cloud.acks), f.p.Outcomes())
		}
	}

	f.cycle() // NOT_FOUND
	held("not found")
	if f.p.refusals["operator_ca"] != 1 {
		t.Fatalf("refusals = %v", f.p.refusals)
	}

	// Cloud serves another tenant's operator authority under the same root,
	// with the same subject Name: refused, never cached.
	otherTenant, otherTenantKey := mkCA(t, "other tenant intermediate", f.root, f.rootKey)
	otherOp, _ := mkCA(t, "operator authority", otherTenant, otherTenantKey)
	notCA := mkLeaf(t, "spiffe://wendy.sh/tenant/"+f.tenant+"/operator/x", &f.tenantCAKey.PublicKey, f.tenantCA, f.tenantCAKey)
	// Neither the tenant CA itself nor a CA under its Device CA sibling is
	// one the tenant CA issued.
	underDeviceCA, _ := mkCA(t, "operator authority", f.deviceCA, f.deviceCAKey)
	f.cloud.orgCAs = map[string][]*x509.Certificate{string(f.opLeaf.RawIssuer): {otherOp, notCA, f.tenantCA, underDeviceCA}}
	f.cycle()
	held("other tenant / not a CA / the tenant CA / under the Device CA")
	if f.p.refusals["not_under_org_ca"] != 3 || f.p.refusals["not_a_ca"] != 1 || f.p.refusals["operator_ca"] != 2 {
		t.Fatalf("refusals = %v", f.p.refusals)
	}

	f.cloud.orgCAs = map[string][]*x509.Certificate{string(f.opLeaf.RawIssuer): {f.opCA}}
	f.cycle()
	if len(f.ctr.stopCalls) != 1 || f.p.snapshot().HighWater != 1 || f.outcome("web").Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED {
		t.Fatalf("held entry not applied once the CA verified: stop %v, outcome %+v", f.ctr.stopCalls, f.outcome("web"))
	}
}

func TestPostboxValidityEdges(t *testing.T) {
	t.Run("call expiry long past is ignored", func(t *testing.T) {
		f := newPBFixture(t)
		f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, func(c map[string]any) {
			c["iat"], c["expiry"] = f.now.Unix()-1800, f.now.Unix()-1770 // offline for 30 minutes
		})
		f.cycle()
		if f.outcome("web").Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED {
			t.Fatalf("outcome = %+v", f.outcome("web"))
		}
	})
	t.Run("valid_until at the platform cap passes", func(t *testing.T) {
		f := newPBFixture(t)
		f.controlUntil("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, timestamppb.New(f.now.Add(postboxPlatformCap)), []*x509.Certificate{f.opLeaf, f.opCA}, nil) // the cap counts from the entry's timestamp
		f.cycle()
		if f.outcome("web").Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED {
			t.Fatalf("outcome = %+v", f.outcome("web"))
		}
	})
	t.Run("checkpoint time ahead of the clock makes it stale", func(t *testing.T) {
		f := newPBFixture(t)
		f.cloud.genTime = f.now.Add(2 * time.Hour)                             // the TSA's time; this clock runs behind
		f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil) // valid_until = clock + 1 h
		f.cycle()
		if f.outcome("web").Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE || f.p.refusals["stale"] != 1 || len(f.ctr.stopCalls) != 0 {
			t.Fatalf("outcome = %+v, refusals %v", f.outcome("web"), f.p.refusals)
		}
		if f.p.snapshot().TimeFloor != f.cloud.genTime.Unix() {
			t.Fatalf("time floor %d", f.p.snapshot().TimeFloor)
		}
	})
	for _, tc := range []struct {
		name, reason string
		until        func(f *pbFixture) *timestamppb.Timestamp
	}{
		{"missing", "validity_missing", func(*pbFixture) *timestamppb.Timestamp { return nil }},
		{"above platform cap", "above_platform_cap", func(f *pbFixture) *timestamppb.Timestamp {
			return timestamppb.New(f.now.Add(postboxPlatformCap + time.Second))
		}},
		{"at trusted now", "stale", func(f *pbFixture) *timestamppb.Timestamp { return timestamppb.New(f.now) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPBFixture(t)
			f.controlUntil("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, tc.until(f), []*x509.Certificate{f.opLeaf, f.opCA}, nil)
			f.cycle()
			if f.outcome("web").Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE || f.p.refusals[tc.reason] != 1 || f.p.snapshot().HighWater != 1 {
				t.Fatalf("outcome = %+v, refusals %v: want %s, consumed", f.outcome("web"), f.p.refusals, tc.reason)
			}
		})
	}
}

func TestPostboxVerifiedButFailedApplyReportsApplyFailed(t *testing.T) {
	f := newPBFixture(t)
	f.ctr.stopErr = errors.New("containerd unavailable")
	f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil)
	f.cycle()
	if o := f.outcome("web"); o.Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED || o.EntryID != 1 || f.p.refusals["apply"] != 1 {
		t.Fatalf("outcome = %+v, refusals %v", o, f.p.refusals)
	}
	if st := f.p.snapshot(); st.HighWater != 1 || st.Acked != 1 {
		t.Fatalf("state %+v: a failed apply is consumed and acked, not retried", st)
	}
}

func TestPostboxHoldsOnCheckpointBelowItsMark(t *testing.T) {
	f := newPBFixture(t)
	f.control("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, nil)
	if err := f.p.load(f.uri); err != nil {
		t.Fatal(err)
	}
	f.p.state.CheckpointMark = 2 // a newer checkpoint was verified before
	f.cycle()
	if len(f.ctr.stopCalls) != 0 || f.p.snapshot().HighWater != 0 || f.p.refusals["checkpoint"] != 1 {
		t.Fatalf("applied under a rolled-back checkpoint: stop %v, refusals %v", f.ctr.stopCalls, f.p.refusals)
	}
}

// The entry's RFC 3161 token is its signing time (RULINGS-LOG 2026-10-24
// 03:25): the operator chain is checked and the platform cap counted at its
// genTime, an iat after it is refused, and an entry without a verifying
// token is held.
func TestPostboxEntryTimestampIsTheSigningTime(t *testing.T) {
	stop := cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP
	t.Run("valid: cap counted from genTime, not iat", func(t *testing.T) {
		f := newPBFixture(t)
		// From iat the cap is exceeded by an hour; from genTime it is met.
		f.controlUntil("web", stop, timestamppb.New(f.now.Add(postboxPlatformCap)), []*x509.Certificate{f.opLeaf, f.opCA},
			func(c map[string]any) { c["iat"] = f.now.Unix() - 3600 })
		f.cycle()
		if f.outcome("web").Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_APPLIED || len(f.ctr.stopCalls) != 1 {
			t.Fatalf("outcome = %+v, refusals %v", f.outcome("web"), f.p.refusals)
		}
	})
	t.Run("iat after the timestamp refused", func(t *testing.T) {
		f := newPBFixture(t)
		f.control("web", stop, func(c map[string]any) { c["iat"] = f.now.Add(postboxCheckpointSkew).Unix() + 1 })
		f.cycle()
		if f.outcome("web").Outcome != cloudpbv2.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE || f.p.refusals["iat_after_timestamp"] != 1 || f.p.snapshot().HighWater != 1 || len(f.ctr.stopCalls) != 0 {
			t.Fatalf("outcome = %+v, refusals %v: want iat_after_timestamp, consumed", f.outcome("web"), f.p.refusals)
		}
	})
	t.Run("back-dated iat from a cert expired at genTime refused", func(t *testing.T) {
		f := newPBFixture(t)
		u, _ := url.Parse("spiffe://wendy.sh/tenant/" + f.tenant + "/operator/alice")
		expired := mkCert(t, &x509.Certificate{
			Subject: pkix.Name{CommonName: "leaf"}, URIs: []*url.URL{u},
			NotBefore: f.now.Add(-2 * time.Hour), NotAfter: f.now.Add(-10 * time.Minute),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}, f.opCA, f.opKey.PublicKey(), f.opCAKey)
		f.controlUntil("web", stop, timestamppb.New(f.now.Add(time.Hour)), []*x509.Certificate{expired, f.opCA},
			func(c map[string]any) { c["iat"] = f.now.Unix() - 1800 }) // inside the cert's old validity
		f.cycle()
		// An unverified signer sets no app outcome: audit and refusal count only.
		if f.p.refusals["signature"] != 1 || f.p.snapshot().HighWater != 1 || len(f.ctr.stopCalls) != 0 {
			t.Fatalf("refusals %v, state %+v: want signature, consumed", f.p.refusals, f.p.snapshot())
		}
	})
	for name, token := range map[string]func(f *pbFixture, e *cloudpbv2.PostboxEntry) []byte{
		"token missing":          func(*pbFixture, *cloudpbv2.PostboxEntry) []byte { return nil },
		"token over another jws": func(f *pbFixture, _ *cloudpbv2.PostboxEntry) []byte { return f.timestamp([]byte("other"), f.now) },
		"token by an unknown TSA": func(f *pbFixture, e *cloudpbv2.PostboxEntry) []byte {
			f.tsaSigner = f.opLeaf
			f.tsaKey = f.opKey
			return f.timestamp(e.RequestJws, f.now)
		},
	} {
		t.Run(name+" holds", func(t *testing.T) {
			f := newPBFixture(t)
			e := f.control("web", stop, nil)
			e.TsrResponse = token(f, e)
			e.EntryHash = postboxEntryHash(e.PrevHash, f.tenant, f.uri, 1, e.RequestJws, e.TsrResponse)
			f.cycle()
			if st := f.p.snapshot(); st.HighWater != 0 || len(f.ctr.stopCalls) != 0 || len(f.p.Outcomes()) != 0 || f.p.refusals["entry_timestamp"] != 1 {
				t.Fatalf("not held: state %+v, outcomes %v, refusals %v", st, f.p.Outcomes(), f.p.refusals)
			}
		})
	}
}

// GetOrgCAs: of a response carrying the Device CA and a sibling CA (both
// issued by the tenant CA) besides the requested one, only the CA whose
// subject is the requested issuer is cached.
func TestPostboxCachesOnlyTheRequestedOperatorCA(t *testing.T) {
	f := newPBFixture(t)
	sibling, _ := mkCA(t, "sibling authority", f.tenantCA, f.tenantCAKey)
	f.cloud.orgCAs = map[string][]*x509.Certificate{string(f.opLeaf.RawIssuer): {f.deviceCA, sibling, f.opCA}}
	f.controlUntil("web", cloudpbv2.ContainerAction_CONTAINER_ACTION_STOP, timestamppb.New(f.now.Add(time.Hour)), []*x509.Certificate{f.opLeaf}, nil)
	f.cycle()
	if st := f.p.snapshot(); len(st.OrgCAs) != 1 || string(st.OrgCAs[0]) != string(f.opCA.Raw) || len(f.ctr.stopCalls) != 1 {
		t.Fatalf("cached %d CAs, stop %v: want only the operator authority", len(st.OrgCAs), f.ctr.stopCalls)
	}
}
