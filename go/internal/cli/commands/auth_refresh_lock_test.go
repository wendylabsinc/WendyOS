package commands

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func rotatingOAuthSession(t *testing.T) (*config.AuthConfig, *atomic.Int32) {
	t.Helper()
	t.Setenv("WENDY_SECRET_STORE", "file")
	keyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := operatorJWKThumbprint(key)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	calls := new(atomic.Int32)
	var issuer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/.well-known/openid-configuration" {
			_ = json.NewEncoder(w).Encode(oidcProviderMetadata{
				Issuer: issuer, AuthorizationEndpoint: issuer + "/authorize", TokenEndpoint: issuer + "/token",
				JWKSURI: issuer + "/jwks",
			})
			return
		}
		if r.URL.Path == "/jwks" {
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []oidcJWK{testES256JWK(authority, "authority")}})
			return
		}
		if r.URL.Path != "/token" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		if calls.Add(1) != 1 || r.Form.Get("refresh_token") != "refresh-1" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		if r.Header.Get("DPoP") == "" {
			t.Error("refresh omitted DPoP proof")
		}
		claims := map[string]any{
			"iss": issuer, "aud": "https://cloud.example/api", "sub": "user-1",
			"exp": time.Now().Add(time.Hour).Unix(), "cnf": map[string]string{"jkt": thumbprint},
		}
		_ = json.NewEncoder(w).Encode(oidcTokenResponse{
			AccessToken: testES256Token(t, authority, "authority", claims), RefreshToken: "refresh-2", ExpiresIn: 3600, TokenType: "DPoP",
		})
	}))
	issuer = server.URL
	t.Cleanup(server.Close)
	auth := &config.AuthConfig{
		CloudDashboard: "https://cloud.example", CloudGRPC: "cloud.example:443",
		OAuthIssuer: issuer, OAuthClientID: "wendy-cli", OAuthResource: "https://cloud.example/api",
		OAuthExpiresAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
		APIKey:         "expired-api-token", RefreshToken: "refresh-1", DPoPPrivateKey: keyPEM,
	}
	seedConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
	return auth, calls
}

func checkRefreshedCloudContext(ctx context.Context, auth *config.AuthConfig) error {
	// The refreshed token now reaches an RPC through the DPoP token provider
	// (a cnf-bound token is never sent as Bearer, WDY-3107). The provider drives
	// the refresh under the shared lock and returns the refreshed access token.
	token, _, err := cliDPoPTokenProvider(auth)(ctx)
	if err != nil {
		return err
	}
	if len(strings.Split(token, ".")) != 3 || token == "expired-api-token" {
		return fmt.Errorf("provider did not return the refreshed access token: %q", token)
	}
	return nil
}

func TestOAuthRefreshConcurrentCloudContexts(t *testing.T) {
	auth, calls := rotatingOAuthSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const workers = 6
	results := make(chan error, workers)
	for range workers {
		go func() { results <- checkRefreshedCloudContext(ctx, auth) }()
	}
	for range workers {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh token consumed %d times, want 1", calls.Load())
	}
	if auth.APIKey != "expired-api-token" {
		t.Fatal("cloudContext mutated the shared auth entry")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth[0].RefreshToken != "refresh-2" {
		t.Fatal("rotated refresh token was not persisted")
	}
}

// Each child loads the stale token before telling the parent it is ready.
// The parent then starts both calls, so this exercises an actual filesystem
// lock and reload across processes, not just goroutine synchronization.
func TestOAuthRefreshProcessHelper(t *testing.T) {
	if os.Getenv("WENDY_OAUTH_REFRESH_TEST_CHILD") != "1" {
		return
	}
	// TestMain isolates HOME for every test process. Restore the shared test
	// config directory explicitly after that initialization.
	t.Setenv("HOME", os.Getenv("WENDY_OAUTH_REFRESH_TEST_HOME"))
	cfg, err := config.Load()
	if err != nil || len(cfg.Auth) != 1 {
		t.Fatalf("loading child auth: %v", err)
	}
	fmt.Println("ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := checkRefreshedCloudContext(ctx, &cfg.Auth[0]); err != nil {
		t.Fatal(err)
	}
}

func TestOAuthRefreshAcrossProcesses(t *testing.T) {
	_, calls := rotatingOAuthSession(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	type child struct {
		cmd    *exec.Cmd
		stdin  io.WriteCloser
		stdout *bufio.Scanner
	}
	var children []child
	for range 2 {
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestOAuthRefreshProcessHelper$")
		cmd.Env = append(os.Environ(), "WENDY_OAUTH_REFRESH_TEST_CHILD=1", "WENDY_OAUTH_REFRESH_TEST_HOME="+os.Getenv("HOME"))
		cmd.Stderr = os.Stderr
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(stdout)
		if !scanner.Scan() || scanner.Text() != "ready" {
			var output strings.Builder
			fmt.Fprintln(&output, scanner.Text())
			for scanner.Scan() {
				fmt.Fprintln(&output, scanner.Text())
			}
			_ = cmd.Wait()
			t.Fatalf("child failed before barrier: %s (%v)", output.String(), scanner.Err())
		}
		children = append(children, child{cmd, stdin, scanner})
	}
	for _, c := range children {
		_, _ = io.WriteString(c.stdin, "continue\n")
		_ = c.stdin.Close()
	}
	for _, c := range children {
		var output strings.Builder
		for c.stdout.Scan() {
			fmt.Fprintln(&output, c.stdout.Text())
		}
		if err := c.cmd.Wait(); err != nil {
			t.Errorf("concurrent CLI refresh failed: %v\n%s", err, output.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("processes consumed refresh token %d times, want 1", calls.Load())
	}
}

func TestOAuthRefreshLockCancellation(t *testing.T) {
	auth, calls := rotatingOAuthSession(t)
	unlock, err := acquireAuthRefreshLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := ensureOAuthAccessToken(ctx, auth); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected cancellable lock wait, got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("cancelled waiter contacted token endpoint")
	}
	certCtx, cancelCert := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelCert()
	if err := refreshAllCerts(certCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("certificate refresh must share the token-family lock: %v", err)
	}
}

func TestOAuthRefreshRejectsRemovedSession(t *testing.T) {
	auth, calls := rotatingOAuthSession(t)
	if err := config.Save(&config.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := ensureOAuthAccessToken(context.Background(), auth); err == nil || !strings.Contains(err.Error(), "no longer present") {
		t.Fatalf("missing session error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("removed session attempted a refresh")
	}
}

func TestOAuthRefreshPersistsOnlySelectedSession(t *testing.T) {
	for _, different := range []string{"dashboard", "tenant", "issuer"} {
		t.Run(different, func(t *testing.T) {
			auth, calls := rotatingOAuthSession(t)
			other := *auth
			switch different {
			case "dashboard":
				other.CloudDashboard = "https://other.example"
			case "tenant":
				other.Certificates = []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/operator/test"}}
			case "issuer":
				other.OAuthIssuer = "https://other.example/realm"
			}
			if err := config.Save(&config.Config{Auth: []config.AuthConfig{other, *auth}}); err != nil {
				t.Fatal(err)
			}
			if err := ensureOAuthAccessToken(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || cfg.Auth[0].RefreshToken != "refresh-1" || cfg.Auth[1].RefreshToken != "refresh-2" {
				t.Fatal("refresh changed another session or lost the selected session's token")
			}
		})
	}
}
