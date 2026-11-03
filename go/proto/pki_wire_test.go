package proto

import (
	"bytes"
	"encoding/hex"
	"testing"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/pkicorepb"
	"google.golang.org/protobuf/proto"
)

// Encoded with protoc 34.0 from the local PKI sources at
// 1b3141a471855d24742d1674609116929db199f1, before consolidation.
// Fixed bytes catch field reuse and ensure absent additions keep legacy defaults.
func TestPKIOldWire(t *testing.T) {
	for _, tt := range []struct {
		name string
		wire string
		want proto.Message
	}{
		{"auth", "08061a066465766963652a05746f6b656e", &pb.AuthContext{Mode: pb.AuthMode_ACME, DeviceId: "device", Token: "token"}},
		{"revocation-count", "0803", &pb.RevokeByPrincipalResponse{RevokedCount: 3}},
		{"revocation-request", "0a0674656e616e7412097072696e636970616c180122067369676e65642a06726967687473", &pb.RevokeByPrincipalRequest{TenantId: "tenant", Principal: "principal", Reason: 1, SignedRequest: []byte("signed"), SignedRights: []byte("rights")}},
		{"enrollment-request", "0a0674656e616e74120263611a06646576696365220d6465766963652d746965722d63283c32036a74693a056c6162656c", &pb.CreateEnrollmentTokenRequest{TenantId: "tenant", CaId: "ca", DeviceId: "device", Profile: "device-tier-c", TtlSeconds: 60, SourceJti: "jti", Label: "label"}},
		{"issuance", "0a046c6561661205636861696e1a046f637370", &pb.IssueCertificateResponse{Certificate: []byte("leaf"), Chain: []byte("chain"), OcspStaple: []byte("ocsp")}},
		{"token-list", "0a290a05746f6b656e12066465766963651a0d6465766963652d746965722d6328014a077265766f6b656412046e657874", &pb.ListEnrollmentTokensResponse{Tokens: []*pb.EnrollmentTokenInfo{{TokenId: "token", DeviceId: "device", Profile: "device-tier-c", Consumed: true, RevokedAt: "revoked"}}, NextPageToken: "next"}},
		{"eab-list", "0a170a036b65791a066465766963652801320661637469766512046e657874", &pb.ListEABKeysResponse{Keys: []*pb.EABKeyInfo{{KeyId: "key", DeviceId: "device", AttestationRequired: true, Status: "active"}}, NextPageToken: "next"}},
		{"eab-credential", "0a0365616212036b65791a0430313233", &pb.RelayEnrollmentResponse{CredentialKind: "eab", EabKeyId: "key", EabHmacKey: "0123"}},
		{"token-credential", "0a10656e726f6c6c6d656e745f746f6b656e2205746f6b656e2a067365637265743206657870697279", &pb.RelayEnrollmentResponse{CredentialKind: "enrollment_token", TokenId: "token", TokenValue: "secret", ExpiresAt: "expiry"}},
		{"default-ca-chain", "", &pb.GetCACertsRequest{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wire, err := hex.DecodeString(tt.wire)
			if err != nil {
				t.Fatal(err)
			}
			got := tt.want.ProtoReflect().Type().New().Interface()
			if err := proto.Unmarshal(wire, got); err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(got, tt.want) {
				t.Fatalf("decoded old wire = %v, want %v", got, tt.want)
			}
			encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(tt.want)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(encoded, wire) {
				t.Fatalf("current wire = %x, want old wire %x", encoded, wire)
			}
		})
	}
}
