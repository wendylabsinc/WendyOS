package builddelivery

import (
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

//go:embed testdata/build-delivery-swift.json
var swiftDeliveryVector []byte

//go:embed testdata/build-delivery-swift-mldsa87.json
var swiftDelivery87Vector []byte

func TestNativeSwiftDeliverySignatureConformance(t *testing.T) {
	for name, data := range map[string][]byte{"ML-DSA-65": swiftDeliveryVector, "ML-DSA-87": swiftDelivery87Vector} {
		t.Run(name, func(t *testing.T) {
			var vector struct {
				Root    string `json:"root_pem"`
				Target  string `json:"target"`
				Builder []byte `json:"builder_der"`
				JWS     string `json:"authorization_jws"`
				Session string `json:"session"`
				At      int64  `json:"verify_at"`
			}
			if err := json.Unmarshal(data, &vector); err != nil {
				t.Fatal(err)
			}
			parts := strings.Split(vector.JWS, ".")
			if len(parts) != 3 {
				t.Fatal("invalid vector JWS")
			}
			hb, err := decode(parts[0])
			var h header
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(hb, &h); err != nil || h.Algorithm != name {
				t.Fatalf("wrong vector algorithm: %s (%v)", hb, err)
			}
			roots, err := certs.ParseCertsFromPEM([]byte(vector.Root))
			if err != nil {
				t.Fatal(err)
			}
			target, err := meshid.DeviceFromPrincipal(vector.Target)
			if err != nil {
				t.Fatal(err)
			}
			verified, err := Admit(vector.JWS, roots, target, time.Unix(vector.At, 0))
			if err != nil {
				t.Fatal(err)
			}
			builder, err := x509.ParseCertificate(vector.Builder)
			if err != nil {
				t.Fatal(err)
			}
			if verified.Claims().Reference != "app:latest" || verified.Claims().BuilderSPKI != Fingerprint(builder) || vector.Session != sessionID(vector.JWS) {
				t.Fatal("Swift and Go disagree on the signed assignment")
			}
		})
	}
}
