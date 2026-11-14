package onboarding

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/wendylabsinc/wendy/go/internal/cli/devicename"
)

// Provisioning is what a job writes for first boot. It carries Wi-Fi
// passwords, so it lives only in StartOptions and the job's private input file.
type Provisioning struct {
	DeviceName string
	NoWiFi     bool
	WiFi       []WiFiNetwork
}

// WiFiNetwork is one network for first boot. An empty Password is an open
// network. Password never marshals to JSON; the job store keeps its own record.
type WiFiNetwork struct {
	SSID     string
	Password string `json:"-"`
	Priority int32
	Hidden   bool
	Security string
}

// String and GoString keep the password out of formatted errors and logs.
func (n WiFiNetwork) String() string {
	return fmt.Sprintf("{SSID:%q Password:%s Priority:%d Hidden:%t Security:%q}", n.SSID, redacted(n.Password), n.Priority, n.Hidden, n.Security)
}

func (n WiFiNetwork) GoString() string { return "onboarding.WiFiNetwork" + n.String() }

func redacted(password string) string {
	if password == "" {
		return "none"
	}
	return "redacted"
}

// ProvisioningSummary is the non-secret record of a job's first-boot choices.
// Start always sets it: no input means an auto-generated name and no Wi-Fi.
type ProvisioningSummary struct {
	DeviceName string   `json:"device_name,omitempty"`
	AutoName   bool     `json:"auto_name"`
	WiFi       []string `json:"wifi,omitempty"`
	// OpenWiFi lists the networks given without a password, so whoever
	// authorizes the erase can spot a password that went missing.
	OpenWiFi []string `json:"open_wifi,omitempty"`
	NoWiFi   bool     `json:"no_wifi"`
}

// wifiNames renders the networks for instructions, marking open ones.
func (p *ProvisioningSummary) wifiNames() string {
	names := make([]string, 0, len(p.WiFi))
	for _, ssid := range p.WiFi {
		if slices.Contains(p.OpenWiFi, ssid) {
			ssid += " (open, no password)"
		}
		names = append(names, ssid)
	}
	return strings.Join(names, ", ")
}

// Requested reports whether the job needs its stored first-boot input.
func (p *ProvisioningSummary) Requested() bool {
	return p != nil && (p.DeviceName != "" || len(p.WiFi) > 0)
}

func (p *ProvisioningSummary) equal(o *ProvisioningSummary) bool {
	return p.DeviceName == o.DeviceName && p.AutoName == o.AutoName && p.NoWiFi == o.NoWiFi && slices.Equal(p.WiFi, o.WiFi) && slices.Equal(p.OpenWiFi, o.OpenWiFi)
}

// ErrProvisioningUnavailable means a job's stored first-boot input is gone.
var ErrProvisioningUnavailable = errors.New("this job's first-boot input (device name and Wi-Fi) is no longer stored: it is kept for one install attempt and at most 24 hours")

// ProvisioningError reports a failure after the whole image was written, so
// the job can say what the target holds instead of "outcome unknown".
type ProvisioningError struct {
	Err error
	// Configured means first-boot configuration was written and only
	// releasing the media failed.
	Configured bool
}

func (e *ProvisioningError) Error() string {
	if e.Configured {
		return fmt.Sprintf("the image and its first-boot configuration were written, but the media could not be released: %v", e.Err)
	}
	return fmt.Sprintf("the image was written, but its first-boot configuration was not: %v", e.Err)
}

func (e *ProvisioningError) Unwrap() error { return e.Err }

// maxSSIDBytes is the 802.11 limit; a longer SSID can never match a network.
const maxSSIDBytes = 32

// normalizeProvisioning validates p and returns a cleaned copy. A nil p is
// an explicit choice of an auto-generated name and no Wi-Fi.
func normalizeProvisioning(p *Provisioning) (*Provisioning, error) {
	out := &Provisioning{}
	if p == nil {
		out.NoWiFi = true
		return out, nil
	}
	if p.NoWiFi && len(p.WiFi) > 0 {
		return nil, errors.New("choose Wi-Fi networks or no Wi-Fi, not both")
	}
	out.DeviceName = strings.TrimSpace(p.DeviceName)
	if out.DeviceName != "" {
		if err := devicename.Validate(out.DeviceName); err != nil {
			return nil, err
		}
	}
	for i, n := range p.WiFi {
		n.SSID = strings.TrimSpace(n.SSID)
		n.Security = strings.ToLower(strings.TrimSpace(n.Security))
		switch {
		case n.SSID == "":
			return nil, fmt.Errorf("Wi-Fi network %d has no SSID", i+1)
		case hasControl(n.SSID):
			return nil, fmt.Errorf("Wi-Fi network %d: the SSID must not contain line breaks or other control characters", i+1)
		case len(n.SSID) > maxSSIDBytes:
			return nil, fmt.Errorf("Wi-Fi network %q: an SSID is at most %d bytes", n.SSID, maxSSIDBytes)
		case hasControl(n.Password):
			return nil, fmt.Errorf("Wi-Fi network %q: the password must not contain line breaks or other control characters", n.SSID)
		case n.Password != strings.TrimSpace(n.Password):
			// The device trims config values, so it would join with another password.
			return nil, fmt.Errorf("Wi-Fi network %q: the password must not begin or end with a space", n.SSID)
		case hasControl(n.Security):
			return nil, fmt.Errorf("Wi-Fi network %q: the security type must not contain control characters", n.SSID)
		}
		out.WiFi = append(out.WiFi, n)
	}
	out.NoWiFi = len(out.WiFi) == 0
	return out, nil
}

func hasControl(s string) bool { return strings.IndexFunc(s, unicode.IsControl) >= 0 }

func (p *Provisioning) summary() *ProvisioningSummary {
	s := &ProvisioningSummary{DeviceName: p.DeviceName, AutoName: p.DeviceName == "", NoWiFi: len(p.WiFi) == 0}
	for _, n := range p.WiFi {
		s.WiFi = append(s.WiFi, n.SSID)
		if n.Password == "" {
			s.OpenWiFi = append(s.OpenWiFi, n.SSID)
		}
	}
	return s
}

// provisioningRecord is the input file's format; only it serializes passwords.
type provisioningRecord struct {
	DeviceName string       `json:"device_name,omitempty"`
	NoWiFi     bool         `json:"no_wifi,omitempty"`
	WiFi       []wifiRecord `json:"wifi,omitempty"`
}

type wifiRecord struct {
	SSID     string `json:"ssid"`
	Password string `json:"password,omitempty"`
	Priority int32  `json:"priority,omitempty"`
	Hidden   bool   `json:"hidden,omitempty"`
	Security string `json:"security,omitempty"`
}

func (s JobStore) provisioningPath(id string) (string, error) {
	if !jobIDPattern.MatchString(id) {
		return "", fmt.Errorf("invalid installation job ID")
	}
	return filepath.Join(s.Root, id+".provisioning.json"), nil
}

func (s JobStore) writeProvisioning(id string, p *Provisioning) error {
	path, err := s.provisioningPath(id)
	if err != nil {
		return err
	}
	rec := provisioningRecord{DeviceName: p.DeviceName, NoWiFi: p.NoWiFi}
	for _, n := range p.WiFi {
		rec.WiFi = append(rec.WiFi, wifiRecord(n))
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return s.writeAtomic(path, b, provisioningTempPattern)
}

// hasProvisioning reports whether id's input is stored and has not expired.
func (s JobStore) hasProvisioning(id string) bool {
	path, err := s.provisioningPath(id)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if time.Since(info.ModTime()) > provisioningMaxAge {
		_ = os.Remove(path)
		return false
	}
	return true
}

func (s JobStore) removeProvisioning(id string) {
	if path, err := s.provisioningPath(id); err == nil {
		_ = os.Remove(path)
	}
}

// ReadProvisioning returns the first-boot input for j's worker. It fails
// rather than install without input that the job promised to apply.
func (s JobStore) ReadProvisioning(j *Job) (*Provisioning, error) {
	if !j.Provisioning.Requested() {
		if err := s.checkProvisioningKept(j); err != nil {
			return nil, err
		}
		return &Provisioning{NoWiFi: true}, nil
	}
	path, err := s.provisioningPath(j.ID)
	if err != nil {
		return nil, err
	}
	if !s.hasProvisioning(j.ID) {
		return nil, ErrProvisioningUnavailable
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrProvisioningUnavailable
	}
	if err != nil {
		return nil, fmt.Errorf("reading this job's first-boot input: %w", err)
	}
	var rec provisioningRecord
	// Decoder errors can quote file bytes, which include passwords.
	if json.Unmarshal(b, &rec) != nil {
		return nil, errors.New("this job's stored first-boot input is damaged; start a new job")
	}
	p := &Provisioning{DeviceName: rec.DeviceName, NoWiFi: rec.NoWiFi}
	for _, n := range rec.WiFi {
		p.WiFi = append(p.WiFi, WiFiNetwork(n))
	}
	if !p.summary().equal(j.Provisioning) {
		return nil, errors.New("this job's stored first-boot input does not match the job; start a new job")
	}
	return p, nil
}

const (
	provisioningTempPattern = ".provisioning-*"
	provisioningMaxAge      = 24 * time.Hour
)

// sweepProvisioning removes input left by jobs that never ran a worker.
func (s JobStore) sweepProvisioning(now time.Time) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".provisioning.json") && !strings.HasPrefix(name, ".provisioning-") {
			continue
		}
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() && now.Sub(info.ModTime()) > provisioningMaxAge {
			_ = os.Remove(filepath.Join(s.Root, name))
		}
	}
}

// firstBootLine states what first boot applies; Ethernet only matters without Wi-Fi.
func firstBootLine(p *ProvisioningSummary) string {
	if !p.Requested() {
		return "No device name or Wi-Fi was given, so the image is written as published: first boot picks an auto-generated device name and has no Wi-Fi; reach the board over Ethernet or USB. Cloud enrollment is not pre-seeded."
	}
	name := "an auto-generated device name"
	if p.DeviceName != "" {
		name = "the device name " + p.DeviceName
	}
	if len(p.WiFi) > 0 {
		return fmt.Sprintf("First boot applies %s and joins Wi-Fi %s. Cloud enrollment is not pre-seeded.", name, p.wifiNames())
	}
	return fmt.Sprintf("First boot applies %s and no Wi-Fi: reach the board over Ethernet or USB. Cloud enrollment is not pre-seeded.", name)
}

func discoverLine(p *ProvisioningSummary) string {
	who := "this device"
	if p != nil && p.DeviceName != "" {
		who = "the device named " + p.DeviceName
	}
	return "Discover and identify " + who + ", then resume with its explicit address to verify the installed version and board. Include a known public key if available."
}

func provisioningFailureInstructions(j *Job, e *ProvisioningError) []string {
	if e.Configured {
		steps := []string{"The OS image and first-boot configuration were written, but the media could not be ejected and may still be mounted. Do not repeat this job.", "Eject the media with the host's file manager (Finder on macOS) before removing it, then insert it into the intended board and power it on."}
		return append(steps, bootWaitLine(j.Provisioning)+" "+discoverLine(j.Provisioning))
	}
	return []string{
		"The OS image was written completely, but its first-boot configuration was not, so the board would boot with an auto-generated name and no Wi-Fi. Do not repeat this job.",
		"Eject the media safely before removing it. To apply the requested name and Wi-Fi, create a new installation job for the same target and authorize it again. Or boot the board as it is over Ethernet or USB, resume this job with its explicit address to verify it, then set Wi-Fi with wifi_connect and the name with wendy device rename.",
	}
}

func bootWaitLine(p *ProvisioningSummary) string {
	if p != nil && len(p.WiFi) > 0 {
		return "Allow first boot to finish; the board joins Wi-Fi " + p.wifiNames() + "."
	}
	return "Connect Ethernet and allow first boot to finish."
}

// checkProvisioning refuses to probe or authorize a job whose first-boot
// input is gone, since its worker would have to install without it.
func (s JobStore) checkProvisioning(j *Job) error {
	if j.Provisioning.Requested() && !s.hasProvisioning(j.ID) {
		return fmt.Errorf("%w; start a new job with the same device name and Wi-Fi", ErrProvisioningUnavailable)
	}
	return s.checkProvisioningKept(j)
}

// errProvisioningDropped means the job lost the record of its first-boot
// input while the input itself is still stored, as an older build rewriting
// the job would leave it.
var errProvisioningDropped = errors.New("this job no longer records the device name and Wi-Fi it was started with; start a new job with the current wendy")

func (s JobStore) checkProvisioningKept(j *Job) error {
	if j.Provisioning.Requested() {
		return nil
	}
	path, err := s.provisioningPath(j.ID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return errProvisioningDropped
	}
	return nil
}
