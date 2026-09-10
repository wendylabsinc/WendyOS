package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/pkienroll"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

const (
	testTenant    = "022b7284-f7f3-4d86-b844-d105a7c06d9e"
	testSPIFFEURI = "spiffe://wendy.sh/tenant/022b7284-f7f3-4d86-b844-d105a7c06d9e/device/sh/wendy/2/408"
)

// newStagePKIService wires the handler over a PKIEnrollment rooted at dir.
// enrolledWithCloud decides whether a certificate Common Name can be built,
// which is the difference between an enrolment and a deferral.
func newStagePKIService(t *testing.T, dir string, enrolledWithCloud bool) (*ProvisioningServiceV2, *PKIEnrollment) {
	t.Helper()
	pki := newTestPKIEnrollment(t, dir, enrolledWithCloud)
	svc := NewProvisioningServiceV2(pki.provisioningSvc).WithPKIEnrollment(context.Background(), pki)
	return svc, pki
}

func TestStagePKIEnrollmentEnrolls(t *testing.T) {
	dir := t.TempDir()
	svc, pki := newStagePKIService(t, dir, true)

	notAfter := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Second)
	var got pkienroll.EnrollRequest
	pki.enroll = func(_ context.Context, req pkienroll.EnrollRequest) (pkienroll.Result, error) {
		got = req
		return pkienroll.Result{
			LeafPEM:    "-----BEGIN CERTIFICATE-----\nleaf\n-----END CERTIFICATE-----\n",
			DeviceName: "sh/wendy/2/408",
			SPIFFEURI:  testSPIFFEURI,
			NotAfter:   notAfter,
		}, nil
	}

	resp, err := svc.StagePKIEnrollment(context.Background(), &agentpbv2.StagePKIEnrollmentRequest{
		TenantUuid:  testTenant,
		Token:       "tok-abc",
		DeviceId:    "sh/wendy/2/408",
		Environment: "dev",
	})
	if err != nil {
		t.Fatalf("StagePKIEnrollment: %v", err)
	}

	if resp.GetStatus() != agentpbv2.StagePKIEnrollmentResponse_STATUS_ENROLLED {
		t.Errorf("status = %v, want ENROLLED (%s)", resp.GetStatus(), resp.GetReason())
	}
	if resp.GetSpiffeUri() != testSPIFFEURI {
		t.Errorf("spiffe_uri = %q", resp.GetSpiffeUri())
	}
	if resp.GetDeviceName() != "sh/wendy/2/408" {
		t.Errorf("device_name = %q", resp.GetDeviceName())
	}
	if resp.GetNotAfterUnix() != notAfter.Unix() {
		t.Errorf("not_after_unix = %d, want %d", resp.GetNotAfterUnix(), notAfter.Unix())
	}
	if want := filepath.Join(dir, PKIEnrollmentFileName); resp.GetStagedPath() != want {
		t.Errorf("staged_path = %q, want %q", resp.GetStagedPath(), want)
	}

	// The response must never carry the credential back out: it is a
	// single-use bearer token, and a response is logged in far more places
	// than a request body is.
	if strings.Contains(resp.String(), "tok-abc") {
		t.Errorf("the response echoes the token: %v", resp)
	}

	// The file the RPC wrote is the file the agent reads, and it is gone once
	// redeemed - the same lifecycle as the installer path.
	if _, statErr := os.Stat(resp.GetStagedPath()); !os.IsNotExist(statErr) {
		t.Errorf("the staged file survived redemption: %v", statErr)
	}
	// What was staged is what reached pki-core.
	if got.EnrollmentToken != "tok-abc" {
		t.Errorf("token reaching pki-core = %q", got.EnrollmentToken)
	}
	if got.TenantUUID != testTenant {
		t.Errorf("tenant reaching pki-core = %q", got.TenantUUID)
	}
	if got.DeviceID != "sh/wendy/2/408" {
		t.Errorf("device_id reaching pki-core = %q", got.DeviceID)
	}
	if got.CommonName != "sh/wendy/2/408" {
		t.Errorf("common name = %q, want the CAS subject", got.CommonName)
	}
	if got.CSRFrontendURL != "https://"+pkienroll.DevCSRFrontendHost {
		t.Errorf("frontend = %q, want the dev host for --environment dev", got.CSRFrontendURL)
	}
	// The identity is stored where the data platform dialer looks for it.
	if !pki.Store().Has() {
		t.Error("no identity was stored")
	}
}

func TestStagePKIEnrollmentDefersWhenNotCloudEnrolled(t *testing.T) {
	dir := t.TempDir()
	svc, pki := newStagePKIService(t, dir, false)
	pki.enroll = func(context.Context, pkienroll.EnrollRequest) (pkienroll.Result, error) {
		t.Error("pki-core was contacted although no Common Name can be built")
		return pkienroll.Result{}, errors.New("must not be called")
	}

	resp, err := svc.StagePKIEnrollment(context.Background(), &agentpbv2.StagePKIEnrollmentRequest{
		TenantUuid: testTenant,
		Token:      "tok-abc",
	})
	if err != nil {
		t.Fatalf("StagePKIEnrollment: %v", err)
	}
	if resp.GetStatus() != agentpbv2.StagePKIEnrollmentResponse_STATUS_DEFERRED {
		t.Fatalf("status = %v, want DEFERRED", resp.GetStatus())
	}
	if resp.GetReason() == "" {
		t.Error("a deferral must say why")
	}
	// The token is NOT spent, so the file has to survive for the next start.
	if _, statErr := os.Stat(resp.GetStagedPath()); statErr != nil {
		t.Errorf("the staged token was discarded on a deferral: %v", statErr)
	}
}

func TestStagePKIEnrollmentReportsARefusal(t *testing.T) {
	dir := t.TempDir()
	svc, pki := newStagePKIService(t, dir, true)

	calls := 0
	pki.enroll = func(context.Context, pkienroll.EnrollRequest) (pkienroll.Result, error) {
		calls++
		return pkienroll.Result{}, &pkienroll.StatusError{StatusCode: 401, Detail: "token already redeemed"}
	}

	resp, err := svc.StagePKIEnrollment(context.Background(), &agentpbv2.StagePKIEnrollmentRequest{
		TenantUuid: testTenant,
		Token:      "tok-spent",
	})
	if err != nil {
		t.Fatalf("StagePKIEnrollment: %v", err)
	}
	if resp.GetStatus() != agentpbv2.StagePKIEnrollmentResponse_STATUS_REFUSED {
		t.Fatalf("status = %v, want REFUSED", resp.GetStatus())
	}
	if resp.GetReason() == "" {
		t.Error("a refusal must say why")
	}
	// A single-use credential refused once is refused identically on a retry.
	if calls != 1 {
		t.Errorf("attempts = %d, want 1: a 401 must not be retried", calls)
	}
	if pki.Store().Has() {
		t.Error("an identity was stored despite the refusal")
	}
}

func TestStagePKIEnrollmentReportsAFailure(t *testing.T) {
	dir := t.TempDir()
	svc, pki := newStagePKIService(t, dir, true)
	pki.enroll = func(context.Context, pkienroll.EnrollRequest) (pkienroll.Result, error) {
		return pkienroll.Result{}, errors.New("dial tcp: connection refused")
	}

	// The bounded retry sleeps five seconds between attempts; a cancelled
	// context short-circuits that wait without changing the outcome.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resp, err := svc.StagePKIEnrollment(ctx, &agentpbv2.StagePKIEnrollmentRequest{
		TenantUuid: testTenant,
		Token:      "tok-abc",
	})
	if err != nil {
		t.Fatalf("StagePKIEnrollment: %v", err)
	}
	if resp.GetStatus() != agentpbv2.StagePKIEnrollmentResponse_STATUS_FAILED {
		t.Fatalf("status = %v, want FAILED (%s)", resp.GetStatus(), resp.GetReason())
	}
	if resp.GetReason() == "" {
		t.Error("a failure must say why")
	}
}

func TestStagePKIEnrollmentKeepsAnExistingIdentity(t *testing.T) {
	dir := t.TempDir()
	svc, pki := newStagePKIService(t, dir, true)

	// Enrol once.
	pki.enroll = func(context.Context, pkienroll.EnrollRequest) (pkienroll.Result, error) {
		return pkienroll.Result{
			LeafPEM:    "-----BEGIN CERTIFICATE-----\nleaf\n-----END CERTIFICATE-----\n",
			DeviceName: "sh/wendy/2/408",
			SPIFFEURI:  testSPIFFEURI,
		}, nil
	}
	if _, err := svc.StagePKIEnrollment(context.Background(), &agentpbv2.StagePKIEnrollmentRequest{
		TenantUuid: testTenant, Token: "tok-one",
	}); err != nil {
		t.Fatalf("first enrolment: %v", err)
	}

	// A second token must not burn against a working identity.
	pki.enroll = func(context.Context, pkienroll.EnrollRequest) (pkienroll.Result, error) {
		t.Error("a second token was redeemed although an identity is already stored")
		return pkienroll.Result{}, errors.New("must not be called")
	}
	resp, err := svc.StagePKIEnrollment(context.Background(), &agentpbv2.StagePKIEnrollmentRequest{
		TenantUuid: testTenant, Token: "tok-two",
	})
	if err != nil {
		t.Fatalf("second enrolment: %v", err)
	}
	if resp.GetStatus() != agentpbv2.StagePKIEnrollmentResponse_STATUS_ALREADY_ENROLLED {
		t.Fatalf("status = %v, want ALREADY_ENROLLED", resp.GetStatus())
	}
	if _, statErr := os.Stat(resp.GetStagedPath()); !os.IsNotExist(statErr) {
		t.Error("the discarded token was left on disk")
	}
}

func TestStagePKIEnrollmentRejectsAnEmptyToken(t *testing.T) {
	dir := t.TempDir()
	svc, _ := newStagePKIService(t, dir, true)
	_, err := svc.StagePKIEnrollment(context.Background(), &agentpbv2.StagePKIEnrollmentRequest{TenantUuid: testTenant})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
	// Nothing may be written for a request that carries no credential.
	if _, statErr := os.Stat(filepath.Join(dir, PKIEnrollmentFileName)); !os.IsNotExist(statErr) {
		t.Error("a file was staged for a tokenless request")
	}
}

func TestStagePKIEnrollmentUnimplementedWithoutAManager(t *testing.T) {
	svc := NewProvisioningServiceV2(nil)
	_, err := svc.StagePKIEnrollment(context.Background(), &agentpbv2.StagePKIEnrollmentRequest{Token: "tok"})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("err = %v, want Unimplemented", err)
	}
}

func TestEnsureRenewerStartsOnceAndOnlyWithAnIdentity(t *testing.T) {
	dir := t.TempDir()
	svc, pki := newStagePKIService(t, dir, true)

	// Nothing enrolled: there is nothing to renew, and claiming otherwise
	// would stop a later enrolment from ever starting the loop.
	pki.EnsureRenewer(context.Background())
	if renewerRunning(pki) {
		t.Fatal("a renewer was started with no identity")
	}

	pki.enroll = func(context.Context, pkienroll.EnrollRequest) (pkienroll.Result, error) {
		return pkienroll.Result{
			LeafPEM:    "-----BEGIN CERTIFICATE-----\nleaf\n-----END CERTIFICATE-----\n",
			DeviceName: "sh/wendy/2/408",
			SPIFFEURI:  testSPIFFEURI,
		}, nil
	}
	if _, err := svc.StagePKIEnrollment(context.Background(), &agentpbv2.StagePKIEnrollmentRequest{
		TenantUuid: testTenant, Token: "tok-abc", Environment: "dev",
	}); err != nil {
		t.Fatalf("StagePKIEnrollment: %v", err)
	}

	// A successful enrolment must leave a renewal loop behind without waiting
	// for a restart: the leaf is thirty days, so a restart-only renewer is a
	// one-month fuse.
	if !renewerRunning(pki) {
		t.Fatal("no renewer was started after enrolment")
	}
	// And exactly one: a second loop over the same triple renews twice per
	// interval.
	pki.EnsureRenewer(context.Background())
	if !renewerRunning(pki) {
		t.Error("the renewer stopped")
	}
}

// renewerRunning reads the flag under its own lock, because the loop clears it
// from its own goroutine when it ends.
func renewerRunning(p *PKIEnrollment) bool {
	p.renewMu.Lock()
	defer p.renewMu.Unlock()
	return p.renewerRunning
}
