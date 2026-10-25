package cloud

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/cloud/internal/cloudpb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform/platformtest"
	"google.golang.org/protobuf/proto"
)

const tenant = "2558fd76-afc7-466e-9613-6b715296a526"
const assetID = "041766cd-9777-439c-b3b4-9a2a45cf90dc"
const deleteMethod = "/wendycloud.v2.AssetService/DeleteAsset"

type testEntropy struct{ next byte }

func (e *testEntropy) RandomBytes(ctx context.Context, size int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.next++
	return bytes.Repeat([]byte{e.next}, size), nil
}

func testSigner(t *testing.T) *Signer {
	t.Helper()
	key := platformtest.Signer{
		AlgorithmFunc: func() platform.SignatureAlgorithm { return platform.MLDSA65 },
		CertificateChainFunc: func(ctx context.Context) ([][]byte, error) {
			return [][]byte{[]byte(t.Name()), []byte("opaque-issuer")}, ctx.Err()
		},
		SignFunc: func(ctx context.Context, data []byte) ([]byte, error) { return append([]byte(nil), data...), ctx.Err() },
	}
	s, err := NewSigner(context.Background(), key, tenant, &testEntropy{}, SignerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(1770000000, 0) }
	registeredKIDs.Delete(s.kid)
	t.Cleanup(func() { registeredKIDs.Delete(s.kid) })
	return s
}

func claims(t *testing.T, compact []byte) (map[string]any, map[string]any) {
	t.Helper()
	parts := strings.Split(string(compact), ".")
	if len(parts) != 3 {
		t.Fatalf("invalid compact JWS %q", compact)
	}
	signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if string(signature) != parts[0]+"."+parts[1] {
		t.Fatal("host did not sign exact JWS input")
	}
	var header, payload map[string]any
	for i, dest := range []*map[string]any{&header, &payload} {
		wire, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(wire, dest); err != nil {
			t.Fatal(err)
		}
	}
	return header, payload
}

func wire(t *testing.T, message proto.Message) []byte {
	t.Helper()
	data, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestExactBodyAndSameKeyDeletionAuthority(t *testing.T) {
	s := testSigner(t)
	request := &pb.DeleteAssetRequest{Id: assetID, ExpectedDeviceId: proto.String(assetID), ResetCertificateSha256: proto.String("installed-leaf-hash")}
	signed, err := s.signRequest(context.Background(), deleteMethod, request, false, "flow.1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(signed.Payload, wire(t, request)) || signed.PayloadType != "wendycloud.v2.DeleteAssetRequest" {
		t.Fatal("payload changed")
	}
	header, payload := claims(t, signed.Signature)
	if header["alg"] != "ML-DSA-65" || len(header["x5c"].([]any)) != 1 || header["kid"] != nil {
		t.Fatalf("leaf reference %v", header)
	}
	sum := sha256.Sum256(signed.Payload)
	if payload["body_sha256"] != base64.RawURLEncoding.EncodeToString(sum[:]) || payload["correlation_id"] != "flow.1" || payload["operation"] != "wendycloud.v2.AssetService/DeleteAsset" || payload["aud"] != BrokerAudience {
		t.Fatalf("descriptor %v", payload)
	}
	target := payload["target"].(map[string]any)
	if target["tenant"] != tenant || target["resource"] != "asset/"+assetID || payload["expiry"].(float64)-payload["iat"].(float64) != 30 {
		t.Fatalf("target/freshness %v", payload)
	}
	managementHeader, management := claims(t, signed.PkiManagementRequest)
	if len(managementHeader["x5c"].([]any)) != 2 || management["principal"] != "spiffe://wendy.sh/tenant/"+tenant+"/device/"+assetID || management["op"] != "revoke_principal" {
		t.Fatalf("management %v %v", managementHeader, management)
	}
}

func TestRegistrationRefusalRetryAndTrailers(t *testing.T) {
	for _, code := range []platform.RPCStatusCode{platform.RPCPermissionDenied, platform.RPCFailedPrecondition} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			s := testSigner(t)
			var methods, nonces, jtis []string
			var refs []bool
			metadata := platform.Metadata{{Name: "x-correlation-id", Value: []byte("flow.origin")}, {Name: "custom-bin", Value: []byte{0, 255}}}
			trailer := platform.Metadata{{Name: CollidingFieldMetadataKey, Value: []byte("name")}}
			transport := platformtest.Transport{UnaryFunc: func(ctx context.Context, r platform.RPCRequest) (platform.RPCResponse, error) {
				methods = append(methods, r.Method)
				if !reflect.DeepEqual(r.Metadata, metadata) {
					t.Fatal("metadata changed")
				}
				var signed pb.SignedRequest
				if err := proto.Unmarshal(r.Message, &signed); err != nil {
					t.Fatal(err)
				}
				h, c := claims(t, signed.Signature)
				if c["correlation_id"] != "flow.origin" {
					t.Fatal("flow replaced")
				}
				nonces = append(nonces, c["nonce"].(string))
				refs = append(refs, h["kid"] != nil)
				if len(methods) == 1 {
					return platform.RPCResponse{Message: wire(t, &pb.RegisterOperatorLeafResponse{Kid: s.kid})}, nil
				}
				_, m := claims(t, signed.PkiManagementRequest)
				jtis = append(jtis, m["jti"].(string))
				if len(methods) == 2 {
					return platform.RPCResponse{}, &platform.RPCStatusError{Code: code, Message: "leaf not stored"}
				}
				return platform.RPCResponse{Message: wire(t, &pb.DeleteAssetResponse{Success: true}), TrailingMetadata: trailer}, nil
			}}
			response, err := s.Invoke(context.Background(), transport, deleteMethod, &pb.DeleteAssetRequest{Id: assetID, ExpectedDeviceId: proto.String(assetID)}, InvokeOptions{Metadata: metadata})
			if err != nil {
				t.Fatal(err)
			}
			if len(methods) != 3 || !reflect.DeepEqual(refs, []bool{false, true, false}) || nonces[0] == nonces[1] || nonces[1] == nonces[2] || jtis[0] == jtis[1] {
				t.Fatalf("retry state %v %v %v %v", methods, refs, nonces, jtis)
			}
			if !reflect.DeepEqual(response.TrailingMetadata, trailer) {
				t.Fatal("trailers lost")
			}
			if _, ok := registeredKIDs.Load(s.kid); ok {
				t.Fatal("refused kid retained")
			}
		})
	}
}

func TestUnimplementedRegistrationAndStatusPreservation(t *testing.T) {
	s := testSigner(t)
	failure := &platform.RPCStatusError{Code: platform.RPCNotFound, Message: "missing", TrailingMetadata: platform.Metadata{{Name: "grpc-status-details-bin", Value: []byte{0, 128}}}}
	calls := 0
	transport := platformtest.Transport{UnaryFunc: func(ctx context.Context, r platform.RPCRequest) (platform.RPCResponse, error) {
		calls++
		if calls == 1 {
			return platform.RPCResponse{}, &platform.RPCStatusError{Code: platform.RPCUnimplemented}
		}
		var signed pb.SignedRequest
		_ = proto.Unmarshal(r.Message, &signed)
		h, _ := claims(t, signed.Signature)
		if h["kid"] != nil {
			t.Fatal("unregistered kid")
		}
		return platform.RPCResponse{TrailingMetadata: failure.TrailingMetadata}, failure
	}}
	result, err := s.Invoke(context.Background(), transport, deleteMethod, &pb.DeleteAssetRequest{Id: assetID}, InvokeOptions{})
	var status *platform.RPCStatusError
	if !errors.As(err, &status) || status != failure || calls != 2 || !reflect.DeepEqual(result.TrailingMetadata, failure.TrailingMetadata) {
		t.Fatalf("status lost %v %v", result, err)
	}
}

func TestInvalidRequestAndCancellationDoNotCallHost(t *testing.T) {
	s := testSigner(t)
	for _, request := range []proto.Message{&pb.DeleteAssetRequest{Id: "bad"}, &pb.GetAssetRequest{Id: assetID}, nil} {
		_, err := s.Invoke(context.Background(), platformtest.Transport{}, deleteMethod, request, InvokeOptions{})
		if err == nil {
			t.Fatal("invalid request accepted")
		}
	}
	for _, value := range []string{"line\nbreak", "space value", strings.Repeat("a", 65), "ünicode"} {
		_, err := s.Invoke(context.Background(), platformtest.Transport{}, deleteMethod, &pb.DeleteAssetRequest{Id: assetID}, InvokeOptions{Metadata: platform.Metadata{{Name: "x-correlation-id", Value: []byte(value)}}})
		if err == nil || !strings.Contains(err.Error(), "invalid supplied Cloud correlation id") {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Invoke(ctx, platformtest.Transport{}, deleteMethod, &pb.DeleteAssetRequest{Id: assetID}, InvokeOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestEnrollmentEncoderRetainsFullChainAndTierBinding(t *testing.T) {
	s := testSigner(t)
	first, err := s.EnrollmentRequest(context.Background(), "board-1", "C", EnrollmentChainP256)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.EnrollmentRequest(context.Background(), "board-1", "C", EnrollmentChainP256)
	if err != nil {
		t.Fatal(err)
	}
	h, c := claims(t, first)
	_, d := claims(t, second)
	if len(h["x5c"].([]any)) != 2 || c["device_class"] != "C" || c["chain_algorithm"] != EnrollmentChainP256 || c["jti"] == d["jti"] {
		t.Fatalf("enrollment %v %v", h, c)
	}
	if _, err := s.EnrollmentRequest(context.Background(), "board", "B", EnrollmentChainP256); err == nil {
		t.Fatal("wrong tier accepted")
	}
}

func TestDPoPMetadataFreshProofAndTokenBinding(t *testing.T) {
	entropy := &testEntropy{}
	key := platformtest.Signer{
		AlgorithmFunc: func() platform.SignatureAlgorithm { return platform.MLDSA65 },
		PublicKeyFunc: func(context.Context) ([]byte, error) {
			return asn1.Marshal(struct {
				Algorithm struct{ OID asn1.ObjectIdentifier }
				PublicKey asn1.BitString
			}{Algorithm: struct{ OID asn1.ObjectIdentifier }{asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 3, 18}}, PublicKey: asn1.BitString{Bytes: make([]byte, 1952), BitLength: 1952 * 8}})
		},
		SignFunc: func(ctx context.Context, data []byte) ([]byte, error) { return append([]byte(nil), data...), ctx.Err() },
	}
	original := platform.Metadata{{Name: "authorization", Value: []byte("Bearer stale")}, {Name: "dpop", Value: []byte("old")}, {Name: "custom", Value: []byte("retained")}}
	var proofs []string
	for i := 0; i < 2; i++ {
		result, err := AuthenticationMetadata(context.Background(), entropy, "cloud.example:443", deleteMethod, "access-token", key, original)
		if err != nil {
			t.Fatal(err)
		}
		if len(result) != 3 || string(result[0].Value) != "retained" || string(result[1].Value) != "DPoP access-token" {
			t.Fatal(result)
		}
		proofs = append(proofs, string(result[2].Value))
		h, c := claims(t, result[2].Value)
		sum := sha256.Sum256([]byte("access-token"))
		if h["typ"] != "dpop+jwt" || c["ath"] != base64.RawURLEncoding.EncodeToString(sum[:]) || c["htu"] != "https://cloud.example:443"+deleteMethod || c["htm"] != "POST" {
			t.Fatalf("proof %v %v", h, c)
		}
	}
	if proofs[0] == proofs[1] || string(original[0].Value) != "Bearer stale" {
		t.Fatal("proof reused or metadata mutated")
	}
}

// Moved from the CLI: forwarded authority must carry x5c, and x5c-only calls
// must skip registration entirely. Use a supported request for the call probe.
func TestInvokeSignsDeviceForwardedMethodsWithX5C(t *testing.T) {
	for _, method := range []string{"/wendycloud.v2.DeploymentService/CreateDeployment", "/wendycloud.v2.DeploymentService/ControlContainer"} {
		if !x5cOnly[method] {
			t.Errorf("%s may be signed by kid", method)
		}
	}
	s := testSigner(t)
	x5cOnly[deleteMethod] = true
	defer delete(x5cOnly, deleteMethod)
	calls := 0
	transport := platformtest.Transport{UnaryFunc: func(ctx context.Context, r platform.RPCRequest) (platform.RPCResponse, error) {
		calls++
		if r.Method != deleteMethod {
			t.Fatal("unexpected registration")
		}
		var signed pb.SignedRequest
		if err := proto.Unmarshal(r.Message, &signed); err != nil {
			t.Fatal(err)
		}
		header, _ := claims(t, signed.Signature)
		if header["kid"] != nil || len(header["x5c"].([]any)) != 1 {
			t.Fatal("forwarded kid reference")
		}
		return platform.RPCResponse{}, nil
	}}
	if _, err := s.Invoke(context.Background(), transport, deleteMethod, &pb.DeleteAssetRequest{Id: assetID}, InvokeOptions{}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("retried x5c-only call")
	}
}

func TestSignerWorkflowMetadataPreservesCorrelationAndOwnership(t *testing.T) {
	base := testSigner(t)
	metadata := platform.Metadata{{Name: "x-correlation-id", Value: []byte("lite-flow")}, {Name: "authorization", Value: []byte("test-session")}}
	signer := base.WithMetadata(metadata)
	metadata[0].Value[0] = 'X'
	metadata[1].Name = "changed"
	for _, tc := range []struct {
		name, want string
		metadata   platform.Metadata
	}{{"default", "lite-flow", nil}, {"override", "explicit", platform.Metadata{{Name: "x-correlation-id", Value: []byte("explicit")}}}} {
		t.Run(tc.name, func(t *testing.T) {
			transport := platformtest.Transport{UnaryFunc: func(ctx context.Context, r platform.RPCRequest) (platform.RPCResponse, error) {
				if r.Method == "/wendycloud.v2.OperatorSessionService/RegisterOperatorLeaf" {
					return platform.RPCResponse{}, &platform.RPCStatusError{Code: platform.RPCUnimplemented}
				}
				signed := new(pb.SignedRequest)
				if err := proto.Unmarshal(r.Message, signed); err != nil {
					t.Fatal(err)
				}
				_, payload := claims(t, signed.Signature)
				if payload["correlation_id"] != tc.want {
					t.Fatalf("correlation=%v", payload["correlation_id"])
				}
				if tc.metadata == nil && (r.Metadata[0].Name != "x-correlation-id" || string(r.Metadata[0].Value) != "lite-flow" || r.Metadata[1].Name != "authorization") {
					t.Fatalf("metadata mutated: %v", r.Metadata)
				}
				return platform.RPCResponse{}, nil
			}}
			_, err := signer.Invoke(context.Background(), transport, deleteMethod, &pb.DeleteAssetRequest{Id: assetID}, InvokeOptions{Metadata: tc.metadata})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	if base.defaultMetadata != nil {
		t.Fatal("original signer mutated")
	}
}
