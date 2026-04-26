package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/shared/certs"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const defaultCloudDashboard = "https://cloud.wendy.sh"
const defaultCloudGRPC = "wendy-cloud-services-114319063177.us-central1.run.app:443"

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage authentication with Wendy Cloud",
	}

	cmd.AddCommand(
		newAuthLoginCmd(),
		newAuthLogoutCmd(),
		newAuthRefreshCertsCmd(),
	)

	return cmd
}

func newAuthLoginCmd() *cobra.Command {
	var cloudDashboard string
	var cloudGRPC string

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to Wendy Cloud",
		Long:  "Opens a browser for authentication, receives a callback with an enrollment token, generates certificates, and saves them to config.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if cloudDashboard == "" {
				cloudDashboard = defaultCloudDashboard
			}
			if cloudGRPC == "" {
				cloudGRPC = defaultCloudGRPC
			}

			return performLogin(cmd.Context(), cloudDashboard, cloudGRPC)
		},
	}

	cmd.Flags().StringVar(&cloudDashboard, "cloud", "", "Cloud dashboard URL")
	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint")
	return cmd
}

func performLogin(ctx context.Context, cloudDashboard, cloudGRPC string) error {
	// Step 1: Start a local HTTP server to receive the OAuth callback.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("starting local callback server: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	// Channel to receive the enrollment token from the callback.
	tokenCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/cli-callback", func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if token == "" {
			http.Error(w, "missing token parameter", http.StatusBadRequest)
			errCh <- fmt.Errorf("callback received without token")
			return
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
		tokenCh <- token
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
	fmt.Printf("Opening browser for authentication: %s\n", loginURL)

	if err := openBrowser(loginURL); err != nil {
		fmt.Printf("Could not open browser automatically. Please visit:\n  %s\n", loginURL)
	}

	fmt.Println("Waiting for authentication...")

	// Wait for the token.
	var enrollmentToken string
	select {
	case enrollmentToken = <-tokenCh:
		fmt.Println("Received enrollment token.")
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

	csrPEM, err := certs.GenerateCSR(privateKeyPEM, "wendy-cli-user")
	if err != nil {
		return fmt.Errorf("generating CSR: %w", err)
	}

	// Step 4: Issue certificate via cloud CertificateService.
	var transportCreds grpc.DialOption
	if strings.HasSuffix(cloudGRPC, ":443") {
		transportCreds = grpc.WithTransportCredentials(credentials.NewTLS(nil))
	} else {
		transportCreds = grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	certConn, err := grpc.NewClient(cloudGRPC, transportCreds)
	if err != nil {
		return fmt.Errorf("connecting to cloud: %w", err)
	}
	defer certConn.Close()

	certClient := cloudpb.NewCertificateServiceClient(certConn)
	issueResp, err := certClient.IssueCertificate(ctx, &cloudpb.IssueCertificateRequest{
		PemCsr:          csrPEM,
		EnrollmentToken: enrollmentToken,
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
		Certificates:   []config.CertificateInfo{certInfo},
	}

	cfg.AddAuth(authEntry)
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}

	fmt.Println("Authentication successful. Certificates saved.")

	if len(issueResp.GetWarnings()) > 0 {
		fmt.Println("Warnings:")
		for _, w := range issueResp.GetWarnings() {
			fmt.Printf("  - %s\n", w)
		}
	}

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

			cfg.Auth = nil
			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}

			fmt.Println("Logged out. All authentication credentials removed.")
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

			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}

			if len(cfg.Auth) == 0 {
				return fmt.Errorf("not logged in; run 'wendy auth login' first")
			}

			// Refresh certificates for each auth entry.
			for i, auth := range cfg.Auth {
				if len(auth.Certificates) == 0 {
					fmt.Printf("Skipping %s: no certificates to refresh\n", auth.CloudDashboard)
					continue
				}

				fmt.Printf("Refreshing certificates for %s...\n", auth.CloudDashboard)

				if err := refreshCertsForAuth(ctx, &cfg.Auth[i]); err != nil {
					fmt.Printf("Failed to refresh for %s: %v\n", auth.CloudDashboard, err)
					continue
				}

				fmt.Printf("Certificates refreshed for %s.\n", auth.CloudDashboard)
			}

			if err := config.Save(cfg); err != nil {
				return fmt.Errorf("saving config: %w", err)
			}

			return nil
		},
	}
}

// refreshCertsForAuth generates a new CSR and refreshes certificates for a single auth entry.
func refreshCertsForAuth(ctx context.Context, auth *config.AuthConfig) error {
	if len(auth.Certificates) == 0 {
		return fmt.Errorf("no existing certificates")
	}

	existingCert := auth.Certificates[0]

	// Generate new key pair.
	newKeyPEM, err := certs.GenerateKeyPair()
	if err != nil {
		return fmt.Errorf("generating key pair: %w", err)
	}

	csrPEM, err := certs.GenerateCSR(newKeyPEM, "wendy-cli-user")
	if err != nil {
		return fmt.Errorf("generating CSR: %w", err)
	}

	// Connect to cloud using existing mTLS credentials.
	tlsCfg, err := certs.LoadTLSConfig(
		existingCert.PemCertificate,
		existingCert.PemCertificateChain,
		existingCert.PemPrivateKey,
		"",
	)
	if err != nil {
		return fmt.Errorf("loading existing TLS config: %w", err)
	}
	var refreshTransportCreds grpc.DialOption
	if strings.HasSuffix(auth.CloudGRPC, ":443") {
		refreshTransportCreds = grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))
	} else {
		refreshTransportCreds = grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	certConn, err := grpc.NewClient(auth.CloudGRPC, refreshTransportCreds)
	if err != nil {
		return fmt.Errorf("connecting to cloud: %w", err)
	}
	defer certConn.Close()

	certClient := cloudpb.NewCertificateServiceClient(certConn)

	// Use RefreshCertificate RPC.
	refreshResp, err := certClient.RefreshCertificate(ctx, &cloudpb.RefreshCertificateRequest{
		PemCsr: csrPEM,
	})
	if err != nil {
		return fmt.Errorf("refreshing certificate: %w", err)
	}

	cert := refreshResp.GetCertificate()
	if cert == nil {
		return fmt.Errorf("no certificate returned from refresh")
	}

	// Update the auth entry with new certificates.
	auth.Certificates = []config.CertificateInfo{
		{
			PemCertificate:      cert.GetPemCertificate(),
			PemCertificateChain: cert.GetPemCertificateChain(),
			PemPrivateKey:       newKeyPEM,
			OrganizationID:      existingCert.OrganizationID,
			UserID:              existingCert.UserID,
		},
	}

	return nil
}

// openBrowser opens the given URL in the default browser.
// It is non-blocking: the browser process is detached so callers like
// auth login don't hang. It is a package-level var so tests can replace it.
var openBrowser = func(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return fmt.Errorf("unsupported platform")
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}
	return nil
}

// authConfigToJSON marshals an auth config for debugging.
func authConfigToJSON(auth *config.AuthConfig) ([]byte, error) {
	return json.MarshalIndent(auth, "", "  ")
}
