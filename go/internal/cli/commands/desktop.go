package commands

// The native desktop links this adapter, keeping discovery, authentication,
// certificate verification and cloud transport identical to the CLI.
import (
	"context"
	"errors"
	"fmt"
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
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
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
	// Person is shared by every saved sign-in of one person on one Cloud, so the
	// apps can show one account with its organizations under it. It groups the
	// display only: each organization is still its own context and credentials.
	Person string
	// OrganizationID is the organization of the context's certificate, and
	// OrganizationName its last name seen from Cloud ("" when never seen).
	OrganizationID, OrganizationName string
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
	for _, info := range desktopAccountList(cfg, auth) {
		if info.Context == auth.Name {
			return info, nil
		}
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
	return desktopAccountList(cfg, current), nil
}

func desktopAccountList(cfg *config.Config, current *config.AuthConfig) []DesktopAccountInfo {
	result := make([]DesktopAccountInfo, 0, len(cfg.Auth))
	emails := make([]string, 0, len(cfg.Auth))
	for i, auth := range cfg.Auth {
		info := desktopAccountInfo(auth)
		info.Active = current != nil && current.Name == auth.Name
		if len(auth.Certificates) > 0 {
			info.OrganizationID = auth.OrganizationKey()
			info.OrganizationName = cachedCloudOrganizationName(&cfg.Auth[i])
		}
		result = append(result, info)
		emails = append(emails, info.Email)
	}
	for i, person := range desktopAccountPeople(cfg.Auth, emails) {
		result[i].Person = person
	}
	return result
}

// desktopAccountPeople returns, for each saved context, a key shared by all
// contexts of the same person on the same Cloud. Cloud keeps one sign-in realm
// per organization, so one person's operator ID differs between organizations
// and the email in their tokens is what links them. Contexts are joined when
// they share an operator ID or an email on one Cloud endpoint; a context with
// neither stands alone. The key is display metadata, never an identity proof.
func desktopAccountPeople(auths []config.AuthConfig, emails []string) []string {
	parent := make([]int, len(auths))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	owners := map[string]int{}
	identities := make([][]string, len(auths))
	for i, auth := range auths {
		cloud := strings.ToLower(auth.CloudGRPC)
		if cloud == "" {
			cloud = strings.ToLower(auth.CloudDashboard)
		}
		if email := strings.ToLower(strings.TrimSpace(emails[i])); email != "" {
			identities[i] = append(identities[i], cloud+"|email:"+email)
		}
		if operator := desktopOperatorID(auth); operator != "" {
			identities[i] = append(identities[i], cloud+"|operator:"+operator)
		}
		for _, identity := range identities[i] {
			if j, ok := owners[identity]; ok {
				parent[find(i)] = find(j)
			} else {
				owners[identity] = i
			}
		}
	}
	// Name each group by its smallest identity, so the key is stable across
	// loads and an email (sorting before "operator:") names it when known.
	names := map[int]string{}
	for i := range auths {
		root := find(i)
		for _, identity := range identities[i] {
			if current, ok := names[root]; !ok || identity < current {
				names[root] = identity
			}
		}
	}
	result := make([]string, len(auths))
	for i, auth := range auths {
		if name, ok := names[find(i)]; ok {
			result[i] = name
		} else {
			result[i] = "context:" + auth.Name
		}
	}
	return result
}

// desktopOperatorID is the operator in the context's certificate principal
// (spiffe://wendy.sh/tenant/<tenant>/operator/<operator-id>), or "".
func desktopOperatorID(auth config.AuthConfig) string {
	if len(auth.Certificates) == 0 {
		return ""
	}
	principal := auth.Certificates[0].PrincipalURI
	if principal == "" {
		principal = auth.Certificates[0].CertificatePrincipal()
	}
	identity, err := certs.ParsePrincipal(principal)
	if err != nil || identity.EntityType != certs.EntityUser || !strings.HasSuffix(principal, "/operator/"+identity.EntityID) {
		return ""
	}
	return identity.EntityID
}

// DesktopSignOutResult reports the sign-ins a sign-out removed and the context
// that is current afterwards ("" when none is).
type DesktopSignOutResult struct {
	Removed []string
	Current string
}

// DesktopSignOut removes the named saved sign-ins (auth contexts) from this
// device, like `wendy auth logout <name>`. Names that are already gone are
// skipped. Only local data is deleted: Cloud is not told.
func DesktopSignOut(ctx context.Context, contexts []string) (DesktopSignOutResult, error) {
	if len(contexts) == 0 {
		return DesktopSignOutResult{}, errors.New("choose an account to sign out of")
	}
	return removeAuthContexts(ctx, contexts, false)
}

// DesktopCurrentContext names the current context, or "" when none resolves.
func DesktopCurrentContext() string {
	cfg, err := config.Load()
	if err != nil {
		return ""
	}
	current, err := config.ResolveAuth(cfg, "", nil)
	if err != nil {
		return cfg.CurrentContext
	}
	return current.Name
}

// removeAuthContexts deletes saved sign-ins, the shared path of `wendy auth
// logout` and the apps' sign-out. A nil names removes every context. It holds
// the authentication refresh lock and edits under the config lock, so a token
// or certificate refresh running beside it cannot write a removed sign-in
// back. When the current context is removed, another organization of the same
// person becomes current, then any other sign-in, then none. strict rejects
// names that do not exist; otherwise they are skipped.
func removeAuthContexts(ctx context.Context, names []string, strict bool) (DesktopSignOutResult, error) {
	unlock, err := acquireAuthRefreshLock(ctx)
	if err != nil {
		return DesktopSignOutResult{}, err
	}
	defer unlock()
	var result DesktopSignOutResult
	var discarded []config.AuthConfig
	err = config.Update(func(cfg *config.Config) (bool, error) {
		result, discarded = DesktopSignOutResult{}, nil
		selected := map[string]bool{}
		for _, name := range names {
			if _, ok := cfg.ContextByName(name); !ok && strict {
				return false, fmt.Errorf("no auth context named %q; run 'wendy auth status' to list them", name)
			}
			selected[name] = true
		}
		current, _ := config.ResolveAuth(cfg, "", nil)
		emails := make([]string, len(cfg.Auth))
		for i, auth := range cfg.Auth {
			emails[i] = desktopAccountInfo(auth).Email
		}
		people := desktopAccountPeople(cfg.Auth, emails)
		currentPerson := ""
		var kept, removed []config.AuthConfig
		var keptPeople []string
		for i, auth := range cfg.Auth {
			if current != nil && auth.Name == current.Name {
				currentPerson = people[i]
			}
			if names == nil || selected[auth.Name] {
				removed = append(removed, auth)
				result.Removed = append(result.Removed, auth.Name)
				continue
			}
			kept = append(kept, auth)
			keptPeople = append(keptPeople, people[i])
		}
		if len(removed) == 0 {
			result.Current = cfg.CurrentContext
			return false, nil
		}
		cfg.Auth = kept
		// An unset current context stays unset: choosing one here would silently
		// change which session cloud commands use.
		if _, ok := cfg.ContextByName(cfg.CurrentContext); cfg.CurrentContext != "" && !ok {
			cfg.CurrentContext = nextAuthContext(kept, keptPeople, currentPerson)
		}
		result.Current = cfg.CurrentContext
		discarded = unsharedSecrets(removed, kept)
		return true, nil
	})
	if err != nil {
		return DesktopSignOutResult{}, err
	}
	if len(result.Removed) == 0 {
		if names == nil {
			_ = sessionbroker.InvalidateAll()
		}
		return result, nil
	}
	// Delete the Keychain items only after the config no longer references them:
	// a failed save above leaves both intact for a retry.
	config.DeleteStoredSecrets(&config.Config{Auth: discarded})
	// A prepared session broker keeps an authenticated device transport; signing
	// out must sever it too. Best-effort: the credentials are already gone.
	_ = sessionbroker.InvalidateAll()
	return result, nil
}

// nextAuthContext prefers another selectable organization of the same person,
// then any selectable sign-in, then any sign-in.
func nextAuthContext(kept []config.AuthConfig, people []string, person string) string {
	for _, pass := range []func(int) bool{
		func(i int) bool { return person != "" && people[i] == person && len(kept[i].Certificates) > 0 },
		func(i int) bool { return len(kept[i].Certificates) > 0 },
		func(int) bool { return true },
	} {
		for i := range kept {
			if pass(i) {
				return kept[i].Name
			}
		}
	}
	return ""
}

// unsharedSecrets drops the secret references a remaining sign-in still uses,
// so deleting a removed context's Keychain items can never orphan another's.
func unsharedSecrets(removed, kept []config.AuthConfig) []config.AuthConfig {
	used := map[string]bool{}
	for i := range kept {
		for _, value := range authSecretFields(&kept[i]) {
			if *value != "" {
				used[*value] = true
			}
		}
	}
	out := make([]config.AuthConfig, 0, len(removed))
	for _, auth := range removed {
		auth.Certificates = append([]config.CertificateInfo(nil), auth.Certificates...)
		for _, value := range authSecretFields(&auth) {
			if used[*value] {
				*value = ""
			}
		}
		out = append(out, auth)
	}
	return out
}

func authSecretFields(auth *config.AuthConfig) []*string {
	fields := []*string{&auth.APIKey, &auth.RefreshToken, &auth.DPoPPrivateKey, &auth.LitePrivateKey, &auth.RegistryPrivateKey}
	for i := range auth.Certificates {
		fields = append(fields, &auth.Certificates[i].PemPrivateKey)
	}
	return fields
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
