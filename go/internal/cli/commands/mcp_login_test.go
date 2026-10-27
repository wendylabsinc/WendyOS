package commands

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// auth_login in `wendy mcp serve` starts the production Wendy Cloud sign-in
// that `wendy auth login` runs. It returns the link while the sign-in still
// waits for the browser, and never writes to stdout, the MCP protocol stream.
func TestMCPLoginStarter_ProductionSignInAndCleanStdout(t *testing.T) {
	isolateLoginConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var authURL string
	out := captureStdout(t, func() {
		type started struct {
			session wendymcp.LoginSession
			err     error
		}
		result := make(chan started, 1)
		go func() {
			session, err := mcpLoginStarter(ctx)
			result <- started{session, err}
		}()
		var got started
		select {
		case got = <-result:
		case <-time.After(10 * time.Second):
			t.Error("mcpLoginStarter blocked waiting for the browser")
			cancel()
			got = <-result
		}
		if got.err != nil {
			t.Errorf("mcpLoginStarter: %v", got.err)
			return
		}
		session := got.session
		authURL = session.URL()
		select {
		case <-session.Done():
			t.Errorf("sign-in ended before the browser came back: %v", session.Err())
		default:
		}
		if until := time.Until(session.ExpiresAt()); until < browserLoginTimeout-time.Minute || until > browserLoginTimeout {
			t.Errorf("expires in %s, want about %s", until, browserLoginTimeout)
		}
		cancel()
		select {
		case <-session.Done():
		case <-time.After(5 * time.Second):
			t.Error("session did not end after cancel")
		}
		if !errors.Is(session.Err(), context.Canceled) {
			t.Errorf("Err = %v, want context.Canceled", session.Err())
		}
	})
	if out != "" {
		t.Fatalf("MCP sign-in wrote to stdout: %q", out)
	}
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host != defaultProdAuthBase || u.Path != "/authorize" {
		t.Fatalf("URL = %q, want wendy-auth's production authorize endpoint", authURL)
	}
	q := u.Query()
	if q.Get("client_id") != "wendy-cli" || q.Get("redirect_uri") != oidcRedirectURI ||
		q.Get("resource") != defaultPKIIdentityResource || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize request = %v", q)
	}
	if strings.Contains(authURL, defaultCloudDashboard) {
		t.Fatalf("URL = %q still targets the legacy dashboard", authURL)
	}
}

// The MCP sign-in saves the same session `wendy auth login` would with no
// flags: the production Wendy Cloud through wendy-auth's universal page.
func TestMCPLoginTargetsWhatAuthLoginDoes(t *testing.T) {
	t.Setenv(serviceAccountKeyEnv, "")
	original := performOIDCLoginFn
	t.Cleanup(func() { performOIDCLoginFn = original })
	var got oidcLoginOptions
	performOIDCLoginFn = func(_ context.Context, opts oidcLoginOptions) error {
		got = opts
		return nil
	}
	cmd := newAuthLoginCmd()
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if want := productionOIDCLoginOptions(); got != want {
		t.Fatalf("wendy auth login = %+v, MCP sign-in = %+v", got, want)
	}
	if got.CloudGRPC != defaultProdCloudGRPC {
		t.Fatalf("CloudGRPC = %q, want %q", got.CloudGRPC, defaultProdCloudGRPC)
	}
}

// auth_login is how an agent recovers from AUTH_REQUIRED. When cloud tools
// could not use the current session, the new one becomes current; a usable
// current session stays current, as after `wendy auth login`.
func TestAdoptLoginWhenCurrentUnusable(t *testing.T) {
	now := time.Now()
	session := func(name, grpc string, notAfter time.Time) config.AuthConfig {
		return config.AuthConfig{
			Name:         name,
			CloudGRPC:    grpc,
			Certificates: []config.CertificateInfo{{PemCertificate: certPEM(t, notAfter)}},
		}
	}
	added := session("acme", defaultProdCloudGRPC, now.Add(time.Hour))
	for _, tc := range []struct {
		name    string
		current string
		other   config.AuthConfig
		want    string
	}{
		{name: "usable current session stays", current: "dev", other: session("dev", defaultDevCloudGRPC, now.Add(time.Hour)), want: "dev"},
		{name: "expired current session is replaced", current: "dev", other: session("dev", defaultDevCloudGRPC, now.Add(-time.Hour)), want: "acme"},
		{name: "no current session among several", current: "", other: session("dev", defaultDevCloudGRPC, now.Add(time.Hour)), want: "acme"},
		{name: "current context names a removed session", current: "gone", other: session("dev", defaultDevCloudGRPC, now.Add(time.Hour)), want: "acme"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{CurrentContext: tc.current, Auth: []config.AuthConfig{tc.other, added}}
			adoptLoginWhenCurrentUnusable(cfg, added)
			if cfg.CurrentContext != tc.want {
				t.Fatalf("current context = %q, want %q", cfg.CurrentContext, tc.want)
			}
		})
	}
}
