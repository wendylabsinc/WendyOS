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
// Uncertain distinguishes that boot state from verified current UTC.
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

// LeafTime applies the existing bounded leaf NotBefore accommodation while
// the explicitly uncertain local clock has not overtaken its local floor.
// Issuer dates and every NotAfter check still use the original wall time.
func (p CertificateTimePolicy) LeafTime(wall, notBefore time.Time) time.Time {
	if p.Floor == nil || p.Uncertain == nil || !p.Uncertain() {
		return wall
	}
	floor := p.Floor()
	return CertificateLeafTime(wall, floor, notBefore, !wall.After(floor))
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
