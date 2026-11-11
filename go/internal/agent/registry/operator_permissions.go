package registry

import (
	"context"
	"crypto/x509"
	"net/http"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/shared/operatorpermission"
)

func operatorAuthorization(policy *operatorpermission.Policy, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var leaf *x509.Certificate
		if r.TLS != nil && r.TLS.HandshakeComplete && len(r.TLS.PeerCertificates) > 0 {
			leaf = r.TLS.PeerCertificates[0]
		}
		// Tenant and operator identity are mandatory even during the permission
		// audit phase. Device staging uses the separately allowlisted gRPC methods.
		if err := interceptor.CheckOperatorCertificate(leaf, policy.Scope, interceptor.OrgModeStrict); err != nil {
			http.Error(w, "operator identity required", http.StatusForbidden)
			return
		}
		permission := ""
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			permission = "device:read"
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			permission = "deployment:create"
		}
		if err := policy.Check(leaf, permission, "registry:"+r.Method); err != nil {
			http.Error(w, "operator permission denied", http.StatusForbidden)
			return
		}
		if policy.Enforce && leaf != nil {
			now := time.Now()
			if policy.Now != nil {
				now = policy.Now()
			}
			ctx, cancel := context.WithTimeout(r.Context(), leaf.NotAfter.Sub(now))
			defer cancel()
			// Interrupt slow upload bodies as well as the containerd operation.
			stop := context.AfterFunc(ctx, func() { _ = r.Body.Close() })
			defer stop()
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}
