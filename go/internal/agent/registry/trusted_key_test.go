package registry

import (
	"crypto/mldsa"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
)

func TestTrustedKeyMiddleware(t *testing.T) {
	keys, err := trustedkeys.Open(filepath.Join(t.TempDir(), trustedkeys.FileName))
	if err != nil {
		t.Fatal(err)
	}
	spki := func() []byte {
		k, _ := mldsa.GenerateKey(mldsa.MLDSA65())
		der, _ := x509.MarshalPKIXPublicKey(k.Public())
		return der
	}
	ownerKey, viewerKey := spki(), spki()
	owner, err := keys.Close(ownerKey, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Add(viewerKey, trustedkeys.RoleViewer, "", owner.Fingerprint(), time.Now()); err != nil {
		t.Fatal(err)
	}
	h := trustedKeyMiddleware(keys)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	push := func(key []byte) int {
		r := httptest.NewRequest(http.MethodPost, "/v2/app/blobs/uploads/", nil)
		if key != nil {
			r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{RawSubjectPublicKeyInfo: key}}}
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	if got := push(ownerKey); got != http.StatusAccepted {
		t.Errorf("owner push = %d", got)
	}
	if got := push(viewerKey); got == http.StatusAccepted {
		t.Error("viewer pushed an image")
	}
	if got := push(nil); got == http.StatusAccepted {
		t.Error("plain HTTP push accepted with the keyless set off")
	}
	if err := keys.SetKeylessApps(true, owner.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	if got := push(nil); got != http.StatusAccepted {
		t.Errorf("plain HTTP push with the keyless set on = %d", got)
	}
}

// A Closed device caps manifest bodies like an Open one, keyless or not.
func TestTrustedKeyMiddlewareCapsManifestBodies(t *testing.T) {
	keys, err := trustedkeys.Open(filepath.Join(t.TempDir(), trustedkeys.FileName))
	if err != nil {
		t.Fatal(err)
	}
	k, _ := mldsa.GenerateKey(mldsa.MLDSA65())
	der, _ := x509.MarshalPKIXPublicKey(k.Public())
	owner, err := keys.Close(der, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.SetKeylessApps(true, owner.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	var read int64
	h := trustedKeyMiddleware(keys)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		read, _ = io.Copy(io.Discard, r.Body)
	}))
	body := strings.NewReader(strings.Repeat("x", 2*maxManifestBody))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/v2/app/manifests/latest", body))
	if read > maxManifestBody {
		t.Fatalf("read %d manifest bytes, cap is %d", read, maxManifestBody)
	}
}

// A tag push is checked against the apps made from that image, so the
// middleware hands the caller's role to the backend.
func TestTrustedKeyMiddlewarePassesTheCallerRole(t *testing.T) {
	keys, err := trustedkeys.Open(filepath.Join(t.TempDir(), trustedkeys.FileName))
	if err != nil {
		t.Fatal(err)
	}
	k, _ := mldsa.GenerateKey(mldsa.MLDSA65())
	ownerKey, _ := x509.MarshalPKIXPublicKey(k.Public())
	owner, err := keys.Close(ownerKey, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := keys.SetKeylessApps(true, owner.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	var got trustedkeys.Role
	var closed bool
	h := trustedKeyMiddleware(keys)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, closed = trustedkeys.CallerRole(r.Context())
	}))
	push := func(key []byte) {
		got, closed = -1, false
		r := httptest.NewRequest(http.MethodPut, "/v2/app/manifests/latest", nil)
		if key != nil {
			r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{RawSubjectPublicKeyInfo: key}}}
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if push(nil); !closed || got != trustedkeys.RoleNone {
		t.Errorf("keyless push: role=%v closed=%v", got, closed)
	}
	if push(ownerKey); !closed || got != trustedkeys.RoleOwner {
		t.Errorf("owner push: role=%v closed=%v", got, closed)
	}
}
