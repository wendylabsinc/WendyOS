package registry

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cuelabs.dev/go/oci/ociregistry"
	"cuelabs.dev/go/oci/ociregistry/ocimem"
	"cuelabs.dev/go/oci/ociregistry/ociserver"
	"go.uber.org/zap"
)

// JSON annotations can contain HTML, and a registry retains the uploaded media
// type. Browsers must treat these opaque image contents as untrusted data even
// if a publisher supplies text/html. Escaping bytes would break their digest.
func activeRegistryContent(t *testing.T) (http.Handler, []byte, string) {
	t.Helper()
	payload := []byte(`{"schemaVersion":2,"annotations":{"description":"<script>document.documentElement.dataset.registryScript='executed'</script>"}}`)
	reg := newTestRegistry(t, newTestImageStore())
	desc, err := reg.PushManifest(context.Background(), "review", "latest", payload, "text/html")
	if err != nil {
		t.Fatal(err)
	}
	backend := &ociregistry.Funcs{GetManifest_: func(context.Context, string, ociregistry.Digest) (ociregistry.BlobReader, error) {
		return ocimem.NewBytesReader(payload, desc), nil
	}}
	handler := loggingMiddleware(zap.NewNop())(securityHeadersMiddleware(ociserver.New(backend, nil)))
	return handler, payload, "/v2/review/manifests/" + string(desc.Digest)
}

func TestRegistryContentsCannotOptIntoBrowserExecution(t *testing.T) {
	handler, payload, path := activeRegistryContent(t)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), payload) {
		t.Fatalf("registry response changed image content: status %d, body %q", response.Code, response.Body.Bytes())
	}
	if got := response.Header().Get("Content-Type"); got != "text/html" {
		t.Fatalf("fixture did not exercise the publisher's media type: %q", got)
	}
	policy := response.Header().Get("Content-Security-Policy")
	directives := make(map[string]bool)
	for _, directive := range strings.Split(policy, ";") {
		directives[strings.TrimSpace(directive)] = true
	}
	if !directives["default-src 'none'"] || !directives["sandbox"] {
		t.Fatalf("registry content may execute with the browser's origin: CSP %q", policy)
	}
}
