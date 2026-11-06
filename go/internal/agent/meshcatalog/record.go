// Package meshcatalog projects services from verified device manifests.
package meshcatalog

import (
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MaxLease          = 15 * time.Minute
	MaxTXTBytes       = 1200
	MaxTXTEntries     = 32
	GatewayAppID      = "wendy.mesh.system"
	GatewayServiceID  = "internet-gateway"
	GatewayOfferLease = MaxLease
)

var (
	ErrExpired   = errors.New("expired manifest service")
	labelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
	typePattern  = regexp.MustCompile(`^_[A-Za-z0-9][A-Za-z0-9-]{0,14}\._(tcp|udp)$`)
)

type Key struct {
	Mesh      string `json:"mesh" cbor:"m"`
	Device    string `json:"device" cbor:"d"`
	AppID     string `json:"appId" cbor:"a"`
	ServiceID string `json:"serviceId" cbor:"s"`
}

type Record struct {
	Version    int      `json:"version" cbor:"v"`
	Key        Key      `json:"key" cbor:"k"`
	Generation uint64   `json:"generation" cbor:"G"`
	Type       string   `json:"type" cbor:"t"`
	Instance   string   `json:"instance" cbor:"n"`
	HostPort   uint16   `json:"hostPort" cbor:"h"`
	TXT        []string `json:"txt,omitempty" cbor:"x,omitempty"`
	Issued     int64    `json:"issued" cbor:"i"`
	Expires    int64    `json:"expires" cbor:"e"`
	Withdraw   bool     `json:"withdraw,omitempty" cbor:"w,omitempty"`
}

func IsGatewayOffer(r Record) bool {
	return r.Version == 1 && r.Key.AppID == GatewayAppID &&
		r.Key.ServiceID == GatewayServiceID && r.Type == "" &&
		r.Instance == "" && r.HostPort == 0 && len(r.TXT) == 0
}

func (r Record) Validate(now time.Time) error {
	if r.Version != 1 || !labelPattern.MatchString(r.Key.Mesh) || appconfig.ValidateAppID(r.Key.AppID) != nil ||
		!labelPattern.MatchString(r.Key.ServiceID) || r.Generation == 0 {
		return errors.New("invalid wendy net service owner or generation")
	}
	if _, err := meshid.DeviceFromPrincipal(r.Key.Device); err != nil {
		return errors.New("invalid wendy net service owner principal")
	}
	issued, expires := time.UnixMilli(r.Issued), time.UnixMilli(r.Expires)
	if !expires.After(now) {
		return ErrExpired
	}
	leaseLimit := MaxLease
	if r.Key.AppID == GatewayAppID && r.Key.ServiceID == GatewayServiceID {
		leaseLimit = GatewayOfferLease
	}
	if issued.After(now.Add(5*time.Second)) || !expires.After(issued) || expires.Sub(issued) > leaseLimit {
		return errors.New("invalid wendy net service lease")
	}
	if r.Key.AppID == GatewayAppID || r.Key.ServiceID == GatewayServiceID {
		if !IsGatewayOffer(r) {
			return errors.New("invalid reserved wendy net gateway offer")
		}
		return nil
	}
	if r.Withdraw {
		if r.HostPort != 0 || r.Type != "" || r.Instance != "" || len(r.TXT) != 0 {
			return errors.New("wendy net service withdrawal carries endpoint data")
		}
		return nil
	}
	if !typePattern.MatchString(r.Type) || r.HostPort == 0 || len(r.Instance) == 0 || len(r.Instance) > 63 || !utf8.ValidString(r.Instance) ||
		strings.ContainsAny(r.Instance, "\x00\r\n") {
		return errors.New("invalid wendy net service endpoint")
	}
	if len(r.TXT) > MaxTXTEntries {
		return errors.New("too many wendy net service TXT entries")
	}
	total := 0
	for _, item := range r.TXT {
		if len(item) == 0 || len(item) > 255 || strings.ContainsAny(item, "\x00\r\n") {
			return errors.New("invalid wendy net service TXT entry")
		}
		total += len(item) + 1
	}
	if total > MaxTXTBytes {
		return errors.New("wendy net service TXT too large")
	}
	return nil
}

// PublishSpec comes only from an already identified local app. HostPort must
// be a live ingress mapping owned by that app; the injected policy enforces it.
type PublishSpec struct {
	AppID, ServiceID, Type, Instance string
	HostPort                         uint16
	TXT                              []string
	Lease                            time.Duration
}

// AuthorizePublication must check the app's network mode, declared service
// type and its live ingress ownership. It is invoked only for local writes.
type AuthorizePublication func(appID, serviceType string, hostPort uint16) error

func (s PublishSpec) record(key Key, generation uint64, now time.Time) (Record, error) {
	lease := s.Lease
	if lease == 0 {
		lease = MaxLease
	}
	if lease < time.Second || lease > MaxLease {
		return Record{}, fmt.Errorf("wendy net service lease must be 1s..%s", MaxLease)
	}
	r := Record{Version: 1, Key: key, Generation: generation, Type: s.Type,
		Instance: s.Instance, HostPort: s.HostPort, TXT: append([]string(nil), s.TXT...),
		Issued: now.UnixMilli(), Expires: now.Add(lease).UnixMilli()}
	return r, r.Validate(now)
}
