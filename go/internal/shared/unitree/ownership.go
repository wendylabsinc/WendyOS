package unitree

import (
	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

// PairingOwner records only this feature's storage ownership, using the
// existing Wendy tenant scope. Carry both identities through dual-SAN renewal;
// no tenant UUID/legacy-org mapping is invented here.
type PairingOwner struct {
	TenantUUID string `json:"tenant_uuid,omitempty"`
	OrgID      int32  `json:"org_id,omitempty"`
	Local      bool   `json:"local,omitempty"`
}

// Scope is the existing certificate scope used for remote access checks.
func (o PairingOwner) Scope() certs.Scope {
	return certs.Scope{TenantUUID: o.TenantUUID, OrgID: o.OrgID}
}

func (o PairingOwner) valid() bool {
	if o.OrgID < 0 || o.Local && o.Scope().Known() {
		return false
	}
	if o.TenantUUID != "" {
		parsed, err := uuid.Parse(o.TenantUUID)
		if err != nil || parsed.String() != o.TenantUUID {
			return false
		}
	}
	return o.Local || o.Scope().Known()
}

// Matches delegates enrolled ownership to the established scope comparison.
// UUID takes precedence when both sides carry it; local ownership is separate.
func (o PairingOwner) Matches(other PairingOwner) bool {
	if !o.valid() || !other.valid() {
		return false
	}
	if o.Local || other.Local {
		return o.Local && other.Local
	}
	return o.Scope().Matches(other.Scope())
}

// enrich retains proven vocabulary in the file, not in remote authorization.
// The caller must first establish Matches against the current authenticated
// scope. A UUID learned during dual-SAN renewal must survive later org-only
// or UUID-only representations without weakening the current listener's scope.
func (o PairingOwner) enrich(previous PairingOwner) PairingOwner {
	if !previous.valid() {
		return o
	}
	if o.TenantUUID == "" {
		o.TenantUUID = previous.TenantUUID
	}
	if o.OrgID == 0 {
		o.OrgID = previous.OrgID
	}
	return o
}
