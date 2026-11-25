// Package proofref defines exact proof identifiers. These values identify
// bytes and source versions; they grant no read or publication authority.
package proofref

import (
	"encoding/hex"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

var ErrInvalid = errors.New("invalid exact proof reference")

type Reference struct {
	Key            world.Key     `json:"key"`
	Version        world.Version `json:"version"`
	EnvelopeSHA256 string        `json:"envelope_sha256"`
	RecordSHA256   string        `json:"record_sha256"`
}

// Source is the exact commissioned read selector, independent of an entity or
// version. The host derives it from current grants, never from hash possession.
type Source struct {
	Tenant    string `json:"tenant"`
	Site      string `json:"site"`
	Device    string `json:"device"`
	App       string `json:"app"`
	Service   string `json:"service"`
	Component string `json:"component"`
	Stream    string `json:"stream"`
}

func (s Source) Validate() error {
	if (serviceorigin.Scope{Tenant: s.Tenant, Site: s.Site, Device: s.Device, App: s.App, Service: s.Service, DeploymentGeneration: 1, ExecutionDomain: "physical", Publish: []serviceorigin.PublishScope{{ComponentType: s.Component, Stream: s.Stream}}}).Validate() != nil {
		return ErrInvalid
	}
	return nil
}
func (r Reference) Source() Source {
	return Source{Tenant: r.Key.Tenant, Site: r.Key.Site, Device: r.Key.Source.Device, App: r.Key.Source.App, Service: r.Key.Source.Service, Component: r.Key.Type, Stream: r.Key.Source.Stream}
}
func digest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func (r Reference) Validate() error {
	if r.Source().Validate() != nil || r.Version.Epoch == 0 || r.Version.Sequence == 0 || r.Key.Entity == "" || len(r.Key.Entity) > 256 || !utf8.ValidString(r.Key.Entity) || strings.TrimSpace(r.Key.Entity) != r.Key.Entity || strings.ContainsAny(r.Key.Entity, "*?") || strings.IndexFunc(r.Key.Entity, unicode.IsControl) >= 0 || !digest(r.EnvelopeSHA256) || !digest(r.RecordSHA256) {
		return ErrInvalid
	}
	return nil
}
func (r Reference) ID() string { return "sha256:" + r.EnvelopeSHA256 }
