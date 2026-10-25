package enrollment

import (
	"context"
	"errors"
	"fmt"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/core/cloud"
	pb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/enrollment/internal/cloudpb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform/platformtest"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
	"time"
)

var testSession = Session{Principal: "spiffe://wendy.sh/tenant/" + testTenant + "/operator/op", PKIEndpoint: "https://identity.test"}

type entropy struct{ n byte }

func (e *entropy) RandomBytes(ctx context.Context, n int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw := make([]byte, n)
	e.n++
	for i := range raw {
		raw[i] = e.n
	}
	return raw, nil
}
func testClient(t *testing.T, transport platform.Transport) *Client {
	t.Helper()
	rng := new(entropy)
	signer, err := cloud.NewSigner(context.Background(), platformtest.Signer{
		AlgorithmFunc:        func() platform.SignatureAlgorithm { return platform.MLDSA65 },
		CertificateChainFunc: func(context.Context) ([][]byte, error) { return [][]byte{[]byte("test leaf")}, nil },
		SignFunc:             func(context.Context, []byte) ([]byte, error) { return []byte("host signature"), nil },
	}, testTenant, rng, cloud.SignerOptions{Now: func() time.Time { return time.Unix(1700000000, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	return &Client{Cloud: transport, Signer: signer, Entropy: rng}
}
func TestMintCollisionOnlyRetriesExactUUIDFields(t *testing.T) {
	for _, field := range []string{"device_id", "mesh_suffix", "name", ""} {
		t.Run(field, func(t *testing.T) {
			var ids []string
			transport := platformtest.Transport{UnaryFunc: func(ctx context.Context, r platform.RPCRequest) (platform.RPCResponse, error) {
				if strings.HasSuffix(r.Method, "/RegisterOperatorLeaf") {
					return platform.RPCResponse{}, &platform.RPCStatusError{Code: platform.RPCUnimplemented}
				}
				var signed pb.SignedRequest
				if err := proto.Unmarshal(r.Message, &signed); err != nil {
					t.Fatal(err)
				}
				var request pb.EnrollDeviceRequest
				if err := proto.Unmarshal(signed.Payload, &request); err != nil {
					t.Fatal(err)
				}
				if len(request.EnrollmentRequestJws) == 0 || request.DeviceClass != pb.DeviceClass_DEVICE_CLASS_B {
					t.Fatal("missing enrollment authority")
				}
				ids = append(ids, request.DeviceId)
				if len(ids) == 1 {
					return platform.RPCResponse{TrailingMetadata: platform.Metadata{{Name: cloud.CollidingFieldMetadataKey, Value: []byte(field)}}}, &platform.RPCStatusError{Code: platform.RPCAlreadyExists}
				}
				wire, _ := proto.Marshal(&pb.EnrollDeviceResponse{CredentialKind: "eab", EabKeyId: "key", EabHmacKey: "abcd", AssetId: request.DeviceId})
				return platform.RPCResponse{Message: wire}, nil
			}}
			cfg, err := EnrollmentConfig(testSession, "device-1", "")
			if err != nil {
				t.Fatal(err)
			}
			out, asset, err := testClient(t, transport).MintEAB(context.Background(), testSession, cfg, "display name", nil)
			retry := field == "device_id" || field == "mesh_suffix"
			if retry {
				if err != nil || len(ids) != 2 || ids[0] == ids[1] || asset != out.DeviceID {
					t.Fatalf("ids=%v out=%+v asset=%s err=%v", ids, out, asset, err)
				}
			} else if err == nil || len(ids) != 1 {
				t.Fatalf("name collision retried: %v %v", ids, err)
			}
		})
	}
}
func TestUUIDRetryBoundAndCancellation(t *testing.T) {
	calls := 0
	err := RetryTakenDeviceUUID(context.Background(), new(entropy), "first", func(string) error { calls++; return fmt.Errorf("taken: %w", ErrDeviceUUIDTaken) })
	if !errors.Is(err, ErrDeviceUUIDTaken) || calls != 4 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = RetryTakenDeviceUUID(ctx, new(entropy), "first", func(string) error { t.Fatal("called after cancellation"); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestEnrollmentConfigRefusesCrossTenantAndUntrustedEndpoint(t *testing.T) {
	for _, directory := range []string{"https://acme.test/7aa28a2c-3747-4dd5-aa38-c863128c1f9d/acme/directory", "http://acme.test/" + testTenant + "/acme/directory"} {
		if _, err := EnrollmentConfig(testSession, "device-1", directory); err == nil {
			t.Fatalf("accepted %s", directory)
		}
	}
}
