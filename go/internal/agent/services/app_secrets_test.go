package services

import (
	"bytes"
	"context"
	"crypto/hpke"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
)

type secretFixture struct {
	Seed    string `json:"device_kem_ikm_hex"`
	Vectors []struct {
		Name       string                         `json:"name"`
		Assignment string                         `json:"assignment_b64"`
		Expected   []struct{ Name, Value string } `json:"expected_secrets"`
	} `json:"vectors"`
}

func secretFixtures(t *testing.T) (secretFixture, hpke.PrivateKey) {
	t.Helper()
	b, err := os.ReadFile("testdata/app-secrets-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var f secretFixture
	if err = json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	seed, err := hex.DecodeString(f.Seed)
	if err != nil {
		t.Fatal(err)
	}
	key, err := hpke.MLKEM768X25519().DeriveKeyPair(seed)
	if err != nil {
		t.Fatal(err)
	}
	return f, key
}
func fixtureAssignment(t *testing.T, b64 string) *cloudpbv2.DeviceAssignment {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	a := &cloudpbv2.DeviceAssignment{}
	if err = proto.Unmarshal(b, a); err != nil {
		t.Fatal(err)
	}
	return a
}
func TestAppSecretConformance(t *testing.T) {
	f, key := secretFixtures(t)
	for _, v := range f.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			a := fixtureAssignment(t, v.Assignment)
			values, err := openAppSecrets(a, key)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range v.Expected {
				if !bytes.Equal(values[want.Name], []byte(want.Value)) {
					t.Fatal("secret bytes changed")
				}
			}
			ClearAppSecrets(values)
			for _, value := range values {
				for _, b := range value {
					if b != 0 {
						t.Fatal("secret buffer not cleared")
					}
				}
			}
		})
	}
}
func TestAppSecretsRejectInvalidAssignments(t *testing.T) {
	f, key := secretFixtures(t)
	for name, mutate := range map[string]func(*cloudpbv2.DeviceAssignment){
		"other tenant":     func(a *cloudpbv2.DeviceAssignment) { a.OrganizationId = "other" },
		"other app":        func(a *cloudpbv2.DeviceAssignment) { a.Secrets[0].Value.AppId = "other" },
		"path traversal":   func(a *cloudpbv2.DeviceAssignment) { a.Secrets[0].Value.Name = "../key" },
		"reserved name":    func(a *cloudpbv2.DeviceAssignment) { a.Secrets[0].Value.Name = "WENDY_TOKEN" },
		"duplicate":        func(a *cloudpbv2.DeviceAssignment) { a.Secrets = append(a.Secrets, a.Secrets[0]) },
		"config collision": func(a *cloudpbv2.DeviceAssignment) { a.ConfigJson = `{"DB_PASSWORD":"plain"}` },
		"tampered":         func(a *cloudpbv2.DeviceAssignment) { a.Secrets[0].Value.Ciphertext[0] ^= 1 },
		"nonce size":       func(a *cloudpbv2.DeviceAssignment) { a.Secrets[0].Value.Nonce = nil },
		"value size":       func(a *cloudpbv2.DeviceAssignment) { a.Secrets[0].Value.Ciphertext = make([]byte, (16<<10)+17) },
		"zero version":     func(a *cloudpbv2.DeviceAssignment) { a.Secrets[0].Value.Version = 0 },
		"wrong scope": func(a *cloudpbv2.DeviceAssignment) {
			a.Secrets[0].DeviceWrap.Scope.Scope = &cloudpbv2.WrapScope_DeploymentId{DeploymentId: a.DeploymentId}
		},
		"wrong key":       func(a *cloudpbv2.DeviceAssignment) { a.Secrets[0].Value.SecretKeyId = "wrong" },
		"wrong recipient": func(a *cloudpbv2.DeviceAssignment) { a.Secrets[0].DeviceWrap.Recipient = "other" },
		"custodian": func(a *cloudpbv2.DeviceAssignment) {
			a.Secrets[0].DeviceWrap.RecipientKind = cloudpbv2.RecipientKind_RECIPIENT_KIND_CUSTODIAN
		},
		"unsupported suite": func(a *cloudpbv2.DeviceAssignment) { a.Secrets[0].DeviceWrap.Suite = 0 },
		"too many": func(a *cloudpbv2.DeviceAssignment) {
			for len(a.Secrets) < 65 {
				a.Secrets = append(a.Secrets, a.Secrets[0])
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := fixtureAssignment(t, f.Vectors[0].Assignment)
			mutate(a)
			values, err := openAppSecrets(a, key)
			if err == nil {
				t.Fatal("accepted invalid secret")
			}
			if strings.Contains(err.Error(), "correct horse") {
				t.Fatal("error leaks plaintext")
			}
			for _, v := range values {
				if !bytes.Equal(v, make([]byte, len(v))) {
					t.Fatal("failure retained plaintext")
				}
			}
		})
	}
}
func TestSecretRevisionAndDetach(t *testing.T) {
	f, _ := secretFixtures(t)
	a := fixtureAssignment(t, f.Vectors[0].Assignment)
	b := proto.Clone(a).(*cloudpbv2.DeviceAssignment)
	b.Secrets[0].DeviceWrap.Ciphertext = []byte("new wrap")
	if !sameContent(a, b) {
		t.Fatal("rewrap changed revision content")
	}
	b.Secrets[0].Value.Version++
	if sameContent(a, b) {
		t.Fatal("equal revision changed secret value")
	}
	raw, _ := proto.Marshal(a)
	detached := detachedStatement(&postboxAssignment{Assignment: raw, Applied: true}, false)
	got := &cloudpbv2.DeviceAssignment{}
	if err := proto.Unmarshal(detached, got); err != nil {
		t.Fatal(err)
	}
	if SecretContentID(got) != SecretContentID(a) || got.Device != a.Device || got.OrganizationId != a.OrganizationId {
		t.Fatal("detach lost encrypted restart material")
	}
	ctx, err := secretStartContext(context.Background(), ReleaseStart{AppID: a.AppId, Secrets: a})
	if err != nil || ReleaseSecretsFromContext(ctx) != a {
		t.Fatal("create lost encrypted secrets")
	}
}

type secretCapabilityControl struct {
	postboxControl
	enabled bool
}

func (c secretCapabilityControl) SupportsAppSecrets() bool { return c.enabled }
func TestAppSecretsCapabilityRequiresRuntime(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		p := &Postbox{control: secretCapabilityControl{enabled: enabled}}
		got := p.acceptedSecretFields()
		if enabled && (len(got) != 1 || got[0] != "wendycloud.v2.DeviceAssignment.secrets") {
			t.Fatal("missing secret capability")
		}
		if !enabled && len(got) != 0 {
			t.Fatal("advertised unsupported secrets")
		}
	}
}
