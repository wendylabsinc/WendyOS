package registry

import (
	"crypto/mldsa"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
