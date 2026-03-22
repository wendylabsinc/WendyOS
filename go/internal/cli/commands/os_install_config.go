//go:build darwin || linux

package commands

import (
	"archive/zip"
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"golang.org/x/term"
)

const (
	osInstallPayloadDirName = "wendy-install"
	defaultSSHUser          = "wendy"
)

var (
	osInstallHostnameRE = regexp.MustCompile(`^wendyos-[a-z0-9](?:[a-z0-9-]{0,53}[a-z0-9])?$`)
	osInstallUserRE     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,30}$`)
)

type osInstallConfigFlags struct {
	Hostname    string
	WiFi        []string
	AgentBinary string
	CertsZip    string
	SSHMode     string
	SSHUser     string
	SSHPassword string
	SSHKeyFile  string
}

type osInstallConfig struct {
	Hostname    string
	WiFi        []osInstallWiFiCredential
	AgentBinary string
	CertsZip    string
	SSH         osInstallSSHConfig
}

type osInstallWiFiCredential struct {
	SSID     string
	Password string
}

type osInstallSSHConfig struct {
	Mode          string
	Username      string
	Password      string
	AuthorizedKey string
}

type osInstallPayload struct {
	Version  int                  `json:"version"`
	Hostname string               `json:"hostname,omitempty"`
	SSH      *osInstallSSHPayload `json:"ssh,omitempty"`
}

type osInstallSSHPayload struct {
	Mode     string `json:"mode"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

func resolveOSInstallConfig(flags osInstallConfigFlags, interactive bool) (*osInstallConfig, error) {
	cfg, err := osInstallConfigFromFlags(flags)
	if err != nil {
		return nil, err
	}

	if interactive {
		reader := bufio.NewReader(os.Stdin)

		if cfg.Hostname == "" {
			hostname, err := promptOSInstallHostname(reader)
			if err != nil {
				return nil, err
			}
			cfg.Hostname = hostname
		}

		if len(cfg.WiFi) == 0 {
			wifi, err := promptOSInstallWiFi(reader)
			if err != nil {
				return nil, err
			}
			cfg.WiFi = wifi
		} else {
			addMore, err := osInstallPromptYesNo(reader, "Add more Wi-Fi networks?", false)
			if err != nil {
				return nil, err
			}
			if addMore {
				wifi, err := promptOSInstallWiFi(reader)
				if err != nil {
					return nil, err
				}
				cfg.WiFi = append(cfg.WiFi, wifi...)
			}
		}

		if !hasOSInstallAdvancedFlags(flags) {
			advanced, err := osInstallPromptYesNo(reader, "Configure advanced install options?", false)
			if err != nil {
				return nil, err
			}
			if advanced {
				if err := promptOSInstallAdvanced(reader, cfg); err != nil {
					return nil, err
				}
			}
		}
	}

	if err := validateOSInstallConfig(cfg); err != nil {
		return nil, err
	}
	if cfg.IsZero() {
		return nil, nil
	}
	return cfg, nil
}

func osInstallConfigFromFlags(flags osInstallConfigFlags) (*osInstallConfig, error) {
	cfg := &osInstallConfig{
		Hostname:    strings.TrimSpace(flags.Hostname),
		AgentBinary: strings.TrimSpace(flags.AgentBinary),
		CertsZip:    strings.TrimSpace(flags.CertsZip),
	}

	for _, entry := range flags.WiFi {
		parsed, err := parseOSInstallWiFiFlag(entry)
		if err != nil {
			return nil, err
		}
		cfg.WiFi = append(cfg.WiFi, parsed)
	}

	if flags.SSHMode != "" {
		cfg.SSH.Mode = normalizeOSInstallSSHMode(flags.SSHMode)
		cfg.SSH.Username = strings.TrimSpace(flags.SSHUser)
		cfg.SSH.Password = flags.SSHPassword
		if flags.SSHKeyFile != "" {
			keyData, err := os.ReadFile(flags.SSHKeyFile)
			if err != nil {
				return nil, fmt.Errorf("reading --ssh-key-file: %w", err)
			}
			cfg.SSH.AuthorizedKey = strings.TrimSpace(string(keyData))
		}
	} else if flags.SSHUser != "" || flags.SSHPassword != "" || flags.SSHKeyFile != "" {
		return nil, fmt.Errorf("ssh settings require --ssh with one of: default, disable, password, key")
	}

	return cfg, nil
}

func hasOSInstallAdvancedFlags(flags osInstallConfigFlags) bool {
	return flags.AgentBinary != "" ||
		flags.CertsZip != "" ||
		flags.SSHMode != "" ||
		flags.SSHUser != "" ||
		flags.SSHPassword != "" ||
		flags.SSHKeyFile != ""
}

func (c *osInstallConfig) IsZero() bool {
	if c == nil {
		return true
	}
	return c.Hostname == "" &&
		len(c.WiFi) == 0 &&
		c.AgentBinary == "" &&
		c.CertsZip == "" &&
		c.SSH.Mode == ""
}

func validateOSInstallConfig(cfg *osInstallConfig) error {
	if cfg == nil {
		return nil
	}
	if cfg.Hostname != "" && !osInstallHostnameRE.MatchString(cfg.Hostname) {
		return fmt.Errorf("hostname must match %q, got %q", osInstallHostnameRE.String(), cfg.Hostname)
	}
	for i, wifi := range cfg.WiFi {
		if err := validateOSInstallWiFiCredential(wifi); err != nil {
			return fmt.Errorf("wifi entry %d: %w", i+1, err)
		}
	}
	if cfg.AgentBinary != "" {
		info, err := os.Stat(cfg.AgentBinary)
		if err != nil {
			return fmt.Errorf("agent binary: %w", err)
		}
		if info.IsDir() {
			return fmt.Errorf("agent binary path %q is a directory", cfg.AgentBinary)
		}
	}
	if cfg.CertsZip != "" {
		reader, err := zip.OpenReader(cfg.CertsZip)
		if err != nil {
			return fmt.Errorf("certificates zip: %w", err)
		}
		reader.Close()
	}

	switch cfg.SSH.Mode {
	case "", "default":
		cfg.SSH = osInstallSSHConfig{}
	case "disable":
		cfg.SSH.Username = ""
		cfg.SSH.Password = ""
		cfg.SSH.AuthorizedKey = ""
	case "password":
		if cfg.SSH.Username == "" {
			cfg.SSH.Username = defaultSSHUser
		}
		if !osInstallUserRE.MatchString(cfg.SSH.Username) {
			return fmt.Errorf("ssh username %q is invalid", cfg.SSH.Username)
		}
		if strings.TrimSpace(cfg.SSH.Password) == "" {
			return fmt.Errorf("ssh password mode requires a password")
		}
		if cfg.SSH.AuthorizedKey != "" {
			return fmt.Errorf("ssh password mode cannot also include an authorized key")
		}
	case "key":
		if cfg.SSH.Username == "" {
			cfg.SSH.Username = defaultSSHUser
		}
		if !osInstallUserRE.MatchString(cfg.SSH.Username) {
			return fmt.Errorf("ssh username %q is invalid", cfg.SSH.Username)
		}
		if strings.TrimSpace(cfg.SSH.AuthorizedKey) == "" {
			return fmt.Errorf("ssh key mode requires an authorized key")
		}
		if cfg.SSH.Password != "" {
			return fmt.Errorf("ssh key mode cannot also include a password")
		}
	default:
		return fmt.Errorf("unsupported ssh mode %q", cfg.SSH.Mode)
	}

	return nil
}

func validateOSInstallWiFiCredential(wifi osInstallWiFiCredential) error {
	if strings.TrimSpace(wifi.SSID) == "" {
		return fmt.Errorf("ssid cannot be empty")
	}
	if strings.ContainsAny(wifi.SSID, "\r\n") {
		return fmt.Errorf("ssid cannot contain newlines")
	}
	if strings.ContainsAny(wifi.Password, "\r\n") {
		return fmt.Errorf("password cannot contain newlines")
	}
	return nil
}

func parseOSInstallWiFiFlag(raw string) (osInstallWiFiCredential, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return osInstallWiFiCredential{}, fmt.Errorf("wifi entries cannot be empty")
	}
	parts := strings.SplitN(raw, "=", 2)
	wifi := osInstallWiFiCredential{SSID: strings.TrimSpace(parts[0])}
	if len(parts) == 2 {
		wifi.Password = parts[1]
	}
	if err := validateOSInstallWiFiCredential(wifi); err != nil {
		return osInstallWiFiCredential{}, err
	}
	return wifi, nil
}

func promptOSInstallHostname(reader *bufio.Reader) (string, error) {
	defaultHostname := randomFriendlyHostname()
	for {
		value, err := osInstallPromptLine(reader, "Hostname", defaultHostname)
		if err != nil {
			return "", err
		}
		if osInstallHostnameRE.MatchString(value) {
			return value, nil
		}
		fmt.Printf("Hostname must start with \"wendyos-\" and contain only lowercase letters, digits, and hyphens.\n")
	}
}

func promptOSInstallWiFi(reader *bufio.Reader) ([]osInstallWiFiCredential, error) {
	var networks []osInstallWiFiCredential
	addMore := true
	for addMore {
		shouldAdd, err := osInstallPromptYesNo(reader, "Add a Wi-Fi network?", len(networks) == 0)
		if err != nil {
			return nil, err
		}
		if !shouldAdd {
			break
		}

		ssid, err := osInstallPromptLine(reader, "Wi-Fi SSID", "")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(ssid) == "" {
			fmt.Println("SSID cannot be empty.")
			continue
		}

		fmt.Print("Wi-Fi password (leave empty for open network): ")
		passwordBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return nil, fmt.Errorf("reading Wi-Fi password: %w", err)
		}

		wifi := osInstallWiFiCredential{
			SSID:     strings.TrimSpace(ssid),
			Password: string(passwordBytes),
		}
		if err := validateOSInstallWiFiCredential(wifi); err != nil {
			fmt.Printf("%v\n", err)
			continue
		}
		networks = append(networks, wifi)
		addMore = true
	}

	return networks, nil
}

func promptOSInstallAdvanced(reader *bufio.Reader, cfg *osInstallConfig) error {
	agentBinary, err := osInstallPromptLine(reader, "Inject wendy-agent binary from path (optional)", "")
	if err != nil {
		return err
	}
	cfg.AgentBinary = strings.TrimSpace(agentBinary)

	certsZip, err := osInstallPromptLine(reader, "Provisioned certificates zip path (optional)", "")
	if err != nil {
		return err
	}
	cfg.CertsZip = strings.TrimSpace(certsZip)

	fmt.Println()
	sshMode, err := pickFromItems("SSH setup", []tui.PickerItem{
		{Name: "Leave SSH as-is", Description: "Keep the image default", Value: "default"},
		{Name: "Disable SSH", Description: "Turn off the SSH service", Value: "disable"},
		{Name: "Enable SSH with password", Description: "Create or update a user password", Value: "password"},
		{Name: "Enable SSH with public key", Description: "Install an authorized_keys file", Value: "key"},
	})
	if err != nil {
		return err
	}

	cfg.SSH.Mode = sshMode
	switch sshMode {
	case "default", "disable":
		return nil
	case "password":
		username, err := osInstallPromptLine(reader, "SSH username", defaultSSHUser)
		if err != nil {
			return err
		}
		fmt.Print("SSH password: ")
		passwordBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return fmt.Errorf("reading SSH password: %w", err)
		}
		cfg.SSH.Username = username
		cfg.SSH.Password = string(passwordBytes)
	case "key":
		username, err := osInstallPromptLine(reader, "SSH username", defaultSSHUser)
		if err != nil {
			return err
		}
		keyFile, err := osInstallPromptLine(reader, "Public key file", "")
		if err != nil {
			return err
		}
		keyData, err := os.ReadFile(strings.TrimSpace(keyFile))
		if err != nil {
			return fmt.Errorf("reading public key file: %w", err)
		}
		cfg.SSH.Username = username
		cfg.SSH.AuthorizedKey = strings.TrimSpace(string(keyData))
	}

	return nil
}

func osInstallPromptYesNo(reader *bufio.Reader, prompt string, defaultYes bool) (bool, error) {
	suffix := "[y/N]"
	if defaultYes {
		suffix = "[Y/n]"
	}
	for {
		fmt.Printf("%s %s ", prompt, suffix)
		line, err := reader.ReadString('\n')
		if err != nil {
			return false, err
		}
		answer := strings.TrimSpace(strings.ToLower(line))
		if answer == "" {
			return defaultYes, nil
		}
		if answer == "y" || answer == "yes" {
			return true, nil
		}
		if answer == "n" || answer == "no" {
			return false, nil
		}
		fmt.Println("Please answer y or n.")
	}
}

func osInstallPromptLine(reader *bufio.Reader, prompt string, defaultValue string) (string, error) {
	if defaultValue != "" && !strings.Contains(prompt, "[") {
		fmt.Printf("%s [%s]: ", prompt, defaultValue)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(line)
	if value == "" {
		return defaultValue, nil
	}
	return value, nil
}

func renderOSInstallConfigSummary(cfg *osInstallConfig) string {
	if cfg == nil || cfg.IsZero() {
		return "No install-time configuration"
	}

	var lines []string
	if cfg.Hostname != "" {
		lines = append(lines, fmt.Sprintf("Hostname: %s", cfg.Hostname))
	}
	if len(cfg.WiFi) > 0 {
		var ssids []string
		for _, wifi := range cfg.WiFi {
			ssids = append(ssids, wifi.SSID)
		}
		lines = append(lines, fmt.Sprintf("Wi-Fi: %s", strings.Join(ssids, ", ")))
	}
	if cfg.AgentBinary != "" {
		lines = append(lines, fmt.Sprintf("Injected agent binary: %s", filepath.Base(cfg.AgentBinary)))
	}
	if cfg.CertsZip != "" {
		lines = append(lines, fmt.Sprintf("Provisioned certs: %s", filepath.Base(cfg.CertsZip)))
	}
	switch cfg.SSH.Mode {
	case "disable":
		lines = append(lines, "SSH: disabled")
	case "password":
		lines = append(lines, fmt.Sprintf("SSH: enabled for %s with password auth", cfg.SSH.Username))
	case "key":
		lines = append(lines, fmt.Sprintf("SSH: enabled for %s with public key auth", cfg.SSH.Username))
	}

	return strings.Join(lines, "\n")
}

func stageOSInstallPayload(cfg *osInstallConfig) (string, error) {
	payloadRoot, err := os.MkdirTemp("", "wendy-os-install-payload-*")
	if err != nil {
		return "", fmt.Errorf("creating install payload temp dir: %w", err)
	}
	payloadDir := filepath.Join(payloadRoot, osInstallPayloadDirName)
	if err := os.MkdirAll(payloadDir, 0o755); err != nil {
		return "", fmt.Errorf("creating payload directory: %w", err)
	}

	payload := osInstallPayload{Version: 1}
	if cfg != nil {
		payload.Hostname = cfg.Hostname
		if cfg.SSH.Mode != "" {
			payload.SSH = &osInstallSSHPayload{
				Mode:     cfg.SSH.Mode,
				Username: cfg.SSH.Username,
				Password: cfg.SSH.Password,
			}
		}
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshaling payload config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(payloadDir, "config.json"), data, 0o644); err != nil {
		return "", fmt.Errorf("writing payload config: %w", err)
	}

	if cfg != nil {
		if err := stageOSInstallWiFiPayload(payloadDir, cfg.WiFi); err != nil {
			return "", err
		}
		if err := stageOSInstallAgentPayload(payloadDir, cfg.AgentBinary); err != nil {
			return "", err
		}
		if err := stageOSInstallCertsPayload(payloadDir, cfg.CertsZip); err != nil {
			return "", err
		}
		if err := stageOSInstallSSHPayload(payloadDir, cfg.SSH); err != nil {
			return "", err
		}
	}

	return payloadRoot, nil
}

func stageOSInstallWiFiPayload(payloadDir string, networks []osInstallWiFiCredential) error {
	if len(networks) == 0 {
		return nil
	}
	networkDir := filepath.Join(payloadDir, "network")
	if err := os.MkdirAll(networkDir, 0o755); err != nil {
		return fmt.Errorf("creating network payload dir: %w", err)
	}

	for i, network := range networks {
		filename := fmt.Sprintf("wifi-%02d-%s.nmconnection", i+1, sanitizeOSInstallFileComponent(network.SSID))
		content := renderWiFiConnectionProfile(network)
		if err := os.WriteFile(filepath.Join(networkDir, filename), []byte(content), 0o600); err != nil {
			return fmt.Errorf("writing network profile: %w", err)
		}
	}
	return nil
}

func renderWiFiConnectionProfile(network osInstallWiFiCredential) string {
	var builder strings.Builder
	builder.WriteString("[connection]\n")
	builder.WriteString(fmt.Sprintf("id=%s\n", sanitizeOSInstallConnectionID(network.SSID)))
	builder.WriteString(fmt.Sprintf("uuid=%s\n", uuid.NewString()))
	builder.WriteString("type=wifi\n")
	builder.WriteString("autoconnect=true\n\n")

	builder.WriteString("[wifi]\n")
	builder.WriteString("mode=infrastructure\n")
	builder.WriteString(fmt.Sprintf("ssid=%s\n\n", network.SSID))

	if network.Password != "" {
		builder.WriteString("[wifi-security]\n")
		builder.WriteString("key-mgmt=wpa-psk\n")
		builder.WriteString(fmt.Sprintf("psk=%s\n\n", network.Password))
	}

	builder.WriteString("[ipv4]\n")
	builder.WriteString("method=auto\n\n")
	builder.WriteString("[ipv6]\n")
	builder.WriteString("addr-gen-mode=default\n")
	builder.WriteString("method=auto\n\n")
	builder.WriteString("[proxy]\n")

	return builder.String()
}

func stageOSInstallAgentPayload(payloadDir string, agentBinary string) error {
	if agentBinary == "" {
		return nil
	}
	assetsDir := filepath.Join(payloadDir, "assets")
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		return fmt.Errorf("creating asset payload dir: %w", err)
	}
	dst := filepath.Join(assetsDir, "wendy-agent")
	if err := copyFile(agentBinary, dst); err != nil {
		return fmt.Errorf("copying agent binary into payload: %w", err)
	}
	if err := os.Chmod(dst, 0o755); err != nil {
		return fmt.Errorf("chmod agent payload: %w", err)
	}
	return nil
}

func stageOSInstallCertsPayload(payloadDir string, certsZip string) error {
	if certsZip == "" {
		return nil
	}
	reader, err := zip.OpenReader(certsZip)
	if err != nil {
		return fmt.Errorf("opening certificates zip: %w", err)
	}
	defer reader.Close()

	certsDir := filepath.Join(payloadDir, "certs")
	if err := os.MkdirAll(certsDir, 0o755); err != nil {
		return fmt.Errorf("creating cert payload dir: %w", err)
	}

	for _, f := range reader.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rel := filepath.Clean(f.Name)
		if rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			return fmt.Errorf("invalid path %q in certificates zip", f.Name)
		}

		src, err := f.Open()
		if err != nil {
			return fmt.Errorf("opening %q in certificates zip: %w", f.Name, err)
		}

		dstPath := filepath.Join(certsDir, rel)
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			src.Close()
			return fmt.Errorf("creating certificate payload path: %w", err)
		}
		dst, err := os.Create(dstPath)
		if err != nil {
			src.Close()
			return fmt.Errorf("creating certificate payload file: %w", err)
		}
		if _, err := io.Copy(dst, src); err != nil {
			dst.Close()
			src.Close()
			return fmt.Errorf("copying certificate payload file: %w", err)
		}
		dst.Close()
		src.Close()
	}

	return nil
}

func stageOSInstallSSHPayload(payloadDir string, ssh osInstallSSHConfig) error {
	if ssh.Mode != "key" {
		return nil
	}
	sshDir := filepath.Join(payloadDir, "ssh")
	if err := os.MkdirAll(sshDir, 0o755); err != nil {
		return fmt.Errorf("creating ssh payload dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "authorized_keys"), []byte(strings.TrimSpace(ssh.AuthorizedKey)+"\n"), 0o600); err != nil {
		return fmt.Errorf("writing authorized_keys payload: %w", err)
	}
	return nil
}

func sanitizeOSInstallFileComponent(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '-':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
	}
	sanitized := strings.Trim(builder.String(), "-")
	if sanitized == "" {
		return "network"
	}
	return sanitized
}

func sanitizeOSInstallConnectionID(ssid string) string {
	id := "wifi-" + sanitizeOSInstallFileComponent(ssid)
	id = strings.Trim(id, "-")
	if id == "" {
		return "wifi"
	}
	return id
}

func normalizeOSInstallSSHMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "default":
		return "default"
	case "disable", "disabled", "off":
		return "disable"
	case "password":
		return "password"
	case "key", "pubkey", "public-key":
		return "key"
	default:
		return strings.ToLower(strings.TrimSpace(mode))
	}
}

func promptOSInstallImageCustomization(imagePath string, cfg *osInstallConfig) (string, func(), error) {
	if cfg == nil || cfg.IsZero() {
		return imagePath, func() {}, nil
	}

	fmt.Println("\nPreparing install payload...")
	tmpImage, err := os.CreateTemp("", "wendyos-configured-*.img")
	if err != nil {
		return "", nil, fmt.Errorf("creating configured image temp file: %w", err)
	}
	tmpImage.Close()

	if err := copyFile(imagePath, tmpImage.Name()); err != nil {
		os.Remove(tmpImage.Name())
		return "", nil, fmt.Errorf("copying source image: %w", err)
	}

	payloadRoot, err := stageOSInstallPayload(cfg)
	if err != nil {
		os.Remove(tmpImage.Name())
		return "", nil, err
	}

	if err := writeOSInstallPayloadToImage(tmpImage.Name(), filepath.Join(payloadRoot, osInstallPayloadDirName)); err != nil {
		os.RemoveAll(payloadRoot)
		os.Remove(tmpImage.Name())
		return "", nil, err
	}

	cleanup := func() {
		os.RemoveAll(payloadRoot)
		os.Remove(tmpImage.Name())
	}
	return tmpImage.Name(), cleanup, nil
}

func copyDir(srcDir, dstDir string) error {
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dstDir, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := copyFile(path, target); err != nil {
			return err
		}
		return os.Chmod(target, info.Mode())
	})
}

var hostnameAdjectives = []string{
	"amber", "brisk", "calm", "cinder", "clear", "cool", "daring", "ember", "fresh", "glossy",
	"golden", "hollow", "jolly", "kind", "lively", "merry", "nimble", "nova", "plucky", "quiet",
	"rapid", "sandy", "sharp", "silver", "spry", "steady", "sunny", "swift", "tidy", "vivid",
	"warm", "wavy", "wild", "zesty",
}

var hostnameNouns = []string{
	"badger", "comet", "dolphin", "falcon", "fox", "gecko", "heron", "ibis", "koala", "lark",
	"lynx", "maple", "meadow", "otter", "owl", "panda", "pebble", "pine", "quill", "raven",
	"ridge", "river", "robin", "seal", "sparrow", "spruce", "stork", "swift", "tiger", "trail",
	"valley", "willow", "wolf", "wren",
}

func randomFriendlyHostname() string {
	first := hostnameAdjectives[randomIndex(len(hostnameAdjectives))]
	second := hostnameNouns[randomIndex(len(hostnameNouns))]
	return fmt.Sprintf("wendyos-%s-%s", first, second)
}

func randomIndex(limit int) int {
	if limit <= 1 {
		return 0
	}
	var b [1]byte
	if _, err := rand.Read(b[:]); err == nil {
		return int(b[0]) % limit
	}
	return limit - 1
}
