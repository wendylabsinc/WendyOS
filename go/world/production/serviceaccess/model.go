// Package serviceaccess defines exact read selectors for the private app API.
// A selector is a filter, never a credential. An omitted device means the
// socket's local device; an explicit device requires its own exact grant.
package serviceaccess

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	MaxSubscriptions = 32
	MaxQuerySources  = 8
	MaxQueryRecords  = 8
	MaxQueryBytes    = 192 << 10
)

var ErrInvalid = errors.New("invalid exact service access scope")
var appName = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,253}$`)
var serviceName = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,55}[a-z0-9])?$`)

type Source struct {
	App           string `json:"app"`
	Service       string `json:"service"`
	ComponentType string `json:"component_type"`
	Stream        string `json:"stream"`
	Device        string `json:"device,omitempty"`
}

func exact(s string, max int) bool {
	return bounded(s, max) && !strings.ContainsAny(s, "*?")
}
func bounded(s string, max int) bool {
	if s == "" || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
func (s Source) Validate() error {
	if !appName.MatchString(s.App) || strings.Trim(s.App, ".") == "" || !serviceName.MatchString(s.Service) || !exact(s.ComponentType, 128) || !exact(s.Stream, 128) {
		return ErrInvalid
	}
	if s.Device != "" {
		if _, err := DeviceTenant(s.Device); err != nil {
			return err
		}
	}
	return nil
}

// DeviceTenant checks canonical syntax only. It does not verify enrollment,
// authenticate a peer, or grant access to any source.
func DeviceTenant(device string) (string, error) {
	const prefix = "spiffe://wendy.sh/tenant/"
	if !strings.HasPrefix(device, prefix) {
		return "", ErrInvalid
	}
	parts := strings.Split(strings.TrimPrefix(device, prefix), "/device/")
	if len(parts) != 2 {
		return "", ErrInvalid
	}
	for _, part := range parts {
		id, err := uuid.Parse(part)
		if err != nil || id.String() != part {
			return "", ErrInvalid
		}
	}
	return parts[0], nil
}

// Resolve preserves an explicit device and resolves only an omitted one. The
// result remains within the local tenant and still needs exact authorization.
func (s Source) Resolve(localDevice string) (Source, error) {
	tenant, err := DeviceTenant(localDevice)
	if err != nil || s.Validate() != nil {
		return Source{}, ErrInvalid
	}
	if s.Device == "" {
		s.Device = localDevice
	} else if remoteTenant, err := DeviceTenant(s.Device); err != nil || remoteTenant != tenant {
		return Source{}, ErrInvalid
	}
	return s, nil
}
func ValidateSources(sources []Source, max int) error {
	if len(sources) > max {
		return ErrInvalid
	}
	seen := map[Source]bool{}
	for _, s := range sources {
		if s.Validate() != nil || seen[s] {
			return ErrInvalid
		}
		seen[s] = true
	}
	return nil
}

// Query pages a changing latest-value replica, not a frozen event history.
// A client must restart from an empty cursor to observe subsequent updates.
type Query struct {
	Sources []Source
	Limit   uint32
	After   string
}
type Page struct {
	SignedRecordsJSON [][]byte
	Next              string
}

func (q Query) Validate() error {
	if len(q.Sources) == 0 || ValidateSources(q.Sources, MaxQuerySources) != nil || q.Limit < 1 || q.Limit > MaxQueryRecords {
		return ErrInvalid
	}
	if q.After != "" {
		if _, err := DecodeCursor(q.After); err != nil {
			return err
		}
	}
	return nil
}
func Key(s Source, entity string) string {
	if s.Device != "" {
		b, _ := json.Marshal([6]string{s.Device, s.App, s.Service, s.ComponentType, s.Stream, entity})
		return string(b)
	}
	b, _ := json.Marshal([5]string{s.App, s.Service, s.ComponentType, s.Stream, entity})
	return string(b)
}
func Cursor(key string) string { return base64.RawURLEncoding.EncodeToString([]byte(key)) }
func DecodeCursor(cursor string) (string, error) {
	if len(cursor) > 1536 {
		return "", ErrInvalid
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", ErrInvalid
	}
	var parts []string
	if json.Unmarshal(b, &parts) != nil || len(parts) != 5 && len(parts) != 6 {
		return "", ErrInvalid
	}
	var device string
	if len(parts) == 6 {
		device, parts = parts[0], parts[1:]
		if device == "" {
			return "", ErrInvalid
		}
	}
	s := Source{App: parts[0], Service: parts[1], ComponentType: parts[2], Stream: parts[3], Device: device}
	if s.Validate() != nil || !bounded(parts[4], 256) || string(b) != Key(s, parts[4]) || Cursor(string(b)) != cursor {
		return "", ErrInvalid
	}
	return string(b), nil
}
