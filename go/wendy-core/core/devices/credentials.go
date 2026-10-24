package devices

import (
	"time"
)

// Credential describes a host-owned client certificate, without key material.
// A zero NotAfter means the host could not parse its leaf and it is still tried.
type Credential struct {
	Reference      string
	OrganizationID int
	NotAfter       time.Time
}

// Expired preserves the CLI's strict comparison at the leaf's expiry instant.
func (c Credential) Expired(now time.Time) bool { return !c.NotAfter.IsZero() && now.After(c.NotAfter) }

// PreferValidCerts skips expired certificates only when usable alternatives exist.
func PreferValidCerts(all []Credential, now time.Time) []Credential {
	return preferValidCerts(all, now)
}

// PromoteOrgNext stably moves an untried credential for the observed org next.
func PromoteOrgNext(order []int, pos int, certs []Credential, org int32) bool {
	for j := pos + 1; j < len(order); j++ {
		if int32(certs[order[j]].OrganizationID) != org {
			continue
		}
		target := pos + 1
		promoted := order[j]
		// Shift the skipped-over entries right by one rather than swapping, so
		// the certs this jump defers keep their relative order behind it.
		copy(order[target+1:j+1], order[target:j])
		order[target] = promoted
		return true
	}
	return false
}

// OrderCertsByOrg stably promotes cached org credentials without filtering any.
func OrderCertsByOrg(certs []Credential, org int, have bool) []Credential {
	if !have || len(certs) < 2 {
		return certs
	}
	ordered := make([]Credential, 0, len(certs))
	for _, c := range certs {
		if c.OrganizationID == org {
			ordered = append(ordered, c)
		}
	}
	if len(ordered) == 0 || len(ordered) == len(certs) {
		return certs
	}
	for _, c := range certs {
		if c.OrganizationID != org {
			ordered = append(ordered, c)
		}
	}
	return ordered
}

// RotateCertsForOrg stably promotes a discovered org while retaining all credentials.
func RotateCertsForOrg(certs []Credential, orgID int32) []Credential {
	if orgID == 0 {
		return certs
	}
	matched := make([]Credential, 0, len(certs))
	rest := make([]Credential, 0, len(certs))
	for _, c := range certs {
		if int32(c.OrganizationID) == orgID {
			matched = append(matched, c)
		} else {
			rest = append(rest, c)
		}
	}
	if len(matched) == 0 {
		return certs
	}
	return append(matched, rest...)
}

func preferValidCerts(all []Credential, now time.Time) []Credential {
	var valid []Credential
	for _, cert := range all {
		if !cert.Expired(now) {
			valid = append(valid, cert)
		}
	}
	if len(valid) > 0 {
		return valid
	}
	return all
}
