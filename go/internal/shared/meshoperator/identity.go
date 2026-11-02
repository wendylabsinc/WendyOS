// Package meshoperator defines the primary operator principal and persistent
// installation identity. Transport admission and Babel routing remain explicit
// caller operations.
package meshoperator

import (
	"crypto/rand"
	"errors"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const MaxPrincipalBytes = 1024

// Operator is an exact, canonical tenant operator principal, not the broader
// compatibility EntityUser role. Parsing establishes shape, never trust.
// Subjects remain opaque URI text and need not be UUIDs or numeric user IDs.
type Operator struct {
	tenant    meshid.Tenant
	principal string
}

func ParsePrincipal(principal string) (Operator, error) {
	if len(principal) > MaxPrincipalBytes || !utf8.ValidString(principal) {
		return Operator{}, errors.New("invalid operator principal length or UTF-8")
	}
	id, err := certs.ParsePrincipal(principal)
	if err != nil {
		return Operator{}, err
	}
	tenant, err := meshid.ParseTenant(id.TenantUUID)
	if err != nil || principal != "spiffe://wendy.sh/tenant/"+id.TenantUUID+"/operator/"+id.EntityID {
		return Operator{}, errors.New("principal is not an exact tenant operator")
	}
	u, err := url.Parse(principal)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.String() != principal {
		return Operator{}, errors.New("operator principal is not canonical URI text")
	}
	for _, c := range principal {
		if c < 0x20 || c == 0x7f {
			return Operator{}, errors.New("operator principal contains a control character")
		}
	}
	return Operator{tenant: tenant, principal: principal}, nil
}

func (o Operator) IsValid() bool         { return o.tenant.IsValid() && o.principal != "" }
func (o Operator) Principal() string     { return o.principal }
func (o Operator) Tenant() meshid.Tenant { return o.tenant }
func (o Operator) Subject() string {
	if !o.IsValid() {
		return ""
	}
	return strings.TrimPrefix(o.principal, "spiffe://wendy.sh/tenant/"+o.tenant.String()+"/operator/")
}

// InstanceID is persistent per CLI installation/context. It is independent of
// credential renewal, carrier choice, attachment incarnation and proof revision.
type InstanceID [16]byte

func NewInstanceID() (id InstanceID, err error) { _, err = rand.Read(id[:]); return }

// Endpoint is the route-owner identity. Two instances of one operator are
// distinct owners even when they use the same credential and adjacent router.
type Endpoint struct {
	Operator Operator
	Instance InstanceID
}
