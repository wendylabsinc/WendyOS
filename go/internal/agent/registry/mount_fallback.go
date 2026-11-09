package registry

import (
	"context"
	"errors"
	"net/http"

	"cuelabs.dev/go/oci/ociregistry"
	"cuelabs.dev/go/oci/ociregistry/ociserver"
)

// newOCIHandler supplies the OCI-recommended upload fallback for a blob mount
// that our content store cannot satisfy. ociserver otherwise returns 404, which
// stops BuildKit instead of allowing it to upload the missing blob (WDY-4114).
func newOCIHandler(backend ociregistry.Interface) http.Handler {
	uploadHandler := ociserver.New(backend, nil)
	return ociserver.New(mountFallbackBackend{backend}, &ociserver.Options{
		WriteError: func(w http.ResponseWriter, req *http.Request, err error) {
			var missing *missingMountBlobError
			if !errors.As(err, &missing) {
				_ = ociregistry.WriteError(w, err)
				return
			}
			// Only a parsed, validated MountBlob call can produce this marker.
			// Reuse the existing upload handler and keep the request's context.
			upload := req.Clone(req.Context())
			query := upload.URL.Query()
			query.Del("mount")
			query.Del("from")
			// Mount parsing takes precedence over a monolithic-upload digest;
			// the fallback must start a session, not try to commit an empty body.
			query.Del("digest")
			upload.URL.RawQuery = query.Encode()
			uploadHandler.ServeHTTP(w, upload)
		},
	})
}

type mountFallbackBackend struct {
	ociregistry.Interface
}

func (b mountFallbackBackend) MountBlob(ctx context.Context, from, to string, digest ociregistry.Digest) (ociregistry.Descriptor, error) {
	desc, err := b.Interface.MountBlob(ctx, from, to, digest)
	if errors.Is(err, ociregistry.ErrBlobUnknown) {
		return desc, &missingMountBlobError{err}
	}
	return desc, err
}

// Tag the backend operation, not an HTTP status: a 404 from another operation
// or a different mount error must retain its original behavior.
type missingMountBlobError struct {
	error
}
