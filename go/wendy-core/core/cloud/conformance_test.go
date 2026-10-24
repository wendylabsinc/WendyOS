package cloud

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	pb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/cloud/internal/cloudpb"
	"google.golang.org/protobuf/proto"
)

// Keep the CLI's existing service-protos signed-request producer vectors with
// the moved canonical encoder. Signature verification remains a host capability.
func TestSignedRequestConformanceVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/signed-request-vectors-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Vectors []struct {
			Name             string          `json:"name"`
			Expect           string          `json:"expect"`
			SignedRequestB64 string          `json:"signed_request_b64"`
			PayloadB64       string          `json:"payload_b64"`
			Claims           json.RawMessage `json:"claims"`
			ClaimsJCS        string          `json:"claims_jcs"`
			JWSCompact       string          `json:"jws_compact"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	valid := 0
	for _, v := range file.Vectors {
		if v.Expect != "valid" {
			continue
		}
		valid++
		t.Run(v.Name, func(t *testing.T) {
			encoded, _ := base64.StdEncoding.DecodeString(v.SignedRequestB64)
			var signed pb.SignedRequest
			if err := proto.Unmarshal(encoded, &signed); err != nil {
				t.Fatal(err)
			}
			payload, _ := base64.StdEncoding.DecodeString(v.PayloadB64)
			var request pb.CreateAssetRequest
			if err := proto.Unmarshal(payload, &request); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(wire(t, &request), payload) || !bytes.Equal(signed.Payload, payload) || signed.PayloadType != string(request.ProtoReflect().Descriptor().FullName()) {
				t.Fatal("wire payload changed")
			}
			decoder := json.NewDecoder(bytes.NewReader(v.Claims))
			decoder.UseNumber()
			var descriptor map[string]any
			if err := decoder.Decode(&descriptor); err != nil {
				t.Fatal(err)
			}
			canonical, err := canonicalJSON(descriptor)
			if err != nil || string(canonical) != v.ClaimsJCS {
				t.Fatalf("canonical descriptor %q: %v", canonical, err)
			}
			hash := sha256.Sum256(payload)
			if descriptor["body_sha256"] != base64.RawURLEncoding.EncodeToString(hash[:]) {
				t.Fatal("wrong body hash")
			}
			parts := strings.Split(v.JWSCompact, ".")
			if string(signed.Signature) != v.JWSCompact || len(parts) != 3 || parts[1] != base64.RawURLEncoding.EncodeToString(canonical) {
				t.Fatal("JWS envelope changed")
			}
		})
	}
	if valid == 0 {
		t.Fatal("no valid vectors")
	}
}
