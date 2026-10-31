package proto

import (
	"bytes"
	"encoding/hex"
	"testing"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
)

// Captured from WendyOS 419db1069ac07ec289afd66ffa2528c97c1e54c9 before
// device enrollment moved to Shared gRPC API contracts. Keep these bytes fixed:
// enrollment signatures bind the request body, including optional presence.
func TestDeviceEnrollmentOldWire(t *testing.T) {
	for _, tt := range []struct {
		name string
		wire string
		want proto.Message
	}{
		{"legacy-slash", "0a0e666c6565742d612f626f782d303110021a186865616465722e7061796c6f61642e7369676e61747572652206626f782d30312a036c6162", &pb.EnrollDeviceRequest{
			DeviceId: "fleet-a/box-01", DeviceClass: pb.DeviceClass_DEVICE_CLASS_B,
			EnrollmentRequestJws: []byte("header.payload.signature"), Name: "box-01", Details: "lab",
		}},
		{"uuid-zero-hardware", "0a2431323334353637382d313233342d343233342d383233342d31323334353637383961626310031a0400ff2e8022086c6974652d6f6e6532080a00300038004000", &pb.EnrollDeviceRequest{
			DeviceId: "12345678-1234-4234-8234-123456789abc", DeviceClass: pb.DeviceClass_DEVICE_CLASS_C,
			EnrollmentRequestJws: []byte{0x00, 0xff, 0x2e, 0x80}, Name: "lite-one",
			Hardware: &pb.DeviceHardware{BoardModel: proto.String(""), CpuCount: proto.Uint32(0), MemTotalBytes: proto.Int64(0), DiskTotalBytes: proto.Int64(0)},
		}},
		{"empty-hardware", "0a0e666c6565742d612f626f782d303110013200", &pb.EnrollDeviceRequest{
			DeviceId: "fleet-a/box-01", DeviceClass: pb.DeviceClass_DEVICE_CLASS_A, Hardware: &pb.DeviceHardware{},
		}},
		{"eab-response", "0a0c6c65676163792d617373657412036561621a056b65792d312206616263646566", &pb.EnrollDeviceResponse{
			AssetId: "legacy-asset", CredentialKind: "eab", EabKeyId: "key-1", EabHmacKey: "abcdef",
		}},
		{"token-response", "0a2431323334353637382d313233342d343233342d383233342d3132333435363738396162631210656e726f6c6c6d656e745f746f6b656e2a07746f6b656e2d3132067365637265743a14323032362d31312d30315431323a30303a30305a", &pb.EnrollDeviceResponse{
			AssetId: "12345678-1234-4234-8234-123456789abc", CredentialKind: "enrollment_token",
			TokenId: "token-1", TokenValue: "secret", ExpiresAt: "2026-11-01T12:00:00Z",
		}},
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
