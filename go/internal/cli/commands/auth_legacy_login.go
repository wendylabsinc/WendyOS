package commands

// The legacy Wendy Cloud dashboard sign-in as a background session. It writes
// nothing to stdout, so `wendy mcp serve` — whose stdout is the MCP protocol
// stream — can run it for the auth_login tool. performLogin wraps it for a
// person at a terminal and prints each step.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	clitimesync "github.com/wendylabsinc/wendy/go/internal/cli/timesync"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// legacyLoginCallbackPage is what the browser shows once the dashboard
// redirects back to the CLI.
const legacyLoginCallbackPage = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>Wendy – Authenticated</title>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body {
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    background: #f8f9fa;
    display: flex;
    align-items: center;
    justify-content: center;
    min-height: 100vh;
    color: #1a1a1a;
  }
  .card {
    background: #fff;
    border-radius: 12px;
    box-shadow: 0 2px 12px rgba(0,0,0,0.08);
    padding: 48px;
    text-align: center;
    max-width: 420px;
  }
  .checkmark {
    width: 56px;
    height: 56px;
    background: #e8f5e9;
    border-radius: 50%;
    display: inline-flex;
    align-items: center;
    justify-content: center;
    margin-bottom: 20px;
    font-size: 28px;
  }
  h2 { font-size: 22px; font-weight: 600; margin-bottom: 8px; }
  p { font-size: 15px; color: #666; line-height: 1.5; }
</style>
</head>
<body>
  <div class="card">
    <div class="checkmark">✓</div>
    <h2>Authentication successful</h2>
    <p>You can close this tab and return to the terminal.</p>
  </div>
</body>
</html>`

// legacyLoginSession is one dashboard sign-in: a loopback listener waiting for
// the browser's /cli-callback, then certificate issuance and the config save.
type legacyLoginSession struct {
	url       string
	mobileURL string
	expiresAt time.Time

	tokenReceived chan struct{} // closed when the browser delivers the enrollment token
	done          chan struct{} // closed when the session ends, signed in or not

	mu           sync.Mutex
	err          error
	warnings     []string
	keyAlgorithm string
}

// URL is the sign-in link for a browser on this machine.
func (l *legacyLoginSession) URL() string { return l.url }

// MobileURL is the same sign-in for the Wendy iOS app (shown as a QR code).
func (l *legacyLoginSession) MobileURL() string { return l.mobileURL }

// ExpiresAt is when the session stops waiting for the browser.
func (l *legacyLoginSession) ExpiresAt() time.Time { return l.expiresAt }

// Done is closed when the session ends, signed in or not.
func (l *legacyLoginSession) Done() <-chan struct{} { return l.done }

// TokenReceived is closed when the browser hands back the enrollment token,
// before the certificate is issued.
func (l *legacyLoginSession) TokenReceived() <-chan struct{} { return l.tokenReceived }

// Err is why the session failed; nil while it runs and after success.
func (l *legacyLoginSession) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// Warnings are what the cloud noted while issuing the certificate.
func (l *legacyLoginSession) Warnings() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.warnings...)
}

// KeyAlgorithm names the algorithm of the session key this login generated;
// empty until the session signs in.
func (l *legacyLoginSession) KeyAlgorithm() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.keyAlgorithm
}

func (l *legacyLoginSession) finish(err error, warnings []string, keyAlgorithm string) {
	l.mu.Lock()
	l.err = err
	l.warnings = warnings
	l.keyAlgorithm = keyAlgorithm
	l.mu.Unlock()
	close(l.done)
}

// legacyIssueTimeout bounds the certificate request made after the browser
// callback. A var so tests can shrink it.
var legacyIssueTimeout = 60 * time.Second

// issueLegacyCertificate asks the cloud's CertificateService to sign a CSR. A
// var so tests can stand in for the cloud.
var issueLegacyCertificate = func(ctx context.Context, cloudGRPC string, req *cloudpb.IssueCertificateRequest) (*cloudpb.IssueCertificateResponse, error) {
	// This is the bootstrap step: no client cert exists yet, so we cannot do
	// mTLS. Non-:443 endpoints are local dev cloud; use plaintext because we
	// have no CA cert to verify the server with at this point.
	var bootstrapCreds grpc.DialOption
	if strings.HasSuffix(cloudGRPC, ":443") {
		bootstrapCreds = grpc.WithTransportCredentials(credentials.NewTLS(nil))
	} else {
		bootstrapCreds = grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	certConn, err := grpc.NewClient(cloudGRPC, bootstrapCreds)
	if err != nil {
		return nil, fmt.Errorf("connecting to cloud: %w", err)
	}
	defer certConn.Close()
	resp, err := cloudpb.NewCertificateServiceClient(certConn).IssueCertificate(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("issuing certificate: %w", err)
	}
	return resp, nil
}

// shutdownLoginServer stops the callback server, letting an in-flight response
// (the success page or the 400) finish first, and ends the serve goroutine and
// listener on every path.
func shutdownLoginServer(server *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		server.Close()
	}
}

// beginLegacyLogin starts a dashboard sign-in and returns as soon as the sign-in
// URL exists. Waiting for the browser (up to browserLoginTimeout), issuing the
// certificate and saving it run in the background until Done closes.
// Cancelling ctx ends the wait with ctx.Err().
func beginLegacyLogin(ctx context.Context, cloudDashboard, cloudGRPC string) (*legacyLoginSession, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("starting local callback server: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	// Buffered and sent without blocking: only the first callback counts, and
	// a stray second request must not wedge its handler.
	tokenCh := make(chan loginCallbackResult, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/cli-callback", func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if token == "" {
			http.Error(w, "missing token parameter", http.StatusBadRequest)
			select {
			case errCh <- fmt.Errorf("callback received without token"):
			default:
			}
			return
		}
		apiKey := r.URL.Query().Get("api_key")
		if !strings.HasPrefix(apiKey, "wnd_pat_") || len(apiKey) > 256 {
			apiKey = ""
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, legacyLoginCallbackPage)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case tokenCh <- loginCallbackResult{EnrollmentToken: token, APIKey: apiKey}:
		default:
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
			select {
			case errCh <- serveErr:
			default:
			}
		}
	}()

	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/cli-callback", port)
	session := &legacyLoginSession{
		url:           fmt.Sprintf("%s/cli-auth?redirect_uri=%s", cloudDashboard, url.QueryEscape(redirectURI)),
		mobileURL:     fmt.Sprintf("%s/cli-auth?redirect_uri=%s", cloudDashboard, url.QueryEscape("wendy://cloud-login")),
		expiresAt:     time.Now().Add(browserLoginTimeout),
		tokenReceived: make(chan struct{}),
		done:          make(chan struct{}),
	}
	go func() {
		defer shutdownLoginServer(server)
		timeout := time.NewTimer(browserLoginTimeout)
		defer timeout.Stop()
		var result loginCallbackResult
		select {
		case result = <-tokenCh:
			close(session.tokenReceived)
		case loginErr := <-errCh:
			session.finish(fmt.Errorf("login failed: %w", loginErr), nil, "")
			return
		case <-ctx.Done():
			session.finish(ctx.Err(), nil, "")
			return
		case <-timeout.C:
			session.finish(browserLoginTimeoutError(), nil, "")
			return
		}
		warnings, keyAlgorithm, err := completeLegacyLogin(ctx, cloudDashboard, cloudGRPC, result)
		session.finish(err, warnings, keyAlgorithm)
	}()
	return session, nil
}

// completeLegacyLogin turns the browser's enrollment token into a saved
// certificate: a new key, a CSR the cloud signs, and a new auth context in
// config.json. It returns the cloud's issuance warnings and the session key's
// algorithm.
func completeLegacyLogin(ctx context.Context, cloudDashboard, cloudGRPC string, result loginCallbackResult) ([]string, string, error) {
	privateKeyPEM, err := certs.GenerateKeyPair()
	if err != nil {
		return nil, "", fmt.Errorf("generating key pair: %w", err)
	}
	commonName, identityURIs, err := enrollmentTokenIdentity(result.EnrollmentToken)
	if err != nil {
		return nil, "", fmt.Errorf("reading enrollment token identity: %w", err)
	}
	csrPEM, err := certs.GenerateCSR([]byte(privateKeyPEM), commonName, identityURIs)
	if err != nil {
		return nil, "", fmt.Errorf("generating CSR: %w", err)
	}
	// Only the cloud request is bounded; cancelling ctx still aborts it at once.
	issueCtx, cancelIssue := context.WithTimeout(ctx, legacyIssueTimeout)
	defer cancelIssue()
	issueResp, err := issueLegacyCertificate(issueCtx, cloudGRPC, &cloudpb.IssueCertificateRequest{
		PemCsr:          csrPEM,
		EnrollmentToken: result.EnrollmentToken,
	})
	if err != nil {
		if ctx.Err() == nil && issueCtx.Err() != nil {
			return nil, "", fmt.Errorf("cloud did not issue a certificate within %s: %w (%w)", legacyIssueTimeout, err, context.DeadlineExceeded)
		}
		return nil, "", err
	}
	if issueResp.GetError() != nil {
		return nil, "", fmt.Errorf("certificate issuance error: %s", issueResp.GetError().GetMessage())
	}
	cert := issueResp.GetCertificate()
	if cert == nil {
		return nil, "", fmt.Errorf("no certificate returned from cloud")
	}

	authEntry := config.AuthConfig{
		CloudDashboard: cloudDashboard,
		CloudGRPC:      cloudGRPC,
		APIKey:         result.APIKey,
		Certificates: []config.CertificateInfo{{
			PemCertificate:      cert.GetPemCertificate(),
			PemCertificateChain: cert.GetPemCertificateChain(),
			PemPrivateKey:       privateKeyPEM,
			OrganizationID:      int(issueResp.GetOrganizationId()),
			UserID:              issueResp.GetUserId(),
		}},
	}
	// Update, not Load then Save: the MCP server is long-lived, and another
	// wendy process may have written config.json while the browser was open.
	if err := config.Update(func(cfg *config.Config) (bool, error) {
		cfg.AddAuth(authEntry)
		// Name the new session as a context; the first login becomes "default"
		// and current. A later login does not change the current context.
		cfg.EnsureContexts()
		return true, nil
	}); err != nil {
		return nil, "", fmt.Errorf("saving config: %w", err)
	}
	clitimesync.CacheProof(ctx)
	return issueResp.GetWarnings(), privateKeyAlgorithm(privateKeyPEM), nil
}
