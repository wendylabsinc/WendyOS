package certs

import "time"

// CertificateWindow is the intersection of validity windows on one verified
// path. Unused peer issuers or alternate anchors do not change it.
type CertificateWindow struct {
	NotBefore time.Time
	NotAfter  time.Time
}

// CertificateTimePolicy supplies only local observations. Floor is a local
// boot-time lower bound and must never be selected from a peer announcement.
// Uncertain distinguishes that boot state from verified current UTC. Providers
// must be safe for concurrent calls and cannot come from peer message dates.
// The zero value uses ordinary wall-time certificate validation.
type CertificateTimePolicy struct {
	Now       func() time.Time
	Floor     func() time.Time
	Uncertain func() bool
}

func (p CertificateTimePolicy) WallTime() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// BaseTime is the local verification clock for every certificate date and
// expiry. A selected local anchor is a lower bound, never a peer time claim.
func (p CertificateTimePolicy) BaseTime(wall time.Time) time.Time {
	if p.Floor != nil {
		if floor := p.Floor(); floor.After(wall) {
			return floor
		}
	}
	return wall
}

// LeafTime additionally accommodates at most 24h of leaf NotBefore while boot
// time is explicitly uncertain. This does not change BaseTime for issuer or
// expiry checks and never advances the system clock.
func (p CertificateTimePolicy) LeafTime(wall, notBefore time.Time) time.Time {
	return p.LeafTimeAtBase(p.BaseTime(wall), notBefore)
}

// LeafTimeAtBase keeps one sampled local base consistent across every check.
func (p CertificateTimePolicy) LeafTimeAtBase(base, notBefore time.Time) time.Time {
	if p.Uncertain == nil || !p.Uncertain() {
		return base
	}
	return CertificateLeafTime(base, base, notBefore, true)
}

const MaxCertificateClockSkew = 24 * time.Hour
const MaxPeerCertificateLifetime = (2*365 + 2) * 24 * time.Hour

// CertificateLeafTime is shared with the legacy agent floor wrapper, whose
// strictly-before-floor trigger remains unchanged. The caller chooses whether
// its own local floor policy is active; no peer chooses that condition.
func CertificateLeafTime(wall, floor, notBefore time.Time, active bool) time.Time {
	if !active {
		return wall
	}
	effective := wall
	if floor.After(effective) {
		effective = floor
	}
	advanced := notBefore
	if cap := floor.Add(MaxCertificateClockSkew); advanced.After(cap) {
		advanced = cap
	}
	if advanced.After(effective) {
		effective = advanced
	}
	return effective
}

func (w CertificateWindow) Intersect(other CertificateWindow) CertificateWindow {
	if other.NotBefore.After(w.NotBefore) {
		w.NotBefore = other.NotBefore
	}
	if other.NotAfter.Before(w.NotAfter) {
		w.NotAfter = other.NotAfter
	}
	return w
}

func (w CertificateWindow) Valid() bool { return w.NotAfter.After(w.NotBefore) }
