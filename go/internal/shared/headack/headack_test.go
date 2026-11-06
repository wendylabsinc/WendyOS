package headack

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Vectors: service-protos conformance/reqsig at the commit in testdata/SOURCE.
type vector struct {
	Name           string `json:"name"`
	Device         string `json:"device"`
	OrganizationID string `json:"organization_id"`
	Response       []byte `json:"response_b64"`
	Expect         string `json:"expect"`
	Refusal        string `json:"refusal"`
	EntryID        int64  `json:"expected_entry_id"`
	ObservedAt     int64  `json:"expected_observed_at"`
	LastSeen       *struct {
		EntryID    int64  `json:"entry_id"`
		ObservedAt int64  `json:"observed_at"`
		EntryHash  []byte `json:"entry_hash_b64"`
	} `json:"last_seen"`
}

func load(t *testing.T) (*x509.Certificate, []vector) {
	t.Helper()
	raw, err := os.ReadFile("testdata/device-signed-request-vectors-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Root    string   `json:"root_cert_der_b64"`
		Vectors []vector `json:"head_ack_read_vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	der, err := base64.StdEncoding.DecodeString(file.Root)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Vectors) != 16 {
		t.Fatalf("%d head-ack vectors, want 16", len(file.Vectors))
	}
	return root, file.Vectors
}

func (v vector) run(root *x509.Certificate, kept Ack) (Ack, error) {
	var resp cloudpbv2.GetLastHeadAckResponse
	if err := proto.Unmarshal(v.Response, &resp); err != nil {
		return Ack{}, err
	}
	return verify(&resp, root, v.Device, v.OrganizationID, kept, time.Unix(1790000000, 0))
}

func (v vector) kept() Ack {
	if v.LastSeen == nil {
		return Ack{}
	}
	return Ack{EntryID: v.LastSeen.EntryID, EntryHash: v.LastSeen.EntryHash, ObservedAt: time.Unix(v.LastSeen.ObservedAt, 0)}
}

func TestHeadAckReadVectors(t *testing.T) {
	root, vectors := load(t)
	for _, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			got, err := v.run(root, v.kept())
			if v.Expect == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				if got.EntryID != v.EntryID || got.ObservedAt.Unix() != v.ObservedAt || len(got.EntryHash) == 0 {
					t.Fatalf("verified %+v, want entry %d observed at %d", got, v.EntryID, v.ObservedAt)
				}
				return
			}
			if want := Refusal(v.Refusal); !errors.Is(err, want) {
				t.Fatalf("got %v, want %s", err, want)
			}
		})
	}
}

func TestWrongPinnedRoot(t *testing.T) {
	root, vectors := load(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// Same subject as the real root, another key: no path verifies.
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: root.Subject, NotBefore: root.NotBefore, NotAfter: root.NotAfter,
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	other, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vectors[0].run(other, Ack{}); !errors.Is(err, UnpinnedRoot) {
		t.Fatalf("got %v under another root, want %s", err, UnpinnedRoot)
	}
	if _, err := vectors[0].run(nil, Ack{}); err == nil {
		t.Fatal("verified without a pinned root")
	}
}

func TestKeptState(t *testing.T) {
	root, vectors := load(t)
	valid := vectors[0]
	first, err := valid.run(root, Ack{})
	if err != nil {
		t.Fatal(err)
	}
	// The same ack served again passes against what it left behind.
	again, err := valid.run(root, first)
	if err != nil || again.EntryID != first.EntryID || !bytes.Equal(again.EntryHash, first.EntryHash) || !again.ObservedAt.Equal(first.ObservedAt) {
		t.Fatalf("re-served ack: %+v, %v", again, err)
	}
	for _, c := range []struct {
		name string
		kept Ack
		want Refusal
	}{
		{"higher entry", Ack{EntryID: first.EntryID + 1, ObservedAt: first.ObservedAt}, EntryIDRegressed},
		{"newer observation", Ack{EntryID: first.EntryID, EntryHash: first.EntryHash, ObservedAt: first.ObservedAt.Add(time.Second)}, ObservedAtRegressed},
		{"other hash at the id", Ack{EntryID: first.EntryID, EntryHash: bytes.Repeat([]byte{1}, 32), ObservedAt: first.ObservedAt}, EntryHashEquivocation},
	} {
		if _, err := valid.run(root, c.kept); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %s", c.name, err, c.want)
		}
	}
	// A lower kept ack at another id is an older read: the new one passes.
	if _, err := valid.run(root, Ack{EntryID: first.EntryID - 1, EntryHash: bytes.Repeat([]byte{1}, 32), ObservedAt: first.ObservedAt.Add(-time.Hour)}); err != nil {
		t.Errorf("newer ack after an older kept one: %v", err)
	}
}

func TestDeviceOutsideOrganization(t *testing.T) {
	root, vectors := load(t)
	v := vectors[0]
	v.OrganizationID = "00000000-0000-4000-8000-000000000000"
	if _, err := v.run(root, Ack{}); err == nil || errors.As(err, new(Refusal)) {
		t.Fatalf("device of another organization: %v, want a caller error", err)
	}
}

// resign re-signs the valid vector's ack after edit changes its claims, with
// device A's key from the generator (seed = SHA-256 of its label).
func resign(t *testing.T, v vector, edit func(map[string]any)) *cloudpbv2.GetLastHeadAckResponse {
	t.Helper()
	var resp cloudpbv2.GetLastHeadAckResponse
	if err := proto.Unmarshal(v.Response, &resp); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(resp.GetAck().GetSignature()), ".")
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	claims := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&claims); err != nil {
		t.Fatal(err)
	}
	edit(claims)
	encoded, _ := json.Marshal(claims)
	seed := sha256.Sum256([]byte("wendy-device-request-vector-device-a-2026-10"))
	key, err := mldsa.NewPrivateKey(mldsa.MLDSA65(), seed[:])
	if err != nil {
		t.Fatal(err)
	}
	input := parts[0] + "." + base64.RawURLEncoding.EncodeToString(encoded)
	sig, err := key.Sign(rand.Reader, []byte(input), &mldsa.Options{})
	if err != nil {
		t.Fatal(err)
	}
	resp.Ack.Signature = []byte(input + "." + base64.RawURLEncoding.EncodeToString(sig))
	return &resp
}

func TestChecksNoVectorCovers(t *testing.T) {
	root, vectors := load(t)
	valid := vectors[0]
	at := time.Unix(1790000000, 0)
	check := func(name string, resp *cloudpbv2.GetLastHeadAckResponse, kept Ack, want error) {
		t.Helper()
		_, err := verify(resp, root, valid.Device, valid.OrganizationID, kept, at)
		if (want == nil) != (err == nil) || want != nil && !errors.Is(err, want) {
			t.Errorf("%s: got %v, want %v", name, err, want)
		}
	}
	// The key is device A's: an unchanged re-sign verifies.
	check("re-signed", resign(t, valid, func(map[string]any) {}), Ack{}, nil)
	check("other device", resign(t, valid, func(c map[string]any) {
		c["target"].(map[string]any)["resource"] = valid.Device + "x"
	}), Ack{}, ClaimsMismatch)
	check("other operation", resign(t, valid, func(c map[string]any) {
		c["operation"] = "wendycloud.v2.KeyDirectoryService/RegisterDeviceEncryptionKey"
	}), Ack{}, ClaimsMismatch)
	check("other tenant claimed", resign(t, valid, func(c map[string]any) {
		c["target"].(map[string]any)["tenant"] = "00000000-0000-4000-8000-000000000000"
	}), Ack{}, ClaimsMismatch)
	check("other audience", resign(t, valid, func(c map[string]any) { c["aud"] = "https://example.com" }), Ack{}, ClaimsMismatch)
	check("signed in the future", resign(t, valid, func(c map[string]any) { c["iat"] = at.Add(2 * time.Minute).Unix() }), Ack{}, SignedInFuture)

	tampered := resign(t, valid, func(map[string]any) {})
	parts := strings.Split(string(tampered.Ack.Signature), ".")
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sig[0] ^= 1
	tampered.Ack.Signature = []byte(parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(sig))
	check("bad signature", tampered, Ack{}, BadSignature)

	absent := resign(t, valid, func(map[string]any) {})
	absent.AckedEntryHash = nil
	check("acked hash absent", absent, Ack{}, EntryHashMismatch)

	// chain_der starts at the leaf's issuer, whose key fixes the algorithm.
	misordered := resign(t, valid, func(map[string]any) {})
	misordered.ChainDer = append([][]byte{root.Raw}, misordered.ChainDer...)
	check("chain not starting at the issuer", misordered, Ack{}, UnpinnedRoot)

	var resp cloudpbv2.GetLastHeadAckResponse
	if err := proto.Unmarshal(valid.Response, &resp); err != nil {
		t.Fatal(err)
	}
	if _, err := decodeAck(resp.Ack.Payload); err != nil {
		t.Fatal(err)
	}
	// Field 9, varint 1: unknown to PostboxHeadAck.
	if _, err := decodeAck(append(bytes.Clone(resp.Ack.Payload), 0x48, 0x01)); err == nil {
		t.Error("ack with an unknown field decoded")
	}

	// A re-signed ack with no entry_hash, served with empty cloud hashes.
	unhashed, _ := (proto.MarshalOptions{Deterministic: true}).Marshal(&cloudpbv2.PostboxHeadAck{EntryId: 42, ObservedAt: timestamppb.New(time.Unix(1789999990, 0))})
	sum := sha256.Sum256(unhashed)
	empty := resign(t, valid, func(c map[string]any) { c["body_sha256"] = base64.RawURLEncoding.EncodeToString(sum[:]) })
	empty.Ack.Payload, empty.AckedEntryHash, empty.PostboxHeadEntryId, empty.PostboxHeadEntryHash = unhashed, []byte{}, 42, []byte{}
	check("signed hash empty", empty, Ack{}, EntryHashMissing)
	empty.AckedEntryHash, empty.PostboxHeadEntryHash = nil, nil
	check("signed and cloud hashes absent", empty, Ack{}, EntryHashMissing)

	// A kept ack without its hash cannot show equivocation; it does not refuse.
	first, err := valid.run(root, Ack{})
	if err != nil {
		t.Fatal(err)
	}
	check("kept without hash", resign(t, valid, func(map[string]any) {}), Ack{EntryID: first.EntryID, ObservedAt: first.ObservedAt}, nil)
}
