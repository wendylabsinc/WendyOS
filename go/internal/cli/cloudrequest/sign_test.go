package cloudrequest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

const testTenant = "2558fd76-afc7-466e-9613-6b715296a526"

func testAuth(t *testing.T) (*config.AuthConfig, *mldsa.PrivateKey, []byte) {
	t.Helper()
	// WDY-3032: the operator credential is ML-DSA-65, so the fixture must be
	// too — newSigner refuses anything else.
	key, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	principal := "spiffe://wendy.sh/tenant/" + testTenant + "/operator/op-42"
	u, err := url.Parse(principal)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "op-42"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return &config.AuthConfig{
		OAuthIssuer: "https://auth.wendy.sh/realms/test",
		Certificates: []config.CertificateInfo{{
			PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
			PemPrivateKey:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
			PrincipalURI:   principal,
		}},
	}, key, der
}

func TestSignRequestProducesContractEnvelope(t *testing.T) {
	auth, key, leafDER := testAuth(t)
	signer, err := newSigner(auth)
	if err != nil {
		t.Fatalf("newSigner: %v", err)
	}
	fixed := time.Unix(1_784_659_200, 0)
	signer.now = func() time.Time { return fixed }

	req := &cloudpbv2.EnrollDeviceRequest{DeviceId: "dev-1", Name: "edge-one", EnrollmentRequestJws: []byte("a.b.c")}
	signed, err := signer.signRequest(cloudpbv2.DeviceEnrollmentService_EnrollDevice_FullMethodName, req, false, uuid.NewString())
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	// payload_type is the method's request message, as the proto comment names it.
	if signed.GetPayloadType() != "wendycloud.v2.EnrollDeviceRequest" || signed.PkiManagementRequest != nil {
		t.Fatalf("payload_type = %q, pki_management_request = %v", signed.GetPayloadType(), signed.PkiManagementRequest)
	}
	var decoded cloudpbv2.EnrollDeviceRequest
	if err := proto.Unmarshal(signed.GetPayload(), &decoded); err != nil || !proto.Equal(&decoded, req) {
		t.Fatalf("payload does not decode to the request: %v", err)
	}

	segments := strings.Split(string(signed.GetSignature()), ".")
	if len(segments) != 3 {
		t.Fatalf("JWS has %d segments", len(segments))
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(segments[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var header struct {
		Alg string   `json:"alg"`
		X5C []string `json:"x5c"`
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header.Alg != "ML-DSA-65" || len(header.X5C) != 1 || header.X5C[0] != base64.StdEncoding.EncodeToString(leafDER) {
		t.Fatalf("header = %#v, want ML-DSA-65 with the leaf alone", header)
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	resource := "org/" + testTenant + "/device/dev-1"
	if !strings.HasPrefix(string(payloadBytes), `{"aud":`) ||
		!strings.Contains(string(payloadBytes), `"target":{"resource":"`+resource+`","tenant":"`+testTenant+`"}`) {
		t.Fatalf("payload is not in canonical member order: %s", payloadBytes)
	}
	var descriptor struct {
		Audience      string `json:"aud"`
		BodyDigest    string `json:"body_sha256"`
		CorrelationID string `json:"correlation_id"`
		Expiry        int64  `json:"expiry"`
		IssuedAt      int64  `json:"iat"`
		Nonce         string `json:"nonce"`
		Operation     string `json:"operation"`
	}
	if err := json.Unmarshal(payloadBytes, &descriptor); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	// body_sha256 covers exactly the payload bytes carried in the envelope.
	sum := sha256.Sum256(signed.GetPayload())
	if descriptor.Audience != brokerAudience || descriptor.BodyDigest != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Fatalf("descriptor audience=%q body=%q", descriptor.Audience, descriptor.BodyDigest)
	}
	if descriptor.IssuedAt != fixed.Unix() || descriptor.Expiry != fixed.Add(signatureTTL).Unix() {
		t.Fatalf("descriptor times = iat %d expiry %d", descriptor.IssuedAt, descriptor.Expiry)
	}
	if descriptor.Operation != "wendycloud.v2.DeviceEnrollmentService/EnrollDevice" {
		t.Errorf("operation = %q", descriptor.Operation)
	}
	if id, err := uuid.Parse(descriptor.CorrelationID); err != nil || id.String() != descriptor.CorrelationID {
		t.Errorf("correlation_id = %q, want a lower-case UUID", descriptor.CorrelationID)
	}
	if nonce, err := base64.RawURLEncoding.DecodeString(descriptor.Nonce); err != nil || len(nonce) != 32 {
		t.Errorf("nonce = %q, err %v", descriptor.Nonce, err)
	}

	sig, err := base64.RawURLEncoding.DecodeString(segments[2])
	if err != nil {
		t.Fatalf("signature decode: %v", err)
	}
	// Verified exactly the way pki-core does it: over the signing input, no
	// pre-hash, nil options.
	if err := mldsa.Verify(key.Public().(*mldsa.PublicKey), []byte(segments[0]+"."+segments[1]), sig, nil); err != nil {
		t.Fatalf("JWS signature does not verify: %v", err)
	}
}

func TestSignRequestRefusesMismatchedRequest(t *testing.T) {
	auth, _, _ := testAuth(t)
	signer, err := newSigner(auth)
	if err != nil {
		t.Fatal(err)
	}
	method := cloudpbv2.DeviceEnrollmentService_EnrollDevice_FullMethodName
	if _, err := signer.signRequest(method, &cloudpbv2.GetAssetRequest{}, true, uuid.NewString()); err == nil {
		t.Fatal("signed a request whose type is not the method's")
	}
	if _, err := signer.signRequest(cloudpbv2.AssetService_GetAsset_FullMethodName, &cloudpbv2.GetAssetRequest{}, true, uuid.NewString()); err == nil {
		t.Fatal("signed a method that is not operator-signed")
	}
	auth.Certificates[0].PrincipalURI = ""
	conn := &fakeCloud{t: t}
	if err := Invoke(context.Background(), conn, auth, method, &cloudpbv2.EnrollDeviceRequest{}, &cloudpbv2.EnrollDeviceResponse{}); err == nil || len(conn.calls) != 0 {
		t.Fatal("invoked without an operator certificate")
	}
}

// fakeCloud records each signed call as (method, leaf reference) and answers
// RegisterOperatorLeaf with the kid of the x5c leaf, like the broker.
type fakeCloud struct {
	t           *testing.T
	calls       []string
	refuse      []error // per main-method call, in order; success when exhausted
	registerErr error
	wrongID     bool
	nonces      map[string]bool
	correlation map[string]bool
}

func (f *fakeCloud) Invoke(_ context.Context, method string, args, reply any, _ ...grpc.CallOption) error {
	in := args.(*cloudpbv2.SignedRequest)
	var h struct {
		X5C []string
		Kid string
	}
	parts := strings.Split(string(in.GetSignature()), ".")
	header, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if err := json.Unmarshal(header, &h); err != nil {
		f.t.Fatal(err)
	}
	var claims struct {
		Nonce         string `json:"nonce"`
		CorrelationID string `json:"correlation_id"`
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if err := json.Unmarshal(payload, &claims); err != nil {
		f.t.Fatal(err)
	}
	if f.nonces == nil {
		f.nonces, f.correlation = map[string]bool{}, map[string]bool{}
	}
	if f.nonces[claims.Nonce] {
		f.t.Fatalf("%s reused nonce %q", method, claims.Nonce)
	}
	f.nonces[claims.Nonce], f.correlation[claims.CorrelationID] = true, true
	if (len(h.X5C) == 1) == (h.Kid != "") {
		f.t.Fatalf("%s: header names the leaf by x5c %d and kid %q; want exactly one", method, len(h.X5C), h.Kid)
	}
	ref := "kid"
	if h.Kid == "" {
		ref = "x5c"
	}
	f.calls = append(f.calls, method+" "+ref)
	if method == cloudpbv2.OperatorSessionService_RegisterOperatorLeaf_FullMethodName {
		if f.registerErr != nil {
			return f.registerErr
		}
		der, _ := base64.StdEncoding.DecodeString(h.X5C[0])
		sum := sha256.Sum256(der)
		kid := base64.RawURLEncoding.EncodeToString(sum[:])
		if f.wrongID {
			kid = "other"
		}
		reply.(*cloudpbv2.RegisterOperatorLeafResponse).Kid = kid
		return nil
	}
	if len(f.refuse) > 0 {
		err := f.refuse[0]
		f.refuse = f.refuse[1:]
		return err
	}
	return nil
}

func (f *fakeCloud) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, status.Error(codes.Unimplemented, "no streams")
}

// WDY-3463: register once (x5c), sign by kid; a refused kid call is retried
// exactly once with x5c and the registration dropped; a second refusal is final.
func TestInvokeRegistersOnceThenSignsByKID(t *testing.T) {
	const (
		enroll   = cloudpbv2.DeviceEnrollmentService_EnrollDevice_FullMethodName
		register = cloudpbv2.OperatorSessionService_RegisterOperatorLeaf_FullMethodName
	)
	req := &cloudpbv2.EnrollDeviceRequest{DeviceId: "dev-1"}
	denied := status.Error(codes.PermissionDenied, "the operator request signature was rejected")
	notStored := status.Error(codes.FailedPrecondition, "leaf not stored; present the certificate")
	for _, tc := range []struct {
		name        string
		refuse      []error
		registerErr error
		wrongID     bool
		wantErr     bool
		want        []string
	}{
		{name: "steady state", want: []string{register + " x5c", enroll + " kid", enroll + " kid"}},
		{name: "kid refused", refuse: []error{denied}, want: []string{register + " x5c", enroll + " kid", enroll + " x5c", register + " x5c", enroll + " kid"}},
		{name: "leaf not stored", refuse: []error{notStored}, want: []string{register + " x5c", enroll + " kid", enroll + " x5c", register + " x5c", enroll + " kid"}},
		{name: "refused twice", refuse: []error{denied, denied}, wantErr: true, want: []string{register + " x5c", enroll + " kid", enroll + " x5c"}},
		{name: "handler precondition not retried", refuse: []error{status.Error(codes.FailedPrecondition, "fabric leg not configured")}, wantErr: true, want: []string{register + " x5c", enroll + " kid"}},
		{name: "other error not retried", refuse: []error{status.Error(codes.InvalidArgument, "bad")}, wantErr: true, want: []string{register + " x5c", enroll + " kid"}},
		{name: "registration unimplemented signs with x5c", registerErr: status.Error(codes.Unimplemented, ""), want: []string{register + " x5c", enroll + " x5c", register + " x5c", enroll + " x5c"}},
		{name: "registration refused is loud", registerErr: denied, wantErr: true, want: []string{register + " x5c"}},
		{name: "registered kid mismatch", wrongID: true, wantErr: true, want: []string{register + " x5c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth, _, _ := testAuth(t)
			conn := &fakeCloud{t: t, refuse: tc.refuse, registerErr: tc.registerErr, wrongID: tc.wrongID}
			err := Invoke(context.Background(), conn, auth, enroll, req, &cloudpbv2.EnrollDeviceResponse{})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Invoke err = %v, wantErr %v", err, tc.wantErr)
			}
			// Registration and any retry belong to one flow.
			if len(conn.correlation) != 1 {
				t.Fatalf("one Invoke used %d correlation ids", len(conn.correlation))
			}
			// A second call in the same process reuses the registration (or
			// registers again after a refusal dropped it).
			if !tc.wantErr {
				if err := Invoke(context.Background(), conn, auth, enroll, req, &cloudpbv2.EnrollDeviceResponse{}); err != nil {
					t.Fatal(err)
				}
			}
			if strings.Join(conn.calls, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(conn.calls, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

func TestInvokeSignsDeviceForwardedMethodsWithX5C(t *testing.T) {
	for _, method := range []string{cloudpbv2.DeploymentService_CreateDeployment_FullMethodName, cloudpbv2.DeploymentService_ControlContainer_FullMethodName} {
		if !x5cOnly[method] {
			t.Errorf("%s may be signed by kid", method)
		}
	}
	auth, _, _ := testAuth(t)
	method := cloudpbv2.DeviceEnrollmentService_EnrollDevice_FullMethodName
	x5cOnly[method] = true
	defer delete(x5cOnly, method)
	conn := &fakeCloud{t: t}
	if err := Invoke(context.Background(), conn, auth, method, &cloudpbv2.EnrollDeviceRequest{DeviceId: "d"}, &cloudpbv2.EnrollDeviceResponse{}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(conn.calls, ",") != method+" x5c" {
		t.Fatalf("calls = %v, want one x5c-signed call and no registration", conn.calls)
	}
}

func TestKIDIsLeafThumbprint(t *testing.T) {
	auth, _, leafDER := testAuth(t)
	signer, err := newSigner(auth)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signer.signRequest(cloudpbv2.DeviceEnrollmentService_EnrollDevice_FullMethodName, &cloudpbv2.EnrollDeviceRequest{DeviceId: "dev-1"}, true, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	header, _ := base64.RawURLEncoding.DecodeString(strings.Split(string(signed.GetSignature()), ".")[0])
	sum := sha256.Sum256(leafDER)
	if want := `{"alg":"ML-DSA-65","kid":"` + base64.RawURLEncoding.EncodeToString(sum[:]) + `"}`; string(header) != want {
		t.Fatalf("header = %s, want %s", header, want)
	}
}

// The method option is the one list of signed methods (WDY-3458): it must agree
// with the input type everywhere, and cover every method the CLI signs.
func TestSignedMethodsAgreeWithProtoOption(t *testing.T) {
	signedType := (&cloudpbv2.SignedRequest{}).ProtoReflect().Descriptor().FullName()
	marked := map[string]bool{}
	protoregistry.GlobalFiles.RangeFilesByPackage("wendycloud.v2", func(fd protoreflect.FileDescriptor) bool {
		for i := 0; i < fd.Services().Len(); i++ {
			svc := fd.Services().Get(i)
			for j := 0; j < svc.Methods().Len(); j++ {
				m := svc.Methods().Get(j)
				name := "/" + string(svc.FullName()) + "/" + string(m.Name())
				opt := proto.GetExtension(m.Options(), cloudpbv2.E_SignedRequest).(bool)
				if opt != (m.Input().FullName() == signedType) {
					t.Errorf("%s: signed_request option %v but input %s", name, opt, m.Input().FullName())
				}
				marked[name] = opt
			}
		}
		return true
	})
	if !marked[cloudpbv2.AssetService_CreateAsset_FullMethodName] {
		t.Fatal("vendored protos carry no signed_request options; re-pin them")
	}
	for method := range signedResources {
		if !marked[method] {
			t.Errorf("%s has a signing resource but is not marked signed", method)
		}
	}
}

func TestNewSignerUsesOperatorCertificateWithoutOAuthBookkeeping(t *testing.T) {
	auth, _, _ := testAuth(t)
	auth.OAuthIssuer = ""
	signer, err := newSigner(auth)
	if err != nil || signer == nil {
		t.Fatalf("newSigner = %#v, %v; want signer, nil", signer, err)
	}
}

func TestNewSignerSkipsLegacyCertificateWithoutPrincipal(t *testing.T) {
	auth, _, _ := testAuth(t)
	auth.OAuthIssuer = ""
	auth.Certificates[0].PrincipalURI = ""
	signer, err := newSigner(auth)
	if err != nil || signer != nil {
		t.Fatalf("newSigner = %#v, %v; want nil, nil", signer, err)
	}
}

func TestNewSignerRejectsNonOperatorPrincipal(t *testing.T) {
	auth, _, _ := testAuth(t)
	auth.Certificates[0].PrincipalURI = "urn:wendy:org:7:user:op-42"
	if _, err := newSigner(auth); err == nil {
		t.Fatal("newSigner accepted a non-operator principal")
	}
}

// WDY-3032 is a hard cutover with no ECDSA fallback: an EC operator session is
// refused rather than signed with, and the refusal names the fix. Mirrors
// TestDPoPRefusesLegacyECSessions for the request-signature path.
func TestNewSignerRefusesECOperatorKey(t *testing.T) {
	auth, _, _ := testAuth(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(auth.Certificates[0].PrincipalURI)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), URIs: []*url.URL{u}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	auth.Certificates[0].PemCertificate = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	auth.Certificates[0].PemPrivateKey = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))

	signer, err := newSigner(auth)
	if err == nil {
		t.Fatalf("newSigner accepted an ECDSA operator key (%T); the cutover is not enforced", signer.privateKey)
	}
	if !strings.Contains(err.Error(), "ML-DSA-65") || !strings.Contains(err.Error(), "wendy auth login") {
		t.Errorf("refusal should name the required algorithm and the fix, got: %v", err)
	}
}

// The steady state: an ML-DSA session signs with ML-DSA-65, asserted on the key
// actually selected rather than inferred from the absence of an error.
func TestNewSignerSelectsMLDSA65(t *testing.T) {
	auth, _, _ := testAuth(t)
	signer, err := newSigner(auth)
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := signer.privateKey.Public().(*mldsa.PublicKey)
	if !ok || pub.Parameters() != mldsa.MLDSA65() {
		t.Fatalf("selected signing key %T, want ML-DSA-65", signer.privateKey.Public())
	}
}

func TestEnrollmentRequestUsesFreshReplayID(t *testing.T) {
	auth, _, _ := testAuth(t)
	var previous string
	for i := 0; i < 2; i++ {
		artifact, err := EnrollmentRequest(auth, "fleet/sim")
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(string(artifact), ".")
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		var claims map[string]any
		if err := json.Unmarshal(payload, &claims); err != nil {
			t.Fatal(err)
		}
		jti, _ := claims["jti"].(string)
		if jti == "" || jti == previous {
			t.Fatal("enrollment request reused its replay ID")
		}
		previous = jti
		if _, ok := claims["csr_key_binding"]; ok {
			t.Fatal("unsupported key binding included")
		}
		if _, ok := claims["attestation_ref"]; ok {
			t.Fatal("unsupported attestation included")
		}
	}
	if _, err := EnrollmentRequest(nil, "sim"); err == nil {
		t.Fatal("unsigned enrollment request accepted")
	}
}

// Model the large ML-DSA certificate chain returned by PKI. It belongs in the
// enrollment artifact, but the Cloud request signature carries only the leaf.
func TestCloudSignatureOmitsLargeIssuerChain(t *testing.T) {
	auth, _, leafDER := testAuth(t)
	issuerDER := leafDER
	auth.Certificates[0].PemCertificateChain = strings.Repeat(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuerDER})), 3)
	signer, err := newSigner(auth)
	if err != nil {
		t.Fatal(err)
	}
	header := func(jws string) []string {
		t.Helper()
		parts := strings.Split(jws, ".")
		raw, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil {
			t.Fatal(err)
		}
		var h struct{ X5C []string }
		if err := json.Unmarshal(raw, &h); err != nil {
			t.Fatal(err)
		}
		return h.X5C
	}
	// A representative 43-char base64url SHA-256 digest so the size assertion
	// reflects a real body_sha256 field.
	descriptor, err := signer.sign("wendycloud.v2.DeviceEnrollmentService/EnrollDevice", "org/"+testTenant+"/device/sim", "47DEQpj8HBSa-_TImW-5JCeuQeRkm5NMpJWZG3hSuFU", false, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	certs := header(descriptor)
	if len(certs) != 1 || certs[0] != base64.StdEncoding.EncodeToString(leafDER) {
		t.Fatal("Cloud request signature must carry only the operator leaf")
	}
	artifact, err := EnrollmentRequest(auth, "sim")
	if err != nil {
		t.Fatal(err)
	}
	chain := header(string(artifact))
	if len(chain) != 4 || chain[0] != certs[0] || chain[1] != base64.StdEncoding.EncodeToString(issuerDER) {
		t.Fatal("enrollment body lost its issuer chain")
	}
}

func TestSignerNormalizesCertificateChain(t *testing.T) {
	auth, _, _ := testAuth(t)
	block, _ := pem.Decode([]byte(auth.Certificates[0].PemCertificate))
	block.Bytes = append(block.Bytes, 0, 0)
	padded := string(pem.EncodeToMemory(block))
	auth.Certificates[0].PemCertificate = padded
	auth.Certificates[0].PemCertificateChain = padded
	signer, err := newSigner(auth)
	if err != nil {
		t.Fatal(err)
	}
	for _, encoded := range signer.x5c {
		der, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := x509.ParseCertificate(der); err != nil {
			t.Fatalf("signer retained malformed DER: %v", err)
		}
	}
}
