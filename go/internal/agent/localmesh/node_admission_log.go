package localmesh

import (
	"log"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/timesync"
)

// Admission diagnostics distinguish actual UTC, virtual certificate trust time
// and the receiver's remaining local lease. No announcement body, signature,
// certificate or private provisioning material is logged.
func (n *Node) logAdmission(w SignedManifest, source string, now time.Time) {
	m, err := DecodeManifest(w.Body)
	if err != nil {
		return
	}
	deadline, ok := n.directory.RecordDeadline(w, now)
	if !ok {
		return
	}
	boot, err := timesync.BootTime()
	if err != nil {
		boot = -1
	}
	policy := n.Credentials.CertificateTimePolicy()
	base := policy.BaseTime(policy.WallTime())
	uncertain := policy.Uncertain != nil && policy.Uncertain()
	log.Printf("wendy net announcement admitted origin=%q revision=%d receiver_utc=%s certificate_base=%s issued=%s expires=%s lease_remaining_ns=%d boot_monotonic_ns=%d boot_uncertain=%t source=%s", w.Principal, m.Revision, now.UTC().Format(time.RFC3339Nano), base.UTC().Format(time.RFC3339Nano), time.UnixMilli(m.Issued).UTC().Format(time.RFC3339Nano), time.UnixMilli(m.Expires).UTC().Format(time.RFC3339Nano), deadline.Sub(now).Nanoseconds(), boot, uncertain, source)
}
