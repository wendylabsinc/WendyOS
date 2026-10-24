package commands

// The native desktop links this adapter, keeping discovery, authentication,
// certificate verification and cloud transport identical to the CLI.
import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
)

// DesktopPinRecovery returns the exact target of the CLI's unpin advice. Never
// infer a trust-reset action from an arbitrary connection error's text.
func DesktopPinRecovery(err error) string {
	var refusal *deviceIdentityRefusalError
	if !errors.As(err, &refusal) {
		return ""
	}
	if refusal.diagnostic != nil {
		return refusal.diagnostic.hostname
	}
	var mismatch *devicepin.PinMismatchError
	if errors.As(err, &mismatch) {
		return mismatch.Key
	}
	return ""
}

// DesktopUnpin uses the same store clearing and broker invalidation as the CLI.
func DesktopUnpin(target string) error {
	if strings.TrimSpace(target) == "" {
		return errors.New("missing saved identity target")
	}
	cmd := newDeviceUnpinCmd()
	cmd.SetOut(io.Discard)
	return cmd.RunE(cmd, []string{target})
}

type DesktopDevice struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Selector  string `json:"selector"`
	Transport string `json:"transport"`
	Address   string `json:"address"`
}

// InitializeDesktop must run once, before any other desktop operation.
func InitializeDesktop(cliPath string) error {
	info, err := os.Stat(cliPath)
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode()&0111 == 0 {
		return errors.New("bundled Wendy CLI is not executable")
	}
	meshWorkerExecutable = func() (string, error) { return cliPath, nil }
	jsonOutput = true
	return nil
}

func DesktopNearby(ctx context.Context) ([]DesktopDevice, error) {
	devices, err := discovery.CollectLAN(ctx, cliLANStreamOptions(ctx), 5*time.Second)
	result := make([]DesktopDevice, 0, len(devices))
	for _, d := range devices {
		host := strings.TrimSuffix(d.Hostname, ".")
		if host == "" {
			host = d.IPAddress
		}
		selector := net.JoinHostPort(host, strconv.Itoa(d.Port))
		transport := "LAN"
		if d.Mesh {
			selector = d.Hostname
			transport = "Mesh"
		}
		name := d.DisplayName
		if name == "" {
			name = d.Hostname
		}
		result = append(result, DesktopDevice{ID: selector, Name: name, Selector: selector, Transport: transport, Address: d.IPAddress})
	}
	return result, err
}

func DesktopCloud(ctx context.Context) ([]DesktopDevice, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	auth, err := config.ResolveAuth(cfg, "", nil)
	if err != nil {
		return nil, err
	}
	devices, err := fetchCloudDiscoveryDevices(ctx, auth, true)
	if err != nil {
		return nil, err
	}
	result := make([]DesktopDevice, 0, len(devices))
	for _, d := range devices {
		selector, err := cloudDiscoveryDeviceDefault(auth, d)
		if err != nil {
			return nil, err
		}
		result = append(result, DesktopDevice{ID: selector, Name: d.GetName(), Selector: selector, Transport: "Cloud", Address: d.GetIpAddress()})
	}
	return result, nil
}

func DesktopConnect(ctx context.Context, selector string) (*grpcclient.AgentConnection, error) {
	if strings.TrimSpace(selector) == "" {
		return nil, errors.New("choose a device")
	}
	return connectMCPDevice(ctx, selector)
}

type desktopLoginURLKey struct{}

// DesktopLogin uses the CLI's OIDC/PKCE login, reporting the browser URL to UI.
func DesktopLogin(ctx context.Context, development bool, openURL func(string)) error {
	ctx = context.WithValue(ctx, desktopLoginURLKey{}, openURL)
	cmd := newAuthLoginCmd()
	args := []string{}
	if development {
		args = append(args, "--development")
	}
	cmd.SetArgs(args)
	return cmd.ExecuteContext(ctx)
}

// DesktopAccount exposes display metadata only, never credentials.
type DesktopAccountInfo struct {
	State                 uint32 // 0 signed out, 1 signed in, 2 sign-in required
	Context, Cloud, Email string
	Active                bool
}

func DesktopAccount() (DesktopAccountInfo, error) {
	cfg, err := config.Load()
	if err != nil {
		return DesktopAccountInfo{}, err
	}
	if len(cfg.Auth) == 0 {
		return DesktopAccountInfo{}, nil
	}
	auth, err := config.ResolveAuth(cfg, "", nil)
	if err != nil {
		return DesktopAccountInfo{}, err
	}
	info := desktopAccountInfo(*auth)
	info.Active = true
	return info, nil
}

func DesktopAccounts() ([]DesktopAccountInfo, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	current, _ := config.ResolveAuth(cfg, "", nil)
	result := make([]DesktopAccountInfo, 0, len(cfg.Auth))
	for _, auth := range cfg.Auth {
		info := desktopAccountInfo(auth)
		info.Active = current != nil && current.Name == auth.Name
		result = append(result, info)
	}
	return result, nil
}

func desktopAccountInfo(auth config.AuthConfig) DesktopAccountInfo {
	state := uint32(1)
	if auth.APIKey == "" && auth.RefreshToken == "" && auth.ServiceAccount == "" && len(auth.Certificates) == 0 {
		state = 0
	}
	if expiry, err := time.Parse(time.RFC3339, auth.OAuthExpiresAt); err == nil && time.Now().After(expiry) && auth.RefreshToken == "" && auth.ServiceAccount == "" {
		state = 2
	}
	email := ""
	// Display metadata only. This decoding never establishes identity or authorizes a connection.
	if token, err := auth.BearerToken(); err == nil {
		if claims, err := decodeJWTClaims(token); err == nil {
			email, _ = claims["email"].(string)
		}
	}
	return DesktopAccountInfo{State: state, Context: auth.Name, Cloud: authStatusEndpoint(auth), Email: email}
}
