package services

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// readVectors regenerates a service-protos conformance file with its own
// deterministic generator (vendored from 65830a0a under testdata/postbox; the
// files themselves are too large for the PR review budgets) and checks the
// output is byte for byte the file at that commit.
func readVectors(t *testing.T, gen, file, sha string, v any) {
	t.Helper()
	out := t.TempDir()
	cmd := exec.Command("go", "run", "testdata/postbox/"+gen+"/generate.go", out)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating %s: %v\n%s", file, err, msg)
	}
	raw, err := os.ReadFile(filepath.Join(out, file))
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(raw); hex.EncodeToString(sum[:]) != sha {
		t.Fatalf("%s is not service-protos 65830a0a's (sha256 %x)", file, sum)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

func b64Cert(t *testing.T, s string) *x509.Certificate {
	t.Helper()
	der, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// service-protos conformance/postbox checkpoint-vectors-v1.json: 18 vectors,
// each ending in its expect and refusal code.
func TestPostboxCheckpointVectors(t *testing.T) {
	var doc struct {
		Root    string `json:"root_cert_der_b64"`
		TSACA   string `json:"tsa_intermediate_cert_der_b64"`
		Vectors []struct {
			Name       string `json:"name"`
			Expect     string `json:"expect"`
			Refusal    string `json:"refusal"`
			Tenant     string `json:"expected_tenant"`
			Target     string `json:"expected_target"`
			HWM        int64  `json:"high_water_mark"`
			VerifyAt   int64  `json:"verify_at"`
			Checkpoint string `json:"checkpoint_b64"`
		} `json:"vectors"`
	}
	readVectors(t, "checkpoint", "checkpoint-vectors-v1.json", "5932852a7bd47a24ad1a74e399aca68b49be89ac7f38eeecb1ebaab70dec4c42", &doc)
	roots := x509.NewCertPool()
	roots.AddCert(b64Cert(t, doc.Root))
	tsaCA := b64Cert(t, doc.TSACA)
	if len(doc.Vectors) != 18 {
		t.Fatalf("%d vectors, want 18", len(doc.Vectors))
	}
	tenantWide := 0
	for _, v := range doc.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			raw, _ := base64.StdEncoding.DecodeString(v.Checkpoint)
			cp, err := verifyCheckpoint(raw, roots, v.Tenant, v.Target, v.HWM, time.Unix(v.VerifyAt, 0))
			code := ""
			var r *checkpointRefusal
			if errors.As(err, &r) {
				code = r.code
			} else if err != nil {
				t.Fatalf("unnamed refusal: %v", err)
			}
			if got := map[bool]string{true: "valid", false: "invalid"}[err == nil]; got != v.Expect || code != v.Refusal {
				t.Fatalf("got %s %q (%v), want %s %q", got, code, err, v.Expect, v.Refusal)
			}
			// inter carries the TSA intermediate on to the entry tokens.
			if err == nil && (cp.maxID < v.HWM || len(cp.head) != 32 || cp.genTime.IsZero() || len(cp.inter) != 1 || !cp.inter[0].Equal(tsaCA)) {
				t.Fatalf("verified checkpoint binds %+v", cp)
			}
			// A mark above max_id is a rollback for a device checkpoint only:
			// a tenant-wide one is on another chain (WDY-4015 item 9).
			if err == nil {
				_, err := verifyCheckpoint(raw, roots, v.Tenant, v.Target, cp.maxID+1, time.Unix(v.VerifyAt, 0))
				if cp.tenantWide != (err == nil) {
					t.Fatalf("tenant-wide %t, mark above max_id: %v", cp.tenantWide, err)
				}
				if cp.tenantWide {
					tenantWide++
				}
			}
		})
	}
	if tenantWide == 0 {
		t.Fatal("no valid tenant-wide vector")
	}
}

// postboxEntryVectors is service-protos conformance/postbox-entry
// postbox-entry-vectors-v1.json.
type postboxEntryVectors struct {
	OrganizationID string `json:"organization_id"`
	Device         string `json:"device"`
	OrgCA          string `json:"org_ca_cert_der_b64"`
	OperatorCA     string `json:"operator_ca_cert_der_b64"`
	VerifyAt       int64  `json:"verify_at"`
	PlatformCap    int64  `json:"platform_cap_seconds"`
	OrgCAVectors   []struct {
		Name     string `json:"name"`
		Expect   string `json:"expect"`
		Refusal  string `json:"refusal"`
		Request  string `json:"request_b64"`
		Response string `json:"response_b64"`
		Status   string `json:"status"`
		CachedCN string `json:"expected_cached_subject_cn"`
	} `json:"org_ca_vectors"`
	ValidityVectors []struct {
		Name          string `json:"name"`
		Expect        string `json:"expect"`
		Refusal       string `json:"refusal"`
		SignedRequest string `json:"signed_request_b64"`
		Stage         string `json:"stage"`
		TrustedNow    int64  `json:"trusted_now"`
		TenantCap     int64  `json:"tenant_cap_seconds"`
	} `json:"validity_vectors"`
}

// orgCAClient serves one GetOrgCAs answer, as cloud would for the vector.
type orgCAClient struct {
	cloudpbv2.DevicePostboxServiceClient
	resp *cloudpbv2.GetOrgCAsResponse
	err  error
	asks []*cloudpbv2.GetOrgCAsRequest
}

func (c *orgCAClient) GetOrgCAs(_ context.Context, in *cloudpbv2.GetOrgCAsRequest, _ ...grpc.CallOption) (*cloudpbv2.GetOrgCAsResponse, error) {
	c.asks = append(c.asks, in)
	return c.resp, c.err
}

// The 12 vectors of conformance/postbox-entry: 4 GetOrgCAs answers through
// the device's resolveIssuer, 8 validity cases through its signature, chain
// and validity checks.
func TestPostboxEntryVectors(t *testing.T) {
	var doc postboxEntryVectors
	readVectors(t, "entry", "postbox-entry/postbox-entry-vectors-v1.json", "582c7013f35f84c302af586aad5ae9a184715d3c40be36ffc36682b50128e3fa", &doc)
	orgCA := b64Cert(t, doc.OrgCA)
	if len(doc.OrgCAVectors)+len(doc.ValidityVectors) != 12 || doc.PlatformCap != int64(postboxPlatformCap/time.Second) {
		t.Fatalf("%d+%d vectors, platform cap %d", len(doc.OrgCAVectors), len(doc.ValidityVectors), doc.PlatformCap)
	}
	deviceUUID := doc.Device[strings.LastIndex(doc.Device, "/")+1:]
	ident := func() *postboxIdentity {
		return &postboxIdentity{tenant: doc.OrganizationID, uuid: deviceUUID, uri: doc.Device, anchors: []*x509.Certificate{orgCA}}
	}

	// The operator leaf the vectors sign with, the one GetOrgCAs is asked about.
	raw, _ := base64.StdEncoding.DecodeString(doc.ValidityVectors[0].SignedRequest)
	var first cloudpbv2.SignedRequest
	if err := proto.Unmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	leaf := peekLeaf(first.GetSignature())
	if leaf == nil {
		t.Fatal("no operator leaf in the vectors")
	}

	// served is the operator-ca-valid answer: every validity vector resolves
	// its leaf-only x5c through it.
	var served *cloudpbv2.GetOrgCAsResponse
	for _, v := range doc.OrgCAVectors {
		t.Run("org-ca/"+v.Name, func(t *testing.T) {
			var want cloudpbv2.GetOrgCAsRequest
			reqRaw, _ := base64.StdEncoding.DecodeString(v.Request)
			if err := proto.Unmarshal(reqRaw, &want); err != nil {
				t.Fatal(err)
			}
			client := &orgCAClient{err: status.Error(codes.NotFound, v.Status)}
			if v.Status == "" {
				client.resp, client.err = &cloudpbv2.GetOrgCAsResponse{}, nil
				respRaw, _ := base64.StdEncoding.DecodeString(v.Response)
				if err := proto.Unmarshal(respRaw, client.resp); err != nil {
					t.Fatal(err)
				}
			}
			p := newVectorPostbox(t)
			id := ident()
			asked := *leaf // the not-found vector asks about an issuer no CA has
			asked.RawIssuer = want.GetIssuer()
			err := p.fetchIssuer(context.Background(), client, id, &asked, time.Unix(doc.VerifyAt, 0))
			got, code := "valid", ""
			if err != nil {
				got = "invalid"
				if v.Status == "NOT_FOUND" {
					got = "hold"
				}
				for reason := range p.refusals {
					code = reason
				}
			}
			if got != v.Expect || code != v.Refusal {
				t.Fatalf("got %s %q (%v), want %s %q", got, code, err, v.Expect, v.Refusal)
			}
			if len(client.asks) != 1 || string(client.asks[0].GetIssuer()) != string(want.GetIssuer()) {
				t.Fatalf("asked %v, want the leaf's issuer Name", client.asks)
			}
			cached := p.snapshot().OrgCAs
			if v.Expect == "valid" {
				if len(id.orgCAs) != 1 || id.orgCAs[0].Subject.CommonName != v.CachedCN || len(cached) != 1 {
					t.Fatalf("cached %v / persisted %d, want %q", id.orgCAs, len(cached), v.CachedCN)
				}
				served = client.resp
			} else if len(id.orgCAs)+len(cached) != 0 {
				t.Fatalf("refused CA cached")
			}
		})
	}
	if served == nil {
		t.Fatal("no valid GetOrgCAs answer to resolve the validity vectors with")
	}

	for _, v := range doc.ValidityVectors {
		t.Run("validity/"+v.Name, func(t *testing.T) {
			raw, _ := base64.StdEncoding.DecodeString(v.SignedRequest)
			var sr cloudpbv2.SignedRequest
			if err := proto.Unmarshal(raw, &sr); err != nil {
				t.Fatal(err)
			}
			e := &cloudpbv2.PostboxEntry{RequestJws: []byte(sr.GetSignature()), RequestPayload: sr.GetPayload(), PayloadType: sr.GetPayloadType()}
			id := ident()
			p := newVectorPostbox(t)
			if err := p.resolveIssuer(context.Background(), &orgCAClient{resp: served}, id, e, time.Unix(doc.VerifyAt, 0)); err != nil {
				t.Fatalf("leaf issuer not resolved through GetOrgCAs: %v", err)
			}
			// The vectors' target.resource is the device SPIFFE URI, not
			// cloud's ControlContainer form asset/<id>/container/<name>
			// (bindsThisDevice, covered by the fixture tests), so binding is
			// not part of this run.
			// The vector entries carry no tsr_response (a service-protos
			// gap): the signing time here is the vectors' verify time, and
			// the cap counts from iat, which the generator stamps as genTime.
			desc, outcome, _, err := verifyPostboxSignature(e, id, time.Unix(doc.VerifyAt, 0))
			if err != nil {
				t.Fatalf("signature/chain = %v: %v", outcome, err)
			}
			req, err := decodePostboxPayload(e)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Unix(desc.Iat+10, 0)
			if v.Stage == "device" {
				now = time.Unix(v.TrustedNow, 0)
			}
			code, err := postboxValidity(req, time.Unix(desc.Iat, 0), now)
			if err == nil && v.Stage == "accept" {
				// The tenant cap is cloud's accept check, on top of the
				// platform cap the device shares.
				if postboxValidUntil(req).AsTime().Unix()-desc.Iat > v.TenantCap {
					code, err = "above_tenant_cap", errors.New("above the tenant cap")
				}
			}
			if got := map[bool]string{true: "valid", false: "invalid"}[err == nil]; got != v.Expect || code != v.Refusal {
				t.Fatalf("got %s %q (%v), want %s %q", got, code, err, v.Expect, v.Refusal)
			}
		})
	}
}

func newVectorPostbox(t *testing.T) *Postbox {
	p := NewPostbox(zap.NewNop(), nil, t.TempDir(), nil)
	p.state = &postboxState{}
	return p
}
