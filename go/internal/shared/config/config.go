// Package config manages the CLI configuration stored at ~/.wendy/config.json.
package config

import (
	"encoding/json"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"os"
	"path/filepath"
)

// Config represents the top-level CLI configuration.
type Config struct {
	Auth          []AuthConfig     `json:"auth,omitempty"`
	Analytics     *AnalyticsConfig `json:"analytics,omitempty"`
	DefaultDevice string           `json:"defaultDevice,omitempty"`
	// DefaultBuildHost is the device `wendy run` delegates the image
	// build to when --build-host is not passed. Per-developer rather than
	// per-project: the right build host depends on which network the developer is
	// sitting on, not on the repository.
	DefaultBuildHost string `json:"defaultBuildHost,omitempty"`
	// DefaultCloudGRPC is a pre-context auth-selection field, read once by
	// ensureContexts to seed CurrentContext during migration and no longer
	// written (see CurrentContext).
	DefaultCloudGRPC   string `json:"defaultCloudGRPC,omitempty"`
	LastCLIUpdateCheck string `json:"lastCLIUpdateCheck,omitempty"` // RFC3339
	AvailableCLIUpdate string `json:"availableCLIUpdate,omitempty"` // tag of a newer release, if any
	// LastMCPSetupVersion records the CLI version that last ran `wendy mcp
	// setup`. It lets the root command detect when an upgrade should refresh
	// the MCP server config and bundled skills. Empty means the user has never
	// run setup, so auto-refresh stays off.
	LastMCPSetupVersion string `json:"lastMCPSetupVersion,omitempty"`
	// CompletionInstalled is set once shell completions have been installed
	// through the CLI (via `wendy completion install` or an accepted prompt).
	// While false, the CLI may offer to install completions.
	CompletionInstalled bool `json:"completionInstalled,omitempty"`
	// CompletionPromptDismissed is set when the user declines the ambient
	// "install completions?" prompt with "n". Once true, that prompt never
	// reappears.
	CompletionPromptDismissed bool `json:"completionPromptDismissed,omitempty"`
	// LastCompletionPromptCheck records when the ambient completion prompt was
	// last shown (RFC3339). It throttles the prompt so an unanswered prompt
	// (e.g. Ctrl-C) doesn't reappear on every invocation.
	LastCompletionPromptCheck string `json:"lastCompletionPromptCheck,omitempty"`
	// OptimizeTipShownAt throttles the `wendy project optimize` tip to once per
	// day per project. Keyed by the project directory, value is an RFC3339 date
	// (YYYY-MM-DD) of the last time the tip (or a build-time optimize scan) was
	// surfaced for that project.
	OptimizeTipShownAt map[string]string `json:"optimizeTipShownAt,omitempty"`
	// ImplicitDeviceHintShownAt throttles the follow-up hint that explains how to
	// override an implicitly chosen device, to once per day. The line naming the
	// device is always shown; only the explanation is rate-limited. Value is a
	// date (YYYY-MM-DD).
	ImplicitDeviceHintShownAt string `json:"implicitDeviceHintShownAt,omitempty"`
	// DevicePins binds a device hostname to the organisation + cloud host its
	// TLS identity must belong to (WDY-1149), so a different trust domain
	// answering at that hostname is caught. Renewal/re-enrollment within the
	// same org+cloud does not trip it. Keyed by normalized hostname.
	DevicePins map[string]DevicePin `json:"devicePins,omitempty"`
	// CrashReport holds opt-in crash-reporting state: the suppression flag,
	// tracking ids awaiting a fix, the last status-poll time, and pending
	// fix notices to surface on the next run. Nil until first used.
	CrashReport *CrashReportConfig `json:"crashReport,omitempty"`
	// CurrentContext names the active auth context (an AuthConfig.Name). It is the
	// single selector for which session cloud/device commands use; `wendy auth
	// use <context>` writes it. Empty with several contexts means "unset" — the
	// resolver then shows a picker or errors.
	CurrentContext string `json:"currentContext,omitempty"`
	// DefaultOrgID is the remembered device-enroll target organization (a
	// separate axis from auth-session selection, chosen from the cloud's full
	// org list — see org_picker.go). It is also read once by ensureContexts to
	// seed CurrentContext during migration.
	DefaultOrgID int32 `json:"defaultOrgId,omitempty"`
	// DefaultTenantUUID selects a PKI organization on DefaultCloudGRPC.
	DefaultTenantUUID string `json:"defaultTenantUUID,omitempty"`
}

// AuthConfig holds authentication details for a cloud environment.
type AuthConfig struct {
	// Name is the human context name (`wendy auth use <Name>`). Assigned by
	// ensureContexts: the first login is "default"; others get a derived,
	// unique name. Renamable via `wendy auth rename`.
	Name           string            `json:"name,omitempty"`
	CloudDashboard string            `json:"cloudDashboard"`
	CloudGRPC      string            `json:"cloudGRPC"`
	APIKey         string            `json:"apiKey,omitempty"`
	OAuthIssuer    string            `json:"oauthIssuer,omitempty"`
	OAuthClientID  string            `json:"oauthClientId,omitempty"`
	OAuthResource  string            `json:"oauthResource,omitempty"`
	PKIResource    string            `json:"pkiResource,omitempty"`
	PKIEndpoint    string            `json:"pkiEndpoint,omitempty"`
	OAuthExpiresAt string            `json:"oauthExpiresAt,omitempty"`
	RefreshToken   string            `json:"refreshToken,omitempty"`
	DPoPPrivateKey string            `json:"dpopPrivateKey,omitempty"`
	Certificates   []CertificateInfo `json:"certificates,omitempty"`
}

// CertificateInfo holds certificate material for mTLS authentication.
type CertificateInfo struct {
	PemCertificate      string `json:"pemCertificate,omitempty"`
	PemCertificateChain string `json:"pemCertificateChain,omitempty"`
	PemPrivateKey       string `json:"pemPrivateKey,omitempty"`
	OrganizationID      int    `json:"organizationId"`
	UserID              string `json:"userId,omitempty"`
	AssetID             int    `json:"assetId,omitempty"`
	PrincipalURI        string `json:"principalUri,omitempty"`
}

// AnalyticsConfig holds analytics preferences.
type AnalyticsConfig struct {
	Enabled bool `json:"enabled"`
}

// CrashReportConfig holds opt-in crash-reporting preferences and state.
type CrashReportConfig struct {
	Suppressed           bool        `json:"suppressed,omitempty"`
	SubscribedReports    []string    `json:"subscribedReports,omitempty"`
	LastCrashStatusCheck string      `json:"lastCrashStatusCheck,omitempty"` // RFC3339 UTC
	PendingFixNotices    []FixNotice `json:"pendingFixNotices,omitempty"`
}

// FixNotice records that a reported crash was fixed in a given release.
type FixNotice struct {
	TrackingID     string `json:"trackingId"`
	FixedInRelease string `json:"fixedInRelease,omitempty"`
}

// ConfigDir returns the device/cloud state directory, creating it if necessary.
// WENDY_CONFIG_DIR gives automation an isolated config, trust store and VM store
// without changing the user's home or their coding agent/model settings.
func ConfigDir() (string, error) {
	if dir := os.Getenv("WENDY_CONFIG_DIR"); dir != "" {
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("WENDY_CONFIG_DIR must be an absolute directory")
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("creating configured device state directory: %w", err)
		}
		return filepath.Clean(dir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determining home directory: %w", err)
	}

	dir := filepath.Join(home, ".wendy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating config directory: %w", err)
	}

	return dir, nil
}

// CacheDir returns the platform-appropriate cache directory for wendy, creating
// it if necessary.
//
//   - macOS:   ~/Library/Caches/wendy
//   - Linux:   $XDG_CACHE_HOME/wendy  (falls back to ~/.cache/wendy)
func CacheDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("determining cache directory: %w", err)
	}

	cacheDir := filepath.Join(dir, "wendy")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("creating cache directory: %w", err)
	}

	return cacheDir, nil
}

// LogDir returns the directory for CLI-written log files (e.g. the Thor flash log),
// creating it if needed.
func LogDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("determining log directory: %w", err)
	}
	logDir := filepath.Join(dir, "wendy", "logs")
	// 0o700: flash logs can contain hardware identifiers (e.g. the device ECID),
	// so keep them readable only by the owner.
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return "", fmt.Errorf("creating log directory: %w", err)
	}
	return logDir, nil
}

// configPath returns the full path to config.json.
func configPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads the CLI configuration from ~/.wendy/config.json.
// If the file does not exist, an empty Config is returned without error.
func Load() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	// Older clients can rewrite the config while dropping newer identity fields.
	// Recover the PKI identity from the certificate instead of interpreting an
	// absent legacy numeric organization id as a real organization zero.
	for i := range cfg.Auth {
		for j := range cfg.Auth[i].Certificates {
			c := &cfg.Auth[i].Certificates[j]
			if c.PrincipalURI == "" {
				c.PrincipalURI = c.CertificatePrincipal()
			}
		}
	}
	// Assign context names and, on first load of a pre-context config, seed
	// CurrentContext from the legacy default fields. In-memory only; the next
	// Save persists it. No re-login: existing sessions become named contexts.
	ensureContexts(&cfg)
	return &cfg, nil
}

// Save writes the configuration to ~/.wendy/config.json. On platforms with
// a credential store, inline secrets are moved into it and the file holds
// only references (see secrets.go); the caller's cfg is never mutated. If
// cfg cannot be cloned, Save fails rather than falling back to mutating the
// caller's struct in place.
func Save(cfg *Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}

	out, err := cfg.clone()
	if err != nil {
		return fmt.Errorf("cloning config: %w", err)
	}
	if dehydrateEnabled() {
		dehydrate(out)
	} else {
		// This branch covers two cases: WENDY_SECRET_STORE=file (explicit
		// de-migration — resolve any existing refs back inline) and
		// non-darwin platforms (no store; refs only exist if the config
		// file was copied over from a Mac, in which case resolution fails
		// and the ref is left as-is — normal non-darwin configs contain no
		// refs, so this is a no-op for them).
		inlineSecrets(out)
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}

	return nil
}

// authEntryOrgID returns the organization ID from the first certificate in an
// auth entry, or 0 if none is present.
func authEntryOrgID(a AuthConfig) int {
	if len(a.Certificates) > 0 {
		return a.Certificates[0].OrganizationID
	}
	return 0
}

// AddAuth adds or replaces an auth entry. Matching is by (cloudDashboard,
// cloudGRPC, orgID) so that multiple orgs on the same cloud endpoint each
// keep their own entry instead of overwriting one another.
func (c *Config) AddAuth(auth AuthConfig) {
	incomingOrg := auth.OrganizationKey()
	for i, existing := range c.Auth {
		if existing.CloudDashboard == auth.CloudDashboard &&
			existing.CloudGRPC == auth.CloudGRPC &&
			existing.OAuthIssuer == auth.OAuthIssuer &&
			existing.OrganizationKey() == incomingOrg {
			c.Auth[i] = auth
			return
		}
	}
	c.Auth = append(c.Auth, auth)
}

// CertificatePrincipal reads exactly one tenant identity from the leaf SAN.
// This recovers identity metadata; transport/request verification still proves it.
func (c CertificateInfo) CertificatePrincipal() string {
	leaves, err := certs.ParseCertsFromPEM([]byte(c.PemCertificate))
	if err != nil || len(leaves) == 0 {
		return ""
	}
	principal, ok := certs.TenantPrincipalFromCert(leaves[0])
	if !ok {
		return ""
	}
	if _, err := certs.ParsePrincipal(principal); err != nil {
		return ""
	}
	return principal
}
func (c CertificateInfo) TenantUUID() string {
	principal := c.PrincipalURI
	if principal == "" {
		principal = c.CertificatePrincipal()
	}
	identity, err := certs.ParsePrincipal(principal)
	if err != nil {
		return ""
	}
	return identity.TenantUUID
}

// AuthOrganizationKey separates UUID organizations, even when all their
// legacy organizationId fields are zero.
func (a AuthConfig) OrganizationKey() string {
	if len(a.Certificates) == 0 {
		return "0"
	}
	if tenant := a.Certificates[0].TenantUUID(); tenant != "" {
		return tenant
	}
	return fmt.Sprint(a.Certificates[0].OrganizationID)
}
