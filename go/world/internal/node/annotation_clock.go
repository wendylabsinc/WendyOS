package node

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotationclock"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
)

type AnnotationClockConfig struct {
	Credentials Credentials
	RunID       string
	ClockDomain string
}

func (n *Node) configureAnnotationClock() error {
	c := n.cfg.AnnotationClock
	if c == nil {
		return nil
	}
	g := c.Credentials.Grant.Grant
	if n.cfg.Audience == "" || g.Domain != "simulation" || g.Kind != "device" || g.PublishAudience != n.cfg.Audience || g.Tenant != n.cfg.Tenant || g.Site != n.cfg.Site || c.RunID == "" || c.ClockDomain != "simulation:"+c.RunID || len(c.Credentials.PrivateKey) != ed25519.PrivateKeySize || !bytes.Equal(c.Credentials.PrivateKey[32:], g.PublicKey) {
		return errors.New("invalid annotation clock signer")
	}
	if _, err := n.verifier.VerifyGrant(c.Credentials.Grant, "annotation-clock-server"); err != nil {
		return err
	}
	var boot [32]byte
	if _, err := rand.Read(boot[:]); err != nil {
		return err
	}
	n.annotationClockBoot = hex.EncodeToString(boot[:])
	return nil
}
func (n *Node) annotationClock(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !method(w, r, http.MethodPost) {
		return
	}
	if n.cfg.AnnotationClock == nil {
		http.Error(w, "annotation clock unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.RawPath != "" || r.RequestURI != "/v1/annotations/clock" || r.URL.EscapedPath() != "/v1/annotations/clock" {
		http.Error(w, "invalid clock path", http.StatusBadRequest)
		return
	}
	// One exchange in flight and at most twenty started exchanges per second.
	// The bound is global, independent of caller-controlled principal/key values.
	if !n.annotationClockMu.TryLock() {
		http.Error(w, "annotation clock busy", http.StatusTooManyRequests)
		return
	}
	defer n.annotationClockMu.Unlock()
	now := time.Now()
	if !n.annotationClockLast.IsZero() && now.Sub(n.annotationClockLast) < 50*time.Millisecond {
		http.Error(w, "annotation clock rate limit", http.StatusTooManyRequests)
		return
	}
	n.annotationClockLast = now
	_ = http.NewResponseController(w).SetReadDeadline(now.Add(2 * time.Second))
	_ = http.NewResponseController(w).SetWriteDeadline(now.Add(2 * time.Second))
	body, err := io.ReadAll(io.LimitReader(r.Body, (16<<10)+1))
	if err != nil || len(body) > 16<<10 {
		http.Error(w, "invalid clock request size", http.StatusBadRequest)
		return
	}
	var request annotationclock.SignedRequest
	if authority.DecodeStrict(body, &request) != nil {
		http.Error(w, "invalid clock request", http.StatusBadRequest)
		return
	}
	c := n.cfg.AnnotationClock
	response, err := n.verifier.IssueAnnotationClock(request, c.Credentials.Grant, c.Credentials.PrivateKey, c.RunID, c.ClockDomain, n.annotationClockBoot)
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, authority.ErrDenied) {
			status = http.StatusForbidden
		}
		http.Error(w, "annotation clock unavailable", status)
		return
	}
	if r.Context().Err() != nil {
		return
	}
	writeJSON(w, http.StatusOK, response)
}
