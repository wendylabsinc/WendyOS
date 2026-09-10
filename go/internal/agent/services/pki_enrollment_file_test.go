package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/pkienroll"
)

func newTestPKIEnrollment(t *testing.T, configPath string, enrolledWithCloud bool) *PKIEnrollment {
	t.Helper()
	prov := &ProvisioningService{
		logger:     zap.NewNop(),
		configPath: configPath,
		enrolled:   enrolledWithCloud,
	}
	if enrolledWithCloud {
		prov.orgID = 2
		prov.assetID = 408
	}
	return NewPKIEnrollment(zap.NewNop(), configPath, prov)
}

func stageFile(t *testing.T, configPath string, staged map[string]string) string {
	t.Helper()
	data, err := json.Marshal(staged)
	if err != nil {
		t.Fatalf("encoding staged file: %v", err)
	}
	path := filepath.Join(configPath, PKIEnrollmentFileName)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing staged file: %v", err)
	}
	return path
}

// jwtWithTenant builds a token whose payload carries a tenant_uuid claim. The
// signature is not checked here or by the agent: verification is the issuer's
// job at certificate-issuance time.
func jwtWithTenant(t *testing.T, tenant string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"type": "asset_enrollment", "org_id": 2, "asset_id": 408, "tenant_uuid": tenant})
	if err != nil {
		t.Fatalf("encoding claims: %v", err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + "." + "sig"
}

func TestApplyStagedFileEnrollsAndDeletes(t *testing.T) {
	dir := t.TempDir()
	p := newTestPKIEnrollment(t, dir, true)
	path := stageFile(t, dir, map[string]string{
		"token":       "tok-abc",
		"tenantUUID":  "022b7284-f7f3-4d86-b844-d105a7c06d9e",
		"csrEndpoint": "https://csr.dev.pki.wendy.sh",
	})

	var got pkienroll.EnrollRequest
	p.enroll = func(_ context.Context, req pkienroll.EnrollRequest) (pkienroll.Result, error) {
		got = req
		return pkienroll.Result{
			LeafPEM:    "-----BEGIN CERTIFICATE-----\nleaf\n-----END CERTIFICATE-----\n",
			ChainPEM:   "-----BEGIN CERTIFICATE-----\nchain\n-----END CERTIFICATE-----\n",
			DeviceName: "sh/wendy/2/408",
			SPIFFEURI:  "spiffe://wendy.sh/tenant/022b7284-f7f3-4d86-b844-d105a7c06d9e/device/sh/wendy/2/408",
		}, nil
	}
	p.ApplyStagedFile(context.Background())

	if got.EnrollmentToken != "tok-abc" {
		t.Errorf("token = %q", got.EnrollmentToken)
	}
	if got.TenantUUID != "022b7284-f7f3-4d86-b844-d105a7c06d9e" {
		t.Errorf("tenant = %q", got.TenantUUID)
	}
	if got.CSRFrontendURL != "https://csr.dev.pki.wendy.sh" {
		t.Errorf("frontend = %q", got.CSRFrontendURL)
	}
	// The Common Name is the same "sh/wendy/<org>/<asset>" the agent builds
	// for its Certificate Authority Service certificate, so the two CSRs state
	// the same subject.
	if got.CommonName != "sh/wendy/2/408" {
		t.Errorf("common name = %q, want sh/wendy/2/408", got.CommonName)
	}
	if len(got.Key) == 0 {
		t.Error("no key was passed; one must be generated on the device")
	}

	if !p.Store().Has() {
		t.Error("the issued identity was not stored")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the staged credential file was not deleted after redemption")
	}
	meta, err := p.Store().LoadMetadata()
	if err != nil {
		t.Fatalf("LoadMetadata: %v", err)
	}
	if meta.TenantUUID == "" || meta.CSREndpoint == "" {
		t.Errorf("metadata = %+v, want the tenant and frontend renewal needs", meta)
	}
	if p.Renewer() == nil {
		t.Error("Renewer = nil after a successful enrolment")
	}
}

func TestApplyStagedFilePrefersTheTokenClaim(t *testing.T) {
	dir := t.TempDir()
	p := newTestPKIEnrollment(t, dir, true)
	claimTenant := "022b7284-f7f3-4d86-b844-d105a7c06d9e"
	stageFile(t, dir, map[string]string{
		// A staged tenant that disagrees with the token must not win: the
		// token is only valid for the tenant it was minted in.
		"token":      jwtWithTenant(t, claimTenant),
		"tenantUUID": "99999999-9999-9999-9999-999999999999",
	})

	var got pkienroll.EnrollRequest
	p.enroll = func(_ context.Context, req pkienroll.EnrollRequest) (pkienroll.Result, error) {
		got = req
		return pkienroll.Result{LeafPEM: "leaf"}, nil
	}
	p.ApplyStagedFile(context.Background())

	if got.TenantUUID != claimTenant {
		t.Errorf("tenant = %q, want the token's tenant_uuid claim %q", got.TenantUUID, claimTenant)
	}
}

func TestApplyStagedFileDerivesTheFrontendFromEnvironment(t *testing.T) {
	dir := t.TempDir()
	p := newTestPKIEnrollment(t, dir, true)
	stageFile(t, dir, map[string]string{
		"token":       "tok",
		"tenantUUID":  "022b7284-f7f3-4d86-b844-d105a7c06d9e",
		"environment": "dev",
	})

	var got pkienroll.EnrollRequest
	p.enroll = func(_ context.Context, req pkienroll.EnrollRequest) (pkienroll.Result, error) {
		got = req
		return pkienroll.Result{LeafPEM: "leaf"}, nil
	}
	p.ApplyStagedFile(context.Background())

	if want := "https://" + pkienroll.DevCSRFrontendHost; got.CSRFrontendURL != want {
		t.Errorf("frontend = %q, want %q", got.CSRFrontendURL, want)
	}
}

func TestApplyStagedFileNoFileIsANoOp(t *testing.T) {
	dir := t.TempDir()
	p := newTestPKIEnrollment(t, dir, true)
	p.enroll = func(context.Context, pkienroll.EnrollRequest) (pkienroll.Result, error) {
		t.Fatal("enrolled with no staged file")
		return pkienroll.Result{}, nil
	}
	p.ApplyStagedFile(context.Background())
	if p.Renewer() != nil {
		t.Error("Renewer is non-nil with no identity")
	}
}

func TestApplyStagedFileKeepsAnExistingIdentity(t *testing.T) {
	dir := t.TempDir()
	p := newTestPKIEnrollment(t, dir, true)
	if _, err := p.Store().LoadOrGenerateKey(); err != nil {
		t.Fatalf("LoadOrGenerateKey: %v", err)
	}
	if err := p.Store().Save(pkienroll.Result{LeafPEM: "existing-leaf"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path := stageFile(t, dir, map[string]string{"token": "tok", "tenantUUID": "t"})

	p.enroll = func(context.Context, pkienroll.EnrollRequest) (pkienroll.Result, error) {
		t.Fatal("re-enrolled over a working identity; renewal keeps it alive instead")
		return pkienroll.Result{}, nil
	}
	p.ApplyStagedFile(context.Background())

	material, err := p.Store().Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if material.LeafPEM != "existing-leaf" {
		t.Errorf("leaf = %q, want the existing identity untouched", material.LeafPEM)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the redundant staged token was not cleared")
	}
}

func TestApplyStagedFileRemovesUnusableFiles(t *testing.T) {
	for name, contents := range map[string]string{
		"malformed json": "{not json",
		"no token":       `{"tenantUUID":"022b7284-f7f3-4d86-b844-d105a7c06d9e"}`,
		"no tenant":      `{"token":"opaque-pki-core-token"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p := newTestPKIEnrollment(t, dir, true)
			path := filepath.Join(dir, PKIEnrollmentFileName)
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatalf("writing staged file: %v", err)
			}
			p.enroll = func(context.Context, pkienroll.EnrollRequest) (pkienroll.Result, error) {
				t.Fatal("enrolled from an unusable staged file")
				return pkienroll.Result{}, nil
			}
			p.ApplyStagedFile(context.Background())
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Error("the unusable file was left in place")
			}
		})
	}
}

func TestApplyStagedFileKeepsTokenWhenNotCloudEnrolled(t *testing.T) {
	// The one deferral: without a Wendy Cloud org and asset there is no Common
	// Name to build, and that is fixable later without a new token. Keeping
	// the file is the difference between a deferred enrolment and a burnt
	// credential.
	dir := t.TempDir()
	p := newTestPKIEnrollment(t, dir, false)
	path := stageFile(t, dir, map[string]string{"token": "tok", "tenantUUID": "022b7284-f7f3-4d86-b844-d105a7c06d9e"})

	p.enroll = func(context.Context, pkienroll.EnrollRequest) (pkienroll.Result, error) {
		t.Fatal("enrolled before the device has an org and asset")
		return pkienroll.Result{}, nil
	}
	p.ApplyStagedFile(context.Background())

	if _, err := os.Stat(path); err != nil {
		t.Errorf("the staged token was removed although it is still usable: %v", err)
	}
}

func TestApplyStagedFileDoesNotRetryARefusedToken(t *testing.T) {
	// A 401 will be refused identically on a retry: the token is single-use
	// and consumed atomically with issuance, so retrying only delays the
	// message that a fresh one is needed.
	dir := t.TempDir()
	p := newTestPKIEnrollment(t, dir, true)
	stageFile(t, dir, map[string]string{"token": "spent", "tenantUUID": "022b7284-f7f3-4d86-b844-d105a7c06d9e"})

	attempts := 0
	p.enroll = func(context.Context, pkienroll.EnrollRequest) (pkienroll.Result, error) {
		attempts++
		return pkienroll.Result{}, &pkienroll.StatusError{StatusCode: 401, Detail: "bearer token required"}
	}
	p.ApplyStagedFile(context.Background())

	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if p.Store().Has() {
		t.Error("something was stored after a refusal")
	}
}

func TestApplyStagedFileDoesNotStoreAWrongIdentity(t *testing.T) {
	dir := t.TempDir()
	p := newTestPKIEnrollment(t, dir, true)
	stageFile(t, dir, map[string]string{"token": "tok", "tenantUUID": "022b7284-f7f3-4d86-b844-d105a7c06d9e"})

	attempts := 0
	p.enroll = func(context.Context, pkienroll.EnrollRequest) (pkienroll.Result, error) {
		attempts++
		return pkienroll.Result{}, &pkienroll.IdentityError{Want: "device", Got: []string{"spiffe://wendy.sh/tenant/x/service/y"}}
	}
	p.ApplyStagedFile(context.Background())

	if attempts != 1 {
		t.Errorf("attempts = %d, want 1: a wrong identity is not a transient failure", attempts)
	}
	if p.Store().Has() {
		t.Error("a leaf with the wrong principal was stored")
	}
}

func TestRenewerNilWithoutMetadata(t *testing.T) {
	p := newTestPKIEnrollment(t, t.TempDir(), true)
	if p.Renewer() != nil {
		t.Error("Renewer is non-nil with no stored metadata")
	}
}
