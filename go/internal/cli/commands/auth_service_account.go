package commands

// Headless login for a wendy-auth service account (WDY-3421). The CLI holds
// the account's private key and mints a DPoP-bound access token with an
// RFC 7523 jwt-bearer assertion. There is no refresh token: an expired token
// is replaced by minting a new assertion.

import (
	"bytes"
	"context"
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

const jwtBearerGrantType = "urn:ietf:params:oauth:grant-type:jwt-bearer"

// serviceAccountKeyEnv holds the key file's contents for CI, where a secret
// variable is easier to provide than a file.
const serviceAccountKeyEnv = "WENDY_SERVICE_ACCOUNT_KEY"

// serviceAccountKey is the key file written by `wendy auth service-account
// enroll` and read by `wendy auth login --service-account`.
type serviceAccountKey struct {
	Issuer     string `json:"issuer"`
	Subject    string `json:"sub"`
	PrivateKey string `json:"private_key"`
}

func parseServiceAccountKey(data []byte) (*serviceAccountKey, crypto.Signer, error) {
	var key serviceAccountKey
	if err := json.Unmarshal(data, &key); err != nil {
		return nil, nil, fmt.Errorf("parsing service-account key file: %w", err)
	}
	if key.Issuer == "" || key.Subject == "" || key.PrivateKey == "" {
		return nil, nil, fmt.Errorf("service-account key file must contain issuer, sub and private_key")
	}
	signer, err := certs.ParseSigningPrivateKeyPEM([]byte(key.PrivateKey))
	if err != nil {
		return nil, nil, fmt.Errorf("parsing service-account private key: %w", err)
	}
	return &key, signer, nil
}

// mintServiceAccountToken exchanges a fresh assertion for an access token at
// the realm's token endpoint. The assertion signature is the proof of
// possession, so the request carries no DPoP proof; wendy-auth binds the token
// to the registered key named by kid.
func mintServiceAccountToken(ctx context.Context, key crypto.Signer, issuer, subject, resource string) (*oidcTokenResponse, error) {
	issuer = strings.TrimSuffix(issuer, "/")
	meta, err := discoverOIDC(ctx, issuer)
	if err != nil {
		return nil, err
	}
	_, alg, err := operatorPublicJWK(key)
	if err != nil {
		return nil, err
	}
	kid, err := operatorJWKThumbprint(key)
	if err != nil {
		return nil, err
	}
	jti, err := randomURLSafe(16)
	if err != nil {
		return nil, fmt.Errorf("generating assertion jti: %w", err)
	}
	now := time.Now()
	assertion, err := cloudrequest.SignJWS(key,
		map[string]any{"alg": alg, "kid": kid},
		map[string]any{
			"iss": subject, "sub": subject, "aud": meta.TokenEndpoint, "jti": jti,
			"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
		})
	if err != nil {
		return nil, fmt.Errorf("signing service-account assertion: %w", err)
	}
	form := url.Values{}
	form.Set("grant_type", jwtBearerGrantType)
	form.Set("assertion", assertion)
	if resource != "" {
		form.Set("resource", resource)
	}
	token, err := postForToken(ctx, meta.TokenEndpoint, "application/x-www-form-urlencoded", []byte(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("minting service-account token: %w", err)
	}
	if err := checkServiceAccountToken(token, kid, issuer, resource); err != nil {
		return nil, err
	}
	return token, nil
}

func postForToken(ctx context.Context, endpoint, contentType string, body []byte) (*oidcTokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return parseTokenResponse(resp)
}

// checkServiceAccountToken refuses a token not bound to our key, from another
// realm, or for another audience, before it is stored.
func checkServiceAccountToken(token *oidcTokenResponse, kid, issuer, resource string) error {
	claims, err := decodeJWTClaims(token.AccessToken)
	if err != nil {
		return fmt.Errorf("inspecting service-account access token: %w", err)
	}
	if confirmationThumbprint(claims) != kid {
		return fmt.Errorf("service-account access token is not bound to this key")
	}
	if iss, _ := claims["iss"].(string); iss != issuer {
		return fmt.Errorf("service-account access token issuer %q does not match %q", iss, issuer)
	}
	if resource != "" && !audienceContains(claims["aud"], resource) {
		return fmt.Errorf("service-account access token audience does not include %s", resource)
	}
	return nil
}

type serviceAccountLoginOptions struct {
	KeyFile   []byte
	CloudURL  string
	CloudGRPC string
	Resource  string
}

// performServiceAccountLogin stores a DPoP-bound session for the service
// account. It never prompts or opens a browser. No operator certificate is
// issued: the session authenticates with its token alone.
func performServiceAccountLogin(ctx context.Context, opts serviceAccountLoginOptions) error {
	key, signer, err := parseServiceAccountKey(opts.KeyFile)
	if err != nil {
		return err
	}
	issuer := strings.TrimSuffix(key.Issuer, "/")
	token, err := mintServiceAccountToken(ctx, signer, issuer, key.Subject, opts.Resource)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	cfg.AddAuth(config.AuthConfig{
		CloudDashboard: opts.CloudURL,
		CloudGRPC:      opts.CloudGRPC,
		APIKey:         token.AccessToken,
		OAuthIssuer:    issuer,
		OAuthResource:  opts.Resource,
		OAuthExpiresAt: tokenExpiry(token),
		DPoPPrivateKey: key.PrivateKey,
		ServiceAccount: key.Subject,
	})
	cfg.EnsureContexts()
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("saving service-account session: %w", err)
	}
	fmt.Println(tui.SuccessMessage(fmt.Sprintf("Signed in to %s as service account %s.", issuerRealm(issuer), key.Subject)))
	return nil
}

func tokenExpiry(token *oidcTokenResponse) string {
	return time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
}

// remintServiceAccountSession replaces the session's access token with a newly
// minted one and persists it. Callers hold the auth refresh lock.
func remintServiceAccountSession(ctx context.Context, auth *config.AuthConfig) error {
	keyPEM, err := auth.OAuthDPoPKey()
	if err != nil {
		return fmt.Errorf("loading service-account key: %w", err)
	}
	signer, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	if err != nil {
		return fmt.Errorf("parsing service-account key: %w", err)
	}
	token, err := mintServiceAccountToken(ctx, signer, auth.OAuthIssuer, auth.ServiceAccount, auth.OAuthResource)
	if err != nil {
		return err
	}
	auth.APIKey = token.AccessToken
	auth.OAuthExpiresAt = tokenExpiry(token)
	return persistOAuthSession(auth)
}

// enrollServiceAccount registers a new key for the service account named by a
// one-time enrollment secret and writes the key file to outPath (0600).
func enrollServiceAccount(ctx context.Context, issuer, secret, outPath string) (err error) {
	issuer = strings.TrimSuffix(issuer, "/")
	// Claim the path before spending the single-use secret.
	f, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating key file: %w", err)
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(outPath)
		}
	}()

	privateKeyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		return fmt.Errorf("generating service-account key: %w", err)
	}
	signer, err := certs.ParseSigningPrivateKeyPEM([]byte(privateKeyPEM))
	if err != nil {
		return fmt.Errorf("parsing generated key: %w", err)
	}
	jwk, _, err := operatorPublicJWK(signer)
	if err != nil {
		return err
	}
	kid, err := operatorJWKThumbprint(signer)
	if err != nil {
		return err
	}
	enrollURL := issuer + "/service-accounts/enroll"
	pop, err := newDPoPProof(signer, http.MethodPost, enrollURL, "")
	if err != nil {
		return fmt.Errorf("building enrollment proof: %w", err)
	}
	body, err := json.Marshal(map[string]any{"enrollment_secret": secret, "public_jwk": jwk, "pop": pop})
	if err != nil {
		return err
	}
	token, err := postForToken(ctx, enrollURL, "application/json", body)
	if err != nil {
		return fmt.Errorf("enrolling service account: %w", err)
	}
	// The enrollment response names neither the account nor the realm; both
	// come from the access token it returns.
	if err := checkServiceAccountToken(token, kid, issuer, ""); err != nil {
		return err
	}
	claims, err := decodeJWTClaims(token.AccessToken)
	if err != nil {
		return err
	}
	subject, _ := claims["sub"].(string)
	if subject == "" {
		return fmt.Errorf("enrollment access token carries no sub claim")
	}
	data, err := json.MarshalIndent(serviceAccountKey{Issuer: issuer, Subject: subject, PrivateKey: privateKeyPEM}, "", "  ")
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("writing key file: %w", err)
	}
	fmt.Println(tui.SuccessMessage(fmt.Sprintf("Enrolled service account %s. Key saved to %s.", subject, outPath)))
	fmt.Printf("Log in with: wendy auth login --service-account %s\n", outPath)
	return nil
}

// readServiceAccountKey returns the key file named by --service-account, or the
// contents of WENDY_SERVICE_ACCOUNT_KEY when the flag is empty. nil means
// neither was given.
func readServiceAccountKey(path string) ([]byte, error) {
	if path == "" {
		if v := os.Getenv(serviceAccountKeyEnv); v != "" {
			return []byte(v), nil
		}
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading service-account key file: %w", err)
	}
	return data, nil
}

func newAuthServiceAccountCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service-account",
		Short: "Manage wendy-auth service-account keys for headless login",
	}
	var issuer, out string
	enroll := &cobra.Command{
		Use:   "enroll <one-time-secret>",
		Short: "Register a new key for a service account and save it as a key file",
		Long: "Generates an ML-DSA-65 key, registers it with wendy-auth using the one-time enrollment secret a tenant admin minted for the service account, and writes the key file (mode 0600).\n" +
			"Log in with it using 'wendy auth login --service-account <key-file>', or put the file's contents in " + serviceAccountKeyEnv + ".",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if issuer == "" {
				return errors.New("--issuer is required (the service account's realm, e.g. https://auth.dev.wendy.sh/realms/acme)")
			}
			return enrollServiceAccount(cmd.Context(), issuer, args[0], out)
		},
	}
	enroll.Flags().StringVar(&issuer, "issuer", "", "wendy-auth realm issuer URL of the service account")
	enroll.Flags().StringVarP(&out, "output", "o", "wendy-service-account.json", "path of the key file to write (must not exist)")
	cmd.AddCommand(enroll)
	return cmd
}
