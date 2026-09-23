package commands

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	clitimesync "github.com/wendylabsinc/wendy/go/internal/cli/timesync"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/browseropen"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/enrolltoken"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

const defaultCloudDashboard = "https://cloud.wendy.sh"
const defaultCloudGRPC = "api.wendy.sh:443"
const defaultDevAuthBase = "https://auth.dev.wendy.sh"
const defaultDevCloudDashboard = "https://cloud.dev.wendy.sh"
const defaultDevCloudGRPC = "api.dev.wendy.sh:443"
const defaultDevCloudResource = "https://cloud.dev.wendy.sh/api"
const defaultPKIIdentityResource = "https://pki.wendy.sh/identity"
const defaultDevPKIIdentityEndpoint = "https://identity.dev.pki.wendy.sh/v1/identity/certificate"

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage authentication with Wendy Cloud",
	}

	cmd.AddCommand(
		newAuthLoginCmd(),
		newAuthLogoutCmd(),
		newAuthRefreshCertsCmd(),
		newAuthStatusCmd(),
		newAuthUseCmd(),
		newAuthRenameCmd(),
		newAuthDefaultCmd(),
		newAuthListOrgsCmd(),
	)

	return cmd
}

func newAuthLoginCmd() *cobra.Command {
	var cloudDashboard string
	var cloudGRPC string
	var apiKey string
	var orgID int32
	var issuer string
	var email string
	var authBase string
	var clientID string
	var resource string
	var identityResource string
	var identityEndpoint string
	var printClaims bool
	var legacy bool

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to Wendy Cloud or a local pki-core instance",
		Long: "Signs in to Wendy Cloud. For now, defaults to the legacy dashboard login (cloud.wendy.sh). For the OIDC flow, pass --email to discover your realm (or --issuer to name it), sign in with authorization code + PKCE, obtain an operator certificate directly from pki-core, and save a refreshable Cloud API session.\n" +
			"With --api-key: issues a certificate from a self-hosted pki-core instance using a Bearer API key.\n" +
			"With --legacy: uses the old Wendy Cloud dashboard enrollment callback (cloud.wendy.sh). Kept for the previous cloud only.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Temporarily default to legacy login until the new cloud is ready.
			// Explicit OIDC or local authentication options keep their existing behavior.
			if !cmd.Flags().Changed("legacy") && email == "" && issuer == "" && apiKey == "" {
				legacy = true
			}
			if legacy {
				if apiKey != "" || issuer != "" || email != "" {
					return fmt.Errorf("--legacy selects the old cloud-dashboard login and cannot be combined with --api-key, --issuer, or --email")
				}
				if cloudDashboard == "" {
					cloudDashboard = defaultCloudDashboard
				}
				if cloudGRPC == "" {
					cloudGRPC = defaultCloudGRPC
				}
				if !strings.HasPrefix(cloudDashboard, "http://") && !strings.HasPrefix(cloudDashboard, "https://") {
					cloudDashboard = "https://" + cloudDashboard
				}
				return performLogin(cmd.Context(), cloudDashboard, cloudGRPC)
			}

			// Self-hosted pki-core with a bearer key.
			if apiKey != "" {
				if issuer != "" || email != "" {
					return fmt.Errorf("OIDC and --api-key select different login modes; pass only one")
				}
				if cloudGRPC == "" {
					return fmt.Errorf("--cloud-grpc is required for local authentication")
				}
				return performLocalLogin(cmd.Context(), cloudGRPC, apiKey, orgID)
			}

			// OIDC login requires an email address or an explicit realm issuer.
			if authBase == "" {
				authBase = defaultDevAuthBase
			}
			if issuer == "" {
				if email == "" {
					return fmt.Errorf("provide --email to discover your realm, or --issuer to name it; use --legacy for the old cloud-dashboard login")
				}
				var err error
				issuer, err = discoverOIDCIssuer(cmd.Context(), authBase, email)
				if err != nil {
					return err
				}
			}
			if cloudDashboard == "" {
				cloudDashboard = defaultDevCloudDashboard
			}
			if cloudGRPC == "" {
				cloudGRPC = defaultDevCloudGRPC
			}
			if resource == "" {
				resource = defaultDevCloudResource
			}
			if identityResource == "" {
				identityResource = defaultPKIIdentityResource
			}
			if identityEndpoint == "" {
				identityEndpoint = defaultDevPKIIdentityEndpoint
			}
			return performOIDCLogin(cmd.Context(), oidcLoginOptions{
				Issuer:           issuer,
				ClientID:         clientID,
				CloudResource:    resource,
				IdentityResource: identityResource,
				IdentityEndpoint: identityEndpoint,
				CloudURL:         cloudDashboard,
				CloudGRPC:        cloudGRPC,
				PrintClaims:      printClaims,
			})
		},
	}

	cmd.Flags().StringVar(&cloudDashboard, "cloud", "", "Cloud dashboard URL")
	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint, or local pki-core address (host:port) when using --api-key")
	cmd.Flags().StringVar(&apiKey, "api-key", "", "Bearer API key for local pki-core authentication")
	cmd.Flags().Int32Var(&orgID, "org", 1, "Organization ID for --api-key local login. For Wendy Cloud, each login is stored as an auth context; switch with 'wendy auth use <context>'.")
	cmd.Flags().StringVar(&issuer, "issuer", "", "wendy-auth realm issuer URL, e.g. https://auth.wendy.sh/realms/acme (enables OIDC login)")
	cmd.Flags().StringVar(&email, "email", "", "Email address used to discover your organization and sign in with wendy-auth")
	cmd.Flags().StringVar(&authBase, "auth", defaultDevAuthBase, "wendy-auth base URL used with --email")
	cmd.Flags().StringVar(&clientID, "client-id", "wendy-cli", "public DPoP OAuth client ID registered in wendy-auth")
	cmd.Flags().StringVar(&resource, "resource", "", "RFC 8707 API resource indicator (used with OIDC login)")
	cmd.Flags().StringVar(&identityResource, "pki-resource", defaultPKIIdentityResource, "RFC 8707 pki-core identity resource (used with OIDC login)")
	cmd.Flags().StringVar(&identityEndpoint, "pki-identity-endpoint", defaultDevPKIIdentityEndpoint, "pki-core operator identity CSR endpoint (used with OIDC login)")
	cmd.Flags().BoolVar(&printClaims, "print-claims", false, "Print the decoded access-token claims after login (used with --issuer)")
	cmd.Flags().BoolVar(&legacy, "legacy", false, "Use the old Wendy Cloud dashboard enrollment flow (cloud.wendy.sh) (the temporary default unless --email, --issuer, or --api-key is provided)")
	return cmd
}

type loginCallbackResult struct {
	EnrollmentToken string
	APIKey          string
}

func performLogin(ctx context.Context, cloudDashboard, cloudGRPC string) error {
	// Step 1: Start a local HTTP server to receive the OAuth callback.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("starting local callback server: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	// Channel to receive the enrollment token and PAT from the callback.
	tokenCh := make(chan loginCallbackResult, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/cli-callback", func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if token == "" {
			http.Error(w, "missing token parameter", http.StatusBadRequest)
			errCh <- fmt.Errorf("callback received without token")
			return
		}
		apiKey := r.URL.Query().Get("api_key")
		if !strings.HasPrefix(apiKey, "wnd_pat_") || len(apiKey) > 256 {
			apiKey = ""
		}

		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!DOCTYPE html>
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
    border-radius: 50%%;
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
</html>`)
		tokenCh <- loginCallbackResult{EnrollmentToken: token, APIKey: apiKey}
	})

	server := &http.Server{Handler: mux}
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
			errCh <- serveErr
		}
	}()
	defer server.Close()

	// Step 2: Open browser to login URL with callback port.
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/cli-callback", port)
	loginURL := fmt.Sprintf("%s/cli-auth?redirect_uri=%s", cloudDashboard, url.QueryEscape(redirectURI))
	fmt.Println(tui.InfoMessage("Opening browser for authentication"))
	fmt.Printf("  %s\n", loginURL)

	if err := openBrowser(loginURL); err != nil {
		fmt.Println(tui.WarningMessage("Could not open browser automatically. Please visit:"))
		fmt.Printf("  %s\n", loginURL)
	}

	// Show a QR code the user can scan with the Wendy iOS app to log in on their phone.
	mobileRedirect := url.QueryEscape("wendy://cloud-login")
	mobileLoginURL := fmt.Sprintf("%s/cli-auth?redirect_uri=%s", cloudDashboard, mobileRedirect)
	if qr, qrErr := qrcode.New(mobileLoginURL, qrcode.Medium); qrErr == nil {
		fmt.Println(tui.InfoMessage("Or scan with the Wendy iOS app:"))
		fmt.Println(qr.ToSmallString(false))
	}

	fmt.Println(tui.InfoMessage("Waiting for authentication..."))

	// Wait for the token and PAT.
	var result loginCallbackResult
	select {
	case result = <-tokenCh:
		fmt.Println(tui.SuccessMessage("Received enrollment token."))
	case loginErr := <-errCh:
		return fmt.Errorf("login failed: %w", loginErr)
	case <-ctx.Done():
		return ctx.Err()
	}

	// Step 3: Generate a key pair and CSR.
	privateKeyPEM, err := certs.GenerateKeyPair()
	if err != nil {
		return fmt.Errorf("generating key pair: %w", err)
	}

	commonName, identityURIs, err := enrollmentTokenIdentity(result.EnrollmentToken)
	if err != nil {
		return fmt.Errorf("reading enrollment token identity: %w", err)
	}
	csrPEM, err := certs.GenerateCSR([]byte(privateKeyPEM), commonName, identityURIs)
	if err != nil {
		return fmt.Errorf("generating CSR: %w", err)
	}

	// Step 4: Issue certificate via cloud CertificateService.
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
		return fmt.Errorf("connecting to cloud: %w", err)
	}
	defer certConn.Close()

	certClient := cloudpb.NewCertificateServiceClient(certConn)
	issueResp, err := certClient.IssueCertificate(ctx, &cloudpb.IssueCertificateRequest{
		PemCsr:          csrPEM,
		EnrollmentToken: result.EnrollmentToken,
	})
	if err != nil {
		return fmt.Errorf("issuing certificate: %w", err)
	}

	if issueResp.GetError() != nil {
		return fmt.Errorf("certificate issuance error: %s", issueResp.GetError().GetMessage())
	}

	cert := issueResp.GetCertificate()
	if cert == nil {
		return fmt.Errorf("no certificate returned from cloud")
	}

	// Step 5: Save certificates to config.
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	certInfo := config.CertificateInfo{
		PemCertificate:      cert.GetPemCertificate(),
		PemCertificateChain: cert.GetPemCertificateChain(),
		PemPrivateKey:       privateKeyPEM,
		OrganizationID:      int(issueResp.GetOrganizationId()),
		UserID:              issueResp.GetUserId(),
	}

	authEntry := config.AuthConfig{
		CloudDashboard: cloudDashboard,
		CloudGRPC:      cloudGRPC,
		APIKey:         result.APIKey,
		Certificates:   []config.CertificateInfo{certInfo},
	}

	cfg.AddAuth(authEntry)
	// Name the new session as a context; the first login becomes "default" and
	// current. A later login does not change the current context.
	cfg.EnsureContexts()
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}

	fmt.Println(tui.SuccessMessage("Authentication successful. Certificates saved."))
	clitimesync.CacheProof(ctx)

	if len(issueResp.GetWarnings()) > 0 {
		fmt.Println(tui.WarningMessage("Warnings:"))
		for _, w := range issueResp.GetWarnings() {
			fmt.Printf("  - %s\n", w)
		}
	}

	return nil
}

// enrollmentTokenIdentity derives the CSR Subject CommonName and the
// authoritative identity URI SAN from an enrollment token's claims. The URN
// ("urn:wendy:org:<org>:user:<userID>" for users, "urn:wendy:org:<org>:asset:<assetID>"
// for assets) is what IdentityFromCert prefers over the legacy CommonName.
func enrollmentTokenIdentity(token string) (commonName string, identityURIs []string, err error) {
	claims, err := enrolltoken.Parse(token)
	if err != nil {
		return "", nil, err
	}
	// The tenant SPIFFE principal rides alongside the urn:wendy SAN whenever
	// the token carries a tenant; cloud refuses to sign a relay grant without
	// it. Orgs with no pki tenant get no claim, and enroll as they always did.
	withTenant := func(cn, urn string) (string, []string, error) {
		uris := []string{urn}
		if spiffeURI, ok := claims.TenantSPIFFEURI(); ok {
			uris = append(uris, spiffeURI)
		}
		return cn, uris, nil
	}
	switch claims.Type {
	case "user_enrollment":
		if claims.UserID == "" {
			return "", nil, fmt.Errorf("user enrollment token missing user_id")
		}
		if strings.Contains(claims.UserID, ":") {
			// A ':' in the user ID would make the URN unreadable for every
			// identity parser (they expect exactly 6 colon-separated parts),
			// yielding a cert that cannot authenticate anywhere.
			return "", nil, fmt.Errorf("user_id %q contains ':', cannot build identity URN", claims.UserID)
		}
		cn := fmt.Sprintf("wendy/user/%s", claims.UserID)
		if claims.OrganizationID == 0 {
			// Legacy token without an org claim: keep login working, CN only.
			return cn, nil, nil
		}
		return withTenant(cn, certs.UserURN(claims.OrganizationID, claims.UserID))
	case "asset_enrollment":
		if claims.OrganizationID == 0 || claims.AssetID == 0 {
			return "", nil, fmt.Errorf("asset enrollment token missing org_id or asset_id")
		}
		return withTenant(
			fmt.Sprintf("wendy/%d/%d", claims.OrganizationID, claims.AssetID),
			certs.AssetURN(claims.OrganizationID, claims.AssetID))
	default:
		return "", nil, fmt.Errorf("unsupported enrollment token type %q", claims.Type)
	}
}

func performLocalLogin(ctx context.Context, cloudGRPC, apiKey string, orgID int32) error {
	cloudConn, err := grpc.NewClient(cloudGRPC, grpc.WithTransportCredentials(credentials.NewTLS(nil)))
	if err != nil {
		return fmt.Errorf("connecting to pki-core: %w", err)
	}
	defer cloudConn.Close()

	authCtx := metadata.NewOutgoingContext(ctx,
		metadata.Pairs("authorization", "Bearer "+apiKey))

	certClient := cloudpb.NewCertificateServiceClient(cloudConn)

	tokenResp, err := certClient.CreateAssetEnrollmentToken(authCtx, &cloudpb.CreateAssetEnrollmentTokenRequest{
		OrganizationId: orgID,
		Name:           "cli-user",
		TtlSeconds:     120,
	})
	if err != nil {
		return fmt.Errorf("creating enrollment token from pki-core %s: %w", cloudGRPC, err)
	}
	// Reconstruct the device_id that pki-core stored in the token.
	deviceID := fmt.Sprintf("sh/wendy/%d/%d", tokenResp.GetOrganizationId(), tokenResp.GetAssetId())
	identityURIs := []string{certs.AssetURN(tokenResp.GetOrganizationId(), tokenResp.GetAssetId())}
	if spiffeURI, ok := enrolltoken.TenantSPIFFEURIFromToken(tokenResp.GetEnrollmentToken()); ok {
		identityURIs = append(identityURIs, spiffeURI)
	}

	privateKeyPEM, err := certs.GenerateKeyPair()
	if err != nil {
		return fmt.Errorf("generating key pair: %w", err)
	}
	csrPEM, err := certs.GenerateCSR([]byte(privateKeyPEM), deviceID, identityURIs)
	if err != nil {
		return fmt.Errorf("generating CSR: %w", err)
	}

	issueResp, err := certClient.IssueCertificate(ctx, &cloudpb.IssueCertificateRequest{
		PemCsr:          csrPEM,
		EnrollmentToken: tokenResp.GetEnrollmentToken(),
	})
	if err != nil {
		return fmt.Errorf("issuing certificate: %w", err)
	}
	if issueResp.GetError() != nil {
		return fmt.Errorf("certificate issuance error: %s", issueResp.GetError().GetMessage())
	}
	cert := issueResp.GetCertificate()
	if cert == nil {
		return fmt.Errorf("no certificate returned from pki-core")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	certInfo := config.CertificateInfo{
		PemCertificate:      cert.GetPemCertificate(),
		PemCertificateChain: cert.GetPemCertificateChain(),
		PemPrivateKey:       privateKeyPEM,
		OrganizationID:      int(issueResp.GetOrganizationId()),
		AssetID:             int(issueResp.GetAssetId()),
	}
	authEntry := config.AuthConfig{
		CloudGRPC:    cloudGRPC,
		APIKey:       apiKey,
		Certificates: []config.CertificateInfo{certInfo},
	}

	cfg.AddAuth(authEntry)
	cfg.EnsureContexts()

	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}

	fmt.Println(tui.SuccessMessage(fmt.Sprintf("Local authentication successful (org=%d, device=%s). Certificates saved.",
		issueResp.GetOrganizationId(), deviceID)))
	clitimesync.CacheProof(ctx)

	return nil
}

func newAuthLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Log out from Wendy Cloud",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			// Drop the references and save FIRST, then delete the Keychain
			// items they pointed at. A failed Save here leaves config.json
			// and the Keychain items both untouched, so a retry (or a
			// manual fix) can still recover; deleting the items first would
			// instead risk leaving config.json referencing Keychain items
			// that no longer exist, breaking every command until the user
			// re-logs in.
			entries := cfg.Auth
			cfg.Auth = nil
			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}
			config.DeleteStoredSecrets(&config.Config{Auth: entries})

			// A prepared session broker retains an authenticated device
			// transport; logging out must sever that too, not just future
			// dials. Best-effort: the credentials above are already gone, and
			// a broker that survives unpublication only lasts until its next
			// liveness check.
			_ = sessionbroker.InvalidateAll()

			fmt.Println(tui.SuccessMessage("Logged out. All authentication credentials removed."))
			return nil
		},
	}
}

func newAuthRefreshCertsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh-certs",
		Short: "Refresh mTLS certificates",
		Long:  "Generates a new key pair and CSR, then issues new certificates using existing credentials.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			err := refreshAllCerts(ctx)
			if err == nil {
				return nil
			}
			// An expired/absent session cannot be fixed by refreshing (there is no
			// valid identity to refresh with) — offer to log in again instead, which
			// issues fresh certificates directly.
			if offerReloginOnUnauthenticated(ctx, firstAuthEntryForRelogin(), err) {
				fmt.Println(tui.SuccessMessage("Logged in again and issued fresh certificates."))
				return nil
			}
			return err
		},
	}
}

// refreshAllCerts re-issues certificates for every stored auth entry and
// saves the updated config. It returns an error when not logged in or when
// no entry could be refreshed, so callers that retry a connection afterwards
// do not retry with the same stale certificates.
func refreshAllCerts(ctx context.Context) error {
	// OIDC certificate refresh consumes the same rotating token family as API
	// refresh. Keep the lock through the final Save, including partial failures.
	unlock, err := acquireAuthRefreshLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if len(cfg.Auth) == 0 {
		return fmt.Errorf("not logged in; run 'wendy auth login' first")
	}
	for i := range cfg.Auth {
		cfg.Auth[i].InvalidateCachedSecrets()
	}

	refreshed := 0
	var lastErr error
	for i, auth := range cfg.Auth {
		if len(auth.Certificates) == 0 {
			fmt.Println(tui.WarningMessage(fmt.Sprintf("Skipping %s: no certificates to refresh", auth.CloudDashboard)))
			continue
		}

		fmt.Println(tui.InfoMessage(fmt.Sprintf("Refreshing certificates for %s...", auth.CloudDashboard)))

		if err := refreshCertsForAuth(ctx, &cfg.Auth[i]); err != nil {
			fmt.Println(tui.ErrorMessage(fmt.Sprintf("Failed to refresh for %s: %v", auth.CloudDashboard, err)))
			lastErr = err
			continue
		}

		refreshed++
		fmt.Println(tui.SuccessMessage(fmt.Sprintf("Certificates refreshed for %s.", auth.CloudDashboard)))
	}

	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}

	if refreshed == 0 {
		// Preserve the underlying failure (e.g. an unauthorized CertificateError)
		// so the caller can detect an expired session and offer re-login.
		if lastErr != nil {
			return lastErr
		}
		return fmt.Errorf("no certificates were refreshed")
	}
	return nil
}

// firstAuthEntryForRelogin returns the stored auth entry a re-login should target
// — the current context when one is set, otherwise the first entry — or nil when
// there is nothing stored (the caller then falls back to the built-in defaults).
func firstAuthEntryForRelogin() *config.AuthConfig {
	cfg, err := config.Load()
	if err != nil || len(cfg.Auth) == 0 {
		return nil
	}
	if a, ok := cfg.ContextByName(cfg.CurrentContext); ok {
		return a
	}
	return &cfg.Auth[0]
}

// certCommonName extracts the Subject CN from a PEM-encoded certificate.
// It normalizes the input with certs.LeafCertificatePEM first because
// pki-core certificates can contain trailing ASN.1 bytes that cause
// x509.ParseCertificate to fail on the raw stored PEM.
func certCommonName(pemCertificate string) (string, error) {
	leafPEM, err := certs.LeafCertificatePEM(pemCertificate)
	if err != nil {
		return "", fmt.Errorf("normalizing certificate PEM: %w", err)
	}
	block, _ := pem.Decode([]byte(leafPEM))
	if block == nil {
		return "", fmt.Errorf("decoding certificate PEM")
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parsing certificate: %w", err)
	}
	return parsed.Subject.CommonName, nil
}

// storedCertIdentityURN builds the authoritative Wendy identity URN from a
// stored certificate's org/user/asset fields, or "" when there is no positive
// org id to anchor it (a legacy entry that predates org-scoped identity).
func storedCertIdentityURN(cert config.CertificateInfo) string {
	if cert.OrganizationID <= 0 {
		return ""
	}
	if cert.UserID != "" {
		if strings.Contains(cert.UserID, ":") {
			// A ':' would make the URN unreadable for every identity parser;
			// refresh CN-only rather than minting a cert nothing can parse.
			return ""
		}
		return certs.UserURN(int32(cert.OrganizationID), cert.UserID)
	}
	if cert.AssetID > 0 {
		return certs.AssetURN(int32(cert.OrganizationID), int32(cert.AssetID))
	}
	return ""
}

// refreshCertsForAuth renews one auth entry's certificate against pki-core's
// renew frontend. Cloud is not in this path: it neither mints the certificate
// nor relays the request. The certificate being replaced is itself the proof of
// possession — it is presented in the mTLS handshake that carries the CSR.
func refreshCertsForAuth(ctx context.Context, auth *config.AuthConfig) error {
	if len(auth.Certificates) == 0 {
		return fmt.Errorf("no existing certificates")
	}
	if auth.OAuthIssuer != "" {
		return refreshOIDCCertificate(ctx, auth)
	}

	current := auth.Certificates[0]

	// pki-core routes a renewal by the tenant SPIFFE principal on the presented
	// certificate, and renews only lineages it issued itself. A leaf carrying no
	// tenant principal is not renewable there by any route, so report that here
	// rather than spend a round trip to be told the same thing as a bare 403.
	if _, ok := tenantPrincipalFor(current.PemCertificate); !ok {
		return errCertNotPKIIssued
	}

	endpoint := renewEndpoint(auth)
	if endpoint == "" {
		return errNoRenewEndpoint
	}

	certPEM, chainPEM, keyPEM, err := renewViaPKICore(ctx, endpoint, auth)
	if err != nil {
		return err
	}

	// Mutate the stored entry rather than rebuild it: a renewal replaces key
	// material and nothing else. Rebuilding dropped the asset and principal
	// fields, which is what identifies a device session to every later command.
	updated := current
	updated.PemCertificate = certPEM
	updated.PemCertificateChain = chainPEM
	updated.PemPrivateKey = keyPEM
	auth.Certificates[0] = updated

	// This cert has a later NotBefore than the one it replaces, so the proof kept
	// for offline use has to move with it.
	clitimesync.CacheProof(ctx)

	return nil
}

// certExpiryWindow is how far ahead of NotAfter `auth status` starts warning
// that a certificate is about to expire.
const certExpiryWindow = 7 * 24 * time.Hour

// authStatusCert is the certificate half of one `auth status` session in JSON.
type authStatusCert struct {
	ExpiresAt    time.Time `json:"expiresAt"`
	Expired      bool      `json:"expired"`
	ExpiringSoon bool      `json:"expiringSoon"`
}

// authStatusSession is one stored cloud session in `auth status --json`. It
// carries the same facts as the human rendering below; keep the two in step.
type authStatusSession struct {
	Context        string          `json:"context,omitempty"`
	Current        bool            `json:"current,omitempty"`
	Cloud          string          `json:"cloud"`
	CloudGRPC      string          `json:"cloudGrpc,omitempty"`
	UserID         string          `json:"userId,omitempty"`
	OrganizationID int             `json:"organizationId,omitempty"`
	PrincipalURI   string          `json:"principalUri,omitempty"`
	Certificate    *authStatusCert `json:"certificate,omitempty"`
}

type authStatusJSON struct {
	LoggedIn bool                `json:"loggedIn"`
	Sessions []authStatusSession `json:"sessions"`
}

// authStatusCertInfo summarizes a stored PEM certificate's expiry. It returns
// nil when the certificate is absent or unparseable, matching the human
// rendering, which simply omits the line in that case.
func authStatusCertInfo(pemCert string, now time.Time) *authStatusCert {
	if pemCert == "" {
		return nil
	}
	block, _ := pem.Decode([]byte(pemCert))
	if block == nil {
		return nil
	}
	x509Cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil
	}
	expiry := x509Cert.NotAfter
	return &authStatusCert{
		ExpiresAt:    expiry,
		Expired:      now.After(expiry),
		ExpiringSoon: !now.After(expiry) && expiry.Sub(now) < certExpiryWindow,
	}
}

// authStatusEndpoint is the address `auth status` labels "Cloud:" — the
// dashboard URL when stored, else the gRPC endpoint.
func authStatusEndpoint(auth config.AuthConfig) string {
	if auth.CloudDashboard != "" {
		return auth.CloudDashboard
	}
	return auth.CloudGRPC
}

func newAuthStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show current authentication status",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			// --json is a root persistent flag, and the root PersistentPreRunE
			// also turns it on for non-interactive stdout, so every scripted
			// invocation lands here. Emit JSON only — no banner, no warning
			// lines — so the output stays pipeable into jq.
			out := cmd.OutOrStdout()
			if jsonOutput {
				return writeAuthStatusJSON(out, cfg, time.Now())
			}

			if len(cfg.Auth) == 0 {
				fmt.Fprintln(out, tui.WarningMessage("Not logged in. Run 'wendy auth login' to authenticate."))
				return nil
			}

			for _, auth := range cfg.Auth {
				marker := ""
				if auth.Name != "" && auth.Name == cfg.CurrentContext {
					marker = " (current)"
				}
				if auth.Name != "" {
					fmt.Fprintf(out, "Context: %s%s\n", auth.Name, marker)
				}
				endpoint := authStatusEndpoint(auth)
				fmt.Fprintf(out, "Cloud:  %s\n", endpoint)
				if auth.CloudGRPC != "" && auth.CloudGRPC != endpoint {
					fmt.Fprintf(out, "  gRPC: %s\n", auth.CloudGRPC)
				}

				if len(auth.Certificates) == 0 {
					fmt.Fprintln(out, tui.WarningMessage("  No certificates stored."))
					continue
				}

				cert := auth.Certificates[0]
				if cert.PrincipalURI != "" {
					fmt.Fprintf(out, "  Identity: %s\n", cert.PrincipalURI)
				}
				if cert.UserID != "" {
					fmt.Fprintf(out, "  User: %s\n", cert.UserID)
				}
				if cert.OrganizationID != 0 {
					fmt.Fprintf(out, "  Org:  %d\n", cert.OrganizationID)
				}

				if info := authStatusCertInfo(cert.PemCertificate, time.Now()); info != nil {
					expiryStr := info.ExpiresAt.Format("2006-01-02 15:04 UTC")
					switch {
					case info.Expired:
						fmt.Fprintln(out, tui.ErrorMessage(fmt.Sprintf("  Certificate expired on %s", expiryStr)))
					case info.ExpiringSoon:
						remaining := time.Until(info.ExpiresAt).Truncate(time.Second)
						fmt.Fprintln(out, tui.WarningMessage(fmt.Sprintf("  Certificate expires %s (in %s)", expiryStr, remaining)))
					default:
						fmt.Fprintln(out, tui.SuccessMessage(fmt.Sprintf("  Certificate valid until %s", expiryStr)))
					}
				}
			}

			return nil
		},
	}
}

// writeAuthStatusJSON renders auth status as JSON. Sessions is always a list
// (never null) so `.sessions | length` works on a logged-out config too.
func writeAuthStatusJSON(w io.Writer, cfg *config.Config, now time.Time) error {
	status := authStatusJSON{
		LoggedIn: len(cfg.Auth) > 0,
		Sessions: make([]authStatusSession, 0, len(cfg.Auth)),
	}
	for _, auth := range cfg.Auth {
		session := authStatusSession{
			Context:   auth.Name,
			Current:   auth.Name != "" && auth.Name == cfg.CurrentContext,
			Cloud:     authStatusEndpoint(auth),
			CloudGRPC: auth.CloudGRPC,
		}
		if len(auth.Certificates) > 0 {
			cert := auth.Certificates[0]
			session.UserID = cert.UserID
			session.OrganizationID = cert.OrganizationID
			session.PrincipalURI = cert.PrincipalURI
			session.Certificate = authStatusCertInfo(cert.PemCertificate, now)
		}
		status.Sessions = append(status.Sessions, session)
	}

	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding auth status: %w", err)
	}
	fmt.Fprintln(w, string(data))
	return nil
}

// openBrowser opens the given URL in the default browser.
// It is non-blocking: the browser process is detached so callers like
// auth login don't hang. It is a package-level var so tests can replace it.
var openBrowser = browseropen.Open

// authConfigToJSON marshals an auth config for debugging.
func authConfigToJSON(auth *config.AuthConfig) ([]byte, error) {
	return json.MarshalIndent(auth, "", "  ")
}

// matchAuthSelector resolves a user-supplied selector to exactly one session.
// An all-digit selector matches a certificate OrganizationID; otherwise it is a
// case-insensitive substring of the gRPC endpoint or dashboard URL. It errors
// when nothing matches or when more than one session matches.
//
// Matching is purely local: it only ever sees orgs this machine holds a
// certificate for. Asking for an org that exists in the cloud but was never
// logged into here is the common no-match case, so the error says so and points
// at 'wendy auth login' rather than implying the org does not exist.
func matchAuthSelector(cfg *config.Config, selector string) (*config.AuthConfig, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil, fmt.Errorf("empty selector")
	}
	var matches []*config.AuthConfig
	if orgID, err := strconv.Atoi(selector); err == nil {
		for i := range cfg.Auth {
			for _, c := range cfg.Auth[i].Certificates {
				if c.OrganizationID == orgID && c.TenantUUID() == "" {
					matches = append(matches, &cfg.Auth[i])
					break
				}
			}
		}
	} else {
		q := strings.ToLower(selector)
		for i := range cfg.Auth {
			if strings.Contains(strings.ToLower(cfg.Auth[i].CloudGRPC), q) ||
				strings.Contains(strings.ToLower(cfg.Auth[i].CloudDashboard), q) || (len(cfg.Auth[i].Certificates) > 0 && strings.EqualFold(cfg.Auth[i].Certificates[0].TenantUUID(), selector)) {
				matches = append(matches, &cfg.Auth[i])
			}
		}
	}
	switch len(matches) {
	case 0:
		return nil, noSessionMatchError(cfg, selector)
	case 1:
		return matches[0], nil
	default:
		var b strings.Builder
		for _, m := range matches {
			fmt.Fprintf(&b, "\n  - %s", authSessionLabel(m))
		}
		return nil, fmt.Errorf("selector %q matches multiple sessions:%s", selector, b.String())
	}
}

// noSessionMatchError explains a failed selector lookup. `wendy auth use` only
// searches locally stored certificates, so "no match" almost always means "you
// are not logged into that org on this machine" — not that the org is missing
// or the selector is malformed. The error therefore names what IS available and
// gives the exact command that fixes it.
func noSessionMatchError(cfg *config.Config, selector string) error {
	var b strings.Builder
	if orgID, err := strconv.Atoi(selector); err == nil {
		fmt.Fprintf(&b, "not logged in to org %d on this machine", orgID)
	} else {
		fmt.Fprintf(&b, "no auth session matches %q", selector)
	}

	if labels := authSessionLabels(cfg); len(labels) > 0 {
		b.WriteString("\n\nSessions stored here:")
		for _, l := range labels {
			fmt.Fprintf(&b, "\n  - %s", l)
		}
	}

	b.WriteString("\n\n'wendy auth use' only selects between orgs this machine already holds a")
	b.WriteString("\ncertificate for; it does not query the cloud. To add another org, run")
	b.WriteString("\n'wendy auth login' and pick it in the browser — existing sessions are kept.")
	return errors.New(b.String())
}

// authSessionLabels lists every stored session, most useful identifier first.
func authSessionLabels(cfg *config.Config) []string {
	labels := make([]string, 0, len(cfg.Auth))
	for i := range cfg.Auth {
		labels = append(labels, authSessionLabel(&cfg.Auth[i]))
	}
	return labels
}

func newAuthUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use [context]",
		Short: "Switch the current auth context",
		Long:  "Switches the auth context used by cloud and device commands. The argument is a context name (see 'wendy auth status'); an organization ID or an endpoint/dashboard substring is also accepted for the session it names. With no argument in an interactive terminal, a picker is shown.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			if len(cfg.Auth) == 0 {
				return fmt.Errorf("not logged in; run 'wendy auth login' first")
			}

			var chosen *config.AuthConfig
			if len(args) == 1 {
				// A context name is the primary selector; fall back to the legacy
				// org-id / endpoint-substring match so existing scripts keep working.
				if a, ok := cfg.ContextByName(args[0]); ok {
					chosen = a
				} else if chosen, err = matchAuthSelector(cfg, args[0]); err != nil {
					return err
				}
			} else {
				if !isInteractiveTerminal() {
					return fmt.Errorf("provide a context name when not running interactively")
				}
				chosen, err = pickAuthSessionFn(cfg)
				if err != nil {
					return err
				}
			}

			if len(chosen.Certificates) == 0 {
				return fmt.Errorf("auth context %q has no certificates; re-run 'wendy auth login'", chosen.Name)
			}
			cfg.CurrentContext = chosen.Name
			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}
			fmt.Println(tui.SuccessMessage(fmt.Sprintf("Switched to context %q (%s).", chosen.Name, authSessionLabel(chosen))))
			return nil
		},
	}
}

func newAuthRenameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rename [old] <new>",
		Short: "Rename an auth context",
		Long:  "Renames an auth context. With one argument, renames the current context; with two, renames <old> to <new>. Context names are how 'wendy auth use' selects a session.",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			if len(cfg.Auth) == 0 {
				return fmt.Errorf("not logged in; run 'wendy auth login' first")
			}

			var oldName, newName string
			if len(args) == 2 {
				oldName, newName = strings.TrimSpace(args[0]), strings.TrimSpace(args[1])
			} else {
				oldName, newName = cfg.CurrentContext, strings.TrimSpace(args[0])
				if oldName == "" {
					return fmt.Errorf("no current context to rename; pass both the old and new name")
				}
			}
			if newName == "" {
				return fmt.Errorf("new context name must not be empty")
			}
			if newName == oldName {
				return fmt.Errorf("context is already named %q", newName)
			}
			target, ok := cfg.ContextByName(oldName)
			if !ok {
				return fmt.Errorf("no auth context named %q", oldName)
			}
			if _, taken := cfg.ContextByName(newName); taken {
				return fmt.Errorf("a context named %q already exists", newName)
			}
			target.Name = newName
			if cfg.CurrentContext == oldName {
				cfg.CurrentContext = newName
			}
			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}
			fmt.Println(tui.SuccessMessage(fmt.Sprintf("Renamed context %q to %q.", oldName, newName)))
			return nil
		},
	}
}

func newAuthDefaultCmd() *cobra.Command {
	var clear bool
	cmd := &cobra.Command{
		Use:   "default",
		Short: "Show or clear the current auth context",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			if clear {
				cfg.CurrentContext = ""
				if err := config.Save(cfg); err != nil {
					return fmt.Errorf("saving config: %w", err)
				}
				fmt.Println(tui.SuccessMessage("Current context cleared."))
				return nil
			}
			if cfg.CurrentContext == "" {
				fmt.Println("No current context set.")
				return nil
			}
			cur, ok := cfg.ContextByName(cfg.CurrentContext)
			if !ok {
				// The named context's session is gone; self-heal by clearing it.
				fmt.Println(tui.WarningMessage(fmt.Sprintf("Current context %q no longer exists; clearing it.", cfg.CurrentContext)))
				cfg.CurrentContext = ""
				if err := config.Save(cfg); err != nil {
					return fmt.Errorf("saving config: %w", err)
				}
				return nil
			}
			fmt.Printf("Current context: %s (%s)\n", cfg.CurrentContext, authSessionLabel(cur))
			return nil
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false, "Unset the current context")
	return cmd
}
