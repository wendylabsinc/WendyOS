package commands

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

const testDashboard = "https://cloud.example.invalid"
const testCloudGRPC = "grpc.example.invalid:443"

// fakeEnrollmentToken is an unsigned token whose claims enrollmentTokenIdentity
// accepts: a user enrollment in org 7. Signatures are the cloud's concern.
func fakeEnrollmentToken() string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"org_id":7,"user_id":"user-1","type":"user_enrollment"}`))
	return "header." + payload + ".sig"
}

// isolateLoginConfig points config.json (and its lock) at a fresh directory.
func isolateLoginConfig(t *testing.T) {
	t.Helper()
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
}

// stubIssueLegacyCertificate stands in for the cloud's CertificateService and
// returns a func reporting the requests it received.
func stubIssueLegacyCertificate(t *testing.T, resp *cloudpb.IssueCertificateResponse, err error) func() []*cloudpb.IssueCertificateRequest {
	t.Helper()
	var mu sync.Mutex
	var reqs []*cloudpb.IssueCertificateRequest
	prev := issueLegacyCertificate
	issueLegacyCertificate = func(_ context.Context, _ string, req *cloudpb.IssueCertificateRequest) (*cloudpb.IssueCertificateResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		reqs = append(reqs, req)
		return resp, err
	}
	t.Cleanup(func() { issueLegacyCertificate = prev })
	return func() []*cloudpb.IssueCertificateRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]*cloudpb.IssueCertificateRequest(nil), reqs...)
	}
}

// issuedCert is a successful issuance for org 7 with one warning.
func issuedCert() *cloudpb.IssueCertificateResponse {
	userID := "user-1"
	return &cloudpb.IssueCertificateResponse{
		Certificate: &cloudpb.Certificate{
			PemCertificate:      "LEAF-PEM",
			PemCertificateChain: "CHAIN-PEM",
		},
		OrganizationId: 7,
		UserId:         &userID,
		Warnings:       []string{"certificate expires soon"},
	}
}

// deliverLegacyCallback plays the dashboard: it redirects the "browser" to the
// session's loopback redirect_uri with query, and returns status and body.
func deliverLegacyCallback(t *testing.T, loginURL string, query url.Values) (int, string) {
	t.Helper()
	u, err := url.Parse(loginURL)
	if err != nil {
		t.Fatalf("parsing login URL: %v", err)
	}
	redirect := u.Query().Get("redirect_uri")
	if !strings.HasPrefix(redirect, "http://127.0.0.1:") || !strings.HasSuffix(redirect, "/cli-callback") {
		t.Fatalf("redirect_uri = %q, want the loopback /cli-callback", redirect)
	}
	resp, err := http.Get(redirect + "?" + query.Encode())
	if err != nil {
		t.Fatalf("calling back: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func waitLoginDone(t *testing.T, s *legacyLoginSession) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("login session did not finish")
	}
}

func TestBeginLegacyLogin_ReturnsDashboardURLWithoutWritingStdout(t *testing.T) {
	isolateLoginConfig(t)
	stubIssueLegacyCertificate(t, nil, errors.New("must not be called"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var session *legacyLoginSession
	var err error
	out := captureStdout(t, func() {
		session, err = beginLegacyLogin(ctx, testDashboard, testCloudGRPC)
		if err == nil {
			cancel()
			waitLoginDone(t, session)
		}
	})
	if err != nil {
		t.Fatalf("beginLegacyLogin: %v", err)
	}
	if out != "" {
		t.Fatalf("beginLegacyLogin wrote to stdout (the MCP protocol stream): %q", out)
	}
	if !strings.HasPrefix(session.URL(), testDashboard+"/cli-auth?redirect_uri=http%3A%2F%2F127.0.0.1%3A") {
		t.Fatalf("URL = %q", session.URL())
	}
	if want := testDashboard + "/cli-auth?redirect_uri=" + url.QueryEscape("wendy://cloud-login"); session.MobileURL() != want {
		t.Fatalf("MobileURL = %q, want %q", session.MobileURL(), want)
	}
	if left := time.Until(session.ExpiresAt()); left <= 0 || left > browserLoginTimeout {
		t.Fatalf("ExpiresAt is %s away, want within browserLoginTimeout (%s)", left, browserLoginTimeout)
	}
	if !errors.Is(session.Err(), context.Canceled) {
		t.Fatalf("Err = %v, want context.Canceled", session.Err())
	}
}

func TestLegacyLoginSession_CallbackIssuesAndSavesCertificate(t *testing.T) {
	isolateLoginConfig(t)
	requests := stubIssueLegacyCertificate(t, issuedCert(), nil)
	token := fakeEnrollmentToken()

	var session *legacyLoginSession
	var status int
	var body string
	out := captureStdout(t, func() {
		var err error
		session, err = beginLegacyLogin(context.Background(), testDashboard, testCloudGRPC)
		if err != nil {
			t.Fatalf("beginLegacyLogin: %v", err)
		}
		status, body = deliverLegacyCallback(t, session.URL(), url.Values{"token": {token}, "api_key": {"wnd_pat_abc"}})
		waitLoginDone(t, session)
	})
	if out != "" {
		t.Fatalf("the login session wrote to stdout: %q", out)
	}
	if status != http.StatusOK || !strings.Contains(body, "Authentication successful") {
		t.Fatalf("browser got %d %q", status, body)
	}
	if session.Err() != nil {
		t.Fatalf("Err = %v", session.Err())
	}
	select {
	case <-session.TokenReceived():
	default:
		t.Fatal("TokenReceived not closed after the callback")
	}
	if got := session.Warnings(); len(got) != 1 || got[0] != "certificate expires soon" {
		t.Fatalf("Warnings = %v", got)
	}
	reqs := requests()
	if len(reqs) != 1 || reqs[0].GetEnrollmentToken() != token || !strings.Contains(reqs[0].GetPemCsr(), "CERTIFICATE REQUEST") {
		t.Fatalf("issuance requests = %v", reqs)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Auth) != 1 {
		t.Fatalf("auth entries = %d, want 1", len(cfg.Auth))
	}
	a := cfg.Auth[0]
	if a.CloudDashboard != testDashboard || a.CloudGRPC != testCloudGRPC || a.APIKey != "wnd_pat_abc" {
		t.Fatalf("auth entry = %+v", a)
	}
	c := a.Certificates[0]
	if c.PemCertificate != "LEAF-PEM" || c.PemCertificateChain != "CHAIN-PEM" || c.OrganizationID != 7 || c.UserID != "user-1" {
		t.Fatalf("certificate = %+v", c)
	}
	if a.Name != "default" || cfg.CurrentContext != "default" {
		t.Fatalf("first login should become context \"default\" and current; name=%q current=%q", a.Name, cfg.CurrentContext)
	}
}

func TestLegacyLoginSession_DropsAPIKeyThatIsNotAPAT(t *testing.T) {
	isolateLoginConfig(t)
	stubIssueLegacyCertificate(t, issuedCert(), nil)
	session, err := beginLegacyLogin(context.Background(), testDashboard, testCloudGRPC)
	if err != nil {
		t.Fatal(err)
	}
	deliverLegacyCallback(t, session.URL(), url.Values{"token": {fakeEnrollmentToken()}, "api_key": {"not-a-pat"}})
	waitLoginDone(t, session)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth[0].APIKey != "" {
		t.Fatalf("APIKey = %q, want it dropped", cfg.Auth[0].APIKey)
	}
}

// Review Focus 4: a second sign-in (another org) is added beside the first
// without switching the current context, and a change another wendy process
// wrote while the browser was open survives the save.
func TestLegacyLoginSession_KeepsCurrentContextAndConcurrentConfigChanges(t *testing.T) {
	isolateLoginConfig(t)
	if err := config.Save(&config.Config{
		CurrentContext: "default",
		Auth: []config.AuthConfig{{
			Name:           "default",
			CloudDashboard: testDashboard,
			CloudGRPC:      testCloudGRPC,
			Certificates:   []config.CertificateInfo{{PemCertificate: "ORG3-PEM", OrganizationID: 3}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	stubIssueLegacyCertificate(t, issuedCert(), nil)

	session, err := beginLegacyLogin(context.Background(), testDashboard, testCloudGRPC)
	if err != nil {
		t.Fatal(err)
	}
	// Another wendy process (e.g. `wendy device set-default` in a terminal)
	// writes while the user is still in the browser.
	if err := config.Update(func(cfg *config.Config) (bool, error) {
		cfg.DefaultDevice = "pinned-device"
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	deliverLegacyCallback(t, session.URL(), url.Values{"token": {fakeEnrollmentToken()}})
	waitLoginDone(t, session)
	if session.Err() != nil {
		t.Fatalf("Err = %v", session.Err())
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultDevice != "pinned-device" {
		t.Fatalf("DefaultDevice = %q: the login save reverted a concurrent change", cfg.DefaultDevice)
	}
	if len(cfg.Auth) != 2 || cfg.CurrentContext != "default" {
		t.Fatalf("auth entries = %d, current = %q; want 2 and the unchanged \"default\"", len(cfg.Auth), cfg.CurrentContext)
	}
	if cfg.Auth[1].Name == "" {
		t.Fatal("the new session was not named as a context")
	}
}

func TestLegacyLoginSession_TimesOutWhenNoBrowserCallsBack(t *testing.T) {
	isolateLoginConfig(t)
	shrinkBrowserLoginTimeout(t)
	stubIssueLegacyCertificate(t, nil, errors.New("must not be called"))
	session, err := beginLegacyLogin(context.Background(), testDashboard, testCloudGRPC)
	if err != nil {
		t.Fatal(err)
	}
	waitLoginDone(t, session)
	if session.Err() == nil || !strings.Contains(session.Err().Error(), "timed out") {
		t.Fatalf("Err = %v, want the browser-login timeout", session.Err())
	}
	select {
	case <-session.TokenReceived():
		t.Fatal("TokenReceived closed without a callback")
	default:
	}
}

func TestLegacyLoginSession_CallbackWithoutTokenFails(t *testing.T) {
	isolateLoginConfig(t)
	stubIssueLegacyCertificate(t, nil, errors.New("must not be called"))
	session, err := beginLegacyLogin(context.Background(), testDashboard, testCloudGRPC)
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := deliverLegacyCallback(t, session.URL(), url.Values{}); status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	waitLoginDone(t, session)
	if session.Err() == nil || session.Err().Error() != "login failed: callback received without token" {
		t.Fatalf("Err = %v", session.Err())
	}
}

func TestLegacyLoginSession_IssuanceErrorIsReported(t *testing.T) {
	isolateLoginConfig(t)
	stubIssueLegacyCertificate(t, &cloudpb.IssueCertificateResponse{
		Error: &cloudpb.CertificateError{Message: "bad csr"},
	}, nil)
	session, err := beginLegacyLogin(context.Background(), testDashboard, testCloudGRPC)
	if err != nil {
		t.Fatal(err)
	}
	deliverLegacyCallback(t, session.URL(), url.Values{"token": {fakeEnrollmentToken()}})
	waitLoginDone(t, session)
	if session.Err() == nil || session.Err().Error() != "certificate issuance error: bad csr" {
		t.Fatalf("Err = %v", session.Err())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Auth) != 0 {
		t.Fatalf("a failed issuance saved %d auth entries", len(cfg.Auth))
	}
}

// A person at a terminal still sees every step, in order, now that the steps
// run in a session.
func TestPerformLogin_SuccessPrintsEveryStep(t *testing.T) {
	isolateLoginConfig(t)
	stubInteractive(t)
	stubHumanPresent(t, true)
	opened := stubOpenBrowser(t)
	stubIssueLegacyCertificate(t, issuedCert(), nil)

	// Play the browser: once performLogin has opened the URL, call back. This
	// runs off the test goroutine, so it reports with t.Error, never t.Fatal.
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if urls := opened(); len(urls) == 1 {
				u, err := url.Parse(urls[0])
				if err != nil {
					t.Errorf("parsing opened URL: %v", err)
					return
				}
				resp, err := http.Get(u.Query().Get("redirect_uri") + "?" + url.Values{"token": {fakeEnrollmentToken()}}.Encode())
				if err != nil {
					t.Errorf("calling back: %v", err)
					return
				}
				resp.Body.Close()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("performLogin never opened the browser")
	}()

	var err error
	out := captureStdout(t, func() {
		err = performLogin(context.Background(), testDashboard, testCloudGRPC)
	})
	if err != nil {
		t.Fatalf("performLogin: %v", err)
	}
	steps := []string{
		"Opening browser for authentication",
		"Or scan with the Wendy iOS app:",
		"Waiting for authentication...",
		"Received enrollment token.",
		"Authentication successful. Certificates saved.",
		"Warnings:",
		"  - certificate expires soon",
	}
	at := 0
	for _, step := range steps {
		i := strings.Index(out[at:], step)
		if i < 0 {
			t.Fatalf("output is missing %q after position %d:\n%s", step, at, out)
		}
		at += i + len(step)
	}
}

func TestLegacyLoginSession_IssuanceTimeoutEndsTheSession(t *testing.T) {
	isolateLoginConfig(t)
	prev := legacyIssueTimeout
	legacyIssueTimeout = 100 * time.Millisecond
	t.Cleanup(func() { legacyIssueTimeout = prev })
	prevIssue := issueLegacyCertificate
	issueLegacyCertificate = func(ctx context.Context, _ string, _ *cloudpb.IssueCertificateRequest) (*cloudpb.IssueCertificateResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	t.Cleanup(func() { issueLegacyCertificate = prevIssue })

	session, err := beginLegacyLogin(context.Background(), testDashboard, testCloudGRPC)
	if err != nil {
		t.Fatal(err)
	}
	deliverLegacyCallback(t, session.URL(), url.Values{"token": {fakeEnrollmentToken()}})
	select {
	case <-session.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("session still pending long after the issuance limit")
	}
	if !errors.Is(session.Err(), context.DeadlineExceeded) {
		t.Fatalf("Err = %v, want a deadline error", session.Err())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Auth) != 0 {
		t.Fatalf("saved %d auth entries after a timed-out issuance", len(cfg.Auth))
	}
}
