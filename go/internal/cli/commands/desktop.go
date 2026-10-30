package commands

// The native desktop links this adapter, keeping discovery, authentication,
// certificate verification and cloud transport identical to the CLI.
import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
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
	Lite      bool   `json:"lite"`
}

// InitializeDesktop must run once, before any other desktop operation.
func InitializeDesktop(cliPath string) error {
	if runtime.GOOS == "ios" {
		sessionbroker.EnableEmbeddedHost()
		jsonOutput = true
		return nil
	}
	info, err := os.Stat(cliPath)
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode()&0111 == 0 {
		return errors.New("bundled Wendy CLI is not executable")
	}
	sessionbroker.HelperExecutable = func() (string, error) { return cliPath, nil }
	// Finder launches without the user's shell PATH. Keep explicit entries first,
	// then allow the macOS app to find Homebrew's QEMU and its firmware tools.
	if runtime.GOOS == "darwin" {
		path := os.Getenv("PATH")
		for _, directory := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
			if !strings.Contains(":"+path+":", ":"+directory+":") {
				path += ":" + directory
			}
		}
		if err := os.Setenv("PATH", path); err != nil {
			return err
		}
	}
	jsonOutput = true
	return nil
}

func DesktopNearby(ctx context.Context) ([]DesktopDevice, error) {
	var devices []models.LANDevice
	var lite []models.ExternalDevice
	var err, liteErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		options := cliLANStreamOptions(ctx)
		options.Supplemental = meshDiscoveryStream
		if runtime.GOOS == "ios" {
			options.Supplemental = mobileMeshDiscoveryStream
		}
		devices, err = discovery.CollectLAN(ctx, options, 5*time.Second)
	}()
	go func() { defer wg.Done(); lite, liteErr = (&providers.MicroWendyProvider{}).DiscoverDevices(ctx) }()
	wg.Wait()
	result := make([]DesktopDevice, 0, len(devices))
	for _, d := range devices {
		result = append(result, desktopNearbyDevice(d))
	}
	for _, d := range lite {
		if d.ConnectionInfo["needsInstall"] == "true" {
			continue
		}
		address := d.ConnectionInfo["ip"]
		if d.ConnectionType() == "USB" {
			address = d.ConnectionInfo["serialPort"]
		}
		result = append(result, DesktopDevice{ID: d.ID, Name: d.DisplayName, Selector: d.ID, Transport: d.ConnectionType(), Address: address, Lite: true})
	}
	// The legacy LAN projection cannot represent native Lite management. Merge
	// the current broker directory as canonical mesh selectors for both platforms.
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if snapshot, readErr := sessionbroker.ReadOperatorDirectory(probe); readErr == nil {
		seen := make(map[string]bool, len(result))
		for _, d := range result {
			seen[d.Selector] = true
		}
		for _, d := range DesktopMeshDevices(snapshot) {
			if !seen[d.Selector] {
				result = append(result, d)
				seen[d.Selector] = true
			}
		}
	}
	if len(result) == 0 {
		return mergeDesktopDevices(result), errors.Join(err, liteErr)
	}
	return mergeDesktopDevices(result), nil
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
		result = append(result, DesktopDevice{ID: selector, Name: d.GetName(), Selector: selector, Transport: "Cloud", Address: d.GetIpAddress(), Lite: d.isLite()})
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

func desktopNearbyDevice(d models.LANDevice) DesktopDevice {
	host := strings.TrimSuffix(d.Hostname, ".")
	if host == "" {
		host = d.IPAddress
	}
	transport := "LAN"
	if d.USB != "" && d.IPAddress != "" {
		// Preserve the cable endpoint and its IPv6 zone. Resolving the mDNS
		// hostname again can select Wi-Fi even when discovery found USB.
		host = d.IPAddress
		if ip := net.ParseIP(host); ip != nil && ip.To4() == nil && ip.IsLinkLocalUnicast() && d.NetworkInterface != "" {
			host += "%" + d.NetworkInterface
		}
		if d.NetworkInterface != "" && strings.EqualFold(d.OS, "wendyos") {
			host = discovery.WellKnownUSBAddr + "%" + d.NetworkInterface
		}
		transport = "USB"
	}
	selector := net.JoinHostPort(host, strconv.Itoa(d.Port))
	if d.WendyLite {
		selector = "wendy-lite:" + d.Hostname
	}
	if d.Mesh {
		selector = d.Hostname
		transport = "Mesh"
	}
	name := d.DisplayName
	if name == "" {
		name = d.Hostname
	}
	id := selector
	if d.PrincipalURI != "" {
		id = d.PrincipalURI
	}
	return DesktopDevice{ID: id, Name: name, Selector: selector, Transport: transport, Address: d.IPAddress, Lite: d.WendyLite}
}

// Addresses and transports may change while the enrolled identity stays the same.
func mergeDesktopDevices(devices []DesktopDevice) []DesktopDevice {
	out := make([]DesktopDevice, 0, len(devices))
	indices := map[string]int{}
	priority := func(transport string) int {
		switch transport {
		case "Mesh":
			return 3
		case "USB":
			return 2
		default:
			return 1
		}
	}
	for _, device := range devices {
		if i, ok := indices[device.ID]; ok {
			if priority(device.Transport) > priority(out[i].Transport) {
				out[i] = device
			}
			continue
		}
		indices[device.ID] = len(out)
		out = append(out, device)
	}
	return out
}
