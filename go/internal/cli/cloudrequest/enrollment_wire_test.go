package cloudrequest

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
)

func TestSignEnrollmentPreservesOldWire(t *testing.T) {
	auth, _, _ := testAuth(t)
	signer, err := newSigner(auth)
	if err != nil {
		t.Fatal(err)
	}
	// Captured before consolidation at WendyOS 419db1069ac07ec289afd66ffa2528c97c1e54c9.
	// Historical slash IDs stay signable. Current UUIDs retain explicit zero hardware.
	for _, wireHex := range []string{
		"0a0e666c6565742d612f626f782d303110021a186865616465722e7061796c6f61642e7369676e61747572652206626f782d30312a036c6162",
		"0a2431323334353637382d313233342d343233342d383233342d31323334353637383961626310031a0400ff2e8022086c6974652d6f6e6532080a00300038004000",
	} {
		wire, err := hex.DecodeString(wireHex)
		if err != nil {
			t.Fatal(err)
		}
		var req pb.EnrollDeviceRequest
		if err := proto.Unmarshal(wire, &req); err != nil {
			t.Fatal(err)
		}
		signed, err := signer.signRequest(pb.DeviceEnrollmentService_EnrollDevice_FullMethodName, &req, false, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(signed.GetPayload(), wire) || signed.GetPayloadType() != "wendycloud.v2.EnrollDeviceRequest" {
			t.Fatalf("signed payload changed for %q", req.GetDeviceId())
		}
		claims, err := base64.RawURLEncoding.DecodeString(strings.Split(string(signed.GetSignature()), ".")[1])
		if err != nil {
			t.Fatal(err)
		}
		var descriptor struct {
			BodyDigest string                    `json:"body_sha256"`
			Target     struct{ Resource string } `json:"target"`
		}
		if err := json.Unmarshal(claims, &descriptor); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(wire)
		if descriptor.BodyDigest != base64.RawURLEncoding.EncodeToString(sum[:]) || descriptor.Target.Resource != "org/"+testTenant+"/device/"+req.GetDeviceId() {
			t.Fatalf("signed descriptor no longer binds the old body and target for %q", req.GetDeviceId())
		}
	}
}
