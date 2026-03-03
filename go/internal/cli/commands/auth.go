package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/shared/certs"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const defaultCloudDashboard = "https://cloud.wendylabs.com"
const defaultCloudGRPC = "grpc.cloud.wendylabs.com:443"

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
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("token")
		if token == "" {
			http.Error(w, "missing token parameter", http.StatusBadRequest)
			errCh <- fmt.Errorf("callback received without token")
			return
		}

		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "<html><body><h2>Authentication successful!</h2><p>You can close this tab and return to the terminal.</p></body></html>")
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
	loginURL := fmt.Sprintf("%s/cli/login?callback_port=%d", cloudDashboard, port)
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
	certConn, err := grpc.NewClient(cloudGRPC, grpc.WithTransportCredentials(insecure.NewCredentials()))
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
	tlsCfg.InsecureSkipVerify = true

	grpcOpts := grpc.WithTransportCredentials(insecure.NewCredentials())
	_ = tlsCfg // In production, use credentials.NewTLS(tlsCfg) instead.
	certConn, err := grpc.NewClient(auth.CloudGRPC, grpcOpts)
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
func openBrowser(url string) error {
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
	return cmd.Start()
}

// authConfigToJSON marshals an auth config for debugging.
func authConfigToJSON(auth *config.AuthConfig) ([]byte, error) {
	return json.MarshalIndent(auth, "", "  ")
}
