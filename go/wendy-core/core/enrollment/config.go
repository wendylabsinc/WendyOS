package enrollment

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Config is the enrollment material staged on the device. The JSON tags are the
// on-disk shape written at provisioning time.
type Config struct {
	// DirectoryURL is the tenant's ACME directory,
	// https://<host>/<tenant-uuid>/acme/directory. The tenant is a canonical
	// lower-case UUID and rides inside this URL, so there is no tenant field.
	DirectoryURL string `json:"directoryURL"`
	// DeviceID is stamped into the issued SPIFFE identity server-side. It must
	// equal the DeviceID bound to the EAB row or newOrder returns 403.
	DeviceID string `json:"deviceID"`
	// EABKeyID is the opaque EAB key row id used as the JWS "kid".
	EABKeyID string `json:"eabKeyID"`
	// EABHMACKey is HEX-encoded, not the base64url every other ACME CA hands
	// out. Feeding the raw string to the MAC produces a well-formed request
	// that fails with an unhelpful 401.
	EABHMACKey string `json:"eabHMACKey"`
}

// Validate checks enrollment material without registering an ACME account.
func (c Config) Validate() error {
	var missing []string
	for _, f := range []struct {
		name, value string
	}{
		{"directoryURL", c.DirectoryURL},
		{"deviceID", c.DeviceID},
		{"eabKeyID", c.EABKeyID},
		{"eabHMACKey", c.EABHMACKey},
	} {
		if f.value == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("acme enrollment config is missing %s", strings.Join(missing, ", "))
	}
	if _, err := c.PrincipalURI(); err != nil {
		return err
	}
	if _, err := hex.DecodeString(c.EABHMACKey); err != nil {
		return errors.New("EAB HMAC key must be hex-encoded")
	}
	return nil
}

// PrincipalURI returns the device identity expected from this tenant's ACME
// endpoint. Validate the endpoint before sending its single-use credential.
func (c Config) PrincipalURI() (string, error) {
	u, err := url.Parse(c.DirectoryURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid ACME directory URL")
	}
	loopback := u.Hostname() == "localhost"
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return "", errors.New("ACME directory must use HTTPS (HTTP is allowed only for loopback development servers)")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 3 || parts[1] != "acme" || parts[2] != "directory" {
		return "", errors.New("ACME directory URL must have the path /<tenant-uuid>/acme/directory")
	}
	tenant := parts[0]
	if !canonicalUUID(tenant) {
		return "", errors.New("ACME directory URL must contain a canonical tenant UUID")
	}
	// Match PKI's device-name contract, including names such as fleet/box-01.
	for _, segment := range strings.Split(c.DeviceID, "/") {
		if len(segment) == 0 || len(segment) > 64 || segment == "." || segment == ".." {
			return "", errors.New("invalid ACME device ID: each path segment must contain 1–64 letters, digits, dots, underscores or hyphens, excluding . and ..")
		}
		for _, ch := range segment {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == '-') {
				return "", errors.New("invalid ACME device ID: use letters, digits, dots, underscores, hyphens and path separators")
			}
		}
	}
	return "spiffe://wendy.sh/tenant/" + tenant + "/device/" + c.DeviceID, nil
}
